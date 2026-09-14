/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// sysfsRoot is a var so tests can point the walk at a fabricated tree.
var sysfsRoot = "/sys"

var sessionRe = regexp.MustCompile(`(^|/)(session\d+)(/|$)`)

// verifyIscsiDevice checks that the device a by-path link resolved to really
// belongs to the target and LUN this volume expects.
//
// The by-path link is udev bookkeeping, not the kernel's own view. Links are
// created and removed asynchronously as sessions come and go, and the E2E
// failures being chased all involve many volumes logging in and out on one
// node at once -- exactly the churn in which a link can outlive its device or
// be read while the name it points to has already been given to someone else's
// LUN. Mounting whatever the link says without checking is how a pod ends up
// reading a different volume's bytes with no error anywhere: the mount itself
// works fine, it is just the wrong disk.
//
// The kernel's view is authoritative and cheap to read: a SCSI disk's sysfs
// device path names the iSCSI session it hangs off, the session publishes the
// target IQN it is logged in to, and the last component carries H:C:T:L with
// the LUN number. Comparing those against what the volume expects decides,
// per mount, whether by-path told the truth.
//
// A mismatch is an error: handing over another volume's disk is never right,
// and failing the stage makes kubelet retry after udev has settled. Not being
// able to *tell* -- sysfs shaped unexpectedly, session gone mid-read -- only
// logs: the identity was not shown wrong, and refusing every mount on an
// unfamiliar kernel layout would trade a rare corruption for a common outage.
func verifyIscsiDevice(devPath, wantIqn string, wantLun int, sysfs string) error {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		log.Warnf("Couldn't resolve %s to verify its identity: %v", devPath, err)
		return nil
	}

	names := []string{filepath.Base(real)}
	if strings.HasPrefix(filepath.Base(real), "dm-") {
		// A multipath device is only as trustworthy as its legs.
		slaves, err := os.ReadDir(filepath.Join(sysfs, "block", filepath.Base(real), "slaves"))
		if err != nil {
			log.Warnf("Couldn't list the slaves of %s to verify its identity: %v", real, err)
			return nil
		}
		names = names[:0]
		for _, s := range slaves {
			names = append(names, s.Name())
		}
	}

	for _, name := range names {
		canonical, err := filepath.EvalSymlinks(filepath.Join(sysfs, "block", name, "device"))
		if err != nil {
			log.Warnf("Couldn't read the kernel's view of %s: %v", name, err)
			continue
		}

		m := sessionRe.FindStringSubmatch(canonical)
		if m == nil {
			return fmt.Errorf("device %s (%s) is not an iSCSI disk (sysfs path %s); "+
				"the by-path link for target[%s] points at something that never came from that target",
				devPath, name, canonical, wantIqn)
		}
		session := m[2]

		hctl := filepath.Base(canonical)
		parts := strings.Split(hctl, ":")
		var gotLun = -1
		if len(parts) == 4 {
			if v, err := strconv.Atoi(parts[3]); err == nil {
				gotLun = v
			}
		}

		data, err := os.ReadFile(filepath.Join(sysfs, "class", "iscsi_session", session, "targetname"))
		if err != nil {
			log.Warnf("Couldn't read the target of %s (%s): %v", name, session, err)
			continue
		}
		gotIqn := strings.TrimSpace(string(data))

		if gotIqn != wantIqn || (gotLun != -1 && gotLun != wantLun) {
			return fmt.Errorf("device %s (%s) belongs to target[%s] lun %d, "+
				"but this volume expects target[%s] lun %d -- refusing to hand over another volume's disk",
				devPath, name, gotIqn, gotLun, wantIqn, wantLun)
		}
	}
	return nil
}
