// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
)

// errListIncomplete stands in for the case this file is about: DSM could not be
// listed in full, so nothing can be concluded about whether a volume exists.
var errListIncomplete = errors.New("DSM[1.2.3.4] failed to load NFS privilege of share(k8s-csi-pvc-x): busy")

// lookupFailingService reports every lookup as "could not tell". It also records
// whether CreateVolume was reached, because reaching it after a failed lookup is
// how a second volume gets created for a PVC that already has one.
type lookupFailingService struct {
	createVolumeCalled bool
}

func (s *lookupFailingService) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	return nil, errListIncomplete
}

func (s *lookupFailingService) GetVolumeByName(volName string) (*models.K8sVolumeRespSpec, error) {
	return nil, errListIncomplete
}

func (s *lookupFailingService) ListVolumes() ([]*models.K8sVolumeRespSpec, error) {
	// A partial list plus an error: callers must not use the partial half.
	return []*models.K8sVolumeRespSpec{{VolumeId: "still-visible"}}, errListIncomplete
}

func (s *lookupFailingService) CreateVolume(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	s.createVolumeCalled = true
	return &models.K8sVolumeRespSpec{VolumeId: "newly-created"}, nil
}

func (s *lookupFailingService) AddDsm(client common.ClientInfo) error { return nil }
func (s *lookupFailingService) RemoveAllDsms()                        {}
func (s *lookupFailingService) GetDsm(ip string) (*webapi.DSM, error) {
	return nil, errors.New("not used in these tests")
}
func (s *lookupFailingService) GetDsmsCount() int { return 1 }
func (s *lookupFailingService) ListDsmVolumes(ip string) ([]webapi.VolInfo, error) {
	return nil, nil
}
func (s *lookupFailingService) DeleteVolume(volId string) error { return nil }
func (s *lookupFailingService) ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error) {
	return nil, nil
}
func (s *lookupFailingService) CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error) {
	return nil, nil
}
func (s *lookupFailingService) DeleteSnapshot(snapshotUuid string) error        { return nil }
func (s *lookupFailingService) ListAllSnapshots() []*models.K8sSnapshotRespSpec { return nil }
func (s *lookupFailingService) ListSnapshots(volId string) []*models.K8sSnapshotRespSpec {
	return nil
}
func (s *lookupFailingService) GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec {
	return nil
}

// testDriver mirrors the access modes the real driver registers, so requests
// reach the lookup instead of being rejected by capability validation first.
func testDriver() *Driver {
	d := &Driver{}
	d.addVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	})
	return d
}

// wantUnavailable checks the RPC failed in a way the caller will retry. NotFound
// is the dangerous answer here: it tells the caller the volume is gone for good.
func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error when the volume could not be looked up, got success")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() == codes.NotFound {
		t.Fatalf("a lookup failure was reported as NotFound, which reads as 'deleted': %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("expected codes.Unavailable so the caller retries, got %v (%v)", st.Code(), err)
	}
}

func TestValidateVolumeCapabilitiesDoesNotReportLookupFailureAsMissing(t *testing.T) {
	cs := &controllerServer{Driver: testDriver(), dsmService: &lookupFailingService{}}

	_, err := cs.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "vol-1",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	})

	wantUnavailable(t, err)
}

func TestListVolumesDoesNotReturnAPartialList(t *testing.T) {
	cs := &controllerServer{Driver: testDriver(), dsmService: &lookupFailingService{}}

	// Returning the half that could be read would tell the caller the missing
	// volumes no longer exist.
	_, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{})

	wantUnavailable(t, err)
}

func TestCreateVolumeDoesNotProvisionWhenIdempotencyCheckFails(t *testing.T) {
	svc := &lookupFailingService{}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "pvc-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 * 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	})

	wantUnavailable(t, err)
	if svc.createVolumeCalled {
		t.Error("provisioned a volume without knowing whether one already existed, which can leave two volumes for one PVC")
	}
}

func TestNodeUnstageDoesNotClaimSuccessWhenLookupFails(t *testing.T) {
	// A real directory that is not a mount point, so unstaging gets past the
	// mount check and reaches the lookup this test is about.
	ns := &nodeServer{
		Driver:     testDriver(),
		dsmService: &lookupFailingService{},
		Mounter:    &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
	}
	stagingPath := t.TempDir()

	// Reporting success here ends kubelet's retries, so an iSCSI session that is
	// still logged in would never be cleaned up.
	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: stagingPath,
	})

	wantUnavailable(t, err)
}
