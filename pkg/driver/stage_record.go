/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"k8s.io/mount-utils"
)

// stageRecordTempPrefix marks the files a record is written through. They live
// in the record directory next to the records and are never read, so one left
// behind by a crash only needs sweeping.
const stageRecordTempPrefix = ".tmp-"

// iscsiStageRecord is what NodeUnstageVolume needs to know about an iSCSI
// volume to tear it down without asking DSM.
//
// NodeUnstageVolume is handed a volume ID and a path, nothing else, and the
// target's IQN is derived from the PV name and the DSM's hostname -- neither of
// which the node has once DSM has forgotten the volume. A node that staged the
// volume is the only one who ever saw the IQN, so it writes it down.
//
// Only filesystem volumes are recorded. A raw block volume is logged in to by
// NodePublishVolume and never staged, so there is nothing for unstage to find
// and it is left out of this.
type iscsiStageRecord struct {
	VolumeId string `json:"volumeId"`
	Iqn      string `json:"iqn"`
	// MappingIndex is the LUN number, which is how the device is found for the
	// pre-logout flush.
	MappingIndex int `json:"mappingIndex"`
}

// validate is what a record has to satisfy before anything is done on its
// word: it names a target this driver could have logged in to, and it is the
// record of the volume it was looked up for.
func (r *iscsiStageRecord) validate(volumeId string) error {
	if volumeId == "" {
		return errors.New("it is not for any volume")
	}
	if r.VolumeId != volumeId {
		return fmt.Errorf("it is for volume[%s], not volume[%s]", r.VolumeId, volumeId)
	}
	if !strings.HasPrefix(r.Iqn, "iqn.") {
		return fmt.Errorf("%q is not an iSCSI target name", r.Iqn)
	}
	if r.MappingIndex < 0 {
		return fmt.Errorf("%d is not a LUN number", r.MappingIndex)
	}
	return nil
}

// stageRecords keeps the iscsiStageRecords of one node, a file per volume in a
// directory the driver owns.
//
// The directory is not the staging path's parent: kubelet owns that, and removes
// it after a failed stage or a successful unstage with a plain rmdir, which a
// file of ours would defeat. It has to be on the host and survive the driver's
// pod restarting, so by default it is a subdirectory of the plugin directory,
// which the node pod already mounts from the host for the CSI socket.
//
// The zero value has no directory and keeps nothing.
type stageRecords struct {
	dir string
}

// path is the record file of a volume. The volume ID is escaped rather than
// trusted to be a file name; escaping, unlike replacing, cannot make two IDs
// share a record.
func (r stageRecords) path(volumeId string) string {
	var name strings.Builder
	for i := 0; i < len(volumeId); i++ {
		c := volumeId[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			name.WriteByte(c)
		case c == '.' && i > 0: // not first, so that no ID can name "." or ".."
			name.WriteByte(c)
		default:
			fmt.Fprintf(&name, "%%%02X", c)
		}
	}
	return filepath.Join(r.dir, name.String()+".json")
}

// sweep removes the temporary files a crash in the middle of a write left
// behind. It is for startup, when nothing is writing.
func (r stageRecords) sweep() {
	if r.dir == "" {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stageRecordTempPrefix) {
			if err := os.Remove(filepath.Join(r.dir, e.Name())); err != nil {
				log.Warnf("Couldn't remove the stale stage record file %s: %v", e.Name(), err)
			}
		}
	}
}

