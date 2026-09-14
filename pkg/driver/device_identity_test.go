// Copyright 2025 Synology Inc.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	rightIqn = "iqn.2000-01.com.synology:nas.pvc-right"
	wrongIqn = "iqn.2000-01.com.synology:nas.pvc-wrong"
)

// fakeSysfs builds the slice of sysfs the verifier walks: a block device whose
// canonical device path names an iSCSI session, and the session's targetname.
func fakeSysfs(t *testing.T, dev, session, hctl, iqn string) (sysfs, devPath string) {
	t.Helper()
	root := t.TempDir()

	canonical := filepath.Join(root, "devices", "platform", "host6", session, "target6:0:0", hctl)
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	blockDir := filepath.Join(root, "block", dev)
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(blockDir, "device")); err != nil {
		t.Fatal(err)
	}

	sessDir := filepath.Join(root, "class", "iscsi_session", session)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "targetname"), []byte(iqn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A stand-in for /dev/<dev>: the verifier only needs its resolved basename.
	devPath = filepath.Join(root, dev)
	if err := os.WriteFile(devPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, devPath
}

func TestDeviceThatMatchesIsAccepted(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", rightIqn)
	if err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err != nil {
		t.Errorf("a device that belongs to the expected target must be accepted, got: %v", err)
	}
}

// The failure being chased: the by-path link hands over a name that meanwhile
// belongs to a different volume's session. Mounting it reads someone else's
// bytes with no error anywhere, so this must refuse.
func TestDeviceFromAnotherTargetIsRefused(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", wrongIqn)
	err := verifyIscsiDevice(dev, rightIqn, 1, sysfs)
	if err == nil {
		t.Fatal("a device belonging to another volume's target must be refused")
	}
	for _, want := range []string{rightIqn, wrongIqn} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name both identities, missing %q in: %v", want, err)
		}
	}
}

func TestDeviceWithTheWrongLunIsRefused(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:2", rightIqn)
	if err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err == nil {
		t.Fatal("a device carrying a different LUN must be refused; " +
			"logoutTarget already assumes one LUN per target, this is where that assumption gets checked")
	}
}

// A by-path link resolving to something that is not an iSCSI disk at all means
// the link itself is lying; that is a refusal, not a shrug.
func TestNonIscsiDeviceIsRefused(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "devices", "pci0000:00", "ata1", "target0:0:0", "0:0:0:0")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	blockDir := filepath.Join(root, "block", "sda")
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(blockDir, "device")); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(root, "sda")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyIscsiDevice(dev, rightIqn, 1, root); err == nil {
		t.Fatal("a local disk behind an iSCSI by-path link must be refused")
	}
}

// Multipath devices are verified through their legs.
func TestMultipathSlavesAreVerified(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", wrongIqn)

	dmDir := filepath.Join(sysfs, "block", "dm-0", "slaves", "sdx")
	if err := os.MkdirAll(dmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(sysfs, "dm-0")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err == nil {
		t.Fatal("a multipath device whose leg belongs to another target must be refused")
	}
}

// Not being able to tell is logged, not fatal: the identity was never shown to
// be wrong, and failing every mount on an unfamiliar sysfs layout would trade
// a rare corruption for a common outage.
func TestUnreadableSysfsDoesNotBlockTheMount(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "sdq") // no block/sdq at all
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyIscsiDevice(dev, rightIqn, 1, root); err != nil {
		t.Errorf("an indeterminate identity should not fail the mount, got: %v", err)
	}
}
