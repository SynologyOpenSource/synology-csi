// Copyright 2021 Synology Inc.

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"k8s.io/mount-utils"
)

// mountInfoPath lists the mounts of the driver's own mount namespace. It is a
// variable so tests can point it at a fixture.
var mountInfoPath = "/proc/self/mountinfo"

// HostProcPath is the proc filesystem listing the processes whose mount
// namespaces can hold a copy of a volume's mount: the host's processes, seen
// through the host root mount. main points it there from --chroot-dir; the
// default only sees the driver's own container.
var HostProcPath = "/proc"

// UnstageHolderTimeout is how long unstage keeps refusing to disconnect a
// device that another mount namespace still has mounted before it syncs that
// filesystem and disconnects anyway. Zero refuses for as long as it takes.
var UnstageHolderTimeout = 5 * time.Minute

// now is time.Now, replaceable in tests.
var now = time.Now

// mountHolder is a process whose mount namespace has a filesystem on a device
// mounted.
type mountHolder struct {
	pid        int
	comm       string
	mountPoint string
}

func (h mountHolder) String() string {
	return fmt.Sprintf("pid %d (%s) at %s", h.pid, h.comm, h.mountPoint)
}

// releaseDevice makes sure no filesystem on devPath is still mounted before the
// device is disconnected.
//
// NodeUnstageVolume runs only after kubelet has unpublished the volume, and
// the staging path has been unmounted by then, so any mount of the device left
// in this namespace is stale. The usual source is a copy inherited through the
// host root mount when the node plugin started: it keeps the filesystem alive
// after the staging path is gone, and disconnecting the device under it fails
// in-flight writes and aborts the journal. Unmount what is left, and refuse to
// go on if anything remains.
//
// Other pods can hold a copy too, in a mount namespace the driver cannot
// unmount from: anything that mounts the host's /var/lib/kubelet, or a parent
// of it, without mount propagation. Those are found through HostProcPath, and
// the disconnect is refused until they are gone, or until UnstageHolderTimeout
// has passed since the first refusal: then their filesystem is synced and the
// device disconnected anyway, rather than leaving the volume attached for good.
// The time of the first refusal is kept in memory, since kubelet's retries come
// back to this same process.
func (ns *nodeServer) releaseDevice(devPath, volumeId string) error {
	var st unix.Stat_t
	if err := unix.Stat(devPath, &st); err != nil {
		return fmt.Errorf("failed to stat device %s: %v", devPath, err)
	}
	return ns.releaseDeviceNumber(devPath, volumeId, unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
}

func (ns *nodeServer) releaseDeviceNumber(devPath, volumeId string, major, minor uint32) error {
	mountPoints, err := deviceMountPoints(major, minor)
	if err != nil {
		return err
	}

	if len(mountPoints) > 0 {
		// Reverse order puts a nested mount point before the one it sits under.
		sort.Sort(sort.Reverse(sort.StringSlice(mountPoints)))
		for _, mountPoint := range mountPoints {
			log.Warnf("Device %s is still mounted at %s after unstage, unmounting it", devPath, mountPoint)
			if err := ns.Mounter.Interface.Unmount(mountPoint); err != nil {
				log.Errorf("Failed to unmount %s: %v", mountPoint, err)
			}
		}

		if mountPoints, err = deviceMountPoints(major, minor); err != nil {
			return err
		}
		if len(mountPoints) > 0 {
			return fmt.Errorf("device %s is still mounted at %s", devPath, strings.Join(mountPoints, ", "))
		}
	}

	holders, err := deviceHolders(HostProcPath, major, minor)
	if err != nil {
		log.Warnf("Can't check other mount namespaces for device %s, only the driver's own was checked: %v", devPath, err)
		return nil
	}
	if len(holders) == 0 {
		ns.clearHolderSince(volumeId)
		return nil
	}

	held := fmt.Sprintf("device %s is still mounted in other mount namespaces: %s", devPath, joinHolders(holders))
	if UnstageHolderTimeout <= 0 {
		return fmt.Errorf("%s", held)
	}
	giveUpAt := ns.holderSinceOrNow(volumeId).Add(UnstageHolderTimeout)
	if now().Before(giveUpAt) {
		return fmt.Errorf("%s; disconnecting anyway at %s unless released", held, giveUpAt.Format(time.RFC3339))
	}

	for _, holder := range holders {
		if err := syncHolder(HostProcPath, holder); err != nil {
			log.Errorf("Failed to sync the filesystem of device %s mounted by %s: %v", devPath, holder, err)
		}
	}
	log.Errorf("Disconnecting device %s although it is still mounted in other mount namespaces after %s: %s; "+
		"its filesystem was synced first, those copies will see I/O errors", devPath, UnstageHolderTimeout, joinHolders(holders))
	ns.clearHolderSince(volumeId)
	return nil
}

// holderSinceOrNow returns when unstage first found volumeId held by another
// mount namespace, recording now if this is the first time.
func (ns *nodeServer) holderSinceOrNow(volumeId string) time.Time {
	ns.holderMutex.Lock()
	defer ns.holderMutex.Unlock()
	if ns.holderSince == nil {
		ns.holderSince = map[string]time.Time{}
	}
	since, ok := ns.holderSince[volumeId]
	if !ok {
		since = now()
		ns.holderSince[volumeId] = since
	}
	return since
}

func (ns *nodeServer) clearHolderSince(volumeId string) {
	ns.holderMutex.Lock()
	defer ns.holderMutex.Unlock()
	delete(ns.holderSince, volumeId)
}

// syncHolder writes out the holder's copy of the filesystem. The path through
// the holder's /proc entry resolves in its mount namespace, so this reaches a
// mount the driver cannot see, without changing anything in that namespace.
func syncHolder(procPath string, holder mountHolder) error {
	f, err := os.Open(filepath.Join(procPath, strconv.Itoa(holder.pid), "root", holder.mountPoint))
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

func joinHolders(holders []mountHolder) string {
	names := make([]string, len(holders))
	for i, holder := range holders {
		names[i] = holder.String()
	}
	return strings.Join(names, "; ")
}

// deviceMountPoints returns where filesystems on the given block device are
// mounted in the driver's mount namespace.
func deviceMountPoints(major, minor uint32) ([]string, error) {
	infos, err := mount.ParseMountInfo(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %v", mountInfoPath, err)
	}

	var mountPoints []string
	for _, info := range infos {
		if info.Major == int(major) && info.Minor == int(minor) {
			mountPoints = append(mountPoints, info.MountPoint)
		}
	}
	return mountPoints, nil
}

// deviceHolders lists the processes under procPath whose mount namespace has a
// filesystem on the given block device mounted, one process per namespace.
func deviceHolders(procPath string, major, minor uint32) ([]mountHolder, error) {
	entries, err := os.ReadDir(procPath)
	if err != nil {
		return nil, err
	}

	var holders []mountHolder
	seen := map[string]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Processes come and go while we look; skip any we can't read.
		pidDir := filepath.Join(procPath, entry.Name())
		mountNs, err := os.Readlink(filepath.Join(pidDir, "ns", "mnt"))
		if err != nil || seen[mountNs] {
			continue
		}
		infos, err := mount.ParseMountInfo(filepath.Join(pidDir, "mountinfo"))
		if err != nil {
			continue
		}
		seen[mountNs] = true

		for _, info := range infos {
			if info.Major == int(major) && info.Minor == int(minor) {
				comm, _ := os.ReadFile(filepath.Join(pidDir, "comm"))
				holders = append(holders, mountHolder{pid, strings.TrimSpace(string(comm)), info.MountPoint})
			}
		}
	}
	return holders, nil
}