// write replaces the record atomically: a torn file would read back as garbage
// at the one moment it is needed.
func (r stageRecords) write(rec iscsiStageRecord) error {
	if r.dir == "" {
		return errors.New("no stage record directory is configured")
	}
	if err := rec.validate(rec.VolumeId); err != nil {
		return fmt.Errorf("refusing to record a bad target: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(r.dir, stageRecordTempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has succeeded

	// CreateTemp already makes the file 0600; stated anyway because the mode is
	// part of what this record promises.
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path(rec.VolumeId))
}

// read returns nil, nil when there is no record, which is the normal state of a
// volume staged by a driver that did not write one. A record that is there but
// cannot be used is an error.
func (r stageRecords) read(volumeId string) (*iscsiStageRecord, error) {
	if r.dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(r.path(volumeId))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var rec iscsiStageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if err := rec.validate(volumeId); err != nil {
		return nil, err
	}
	return &rec, nil
}

// remove has to wait until the teardown it served is complete: the record is
// all a retry has to go on.
func (r stageRecords) remove(volumeId string) error {
	if r.dir == "" {
		return nil
	}
	if err := os.Remove(r.path(volumeId)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// iscsiTarget is a target and LUN as the kernel reports them for a device.
type iscsiTarget struct {
	Iqn string
	Lun int
}

// mountedIscsiTarget finds the iSCSI target behind whatever is mounted at
// mountPath, for volumes staged by a driver that left no record.
//
// It has to run before the unmount: afterwards nothing says which device the
// path used to hold.
func mountedIscsiTarget(mounter mount.Interface, mountPath, sysfs string) (*iscsiTarget, error) {
	device, _, err := mount.GetDeviceNameFromMount(mounter, mountPath)
	if err != nil {
		return nil, err
	}
	if device == "" {
		return nil, fmt.Errorf("no device is mounted at %s", mountPath)
	}
	return iscsiTargetOfDevice(device, sysfs)
}

// iscsiTargetOfDevice reads the target and LUN behind a block device from the
// kernel's own view, the same way verifyIscsiDevice does: the device's sysfs
// path names its iSCSI session, and the session publishes the target IQN.
// Unlike by-path links and iscsiadm output this still works once DSM has
// deleted the target, and needs nothing but the device.
//
// A multipath device is resolved through its legs, which must all agree.
func iscsiTargetOfDevice(devPath, sysfs string) (*iscsiTarget, error) {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		// The node may already be gone while its sysfs entry is not.
		real = devPath
	}

	names := []string{filepath.Base(real)}
	if strings.HasPrefix(filepath.Base(real), "dm-") {
		slaves, err := os.ReadDir(filepath.Join(sysfs, "block", filepath.Base(real), "slaves"))
		if err != nil {
			return nil, fmt.Errorf("couldn't list the slaves of %s: %w", real, err)
		}
		names = names[:0]
		for _, s := range slaves {
			names = append(names, s.Name())
		}
	}

	var found *iscsiTarget
	for _, name := range names {
		canonical, err := filepath.EvalSymlinks(filepath.Join(sysfs, "block", name, "device"))
		if err != nil {
			return nil, fmt.Errorf("couldn't read the kernel's view of %s: %w", name, err)
		}

		m := sessionRe.FindStringSubmatch(canonical)
		if m == nil {
			return nil, fmt.Errorf("%s is not an iSCSI disk (sysfs path %s)", name, canonical)
		}

		// The last component is H:C:T:L.
		parts := strings.Split(filepath.Base(canonical), ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("unexpected sysfs device name %q for %s", filepath.Base(canonical), name)
		}
		lun, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("unexpected LUN in sysfs device name %q for %s: %w", filepath.Base(canonical), name, err)
		}

		data, err := os.ReadFile(filepath.Join(sysfs, "class", "iscsi_session", m[2], "targetname"))
		if err != nil {
			return nil, fmt.Errorf("couldn't read the target of %s (%s): %w", name, m[2], err)
		}

		leg := &iscsiTarget{Iqn: strings.TrimSpace(string(data)), Lun: lun}
		if found != nil && *found != *leg {
			// Logging out of the wrong target is worse than not logging out.
			return nil, fmt.Errorf("the paths of %s disagree: target[%s] lun %d and target[%s] lun %d",
				devPath, found.Iqn, found.Lun, leg.Iqn, leg.Lun)
		}
		found = leg
	}

	if found == nil {
		return nil, fmt.Errorf("no iSCSI path found behind %s", devPath)
	}
	return found, nil
}
