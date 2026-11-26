// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

const ext4SuperMagic = 0xEF53

// statsDsmService answers GetVolume and counts the calls. The embedded
// interface is nil, so any other DSM call panics.
type statsDsmService struct {
	interfaces.IDsmService
	volume *models.K8sVolumeRespSpec
	err    error
	calls  int
}

func (s *statsDsmService) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	s.calls++
	return s.volume, s.err
}

// fakeStatfs replaces statfsFunc for one test and reports a filesystem of the
// given type: 1000 blocks of 4096 bytes with 250 free (200 available to
// unprivileged users), and 100 inodes with 40 free.
func fakeStatfs(t *testing.T, fsType int) *int {
	t.Helper()
	calls := 0
	orig := statfsFunc
	statfsFunc = func(path string, buf *unix.Statfs_t) error {
		calls++
		setFsType(&buf.Type, fsType)
		buf.Bsize = 4096
		buf.Blocks = 1000
		buf.Bfree = 250
		buf.Bavail = 200
		buf.Files = 100
		buf.Ffree = 40
		return nil
	}
	t.Cleanup(func() { statfsFunc = orig })
	return &calls
}

// setFsType stores into f_type, whose Go type differs between platforms.
func setFsType[T ~int32 | ~int64 | ~uint32](p *T, v int) { *p = T(v) }

// newStatsNodeServer returns a node server whose volume path is a mount point.
func newStatsNodeServer(t *testing.T, svc interfaces.IDsmService, mounted bool) (*nodeServer, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mps []mount.MountPoint
	if mounted {
		mps = []mount.MountPoint{{Path: dir}}
	}
	return &nodeServer{
		Driver:     testDriver(),
		dsmService: svc,
		Mounter:    &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(mps)},
	}, dir
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != want {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestVolumeStatsForBlockVolumeDoesNotQueryDsm(t *testing.T) {
	// The DSM would fail every lookup, so success shows it was never asked.
	svc := &statsDsmService{err: errors.New("DSM unreachable")}
	ns, dir := newStatsNodeServer(t, svc, true)
	fakeStatfs(t, ext4SuperMagic)

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "lun-uuid",
		VolumePath: dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.calls != 0 {
		t.Errorf("expected no DSM lookups for a LUN-backed volume, got %d", svc.calls)
	}

	want := []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: 1000 * 4096, Available: 200 * 4096, Used: 750 * 4096},
		{Unit: csi.VolumeUsage_INODES, Total: 100, Available: 40, Used: 60},
	}
	if len(resp.Usage) != len(want) {
		t.Fatalf("expected %d usage entries, got %v", len(want), resp.Usage)
	}
	for i := range want {
		got := resp.Usage[i]
		if got.Unit != want[i].Unit || got.Total != want[i].Total ||
			got.Available != want[i].Available || got.Used != want[i].Used {
			t.Errorf("usage[%d] = %v, want %v", i, got, want[i])
		}
	}
}

func TestVolumeStatsForShareReportsDsmSize(t *testing.T) {
	for _, protocol := range []string{utils.ProtocolNfs, utils.ProtocolSmb} {
		t.Run(protocol, func(t *testing.T) {
			svc := &statsDsmService{volume: &models.K8sVolumeRespSpec{
				VolumeId:    "share-uuid",
				SizeInBytes: 5 << 30,
				Protocol:    protocol,
			}}
			ns, dir := newStatsNodeServer(t, svc, true)
			fakeStatfs(t, nfsSuperMagic)

			resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
				VolumeId:   "share-uuid",
				VolumePath: dir,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if svc.calls != 1 {
				t.Errorf("expected one DSM lookup for a share, got %d", svc.calls)
			}
			// statfs would describe the whole DSM volume, so only the share's
			// size is reported, as before.
			if len(resp.Usage) != 1 || resp.Usage[0].Unit != csi.VolumeUsage_BYTES ||
				resp.Usage[0].Total != 5<<30 || resp.Usage[0].Used != 0 || resp.Usage[0].Available != 0 {
				t.Errorf("unexpected usage for a share: %v", resp.Usage)
			}
		})
	}
}

func TestVolumeStatsForShareKeepsDsmErrors(t *testing.T) {
	tests := []struct {
		name string
		svc  *statsDsmService
		want codes.Code
	}{
		{"share not on DSM", &statsDsmService{}, codes.NotFound},
		{"DSM lookup failed", &statsDsmService{err: errors.New("busy")}, codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, dir := newStatsNodeServer(t, tt.svc, true)
			fakeStatfs(t, nfsSuperMagic)

			_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
				VolumeId:   "share-uuid",
				VolumePath: dir,
			})
			wantCode(t, err, tt.want)
		})
	}
}

func TestVolumeStatsRejectsBadRequestsWithoutDsm(t *testing.T) {
	svc := &statsDsmService{}

	t.Run("missing arguments", func(t *testing.T) {
		ns, dir := newStatsNodeServer(t, svc, true)
		_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumePath: dir})
		wantCode(t, err, codes.InvalidArgument)
		_, err = ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "vol"})
		wantCode(t, err, codes.InvalidArgument)
	})

	t.Run("not mounted", func(t *testing.T) {
		ns, dir := newStatsNodeServer(t, svc, false)
		statfsCalls := fakeStatfs(t, nfsSuperMagic)
		_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "vol",
			VolumePath: dir,
		})
		wantCode(t, err, codes.NotFound)
		if *statfsCalls != 0 {
			t.Errorf("statfs ran on a path that is not mounted")
		}
	})

	t.Run("statfs fails", func(t *testing.T) {
		ns, dir := newStatsNodeServer(t, svc, true)
		orig := statfsFunc
		statfsFunc = func(string, *unix.Statfs_t) error { return unix.EIO }
		t.Cleanup(func() { statfsFunc = orig })
		_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "vol",
			VolumePath: dir,
		})
		wantCode(t, err, codes.Internal)
	})

	if svc.calls != 0 {
		t.Errorf("expected no DSM lookups for rejected requests, got %d", svc.calls)
	}
}

func TestIsNetworkFilesystem(t *testing.T) {
	tests := []struct {
		name   string
		fsType uint32
		want   bool
	}{
		{"nfs", 0x6969, true},
		{"cifs", 0xFF534D42, true},
		{"smb2", 0xFE534D42, true},
		{"ext4", 0xEF53, false},
		{"xfs", 0x58465342, false},
		{"btrfs", 0x9123683E, false},
	}
	for _, tt := range tests {
		if got := isNetworkFilesystem(tt.fsType); got != tt.want {
			t.Errorf("isNetworkFilesystem(%s 0x%X) = %v, want %v", tt.name, tt.fsType, got, tt.want)
		}
	}

	// On 32-bit Linux f_type is an int32, so CIFS arrives negative.
	var signed int32 = -0x00ACB2BE // 0xFF534D42 as an int32
	if !isNetworkFilesystem(uint32(signed)) {
		t.Error("a CIFS f_type read as a signed int32 was not recognised")
	}
}
