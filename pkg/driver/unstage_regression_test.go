// Copyright 2025 Synology Inc.

package driver

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func useFakeSysfs(t *testing.T, iqn string) (device string) {
	t.Helper()
	root, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", iqn)
	prev := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = prev })
	return dev
}

// A volume staged before records were kept is identified by its mount, which
// the first unstage destroys. If that attempt then fails, the retry must still
// know the target.
func TestUnstageRetryAfterAFailedLogoutStillKnowsAnUnrecordedTarget(t *testing.T) {
	dev := useFakeSysfs(t, sysfsIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn), fail: "--logout"}
	v := newStagedVolume(t, dev, e, &volumeService{})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Fatal("the target found through the mount was not kept for the retry")
	}

	e.fail, e.ran = "", nil
	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error on the retry: %v", err)
	}
	wantLoggedOut(t, e, sysfsIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the retry succeeded")
	}
}

// DSM is asked before anything is unmounted or recorded, so a lookup that fails
// leaves the volume exactly as it was for kubelet's retry.
func TestUnstageOfAnUnrecordedMountChangesNothingWhenTheLookupFails(t *testing.T) {
	dev := useFakeSysfs(t, sysfsIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn)}
	svc := &volumeService{err: errListIncomplete}
	v := newStagedVolume(t, dev, e, svc)

	wantUnavailable(t, v.unstage())
	if v.recordExists() {
		t.Error("a record was written before DSM had said which target the volume has")
	}
	if notMount, err := v.ns.Mounter.Interface.IsLikelyNotMountPoint(v.staging); err != nil || notMount {
		t.Errorf("the volume was unmounted although the lookup failed (err %v)", err)
	}
	if len(e.ran) != 0 {
		t.Errorf("expected no iscsiadm calls on a failed lookup, ran: %v", e.ran)
	}

	svc.err = nil
	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error on the retry: %v", err)
	}
	wantLoggedOut(t, e, sysfsIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the retry succeeded")
	}
}

// What is mounted at the staging path need not be this volume's target. DSM can
// still name the volume's target on the first attempt, and that is the one to
// record, so that a retry after DSM loses the volume does not act on the mount.
func TestUnstageOfAnUnrecordedMountGoesByDsmNotByWhatIsMounted(t *testing.T) {
	dev := useFakeSysfs(t, sysfsIqn)
	e := &recordingExecutor{
		sessionOutput: sessionLine(recordedIqn) + "\n" + sessionLine(sysfsIqn),
		fail:          "--targetname " + recordedIqn + " --logout",
	}
	svc := &volumeService{volume: iscsiVolume(recordedIqn)}
	v := newStagedVolume(t, dev, e, svc)

	wantCode(t, v.unstage(), codes.Internal)
	rec, err := v.ns.stageRecords.read(missingVolId)
	if err != nil || rec == nil || rec.Iqn != recordedIqn || rec.MappingIndex != 1 {
		t.Fatalf("expected DSM's target on record, got %+v, %v", rec, err)
	}

	// The volume is deleted between attempts; the retry goes by the record.
	svc.volume, e.fail, e.ran = nil, "", nil
	if err := v.unstage(); err != nil {
		t.Fatal(err)
	}
	wantLoggedOut(t, e, recordedIqn)
	if e.find(sysfsIqn) >= 0 {
		t.Errorf("touched the unrelated target the mount came from; commands: %v", e.ran)
	}
}

func TestUnstageOfAnUnrecordedMountThatAgreesWithDsmRecordsAndLogsOut(t *testing.T) {
	dev := useFakeSysfs(t, recordedIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "--logout"}
	v := newStagedVolume(t, dev, e, &volumeService{volume: iscsiVolume(recordedIqn)})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Fatal("the target was not kept for the retry")
	}
	e.fail, e.ran = "", nil
	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error on the retry: %v", err)
	}
	wantLoggedOut(t, e, recordedIqn)
}

