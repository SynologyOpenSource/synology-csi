package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// recordingDsmService captures the CreateK8sVolumeSpec handed to CreateVolume,
// so the clamp can be verified without running a real DSM.
type recordingDsmService struct {
	lookupFailingService
	lastCreateSpec *models.CreateK8sVolumeSpec
}

func (s *recordingDsmService) GetVolumeByName(volName string) (*models.K8sVolumeRespSpec, error) {
	return nil, nil // does not exist → CreateVolume will be called
}

func (s *recordingDsmService) CreateVolume(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	s.lastCreateSpec = spec
	return &models.K8sVolumeRespSpec{
		VolumeId:    "vol-record",
		SizeInBytes: spec.Size,
		Protocol:    utils.ProtocolIscsi,
	}, nil
}

func (s *recordingDsmService) AddDsm(client common.ClientInfo) error { return nil }
func (s *recordingDsmService) RemoveAllDsms()                        {}
func (s *recordingDsmService) GetDsm(ip string) (*webapi.DSM, error) {
	return nil, nil
}
func (s *recordingDsmService) GetDsmsCount() int { return 1 }
func (s *recordingDsmService) ListDsmVolumes(ip string) ([]webapi.VolInfo, error) {
	return nil, nil
}
func (s *recordingDsmService) DeleteVolume(volId string) error { return nil }
func (s *recordingDsmService) ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error) {
	return nil, nil
}
func (s *recordingDsmService) CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error) {
	return nil, nil
}
func (s *recordingDsmService) DeleteSnapshot(snapshotUuid string) error        { return nil }
func (s *recordingDsmService) ListAllSnapshots() []*models.K8sSnapshotRespSpec { return nil }
func (s *recordingDsmService) ListSnapshots(volId string) []*models.K8sSnapshotRespSpec {
	return nil
}
func (s *recordingDsmService) GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec {
	return nil
}
func (s *recordingDsmService) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	return nil, nil
}
func (s *recordingDsmService) ListVolumes() ([]*models.K8sVolumeRespSpec, error) {
	return nil, nil
}

// baseCreateVolumeRequest returns a CreateVolumeRequest with a valid capability
// and the given capacity; parameters satisfy the minimum path through
// CreateVolume so the capacity clamp is the thing under test.
func baseCreateVolumeRequest(requiredBytes int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "pvc-104",
		CapacityRange: &csi.CapacityRange{RequiredBytes: requiredBytes},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{
			"dsm":      "1.2.3.4",
			"protocol": "iscsi",
			"location": "/volume1",
			"type":     "thin",
		},
	}
}

// TestCreateVolume_ClampsSmallCapacityToMinimum — issue #104.
// Synology DSM rejects LUNs smaller than ~1 GiB. The driver must clamp tiny
// requests up to defaultMinVolumeSizeBytes (1 GiB) instead of failing.
func TestCreateVolume_ClampsSmallCapacityToMinimum(t *testing.T) {
	svc := &recordingDsmService{}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	// 10 MiB — way below the DSM minimum.
	req := baseCreateVolumeRequest(10 * 1024 * 1024)
	resp, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume(10Mi) errored: %v; expected a clamp, not a rejection", err)
	}
	if svc.lastCreateSpec == nil {
		t.Fatal("CreateVolume was not forwarded to DSM service")
	}
	if svc.lastCreateSpec.Size != int64(defaultMinVolumeSizeBytes) {
		t.Errorf("DSM received size %d, want %d (clamped to defaultMinVolumeSizeBytes)", svc.lastCreateSpec.Size, int64(defaultMinVolumeSizeBytes))
	}
	if resp.Volume.CapacityBytes != int64(defaultMinVolumeSizeBytes) {
		t.Errorf("response CapacityBytes = %d, want %d", resp.Volume.CapacityBytes, int64(defaultMinVolumeSizeBytes))
	}
}

// TestCreateVolume_LeavesLargerCapacityAlone — 5 Gi must pass through unchanged.
func TestCreateVolume_LeavesLargerCapacityAlone(t *testing.T) {
	svc := &recordingDsmService{}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	fiveGi := int64(5) * int64(utils.UNIT_GB)
	req := baseCreateVolumeRequest(fiveGi)
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume(5Gi) errored: %v", err)
	}
	if svc.lastCreateSpec == nil {
		t.Fatal("CreateVolume was not forwarded to DSM service")
	}
	if svc.lastCreateSpec.Size != fiveGi {
		t.Errorf("DSM received size %d, want %d (5 Gi unchanged)", svc.lastCreateSpec.Size, fiveGi)
	}
}
