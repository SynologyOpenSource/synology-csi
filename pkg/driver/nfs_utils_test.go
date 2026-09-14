// Copyright 2021 Synology Inc.

package driver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountPermissionsFor(t *testing.T) {
	cases := []struct {
		name      string
		current   os.FileMode
		requested os.FileMode
		want      os.FileMode
	}{
		{
			// Every volume on its first mount: nothing has claimed it, so the
			// StorageClass gets exactly what it asked for.
			name:      "untouched volume takes the requested permissions",
			current:   os.ModeDir | 0o777,
			requested: 0o750,
			want:      0o750,
		},
		{
			// The case that made kubelet redo the recursive chown on every
			// mount: 0750 would have taken the group's write bit away.
			name:      "group keeps its access once kubelet has marked the volume",
			current:   os.ModeDir | os.ModeSetgid | 0o770,
			requested: 0o750,
			want:      os.ModeSetgid | 0o770,
		},
		{
			name:      "the common 0755 also keeps the group's access",
			current:   os.ModeDir | os.ModeSetgid | 0o770,
			requested: 0o755,
			want:      os.ModeSetgid | 0o775,
		},
		{
			// Only the group bits are kubelet's to decide; owner and other
			// still come from the StorageClass.
			name:      "owner and other still follow the request",
			current:   os.ModeDir | os.ModeSetgid | 0o777,
			requested: 0o750,
			want:      os.ModeSetgid | 0o770,
		},
		{
			name:      "the marker is preserved, not dropped",
			current:   os.ModeDir | os.ModeSetgid | 0o770,
			requested: 0o700,
			want:      os.ModeSetgid | 0o770,
		},
		{
			// A wider request is not narrowed by what is already there.
			name:      "a request for more group access is honoured",
			current:   os.ModeDir | 0o700,
			requested: 0o770,
			want:      0o770,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mountPermissionsFor(tc.current, tc.requested); got != tc.want {
				t.Errorf("mountPermissionsFor(%#o, %#o) = %#o, want %#o",
					tc.current, tc.requested, got, tc.want)
			}
		})
	}
}

// kubelet only skips its recursive chown when the group still owns the volume
// and still has rwx. Whatever mountPermissions asks for, the result has to keep
// satisfying that once kubelet has marked the volume -- otherwise the walk
// happens again on every mount and rewrites ownership the workload set itself.
func TestResultKeepsKubeletFromRedoingTheChown(t *testing.T) {
	const kubeletNeeds = 0o070 // group rwx

	managed := os.ModeDir | os.ModeSetgid | 0o770
	for _, requested := range []os.FileMode{0o700, 0o750, 0o755, 0o770, 0o775, 0o777} {
		got := mountPermissionsFor(managed, requested)
		if got.Perm()&kubeletNeeds != kubeletNeeds {
			t.Errorf("mountPermissions %#o produced %#o, which leaves the group without rwx "+
				"and makes kubelet walk the volume again", requested, got)
		}
		if got&os.ModeSetgid == 0 {
			t.Errorf("mountPermissions %#o produced %#o, dropping the setgid marker", requested, got)
		}
	}
}

func TestChmodIfPermissionMismatch(t *testing.T) {
	t.Run("leaves a volume kubelet already set up alone", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.Mkdir(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o770|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}

		if err := chmodIfPermissionMismatch(dir, 0o750); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		after, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mode() != before.Mode() {
			t.Errorf("mode changed from %#o to %#o; a volume kubelet manages should be left as it is",
				before.Mode(), after.Mode())
		}
	})

	t.Run("applies the requested permissions to an untouched volume", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vol")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}

		if err := chmodIfPermissionMismatch(dir, 0o750); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Errorf("got %#o, want %#o", info.Mode().Perm(), 0o750)
		}
	})

	t.Run("reports a missing path", func(t *testing.T) {
		if err := chmodIfPermissionMismatch(filepath.Join(t.TempDir(), "absent"), 0o750); err == nil {
			t.Error("expected an error for a path that does not exist")
		}
	})
}
