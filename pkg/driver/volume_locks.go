// Copyright 2025 Synology Inc.

package driver

import "sync"

// volumeLocks serializes the node operations on one volume. It is a set of the
// volume IDs being operated on, in the manner of the CSI sample drivers
// (csi-driver-host-path, ceph-csi), and the RPCs that cannot take their volume's
// lock answer codes.Aborted rather than queue behind it: the caller retries,
// and by then the other operation has settled what there is to retry against.
//
// The zero value is ready to use.
type volumeLocks struct {
	mu   sync.Mutex
	held map[string]struct{}
}

// tryAcquire takes the lock of a volume, or reports that someone has it.
func (l *volumeLocks) tryAcquire(volumeId string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, held := l.held[volumeId]; held {
		return false
	}
	if l.held == nil {
		l.held = make(map[string]struct{})
	}
	l.held[volumeId] = struct{}{}
	return true
}

func (l *volumeLocks) release(volumeId string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, volumeId)
}
