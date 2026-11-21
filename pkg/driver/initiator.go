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
	"context"
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
	// iscsiReplacementTimeout is node.session.timeo.replacement_timeout in
	// seconds. 0 leaves the value iscsid is configured with untouched.
	iscsiReplacementTimeout int
	// tuning holds what the replacement timeout needs besides the value: shared
	// by every copy of tools, since they all see the same sessions.
	tuning *sessionTuning
}

// ToolsOption customizes the tools created by NewTools.
type ToolsOption func(*tools)

// WithIscsiReplacementTimeout makes the driver set
// node.session.timeo.replacement_timeout (seconds) on the iSCSI sessions it
// manages. 0 leaves the host's iscsid configuration in charge.
func WithIscsiReplacementTimeout(seconds int) ToolsOption {
	return func(t *tools) { t.iscsiReplacementTimeout = seconds }
}

// WithContext bounds the background work started for the replacement timeout,
// i.e. retrying sessions that were not ready to take it. Without it that work
// runs until the process exits.
func WithContext(ctx context.Context) ToolsOption {
	return func(t *tools) { t.tuning.ctx = ctx }
}

// NewTools creates a new tools wrapper for calling utilities with given executor
func NewTools(executor hostexec.Executor, opts ...ToolsOption) tools {
	t := tools{executor: executor, tuning: newSessionTuning()}
	for _, opt := range opts {
		opt(&t)
	}
	return t
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

		// Not every target name has a ':' (eui. and naa. names don't). The
		// node plugin now looks at every session on the host at startup, not
		// just ones for volumes it staged, so don't index past the split.
		name := ""
		if parts := strings.SplitN(e[3], ":", 2); len(parts) == 2 {
			name = parts[1]
		}

		s := iscsiSession{
			Protocol: protocol,
			Id:       int32(id64),
			Portal:   portal,
			Iqn:      e[3],
			Name:     name,
		}
		sessions = append(sessions, s)
	}

	return sessions
}

// iscsiadm_session lists the active sessions. Having none is not an error, but
// failing to find out is: callers must not read it as "no session", or a logout
// would be skipped for a session that is still there.
func (t *tools) iscsiadm_session() ([]iscsiSession, error) {
	cmd := t.iscsiadm("-m", "session")
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitErr, ok := err.(utilexec.ExitError)
		if ok && exitErr.ExitStatus() == 21 { // iscsiadm: No active sessions
			log.Info("No active iscsi session found.")
			return []iscsiSession{}, nil
		}
		return nil, fmt.Errorf("failed to run iscsiadm session: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	return parseSessions(string(out)), nil
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

// iscsiadm_update_node_replacement_timeout sets how long the kernel queues I/O
// for a session whose connection is down before failing it up to the
// filesystem. The value is stored in the node record and applied at login.
//
// iscsid's built-in default is 120s and it normally comes from iscsid.conf,
// but some distros (Talos' iscsi-tools extension, for one) ship no iscsid.conf,
// so a network outage longer than two minutes fails pending I/O and ext4
// aborts its journal. Setting it here lets the admin choose the value per
// driver instead of depending on the host's configuration.
func (t *tools) iscsiadm_update_node_replacement_timeout(iqn, portal string, seconds int) error {
	cmd := t.iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--portal", portal,
		"--op", "update",
		"--name", "node.session.timeo.replacement_timeout",
		"--value", strconv.Itoa(seconds))
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

func (t *tools) hasSession(targetIqn string, portal string) (bool, error) {
	sessions, err := t.iscsiadm_session()
	if err != nil {
		return false, err
	}

	for _, s := range sessions {
		if targetIqn == s.Iqn && (portal == s.Portal || portal == "") {
			return true, nil
		}
	}

	return false, nil
}

// findSessions returns the sessions to a target, limited to one portal unless
// portal is empty.
func (t *tools) findSessions(targetIqn string, portal string) (matchedSessions []iscsiSession, err error) {
	sessions, err := t.iscsiadm_session()
	if err != nil {
		return nil, err
	}

	for _, s := range sessions {
		if targetIqn == s.Iqn && (portal == s.Portal || portal == "") {
			matchedSessions = append(matchedSessions, s)
		}
	}

	return matchedSessions, nil
}

func (t *tools) listSessionsByIqn(targetIqn string) (matchedSessions []iscsiSession, err error) {
	sessions, err := t.iscsiadm_session()
	if err != nil {
		return nil, err
	}

	for _, s := range sessions {
		if targetIqn == s.Iqn {
			matchedSessions = append(matchedSessions, s)
		}
	}

	return matchedSessions, nil
}

// login logs in to the target through one portal. tuneReplacementTimeout says
// whether the configured replacement timeout may be applied to the session; it
// is false when the target is logged in through several portals, because
// multipathd then owns the failover timing (see loginTarget).
func (d *initiatorDriver) login(targetIqn string, portal string, tuneReplacementTimeout bool) error {
	tune := tuneReplacementTimeout && d.tools.iscsiReplacementTimeout > 0

	sessions, err := d.tools.findSessions(targetIqn, portal)
	if err != nil {
		// Login proceeds as it always has; if the session exists, iscsiadm
		// reports that itself.
		log.Warnf("Couldn't list the iSCSI sessions: %v", err)
	}
	if len(sessions) > 0 {
		log.Infof("Session[%s] already exists.", targetIqn)
		if tune {
			// Staged before the timeout was configured, e.g. before an upgrade.
			d.tools.applyReplacementTimeout(sessions, false)
		}
		return nil
	}

	if err := d.tools.iscsiadm_discovery(portal); err != nil {
		log.Errorf("Failed to discover portal [%s]: %v", portal, err)
		return err
	}

	// Has to happen after discovery, which creates the node record, and before
	// login, which is when the record is read into the session.
	// A target that already has a session through another portal is multipath's
	// to tune, the same rule targetTunable applies to existing sessions. When
	// the sessions can't be listed that can't be ruled out, so don't tune.
	if tune {
		others, err := d.tools.listSessionsByIqn(targetIqn)
		switch {
		case err != nil:
			log.Warnf("Not setting the replacement timeout of target [%s] for portal [%s]: couldn't tell whether the target "+
				"is already logged in through another portal: %v", targetIqn, portal, err)
			tune = false
		case len(others) > 0:
			log.Infof("Not setting the replacement timeout of target [%s] for portal [%s]: the target is already logged in "+
				"through another portal, so multipathd decides the failover timing.", targetIqn, portal)
			tune = false
		}
	}
	if tune {
		if err := d.tools.iscsiadm_update_node_replacement_timeout(targetIqn, portal, d.tools.iscsiReplacementTimeout); err != nil {
			log.Warnf("Failed to update target node.session.timeo.replacement_timeout to %d: %v",
				d.tools.iscsiReplacementTimeout, err)
		}
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

	exists, err := d.tools.hasSession(targetIqn, "")
	if err != nil {
		return err
	}
	if exists {
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
	exists, err := d.tools.hasSession(targetIqn, "")
	if err != nil {
		return err
	}
	if !exists {
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
