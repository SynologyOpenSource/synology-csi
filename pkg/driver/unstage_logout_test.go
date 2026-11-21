// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"k8s.io/mount-utils"
	testingexec "k8s.io/utils/exec/testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// While the node is still logged in, DeleteVolume refuses the volume. If a
// failed logout were reported as a successful unstage, kubelet would stop
// calling and the volume could never be deleted.
func TestNodeUnstageReportsFailedLogoutAndSucceedsOnRetry(t *testing.T) {
	// The initiator sees the session and can be made to fail its logout. The node
	// server's own tools see no session, so unstage does not wait for a device
	// path that cannot exist in a test.
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "--logout"}
	ns := &nodeServer{
		Driver: testDriver(),
		dsmService: &volumeService{volume: &models.K8sVolumeRespSpec{
			VolumeId: "vol-1",
			Protocol: utils.ProtocolIscsi,
			DsmIp:    "1.2.3.4",
			Target: webapi.TargetInfo{
				Iqn:        testIqn,
				MappedLuns: []webapi.MappedLun{{MappingIndex: 1}},
			},
		}},
		Mounter:   &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
		Initiator: newTestInitiator(e),
		tools:     NewTools(&recordingExecutor{}),
	}
	req := &csi.NodeUnstageVolumeRequest{VolumeId: "vol-1", StagingTargetPath: t.TempDir()}

	_, err := ns.NodeUnstageVolume(context.Background(), req)
	wantCode(t, err, codes.Internal)

	e.fail = "" // the session can be ended now
	e.ran = nil // so that only the retry's commands are looked at below
	if _, err := ns.NodeUnstageVolume(context.Background(), req); err != nil {
		t.Fatalf("the retry should succeed once the logout works, got %v", err)
	}
	if e.find("--logout") < 0 {
		t.Errorf("expected the retry to log out, ran: %v", e.ran)
	}
}

// If the sessions cannot be listed, whether the target is still logged in is
// unknown, so unstage has to fail rather than assume it is not.
func TestNodeUnstageFailsWhenSessionsCannotBeListed(t *testing.T) {
	e := &recordingExecutor{fail: "-m session"}
	ns := &nodeServer{
		Driver: testDriver(),
		dsmService: &volumeService{volume: &models.K8sVolumeRespSpec{
			VolumeId: "vol-1",
			Protocol: utils.ProtocolIscsi,
			DsmIp:    "1.2.3.4",
			Target: webapi.TargetInfo{
				Iqn:        testIqn,
				MappedLuns: []webapi.MappedLun{{MappingIndex: 1}},
			},
		}},
		Mounter:   &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
		Initiator: newTestInitiator(e),
		tools:     NewTools(e),
	}
	req := &csi.NodeUnstageVolumeRequest{VolumeId: "vol-1", StagingTargetPath: t.TempDir()}

	_, err := ns.NodeUnstageVolume(context.Background(), req)
	wantCode(t, err, codes.Internal)
	if e.find("-o delete") >= 0 || e.find("--logout") >= 0 {
		t.Errorf("expected no logout or record removal, ran: %v", e.ran)
	}
}

// Exit status 21 means no sessions, which unstage treats as already logged out.
func TestNodeUnstageSucceedsWithNoActiveSessions(t *testing.T) {
	e := &recordingExecutor{fail: "-m session", failErr: testingexec.FakeExitError{Status: 21}, failOut: "iscsiadm: No active sessions."}
	ns := &nodeServer{
		Driver: testDriver(),
		dsmService: &volumeService{volume: &models.K8sVolumeRespSpec{
			VolumeId: "vol-1",
			Protocol: utils.ProtocolIscsi,
			DsmIp:    "1.2.3.4",
			Target: webapi.TargetInfo{
				Iqn:        testIqn,
				MappedLuns: []webapi.MappedLun{{MappingIndex: 1}},
			},
		}},
		Mounter:   &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
		Initiator: newTestInitiator(e),
		tools:     NewTools(e),
	}
	req := &csi.NodeUnstageVolumeRequest{VolumeId: "vol-1", StagingTargetPath: t.TempDir()}

	if _, err := ns.NodeUnstageVolume(context.Background(), req); err != nil {
		t.Fatalf("unstage with no sessions should succeed, got %v", err)
	}
	if e.find("-o delete") < 0 {
		t.Errorf("expected the node record to be removed, ran: %v", e.ran)
	}
}
