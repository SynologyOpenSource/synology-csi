/*
Copyright 2021 Synology Inc.

Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils/hostexec"
	log "github.com/sirupsen/logrus"
	utilexec "k8s.io/utils/exec"
)

type initiatorDriver struct {
	chapUser     string
	chapPassword string
	tools        tools
}

type iscsiSession struct {
	Protocol string
	Id       int32
	Portal   string
	Iqn      string
	Name     string
}

const (
	ISCSIPort = 3260
)

type tools struct {
	executor hostexec.Executor
}

// NewTools creates a new tools wrapper for calling utilities with given executor
func NewTools(executor hostexec.Executor) tools {
	return tools{
		executor: executor,
	}
}

func (t *tools) iscsiadm(cmdArgs ...string) utilexec.Cmd {
	return t.executor.Command("iscsiadm", cmdArgs...)
}

// parseSession takes the raw stdout from the `iscsiadm -m session` command and encodes it into an iSCSI session type
func parseSessions(lines string) []iscsiSession {
	entries := strings.Split(strings.TrimSpace(lines), "\n")
	r := strings.NewReplacer("[", "",
		"]", "")

	var sessions []iscsiSession
	for _, entry := range entries {
		e := strings.Fields(entry)
		if len(e) < 4 {
			continue
		}
		protocol := strings.Split(e[0], ":")[0]
		id := r.Replace(e[1])
		id64, _ := strconv.ParseInt(id, 10, 32)
		portal := strings.Split(e[2], ",")[0]

		s := iscsiSession{
			Protocol: protocol,
			Id:       int32(id64),
			Portal:   portal,
			Iqn:      e[3],
			Name:     strings.Split(e[3], ":")[1],
		}
		sessions = append(sessions, s)
	}

	return sessions
}

func (t *tools) iscsiadm_session() []iscsiSession {
	cmd := t.iscsiadm("-m", "session")
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitErr, ok := err.(utilexec.ExitError)
		if ok && exitErr.ExitStatus() == 21 { // iscsiadm: No active sessions
			log.Info("No active iscsi session found.")
		} else {
			log.Errorf("Failed to run iscsiadm session: %v", err)
		}
		return []iscsiSession{}
	}

	return parseSessions(string(out))
}

func (t *tools) iscsiadm_discovery(portal string) error {
	cmd := t.iscsiadm(
		"-m", "discoverydb",
		"--type", "sendtargets",
		"--portal", portal,
		"--op", "new",
		"--discover")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

func (t *tools) iscsiadm_login(iqn, portal string) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--portal", portal,
		"--login")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

func (t *tools) iscsiadm_update_node_startup(iqn, portal string) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--portal", portal,
		"--op", "update",
		"--name", "node.startup",
		"--value", "manual")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

func (t *tools) iscsiadm_logout(iqn string) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--logout")
	if _, err := cmd.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

// iscsiadm_delete_node removes the node record for a target. The record is
// separate from the session: logging out ends the session but leaves the record
// behind, and iscsid then retries the login every 60 seconds against a target
// DSM has already deleted. Reported in #138, where a node kept doing that
// across reboots until the record was removed by hand.
func (t *tools) iscsiadm_delete_node(iqn string, portal string) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--portal", portal,
		"-o", "delete")
	if _, err := cmd.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

// blockdev_flushbufs writes out anything the kernel still holds for a block
// device and drops its buffers.
//
// Raw block volumes are written through the page cache with no filesystem to
// flush them, so the data reaches the LUN only when the last opener closes it.
// Nothing in unstage waited for that before tearing the session down, which
// leaves the write racing the next node's read. The in-tree iSCSI plugin does
// the same thing on its detach path. It is a no-op on a device that is already
// clean.
func (t *tools) blockdev_flushbufs(devPath string) error {
	cmd := t.executor.Command("blockdev", "--flushbufs", devPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

func (t *tools) iscsiadm_rescan(iqn string) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"-R")
	if _, err := cmd.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func (t *tools) hasSession(targetIqn string, portal string) bool {
	sessions := t.iscsiadm_session()

	for _, s := range sessions {
		if targetIqn == s.Iqn && (portal == s.Portal || portal == "") {
			return true
		}
	}

	return false
}

func (t *tools) listSessionsByIqn(targetIqn string) (matchedSessions []iscsiSession) {
	sessions := t.iscsiadm_session()

	for _, s := range sessions {
		if targetIqn == s.Iqn {
			matchedSessions = append(matchedSessions, s)
		}
	}

	return matchedSessions
}

func (d *initiatorDriver) login(targetIqn string, portal string) error {
	if d.tools.hasSession(targetIqn, portal) {
		log.Infof("Session[%s] already exists.", targetIqn)
		return nil
	}

	if err := d.tools.iscsiadm_discovery(portal); err != nil {
		log.Errorf("Failed to discover portal [%s]: %v", portal, err)
		return err
	}

	if err := d.tools.iscsiadm_login(targetIqn, portal); err != nil {
		log.Errorf("Failed in login of the target: %v", err)
		return err
	}

	if err := d.tools.iscsiadm_update_node_startup(targetIqn, portal); err != nil {
		log.Warnf("Failed to update target node.startup to manual: %v", err)
	}

	log.Infof("Login target portal [%s], iqn [%s].", portal, targetIqn)

	return nil
}

func (d *initiatorDriver) logout(targetIqn string, ip string) error {
	portal := fmt.Sprintf("%s:%d", ip, ISCSIPort)

	if d.tools.hasSession(targetIqn, "") {
		if err := d.tools.iscsiadm_logout(targetIqn); err != nil {
			log.Errorf("Failed in logout of the target.\nTarget [%s], Portal [%s], Err[%v]",
				targetIqn, portal, err)
			return err
		}
		log.Infof("Logout target portal [%s], iqn [%s].", portal, targetIqn)
	} else {
		log.Infof("Session[%s] doesn't exist.", targetIqn)
	}

	// Deliberately outside the session check: the case that leaves iscsid
	// retrying forever is exactly the one where the session is already gone but
	// the record is not, so returning early on "no session" would skip the
	// cleanup precisely when it is needed.
	if err := d.tools.iscsiadm_delete_node(targetIqn, portal); err != nil {
		// Not fatal. The volume is detached either way; what is left behind is
		// a record that makes noise rather than one that breaks anything.
		log.Warnf("Failed to remove the iSCSI node record for target [%s] portal [%s], "+
			"iscsid may keep retrying the login: %v", targetIqn, portal, err)
	}

	return nil
}

func (d *initiatorDriver) rescan(targetIqn string) error {
	if !d.tools.hasSession(targetIqn, "") {
		msg := fmt.Sprintf("Session[%s] doesn't exist.", targetIqn)
		log.Error(msg)
		return errors.New(msg)
	}

	if err := d.tools.iscsiadm_rescan(targetIqn); err != nil {
		log.Errorf("Failed in rescan of the target.\nTarget [%s], Err[%v]",
			targetIqn, err)
		return err
	}

	log.Infof("Rescan target iqn [%s].", targetIqn)

	return nil
}

// fsyncDevice opens a block device and fsyncs it, which makes the kernel
// write back its dirty pages and then send a SYNCHRONIZE CACHE to the target
// (the LUNs advertise write-back caching, so the flush is not elided).
//
// It exists because BLKFLSBUF alone is not enough: the DSM's iSCSI backstore
// acknowledges writes into the DSM's own page cache and never fsyncs them, so
// after the pre-logout flush the data exists only in the DSM's memory. Taking
// a snapshot or clone of the LUN in that state can lose the data from the
// source LUN while the copy keeps it -- measured directly: a 1MiB write left
// the backing file at 0 allocated blocks until this fsync, which brought it
// to 2048. CSI unstage always precedes the snapshot and clone operations
// Kubernetes tests do, so syncing here closes that window.
func (t *tools) fsyncDevice(devPath string) error {
	f, err := os.OpenFile(devPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
