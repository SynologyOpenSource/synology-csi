/*
Copyright 2020 The Kubernetes Authors.

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
	"os"

	log "github.com/sirupsen/logrus"
)

// mountPermissionsFor works out what to chmod a freshly mounted volume root to,
// given what the StorageClass asked for and what is already there.
//
// It exists because mountPermissions and kubelet's fsGroup handling were quietly
// fighting each other. When a pod sets an fsGroup, kubelet takes ownership of the
// volume root: it chowns it to the fsGroup, grants the group rwx, and sets the
// setgid bit as a marker that it manages this volume. On the next mount it looks
// at the root again and, with fsGroupChangePolicy: OnRootMismatch, skips the
// recursive chown only if the group still owns the volume *and still has rwx*.
//
// mountPermissions defaults to 0750 and is commonly set to 0755, and both leave
// the group without write. Applying either on top of what kubelet did removed
// that bit, so kubelet found a mismatch and walked the whole volume again on
// every single mount -- undoing any ownership the workload had set for itself in
// the meantime. That is a real data-visible effect, not just wasted work: a file
// chgrp'd inside one pod came back owned by the fsGroup in the next.
//
// So once kubelet has marked the volume, the group keeps whatever access it has.
// mountPermissions still decides the owner and other bits, and still applies in
// full to volumes no fsGroup has ever touched -- which is every volume on its
// first mount, so the requested permissions are what a plain volume gets.
func mountPermissionsFor(current, requested os.FileMode) os.FileMode {
	if current&os.ModeSetgid == 0 {
		return requested.Perm()
	}
	const groupBits = 0o070
	return requested.Perm()&^groupBits | current.Perm()&groupBits | os.ModeSetgid
}

// chmodIfPermissionMismatch only perform chmod when permission mismatches
func chmodIfPermissionMismatch(targetPath string, mode os.FileMode) error {
	info, err := os.Lstat(targetPath)
	if err != nil {
		return err
	}

	want := mountPermissionsFor(info.Mode(), mode)
	// Compare the bits chmod would actually write, so a volume kubelet already
	// set up is left alone instead of being rewritten to the same value.
	if info.Mode()&(os.ModePerm|os.ModeSetgid) != want {
		log.Infof("chmod targetPath(%s, mode:0%o) with permissions(0%o)", targetPath, info.Mode(), want)
		if err := os.Chmod(targetPath, want); err != nil {
			return err
		}
	} else {
		log.Infof("skip chmod on targetPath(%s) since mode is already 0%o)", targetPath, info.Mode())
	}
	return nil
}
