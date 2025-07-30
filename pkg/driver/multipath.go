/*
Copyright 2022 Synology Inc.

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
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	utilexec "k8s.io/utils/exec"
)

// IsMultipathEnabled returns true if multipath is enabled

func (t *tools) IsMultipathEnabled() bool {
	return MultipathEnabled && t.multipathd_is_running()
}

func (t *tools) multipathd_is_running() bool {
	cmd := t.executor.Command("multipathd", "show", "daemon")
	out, err := cmd.CombinedOutput()
	if err == nil {
		matched, _ := regexp.MatchString(`pid \d+ (running|idle)`, string(out))
		if matched {
			return true
		}
	}

	log.Errorf("multipathd is not running. %s", strings.TrimSpace(string(out)))
	return false
}

// execute a command with a timeout and returns an error if timeout is exceeded
func (t *tools) execWithTimeout(command string, args []string, timeout time.Duration) ([]byte, error) {
	log.Infof("Executing command '%v' with args: '%v'.", command, args)

	// Create a new context and add a timeout to it
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := t.executor.CommandContext(ctx, command, args...)
	out, err := cmd.Output()
	log.Debug(err)

	// We want to check the context error to see if the timeout was executed.
	// The error returned by cmd.Output() will be OS specific based on what
	// happens when a process is killed.
	if ctx.Err() == context.DeadlineExceeded {
		log.Debugf("Command '%s' timeout reached.", command)
		return nil, ctx.Err()
	}

	if err != nil {
		if ee, ok := err.(utilexec.ExitError); ok {
			log.Errorf("Non-zero exit code: %s", err)
			err = fmt.Errorf("%s", ee.ExitStatus())
		}
	}

	log.Debugf("Finished executing command.")
	return out, err
}

// resize a multipath device based on its underlying devices
func (t *tools) multipath_resize(devName string) error {
	cmd := t.executor.Command("multipathd", "resize", "map", devName) // use devName not devPath, or it'll fail
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", string(out), err)
	}
	return nil
}

// flushes a multipath device dm-x with command multipath -f /dev/dm-x
func (t *tools) multipath_flush(devPath string) error {
	timeout := 5 * time.Second
	out, err := t.execWithTimeout("multipath", []string{"-f", devPath}, timeout)
	if err != nil {
		if _, e := os.Stat(devPath); os.IsNotExist(e) {
			log.Debugf("Multipath device %v has been removed.", devPath)
		} else {
			return fmt.Errorf("%s (%v)", string(out), err)
		}
	}
	return nil
}

// Device contains information about a device
type Device struct {
	Name      string   `json:"name"`
	Hctl      string   `json:"hctl"`
	Children  []Device `json:"children"`
	Type      string   `json:"type"`
	Transport string   `json:"tran"`
	Size      string   `json:"size,omitempty"`
}

// returns a multipath device for the configured targets if it exists
func GetMultipathDevice(devices []Device) (*Device, error) {
	var multipathDevice *Device

	for _, device := range devices {
		if len(device.Children) != 1 {
			return nil, fmt.Errorf("device is not mapped to exactly one multipath device: %v", device.Children)
		}
		if multipathDevice != nil && device.Children[0].Name != multipathDevice.Name {
			return nil, fmt.Errorf("devices don't share a common multipath device: %v", devices)
		}
		multipathDevice = &device.Children[0]
	}

	if multipathDevice == nil {
		return nil, fmt.Errorf("multipath device not found")
	}

	if multipathDevice.Type != "mpath" {
		return nil, fmt.Errorf("device is not of mpath type: %v", multipathDevice)
	}

	return multipathDevice, nil
}

// lsblk execute the lsblk commands
func lsblk(devicePaths []string, strict bool) ([]Device, error) {
	executor := utilexec.New()
	cmd := executor.Command("lsblk", append([]string{"-rn", "-o", "NAME,KNAME,PKNAME,HCTL,TYPE,TRAN,SIZE"}, devicePaths...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(utilexec.ExitError); ok {
			err = fmt.Errorf("%s", strings.Trim(ee.Error(), "\n"))
			if strict || ee.ExitStatus() != 64 { // ignore the error if some devices have been found when not strict
				return nil, err
			}
			log.Debugf("Could find only some devices: %v", err)
		} else {
			return nil, err
		}
	}

	var devices []*Device
	devicesMap := make(map[string]*Device)
	pkNames := []string{}

	// Parse devices
	lines := strings.Split(strings.Trim(string(out), "\n"), "\n")
	for _, line := range lines {
		columns := strings.Split(line, " ")
		if len(columns) < 5 {
			return nil, fmt.Errorf("invalid output from lsblk: %s", line)
		}
		device := &Device{
			Name:      columns[0],
			Hctl:      columns[3],
			Type:      columns[4],
			Transport: columns[5],
			Size:      columns[6],
		}
		devices = append(devices, device)
		pkNames = append(pkNames, columns[2])
		devicesMap[columns[1]] = device
	}

	// Reconstruct devices tree
	for i, pkName := range pkNames {
		if pkName == "" {
			continue
		}

		device := devices[i]
		parent, ok := devicesMap[pkName]
		if !ok {
			return nil, fmt.Errorf("invalid output from lsblk: parent device %q not found", pkName)
		}
		if parent.Children == nil {
			parent.Children = []Device{}
		}
		parent.Children = append(devicesMap[pkName].Children, *device)
	}

	// Filter devices to keep only the roots of the tree
	var deviceInfo []Device
	for i, device := range devices {
		if pkNames[i] == "" {
			deviceInfo = append(deviceInfo, *device)
		}
	}

	return deviceInfo, nil
}
