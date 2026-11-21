// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// volumeService builds on lookupFailingService, which already covers the rest of
// IDsmService, and lets a test choose what GetVolume and DeleteVolume answer.
type volumeService struct {
	lookupFailingService
	volume    *models.K8sVolumeRespSpec
	getErr    error
	deleteErr error
	deleted   []string
}

func (s *volumeService) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	return s.volume, s.getErr
}

func (s *volumeService) DeleteVolume(volId string) error {
	s.deleted = append(s.deleted, volId)
	return s.deleteErr
}

func iscsiVolume() *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{VolumeId: "vol-1", Protocol: utils.ProtocolIscsi}
}

func mountCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("expected %v, got %v (%v)", want, got, err)
	}
}

func TestPublishUnpublishAreAdvertised(t *testing.T) {
	d, err := NewControllerAndNodeDriver("node-1", "unix:///unused", nil, tools{})
	if err != nil {
		t.Fatal(err)
	}

	// Without this, external-provisioner skips its VolumeAttachment check before
	// DeleteVolume and external-attacher does not hold the attachments back.
	for _, c := range d.csCap {
		if c.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
			return
		}
	}
	t.Error("PUBLISH_UNPUBLISH_VOLUME is not advertised")
}

func TestControllerPublishVolume(t *testing.T) {
	cap := mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	unsupportedMode := mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)

	tests := []struct {
		name string
		svc  *volumeService
		req  *csi.ControllerPublishVolumeRequest
		want codes.Code
	}{
		{"valid request", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: cap}, codes.OK},
		{"missing volume id", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{NodeId: "node-1", VolumeCapability: cap}, codes.InvalidArgument},
		{"missing node id", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", VolumeCapability: cap}, codes.InvalidArgument},
		{"missing capability", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1"}, codes.InvalidArgument},
		{"neither mount nor block", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: &csi.VolumeCapability{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}, codes.InvalidArgument},
		{"nil mount payload", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}, codes.InvalidArgument},
		{"nil block payload", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Block{},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}, codes.InvalidArgument},
		{"unsupported access mode", &volumeService{volume: iscsiVolume()},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: unsupportedMode}, codes.InvalidArgument},
		{"block on a share", &volumeService{volume: &models.K8sVolumeRespSpec{VolumeId: "vol-1", Protocol: utils.ProtocolNfs}},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: block}, codes.InvalidArgument},
		{"unknown volume", &volumeService{},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: cap}, codes.NotFound},
		{"lookup failure is not a missing volume", &volumeService{getErr: errListIncomplete},
			&csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1", VolumeCapability: cap}, codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &controllerServer{Driver: testDriver(), dsmService: tt.svc}

			resp, err := cs.ControllerPublishVolume(context.Background(), tt.req)

			wantCode(t, err, tt.want)
			if tt.want == codes.OK && len(resp.GetPublishContext()) != 0 {
				t.Errorf("expected an empty publish context, got %v", resp.GetPublishContext())
			}
		})
	}
}

func TestControllerPublishVolumeIsIdempotent(t *testing.T) {
	cs := &controllerServer{Driver: testDriver(), dsmService: &volumeService{volume: iscsiVolume()}}
	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-1",
		NodeId:           "node-1",
		VolumeCapability: mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	for i := 0; i < 2; i++ {
		if _, err := cs.ControllerPublishVolume(context.Background(), req); err != nil {
			t.Fatalf("publish #%d failed: %v", i+1, err)
		}
	}
}

func TestControllerUnpublishVolume(t *testing.T) {
	// The volume is gone, which is the usual state when the PVC and pod are
	// deleted together. Unpublish must still succeed or the attachment, and with
	// it the PV, is stuck.
	cs := &controllerServer{Driver: testDriver(), dsmService: &volumeService{}}

	if _, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-gone", NodeId: "node-gone",
	}); err != nil {
		t.Fatalf("unpublish of a missing volume failed: %v", err)
	}

	_, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{NodeId: "node-1"})
	wantCode(t, err, codes.InvalidArgument)
}

func TestDeleteVolumeKeepsFailedPrecondition(t *testing.T) {
	inUse := status.Error(codes.FailedPrecondition, "still in use")
	svc := &volumeService{deleteErr: inUse}
	cs := &controllerServer{Driver: testDriver(), dsmService: svc}

	_, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "vol-1"})
	wantCode(t, err, codes.FailedPrecondition)

	svc.deleteErr = nil
	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "vol-1"}); err != nil {
		t.Fatalf("delete of an unused volume failed: %v", err)
	}

	svc.deleteErr = errors.New("DSM said no")
	_, err = cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "vol-1"})
	wantCode(t, err, codes.Internal)
}
