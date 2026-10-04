// Copyright 2021 Synology Inc.

package driver

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/mount-utils"
)

const (
	stagingPath = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.san.synology.com/abc/globalmount"
	hostCopy    = "/host" + stagingPath
)

// A node plugin started after the volume was staged: the staging mount has
// already been unmounted, but its copy under the host root mount is left.
var staleCopyMountInfo = []string{
	"25 1 8:6 /lib/kubelet /var/lib/kubelet rw,relatime shared:25 - xfs /dev/sda6 rw",
	"30 1 8:6 / /host/var rw,relatime shared:434 - xfs /dev/sda6 rw",
	"40 30 8:144 / " + hostCopy + " rw,relatime shared:506 - ext4 /dev/sdj rw",
	"41 25 8:16 / /var/lib/kubelet/plugins/other/globalmount rw,relatime shared:292 - ext4 /dev/sdb rw",
}

func writeMountInfo(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// useMountInfo points the driver at a mountinfo fixture for the test's duration.
func useMountInfo(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	writeMountInfo(t, path, lines)

	origMountInfo, origHostProc := mountInfoPath, HostProcPath
	mountInfoPath, HostProcPath = path, t.TempDir()
	t.Cleanup(func() { mountInfoPath, HostProcPath = origMountInfo, origHostProc })
	return path
}

// addProcess adds a process to the fake HostProcPath.
func addProcess(t *testing.T, pid int, comm, mountNs string, mountInfo []string) {
	t.Helper()
	dir := filepath.Join(HostProcPath, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mountNs, filepath.Join(dir, "ns", "mnt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMountInfo(t, filepath.Join(dir, "mountinfo"), mountInfo)
}

func newReleaseTestServer(fake *mount.FakeMounter) *nodeServer {
	return &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: fake}}
}

func unmountedTargets(fake *mount.FakeMounter) []string {
	var targets []string
	for _, action := range fake.GetLog() {
		if action.Action == mount.FakeActionUnmount {
			targets = append(targets, action.Target)
		}
	}
	return targets
}

func TestReleaseDeviceDoesNothingWhenTheDeviceIsNotMounted(t *testing.T) {
	useMountInfo(t, staleCopyMountInfo)
	fake := mount.NewFakeMounter(nil)

	if err := newReleaseTestServer(fake).releaseDeviceNumber("/dev/sdx", "vol", 8, 160); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets := unmountedTargets(fake); len(targets) > 0 {
		t.Errorf("expected no unmounts, got %v", targets)
	}
}

// The case that loses data: the copy under /host keeps the filesystem mounted
// after the staging path is gone. It must be unmounted before logout.
func TestReleaseDeviceUnmountsAStaleCopy(t *testing.T) {
	path := useMountInfo(t, staleCopyMountInfo)
	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/sdj", Path: hostCopy}})
	fake.UnmountFunc = func(string) error {
		writeMountInfo(t, path, append(staleCopyMountInfo[:2:2], staleCopyMountInfo[3]))
		return nil
	}

	if err := newReleaseTestServer(fake).releaseDeviceNumber("/dev/sdj", "vol", 8, 144); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets := unmountedTargets(fake); len(targets) != 1 || targets[0] != hostCopy {
		t.Errorf("expected only %s to be unmounted, got %v", hostCopy, targets)
	}
}

func TestReleaseDeviceRefusesWhileStillMounted(t *testing.T) {
	useMountInfo(t, staleCopyMountInfo)
	fake := mount.NewFakeMounter(nil) // the unmount "succeeds" but the mount stays

	err := newReleaseTestServer(fake).releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	if err == nil {
		t.Fatal("expected an error while the device is still mounted; logging out " +
			"under a mounted filesystem loses its in-flight writes")
	}
	if !strings.Contains(err.Error(), hostCopy) {
		t.Errorf("expected the error to name %s, got %v", hostCopy, err)
	}
}

func TestReleaseDeviceUnmountsNestedMountsFirst(t *testing.T) {
	useMountInfo(t, []string{
		"40 1 8:144 / /host/a rw - ext4 /dev/sdj rw",
		"41 40 8:144 / /host/a/b rw - ext4 /dev/sdj rw",
	})
	fake := mount.NewFakeMounter(nil)

	_ = newReleaseTestServer(fake).releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	if targets := unmountedTargets(fake); len(targets) != 2 || targets[0] != "/host/a/b" {
		t.Errorf("expected /host/a/b to be unmounted before /host/a, got %v", targets)
	}
}

// A pod that mounts the host's /var/lib without propagation keeps a copy the
// driver cannot unmount; the disconnect has to wait for it.
func TestReleaseDeviceRefusesWhileAnotherNamespaceHoldsIt(t *testing.T) {
	useMountInfo(t, nil)
	addProcess(t, 1, "kubelet", "mnt:[1]", staleCopyMountInfo[:2])
	addProcess(t, 4242, "vector", "mnt:[2]", []string{
		"50 1 8:144 / /var/lib/kubelet/plugins/x/globalmount rw - ext4 /dev/sdj rw",
	})
	fake := mount.NewFakeMounter(nil)

	err := newReleaseTestServer(fake).releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	if err == nil {
		t.Fatal("expected an error while another mount namespace holds the device")
	}
	if !strings.Contains(err.Error(), "pid 4242 (vector)") {
		t.Errorf("expected the error to name the holding process, got %v", err)
	}
	if targets := unmountedTargets(fake); len(targets) > 0 {
		t.Errorf("must not unmount another namespace's mounts, got %v", targets)
	}
}

func TestReleaseDeviceChecksEachNamespaceOnce(t *testing.T) {
	useMountInfo(t, nil)
	holder := []string{"50 1 8:144 / /data rw - ext4 /dev/sdj rw"}
	addProcess(t, 10, "app", "mnt:[7]", holder)
	addProcess(t, 11, "app", "mnt:[7]", holder)

	holders, err := deviceHolders(HostProcPath, 8, 144)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 {
		t.Errorf("expected one holder for a shared namespace, got %v", holders)
	}
}

// Not being able to look further must not block every unstage.
func TestReleaseDeviceGoesOnWhenOtherNamespacesCannotBeRead(t *testing.T) {
	useMountInfo(t, nil)
	HostProcPath = filepath.Join(HostProcPath, "missing")

	if err := newReleaseTestServer(mount.NewFakeMounter(nil)).releaseDeviceNumber("/dev/sdj", "vol", 8, 144); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// useClock makes now() return *clock for the test's duration.
func useClock(t *testing.T, clock *time.Time) {
	t.Helper()
	origNow, origTimeout := now, UnstageHolderTimeout
	now = func() time.Time { return *clock }
	t.Cleanup(func() { now, UnstageHolderTimeout = origNow, origTimeout })
}

func addVectorHolder(t *testing.T) {
	t.Helper()
	addProcess(t, 4242, "vector", "mnt:[2]", []string{
		"50 1 8:144 / /var/lib/kubelet/plugins/x/globalmount rw - ext4 /dev/sdj rw",
	})
}

// Kubelet's retries see the same deadline, and the error says when the driver
// will stop waiting, so a stuck detach can be traced back to its cause.
func TestReleaseDeviceRefusesUntilTheTimeout(t *testing.T) {
	useMountInfo(t, nil)
	addVectorHolder(t)
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClock(t, &clock)
	UnstageHolderTimeout = 5 * time.Minute
	ns := newReleaseTestServer(mount.NewFakeMounter(nil))

	for _, after := range []time.Duration{0, time.Minute, 4*time.Minute + 59*time.Second} {
		clock = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(after)
		err := ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
		if err == nil {
			t.Fatalf("expected a refusal %s after the first one", after)
		}
		if !strings.Contains(err.Error(), "pid 4242 (vector)") ||
			!strings.Contains(err.Error(), "disconnecting anyway at 2026-10-04T12:05:00Z") {
			t.Errorf("expected the holder and the give-up time in the error, got %v", err)
		}
	}
}

func TestReleaseDeviceDisconnectsAnywayAfterTheTimeout(t *testing.T) {
	useMountInfo(t, nil)
	addVectorHolder(t)
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClock(t, &clock)
	UnstageHolderTimeout = 5 * time.Minute
	fake := mount.NewFakeMounter(nil)
	ns := newReleaseTestServer(fake)

	if err := ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144); err == nil {
		t.Fatal("expected a refusal before the timeout")
	}
	clock = clock.Add(5 * time.Minute)
	// The fixture has no root to sync through; that is logged, not fatal.
	if err := ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144); err != nil {
		t.Fatalf("expected the disconnect to go ahead after the timeout, got %v", err)
	}
	if targets := unmountedTargets(fake); len(targets) > 0 {
		t.Errorf("must not unmount another namespace's mounts, got %v", targets)
	}
	if _, waiting := ns.holderSince["vol"]; waiting {
		t.Error("expected the wait to be forgotten once the device was let go")
	}
}

