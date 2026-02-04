/*
Copyright 2021 Synology Inc.

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
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
)

// mockDsmService implements interfaces.IDsmService for testing
type mockDsmService struct {
	createVolumeFunc func(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error)
	getVolumeByName  *models.K8sVolumeRespSpec
}

func (m *mockDsmService) AddDsm(client common.ClientInfo) error { return nil }
func (m *mockDsmService) RemoveAllDsms()                        {}
func (m *mockDsmService) GetDsm(ip string) (*webapi.DSM, error) { return nil, nil }
func (m *mockDsmService) GetDsmsCount() int                     { return 1 }
func (m *mockDsmService) ListDsmVolumes(ip string) ([]webapi.VolInfo, error) {
	return nil, nil
}
func (m *mockDsmService) CreateVolume(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	if m.createVolumeFunc != nil {
		return m.createVolumeFunc(spec)
	}
	return &models.K8sVolumeRespSpec{
		VolumeId:    "test-volume-id",
		SizeInBytes: spec.Size,
		Protocol:    spec.Protocol,
	}, nil
}
func (m *mockDsmService) DeleteVolume(volId string) error                  { return nil }
func (m *mockDsmService) ListVolumes() []*models.K8sVolumeRespSpec         { return nil }
func (m *mockDsmService) GetVolume(volId string) *models.K8sVolumeRespSpec { return nil }
func (m *mockDsmService) ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error) {
	return nil, nil
}
func (m *mockDsmService) CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error) {
	return nil, nil
}
func (m *mockDsmService) DeleteSnapshot(snapshotUuid string) error                 { return nil }
func (m *mockDsmService) ListAllSnapshots() []*models.K8sSnapshotRespSpec          { return nil }
func (m *mockDsmService) ListSnapshots(volId string) []*models.K8sSnapshotRespSpec { return nil }
func (m *mockDsmService) GetVolumeByName(volName string) *models.K8sVolumeRespSpec {
	return m.getVolumeByName
}
func (m *mockDsmService) GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec {
	return nil
}

func newTestControllerServer(dsmService interfaces.IDsmService) *controllerServer {
	d := &Driver{
		name:    DriverName,
		version: DriverVersion,
	}
	d.addVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	})

	return &controllerServer{
		Driver:     d,
		dsmService: dsmService,
	}
}

func TestCreateVolume_AllowMultipleSessions(t *testing.T) {
	tests := []struct {
		name                string
		params              map[string]string
		accessMode          csi.VolumeCapability_AccessMode_Mode
		wantMultipleSession bool
	}{
		{
			name: "allowMultipleSessions true with SINGLE_NODE_WRITER",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "true",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions false with SINGLE_NODE_WRITER",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "false",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: false,
		},
		{
			name: "allowMultipleSessions not set with SINGLE_NODE_WRITER",
			params: map[string]string{
				"protocol": "iscsi",
				"location": "/volume1",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: false,
		},
		{
			name: "allowMultipleSessions true with MULTI_NODE_MULTI_WRITER",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "true",
			},
			accessMode:          csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions false with MULTI_NODE_MULTI_WRITER - access mode wins",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "false",
			},
			accessMode:          csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions not set with MULTI_NODE_MULTI_WRITER",
			params: map[string]string{
				"protocol": "iscsi",
				"location": "/volume1",
			},
			accessMode:          csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions yes (alternative boolean)",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "yes",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions 1 (alternative boolean)",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "1",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions TRUE (case insensitive)",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "TRUE",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: true,
		},
		{
			name: "allowMultipleSessions empty string",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: false,
		},
		{
			name: "allowMultipleSessions invalid value",
			params: map[string]string{
				"protocol":              "iscsi",
				"location":              "/volume1",
				"allowMultipleSessions": "invalid",
			},
			accessMode:          csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			wantMultipleSession: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedSpec *models.CreateK8sVolumeSpec
			mock := &mockDsmService{
				createVolumeFunc: func(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
					capturedSpec = spec
					return &models.K8sVolumeRespSpec{
						VolumeId:    "test-volume-id",
						SizeInBytes: spec.Size,
						Protocol:    spec.Protocol,
					}, nil
				},
			}

			cs := newTestControllerServer(mock)

			req := &csi.CreateVolumeRequest{
				Name: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: 1 * 1024 * 1024 * 1024, // 1GB
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: tt.accessMode,
						},
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{},
						},
					},
				},
				Parameters: tt.params,
			}

			_, err := cs.CreateVolume(context.Background(), req)
			if err != nil {
				t.Fatalf("CreateVolume failed: %v", err)
			}

			if capturedSpec == nil {
				t.Fatal("CreateVolume did not call dsmService.CreateVolume")
			}

			if capturedSpec.MultipleSession != tt.wantMultipleSession {
				t.Errorf("MultipleSession = %v, want %v", capturedSpec.MultipleSession, tt.wantMultipleSession)
			}
		})
	}
}

func TestCreateVolume_AllowMultipleSessions_NonIscsi(t *testing.T) {
	// Test that allowMultipleSessions is ignored for non-iSCSI protocols
	tests := []struct {
		name     string
		protocol string
	}{
		{
			name:     "SMB protocol ignores allowMultipleSessions",
			protocol: "smb",
		},
		{
			name:     "NFS protocol ignores allowMultipleSessions",
			protocol: "nfs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedSpec *models.CreateK8sVolumeSpec
			mock := &mockDsmService{
				createVolumeFunc: func(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
					capturedSpec = spec
					return &models.K8sVolumeRespSpec{
						VolumeId:    "test-volume-id",
						SizeInBytes: spec.Size,
						Protocol:    spec.Protocol,
					}, nil
				},
			}

			cs := newTestControllerServer(mock)

			req := &csi.CreateVolumeRequest{
				Name: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: 1 * 1024 * 1024 * 1024, // 1GB
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{},
						},
					},
				},
				Parameters: map[string]string{
					"protocol":              tt.protocol,
					"location":              "/volume1",
					"allowMultipleSessions": "true",
				},
			}

			_, err := cs.CreateVolume(context.Background(), req)
			if err != nil {
				t.Fatalf("CreateVolume failed: %v", err)
			}

			if capturedSpec == nil {
				t.Fatal("CreateVolume did not call dsmService.CreateVolume")
			}

			// For non-iSCSI protocols, MultipleSession should still be set
			// based on access mode, but it's not used by the share volume logic.
			// The parameter should not cause an error.
		})
	}
}