// With DSM naming a volume that is not iSCSI, there is no target to record, and
// what the mount says is none of this unstage's business.
func TestUnstageOfAnUnrecordedMountOfAVolumeThatIsNotIscsiRecordsNothing(t *testing.T) {
	dev := useFakeSysfs(t, sysfsIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn)}
	vol := iscsiVolume(recordedIqn)
	vol.Protocol = utils.ProtocolNfs
	v := newStagedVolume(t, dev, e, &volumeService{volume: vol})

	if err := v.unstage(); err != nil {
		t.Fatal(err)
	}
	if v.recordExists() || len(e.ran) != 0 {
		t.Errorf("expected nothing recorded or run, record exists: %v, ran: %v", v.recordExists(), e.ran)
	}
}

// A staging path that is gone has nothing mounted on it; the record still says
// what to log out of.
func TestUnstageCanUseTheRecordAfterTheStagingPathDisappears(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})
	if err := os.Remove(v.staging); err != nil {
		t.Fatal(err)
	}

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the unstage completed")
	}
}

// Unmounting without having written down what was found would lose it.
func TestUnstageDoesNotUnmountWhatItCannotRecord(t *testing.T) {
	dev := useFakeSysfs(t, sysfsIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn)}
	v := newStagedVolume(t, dev, e, &volumeService{})
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	v.ns.stageRecords.dir = filepath.Join(blocker, "stage") // cannot be created

	wantCode(t, v.unstage(), codes.Internal)

	notMount, err := v.ns.Mounter.Interface.IsLikelyNotMountPoint(v.staging)
	if err != nil || notMount {
		t.Errorf("the volume was unmounted although its target could not be recorded (err %v)", err)
	}
}

// The logout failing used to be forgotten as soon as DSM still had the volume.
func TestUnstageOfAVolumeDsmStillHasReportsAFailedLogoutAndKeepsTheRecord(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "--logout"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the logout failed")
	}
}

func TestUnstageOfAMissingVolumeReportsAFailedNodeRecordDeletion(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "-o delete"}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the node record is still there")
	}
}

func TestUnstageOfAVolumeDsmStillHasReportsAFailedNodeRecordDeletion(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "-o delete"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the node record is still there")
	}
}

func TestUnstageSucceedsWhenThereIsNoNodeRecordToDelete(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "-o delete", failStatus: 21, failOutput: "iscsiadm: No records found"}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.recordExists() {
		t.Error("the record has to be gone")
	}
}

func TestUnstageCannotTellTheSessionsAndKeepsTheRecord(t *testing.T) {
	e := &recordingExecutor{fail: "-m session"}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	wantCode(t, v.unstage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the sessions could not be checked")
	}
	if e.find("--logout") >= 0 || e.find("-o delete") >= 0 {
		t.Errorf("nothing may be torn down on a guess about the sessions, ran: %v", e.ran)
	}
}

// What kubelet does after unstage and after a failed stage: remove the staging
// directory and its vol_data.json, then the parent, which fails unless it is
// empty (kubelet's removeMountDir). Nothing of ours may be in the way.
func TestKubeletCanRemoveTheStagingDirectoryAfterStageAndUnstage(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	// The stage fails at the device wait, with a session that was already up.
	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail with no device to mount")
	}
	if !v.recordExists() {
		t.Fatal("the record has to outlive a stage that left a session it did not create")
	}
	// Left by a crash in the middle of a write.
	if err := os.WriteFile(filepath.Join(v.ns.stageRecords.dir, stageRecordTempPrefix+"killed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Dir(v.staging)
	metadata := filepath.Join(parent, "vol_data.json")
	if err := os.WriteFile(metadata, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	removeMountDir := func() {
		t.Helper()
		for _, path := range []string{v.staging, metadata, parent} {
			if err := os.Remove(path); err != nil {
				t.Fatalf("kubelet could not remove %s: %v", path, err)
			}
		}
	}

	if err := v.unstage(); err != nil {
		t.Fatal(err)
	}
	removeMountDir()
}

func TestKubeletCanRemoveTheParentAfterAFailedStage(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})
	_ = v.stage()

	for _, path := range []string{v.staging, filepath.Dir(v.staging)} {
		if err := os.Remove(path); err != nil {
			t.Fatalf("kubelet could not remove %s: %v", path, err)
		}
	}
}

