package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// TestSanitizeDescriptionNamespace — issue #134.
// Synology DSM rejects LUN/namespace descriptions whose namespace component is
// empty or digits-only (error 18990403). The helper must prefix "ns-" in those
// cases and leave non-empty alphabetic namespaces alone.
func TestSanitizeDescriptionNamespace(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "digits only", in: "12345", want: "ns-12345"},
		{name: "pure number with leading zero", in: "007", want: "ns-007"},
		{name: "single digit", in: "7", want: "ns-7"},
		{name: "empty", in: "", want: "ns-"},
		{name: "alpha prefix unchanged", in: "pvc-x", want: "pvc-x"},
		{name: "mixed alphanumeric unchanged", in: "ns123abc", want: "ns123abc"},
		{name: "uuid-style unchanged", in: "a1b2c3d4-e5f6", want: "a1b2c3d4-e5f6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeDescriptionNamespace(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeDescriptionNamespace(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCreateVolume_DescriptionNamespaceDigits — end-to-end: a PVC with a
// digits-only namespace must land on the DSM with "ns-" prefixed.
func TestCreateVolume_DescriptionNamespaceDigits(t *testing.T) {
	svc := &recordingDsmService{}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	req := &csi.CreateVolumeRequest{
		Name:          "pvc-134",
		CapacityRange: &csi.CapacityRange{RequiredBytes: int64(utils.UNIT_GB)},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{
			"dsm":                                  "1.2.3.4",
			"protocol":                             "iscsi",
			"location":                             "/volume1",
			"type":                                 "thin",
			"csi.storage.k8s.io/pvc/name":          "data",
			"csi.storage.k8s.io/pvc/namespace":     "12345",
		},
	}

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume errored: %v", err)
	}
	if svc.lastCreateSpec == nil {
		t.Fatal("CreateVolume was not forwarded to DSM service")
	}
	if svc.lastCreateSpec.Description != "ns-12345/data" {
		t.Errorf("DSM description = %q, want %q", svc.lastCreateSpec.Description, "ns-12345/data")
	}
}

// TestCreateVolume_DescriptionNamespaceAlpha — alphabetic namespace must pass through unchanged.
func TestCreateVolume_DescriptionNamespaceAlpha(t *testing.T) {
	svc := &recordingDsmService{}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	req := &csi.CreateVolumeRequest{
		Name:          "pvc-134-alpha",
		CapacityRange: &csi.CapacityRange{RequiredBytes: int64(utils.UNIT_GB)},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{
			"dsm":                                  "1.2.3.4",
			"protocol":                             "iscsi",
			"location":                             "/volume1",
			"type":                                 "thin",
			"csi.storage.k8s.io/pvc/name":          "data",
			"csi.storage.k8s.io/pvc/namespace":     "pvc-x",
		},
	}

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume errored: %v", err)
	}
	if svc.lastCreateSpec.Description != "pvc-x/data" {
		t.Errorf("DSM description = %q, want %q", svc.lastCreateSpec.Description, "pvc-x/data")
	}
}
