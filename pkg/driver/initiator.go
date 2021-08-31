// Copyright 2021 Synology Inc.

package driver

import (
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	utilexec "k8s.io/utils/exec"
)

type initiatorDriver struct {
	chapUser string
	chapPassword string
}

const (
	ISCSIPort = 3260
)

func iscsiadm(cmdArgs ...string) utilexec.Cmd {
	executor := utilexec.New()

	return executor.Command("iscsiadm", cmdArgs...)
}

func iscsiadm_session() string {
	cmd := iscsiadm("-m", "session")
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitErr, ok := err.(utilexec.ExitError)
		if ok && exitErr.ExitStatus() == 21 { // iscsiadm: No active sessions
			log.Info("No active iscsi session found.")
		} else {
			log.Errorf("Failed to run iscsiadm session: %v", err)
		}
		return ""
	}

	return string(out)
}

func iscsiadm_discovery(ip string) error {
	cmd := iscsiadm(
		"-m", "discoverydb",
		"--type", "sendtargets",
		"--portal", ip,
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

func hasSession(targetIqn string) bool{
	sessions := iscsiadm_session();

	for _, line := range strings.Split(sessions, "\n") {
		if strings.Contains(line, targetIqn) {
			return true
		}
	}

	return false
}

func (d *initiatorDriver) login(targetIqn string, ip string) error{
	portal := fmt.Sprintf("%s:%d", ip, ISCSIPort)

	if (hasSession(targetIqn)) {
		log.Infof("Session[%s] already exists.", targetIqn)
		return nil
	}

	if err := iscsiadm_discovery(ip); err != nil {
		log.Errorf("Failed in discovery of the target: %v", err)
		return err
	}

	if err := iscsiadm_login(targetIqn, portal); err != nil {
		log.Errorf("Failed in login of the target: %v", err)
		return err
	}

	log.Infof("Login target portal [%s], iqn [%s].", portal, targetIqn)

	return nil
}

func (d *initiatorDriver) logout(targetIqn string, ip string) error{
	if (!hasSession(targetIqn)) {
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

func (d *initiatorDriver) rescan(targetIqn string) error{
	if (!hasSession(targetIqn)) {
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
