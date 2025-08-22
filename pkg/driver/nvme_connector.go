/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	utilexec "k8s.io/utils/exec"
	log "github.com/sirupsen/logrus"
)

// Namespace represents a single NVMe namespace device visible to the node.
type Namespace struct {
	DevPath    string // e.g. /dev/nvme1n1
	Subsystem  string // e.g. nqn.2014-08.org.nvmexpress:uuid:...
	Uuid       string
}

var (
	nvmeNamespaceRegex = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)
	nvmeRegex          = regexp.MustCompile(`^nvme[0-9]+$`)
)

const (
	NVME_SUBSYSTEM_PATH = "/sys/class/nvme-subsystem"
	NVME_FABRICS_PATH   = "/sys/class/nvme-fabrics/ctl"
	NVMePort = 4420
)

func (t *tools) nvme(cmdArgs ...string) utilexec.Cmd {
	return t.executor.Command("nvme", cmdArgs...)
}

func (t *tools) nvmeDiscover(ip string, port int, transport string, hostNqn string) error {
	args := []string{"discover", "-t", transport, "-a", ip, "-s", fmt.Sprint(port)}
	if hostNqn != "" {
		args = append(args, "--hostnqn", hostNqn)
	}

	cmd := t.nvme(args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *tools) nvmeConnect(ip string, port int, transport string, nqn string, hostNqn string) error {
	args := []string{"connect", "-t", transport, "-a", ip, "-s", fmt.Sprint(port), "-n", nqn}
	if hostNqn != "" {
		args = append(args, "--hostnqn", hostNqn)
	}

	cmd := t.nvme(args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *tools) nvmeDisconnect(nqn string) error {
	cmd := t.nvme("disconnect", "-n", nqn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func readTrimFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warnf("Failed to read %s: %v", path, err)
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// listNamespacesFromSysfs lists all namespaces for a given subsystem NQN
func listNamespacesFromSysfs(subsysNqn string) ([]Namespace, error) {
	subsysDirs, err := os.ReadDir(NVME_SUBSYSTEM_PATH)
	if err != nil {
		return nil, fmt.Errorf("Failed to read %s: %v", NVME_SUBSYSTEM_PATH, err)
	}

	var res []Namespace
	for _, sd := range subsysDirs {
		subsysPath := filepath.Join(NVME_SUBSYSTEM_PATH, sd.Name())

		nqn, err := readTrimFile(filepath.Join(subsysPath, "subsysnqn"))
		if err != nil || nqn != subsysNqn {
			continue
		}

		nsDirs, err := os.ReadDir(subsysPath)
		if err != nil {
			continue
		}

		for _, nsDir := range nsDirs {
			name := nsDir.Name()
			if nvmeNamespaceRegex.MatchString(name) {
				nsPath := filepath.Join(subsysPath, name)

				uuid, err := readTrimFile(filepath.Join(nsPath, "uuid"))
				if err != nil {
					continue
				}

				devPath := filepath.Join("/dev", name)
				res = append(res, Namespace{
					DevPath:   devPath,
					Subsystem: nqn,
					Uuid:      uuid,
				})
			}
		}
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("No namespaces found for subsystem %s", subsysNqn)
	}
	return res, nil
}

func hasNVMeSession(ip string, port int, transport string, nqn string) bool {
	dirs, err := os.ReadDir(NVME_FABRICS_PATH)
	if err != nil {
		log.Errorf("Failed to read %s: %v", NVME_FABRICS_PATH, err)
		return false
	}

	for _, dir := range dirs {
		if !nvmeRegex.MatchString(dir.Name()) {
			continue
		}
		sessionPath := filepath.Join(NVME_FABRICS_PATH, dir.Name())

		nqnStr, err := readTrimFile(filepath.Join(sessionPath, "subsysnqn"))
		if err != nil || nqnStr != nqn {
			continue
		}

		transportStr, err := readTrimFile(filepath.Join(sessionPath, "transport"))
		if err != nil || transportStr != transport {
			continue
		}

		addrStr, err := readTrimFile(filepath.Join(sessionPath, "address"))
		// e.g. traddr=192.0.2.10,trsvcid=4420,src_addr=198.51.100.20
		if err != nil {
			continue
		}
		if strings.Contains(addrStr, fmt.Sprintf("traddr=%s", ip)) &&
			strings.Contains(addrStr, fmt.Sprintf("trsvcid=%d", port)) {
			log.Infof("NVMe session[%s -> %s:%d] already exists.", nqn, ip, port)
			return true
		}
	}
	return false
}