func TestReleaseDeviceWaitsForeverWithoutATimeout(t *testing.T) {
	useMountInfo(t, nil)
	addVectorHolder(t)
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClock(t, &clock)
	UnstageHolderTimeout = 0
	ns := newReleaseTestServer(mount.NewFakeMounter(nil))

	_ = ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	clock = clock.Add(24 * time.Hour)
	err := ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	if err == nil {
		t.Fatal("expected a refusal with the timeout disabled")
	}
	if strings.Contains(err.Error(), "disconnecting anyway") {
		t.Errorf("did not expect a give-up time with the timeout disabled, got %v", err)
	}
}

// A holder that lets go resets the wait, so a later hold gets the full timeout.
func TestReleaseDeviceForgetsTheWaitWhenTheHolderLetsGo(t *testing.T) {
	useMountInfo(t, nil)
	addVectorHolder(t)
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClock(t, &clock)
	ns := newReleaseTestServer(mount.NewFakeMounter(nil))

	_ = ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144)
	if err := os.RemoveAll(filepath.Join(HostProcPath, "4242")); err != nil {
		t.Fatal(err)
	}
	if err := ns.releaseDeviceNumber("/dev/sdj", "vol", 8, 144); err != nil {
		t.Fatalf("unexpected error once released: %v", err)
	}
	if _, waiting := ns.holderSince["vol"]; waiting {
		t.Error("expected the wait to be forgotten once the holder let go")
	}
}

func TestReleaseDeviceTimesEachVolumeSeparately(t *testing.T) {
	useMountInfo(t, nil)
	addVectorHolder(t)
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClock(t, &clock)
	UnstageHolderTimeout = 5 * time.Minute
	ns := newReleaseTestServer(mount.NewFakeMounter(nil))

	_ = ns.releaseDeviceNumber("/dev/sdj", "first", 8, 144)
	clock = clock.Add(5 * time.Minute)
	if err := ns.releaseDeviceNumber("/dev/sdj", "second", 8, 144); err == nil {
		t.Error("a volume seen held for the first time must get its own full timeout")
	}
}

// The sync reaches the holder's mount through its /proc entry; check the path
// it builds against this process's own.
func TestSyncHolderSyncsThroughTheHoldersRoot(t *testing.T) {
	holder := mountHolder{pid: os.Getpid(), comm: "test", mountPoint: t.TempDir()}
	if err := syncHolder("/proc", holder); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
