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
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	utilexec "k8s.io/utils/exec"
)

type initiatorDriver struct {
	chapUser     string
	chapPassword string
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

func iscsiadm(cmdArgs ...string) utilexec.Cmd {
	executor := utilexec.New()

	return executor.Command("iscsiadm", cmdArgs...)
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

func iscsiadm_session() []iscsiSession {
	cmd := iscsiadm("-m", "session")
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

func iscsiadm_discovery(portal string) error {
	cmd := iscsiadm(
		"-m", "discoverydb",
		"--type", "sendtargets",
		"--portal", portal,
		"--discover")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

func iscsiadm_login(iqn, portal string) error {
	cmd := iscsiadm(
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

func iscsiadm_update_node_startup(iqn, portal string) error {
	cmd := iscsiadm(
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

func iscsiadm_logout(iqn string) error {
	cmd := iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"--logout")
	if _, err := cmd.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func iscsiadm_rescan(iqn string) error {
	cmd := iscsiadm(
		"-m", "node",
		"--targetname", iqn,
		"-R")
	if _, err := cmd.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func hasSession(targetIqn string, portal string) bool {
	sessions := iscsiadm_session()

	for _, s := range sessions {
		if targetIqn == s.Iqn && (portal == s.Portal || portal == "") {
			return true
		}
	}

	return false
}

func listSessionsByIqn(targetIqn string) (matchedSessions []iscsiSession) {
	sessions := iscsiadm_session()

	for _, s := range sessions {
		if targetIqn == s.Iqn {
			matchedSessions = append(matchedSessions, s)
		}
	}

	return matchedSessions
}

func (d *initiatorDriver) login(targetIqn string, portal string) error {
	if hasSession(targetIqn, portal) {
		log.Infof("Session[%s] already exists.", targetIqn)
		return nil
	}

	if err := iscsiadm_discovery(portal); err != nil {
		log.Errorf("Failed in discovery of the target: %v", err)
		return err
	}

	if err := iscsiadm_login(targetIqn, portal); err != nil {
		log.Errorf("Failed in login of the target: %v", err)
		return err
	}

	if err := iscsiadm_update_node_startup(targetIqn, portal); err != nil {
		log.Warnf("Failed to update target node.startup to manual: %v", err)
	}

	log.Infof("Login target portal [%s], iqn [%s].", portal, targetIqn)

	return nil
}

func (d *initiatorDriver) logout(targetIqn string, ip string) error {
	if !hasSession(targetIqn, "") {
		log.Infof("Session[%s] doesn't exist.", targetIqn)
		return nil
	}

	portal := fmt.Sprintf("%s:%d", ip, ISCSIPort)
	if err := iscsiadm_logout(targetIqn); err != nil {
		log.Errorf("Failed in logout of the target.\nTarget [%s], Portal [%s], Err[%v]",
			targetIqn, portal, err)
		return err
	}

	log.Infof("Logout target portal [%s], iqn [%s].", portal, targetIqn)

	return nil
}

func (d *initiatorDriver) rescan(targetIqn string) error {
	if !hasSession(targetIqn, "") {
		msg := fmt.Sprintf("Session[%s] doesn't exist.", targetIqn)
		log.Error(msg)
		return errors.New(msg)
	}

	if err := iscsiadm_rescan(targetIqn); err != nil {
		log.Errorf("Failed in rescan of the target.\nTarget [%s], Err[%v]",
			targetIqn, err)
		return err
	}

	log.Infof("Rescan target iqn [%s].", targetIqn)

	return nil
}