// A session that was there before the stage is not the stage's to end, so the
// record stays for an unstage to find after DSM has lost the volume.
func TestFailedStageKeepsTheRecordOfASessionThatPredatedIt(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	svc := &volumeService{volume: iscsiVolume(recordedIqn)}
	v := newStagedVolume(t, "", e, svc)

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail with no device to mount")
	}
	if e.find("--logout") >= 0 || e.find("-o delete") >= 0 {
		t.Errorf("ended a session the stage did not create, ran: %v", e.ran)
	}
	got, err := v.ns.stageRecords.read(missingVolId)
	if err != nil || got == nil || got.Iqn != recordedIqn || got.MappingIndex != 1 {
		t.Fatalf("expected the target on record, got %v, %v", got, err)
	}

	// DSM loses the volume; the unstage still ends the session.
	svc.volume, e.ran = nil, nil
	if err := v.unstage(); err != nil {
		t.Fatal(err)
	}
	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the unstage completed")
	}
}

// With no login there is nothing for a record to end, and it would only be left
// over.
func TestFailedStageThatNeverLoggedInRemovesTheRecord(t *testing.T) {
	e := &recordingExecutor{fail: "--login"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail")
	}
	if v.recordExists() {
		t.Error("the record of a login that never happened was kept")
	}
}

func TestFailedStageThatCannotTellWhetherItLoggedInKeepsTheRecord(t *testing.T) {
	e := &recordingExecutor{fail: "-m session"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail")
	}
	if !v.recordExists() {
		t.Error("the record was removed although the node may be logged in")
	}
}

// What stage's record is written for must be there before anything is done
// that could leave the node logged in.
func TestStageRecordsTheTargetBeforeLoggingIn(t *testing.T) {
	e := &recordingExecutor{fail: "--login"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})
	var seen *iscsiStageRecord
	e.onCommand = func(line string) {
		if seen == nil && e.find("--login") >= 0 {
			seen, _ = v.ns.stageRecords.read(missingVolId)
		}
	}

	_ = v.stage()
	if seen == nil || seen.Iqn != recordedIqn {
		t.Errorf("expected the record to exist when the login was attempted, saw %v", seen)
	}
}

// Stage and unstage of one volume interleaving could log out a session the
// stage that just succeeded depends on.
func TestNodeOperationsOnOneVolumeDoNotInterleave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc := &volumeService{onGetVolume: func() {
		once.Do(func() { // only the first lookup is held
			entered <- struct{}{}
			<-release
		})
	}}
	e := &recordingExecutor{}
	v := newStagedVolume(t, "", e, svc)

	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		firstErr = v.unstage() // holds the volume inside its DSM lookup
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the unstage never reached the lookup")
	}

	mountCap := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}
	attempts := map[string]func() error{
		"stage":   v.stage,
		"unstage": v.unstage,
		"publish": func() error {
			_, err := v.ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: missingVolId, TargetPath: filepath.Join(v.staging, "t"), StagingTargetPath: v.staging, VolumeCapability: mountCap,
			})
			return err
		},
		"unpublish": func() error {
			_, err := v.ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
				VolumeId: missingVolId, TargetPath: filepath.Join(v.staging, "t"),
			})
			return err
		},
	}
	for name, attempt := range attempts {
		if err := attempt(); status.Code(err) != codes.Aborted {
			t.Errorf("%s: expected Aborted while the volume is busy, got %v", name, err)
		}
	}

	// Another volume is not held up.
	if _, err := v.ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "some-other-volume", TargetPath: filepath.Join(v.staging, "t"),
	}); err != nil {
		t.Errorf("an operation on another volume must not be refused: %v", err)
	}

	close(release)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("unexpected error: %v", firstErr)
	}

	// And the lock is let go of.
	if err := v.unstage(); err != nil {
		t.Errorf("the volume was still locked after the operation finished: %v", err)
	}
}

func TestVolumeLocks(t *testing.T) {
	var l volumeLocks
	if !l.tryAcquire("a") {
		t.Fatal("a free volume must be lockable")
	}
	if l.tryAcquire("a") {
		t.Error("a held volume must not be lockable again")
	}
	if !l.tryAcquire("b") {
		t.Error("another volume must be lockable")
	}
	l.release("a")
	if !l.tryAcquire("a") {
		t.Error("a released volume must be lockable")
	}
}
