// Copyright 2025 Synology Inc.

package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

const (
	missingVolId = "vol-gone"
	// Both carry models.IqnPrefix, as every target this driver creates does.
	recordedIqn = models.IqnPrefix + "nas.pvc-recorded"
	sysfsIqn    = models.IqnPrefix + "nas.pvc-from-sysfs"
)

// volumeService answers GetVolume with whatever the test sets, which is how a
// test makes DSM have, lack or fail to report a volume.
type volumeService struct {
	lookupFailingService
	volume *models.K8sVolumeRespSpec
	err    error
	// onGetVolume, if set, runs inside the lookup, which is how a test holds an
	// RPC halfway through.
	onGetVolume func()
}

func (s *volumeService) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	if s.onGetVolume != nil {
		s.onGetVolume()
	}
	return s.volume, s.err
}

func (s *volumeService) GetDsm(ip string) (*webapi.DSM, error) {
	return &webapi.DSM{Ip: ip}, nil
}

// iscsiVolume is a volume as DSM reports it while it still has it.
func iscsiVolume(iqn string) *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{
		VolumeId: missingVolId,
		DsmIp:    "127.0.0.1",
		Protocol: utils.ProtocolIscsi,
		Target: webapi.TargetInfo{
			Iqn:        iqn,
			MappedLuns: []webapi.MappedLun{{MappingIndex: 1}},
		},
	}
}

// stagedVolume sets up what kubelet leaves behind for a staged volume: a
// per-volume parent directory holding the staging path. The node's stage
// records go elsewhere, as they do in a deployment.
type stagedVolume struct {
	staging string
	ns      *nodeServer
	e       *recordingExecutor
	svc     *volumeService
}

// newStagedVolume builds a node server whose staging path is mounted from
// device (when it is not empty) and whose iscsiadm reports the sessions the
// executor was given.
func newStagedVolume(t *testing.T, device string, e *recordingExecutor, svc *volumeService) *stagedVolume {
	t.Helper()

	// Waiting for a by-path link that cannot exist here takes 20 seconds.
	prevPoll, prevTimeout := devicePathPollInterval, devicePathTimeout
	devicePathPollInterval, devicePathTimeout = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { devicePathPollInterval, devicePathTimeout = prevPoll, prevTimeout })

	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(parent, "globalmount")
	if err := os.Mkdir(staging, 0o750); err != nil {
		t.Fatal(err)
	}

	var mounts []mount.MountPoint
	if device != "" {
		mounts = append(mounts, mount.MountPoint{Device: device, Path: staging, Type: "ext4"})
	}
	return &stagedVolume{
		staging: staging,
		e:       e,
		svc:     svc,
		ns: &nodeServer{
			stageRecords: stageRecords{dir: filepath.Join(t.TempDir(), "stage")},
			Driver:       testDriver(),
			dsmService:   svc,
			Mounter:      &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(mounts)},
			Initiator:    newTestInitiator(e),
			tools:        NewTools(e),
		},
	}
}

func (v *stagedVolume) unstage() error {
	_, err := v.ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          missingVolId,
		StagingTargetPath: v.staging,
	})
	return err
}

func (v *stagedVolume) writeRecord(t *testing.T, rec iscsiStageRecord) {
	t.Helper()
	if err := v.ns.stageRecords.write(rec); err != nil {
		t.Fatal(err)
	}
}

func (v *stagedVolume) writeRawRecord(t *testing.T, raw string) {
	t.Helper()
	if err := os.MkdirAll(v.ns.stageRecords.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v.ns.stageRecords.path(missingVolId), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (v *stagedVolume) recordExists() bool {
	_, err := os.Stat(v.ns.stageRecords.path(missingVolId))
	return err == nil
}

func (v *stagedVolume) stage() error {
	_, err := v.ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          missingVolId,
		StagingTargetPath: v.staging,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		},
	})
	return err
}

// wantCode checks the RPC failed with this status code.
func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("expected code %v, got %v (err: %v)", want, got, err)
	}
}

// wantLoggedOut checks iscsiadm was told to log out of, and then to forget,
// exactly this target.
func wantLoggedOut(t *testing.T, e *recordingExecutor, iqn string) {
	t.Helper()
	logout := e.find("--targetname " + iqn + " --logout")
	del := e.find("--targetname " + iqn + " -o delete")
	if logout < 0 {
		t.Errorf("expected a logout of %s, ran: %v", iqn, e.ran)
	}
	if del < 0 {
		t.Errorf("expected the node record of %s to be deleted, ran: %v", iqn, e.ran)
	}
	if logout >= 0 && del >= 0 && del < logout {
		t.Errorf("expected the node record to be removed after the logout, ran: %v", e.ran)
	}
}

// The reported leak: DSM lost the LUN before unstage, so the lookup came back
// empty and the node was never logged out of the target.
func TestUnstageLogsOutOfARecordedTargetWhenDsmLostTheVolume(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone, or kubelet cannot remove the staging directory")
	}
}

// Volumes staged by a driver that kept no record: the mounted device is the
// only thing that still says which target they came from.
func TestUnstageFindsTheTargetOfAnUnrecordedMountThroughSysfs(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", sysfsIqn)
	prev := sysfsRoot
	sysfsRoot = sysfs
	t.Cleanup(func() { sysfsRoot = prev })

	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn)}
	v := newStagedVolume(t, dev, e, &volumeService{})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantLoggedOut(t, e, sysfsIqn)
}

func TestUnstageFindsTheTargetOfAnUnrecordedMultipathMountThroughItsSlaves(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", sysfsIqn)
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "dm-3", "slaves", "sdx"), 0o755); err != nil {
		t.Fatal(err)
	}
	dm := filepath.Join(sysfs, "dm-3")
	if err := os.WriteFile(dm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := sysfsRoot
	sysfsRoot = sysfs
	t.Cleanup(func() { sysfsRoot = prev })

	e := &recordingExecutor{sessionOutput: sessionLine(sysfsIqn)}
	v := newStagedVolume(t, dm, e, &volumeService{})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantLoggedOut(t, e, sysfsIqn)
}

// Logging out of a target this driver did not create, on the strength of a
// guess about what was mounted, is worse than leaving the stale session.
func TestUnstageLeavesAForeignTargetFoundThroughSysfsAlone(t *testing.T) {
	foreign := "iqn.2001-04.com.example:someone-elses-disk"
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", foreign)
	prev := sysfsRoot
	sysfsRoot = sysfs
	t.Cleanup(func() { sysfsRoot = prev })

	e := &recordingExecutor{sessionOutput: sessionLine(foreign)}
	v := newStagedVolume(t, dev, e, &volumeService{})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.ran) != 0 {
		t.Errorf("expected no iscsiadm calls for a target this driver does not own, ran: %v", e.ran)
	}
}

// A record that names another volume describes some other volume's target.
func TestUnstageIgnoresARecordOfAnotherVolume(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRawRecord(t, `{"volumeId":"some-other-volume","iqn":"`+recordedIqn+`","mappingIndex":1}`)

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.ran) != 0 {
		t.Errorf("expected nothing to be logged out, ran: %v", e.ran)
	}
}

// An empty target name would, handed to iscsiadm, address every target on the
// node.
func TestUnstageDoesNotActOnARecordWithoutATarget(t *testing.T) {
	for name, raw := range map[string]string{
		"no iqn":         `{"volumeId":"` + missingVolId + `"}`,
		"not an iqn":     `{"volumeId":"` + missingVolId + `","iqn":"nas.pvc-x","mappingIndex":1}`,
		"negative lun":   `{"volumeId":"` + missingVolId + `","iqn":"` + recordedIqn + `","mappingIndex":-1}`,
		"not even json":  `{"volumeId":`,
		"another volume": `{"volumeId":"other","iqn":"` + recordedIqn + `","mappingIndex":1}`,
	} {
		e := &recordingExecutor{}
		v := newStagedVolume(t, "", e, &volumeService{})
		v.writeRawRecord(t, raw)

		if err := v.unstage(); err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if len(e.ran) != 0 {
			t.Errorf("%s: expected iscsiadm not to be run on an unusable record, ran: %v", name, e.ran)
		}
		if v.recordExists() {
			t.Errorf("%s: an unusable record should not outlive a completed unstage", name)
		}
	}
}

func TestUnstageSucceedsWhenDsmLostTheVolumeAndNothingIdentifiesItsTarget(t *testing.T) {
	e := &recordingExecutor{}
	v := newStagedVolume(t, "", e, &volumeService{})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.ran) != 0 {
		t.Errorf("expected no iscsiadm calls with no target to act on, ran: %v", e.ran)
	}
}

// Not knowing whether the volume exists is not knowing it is gone: the target
// may well still be needed, and the record is what a retry will go on.
func TestUnstageKeepsTheRecordWhenTheLookupFails(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{err: errListIncomplete})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	wantUnavailable(t, v.unstage())

	if !v.recordExists() {
		t.Error("the record was removed although the unstage did not complete")
	}
	if len(e.ran) != 0 {
		t.Errorf("expected nothing to be logged out on a failed lookup, ran: %v", e.ran)
	}
}

func TestUnstageOfAVolumeDsmStillHasLogsOutAndRemovesTheRecord(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone, or kubelet cannot remove the staging directory")
	}
}

// The record exists to survive a failed attempt: kubelet calls again, and by
// then DSM still has nothing to say about the target.
func TestUnstageRetriedAfterAFailedLogoutStillFindsTheRecord(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn), fail: "--logout"}
	v := newStagedVolume(t, "", e, &volumeService{})
	v.writeRecord(t, iscsiStageRecord{VolumeId: missingVolId, Iqn: recordedIqn, MappingIndex: 1})

	if err := v.unstage(); err == nil {
		t.Fatal("expected the unstage to fail so that kubelet retries it")
	}
	if !v.recordExists() {
		t.Fatal("the record was removed although the logout failed")
	}

	e.fail = ""
	e.ran = nil
	if err := v.unstage(); err != nil {
		t.Fatalf("unexpected error on the retry: %v", err)
	}
	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the retry succeeded")
	}
}

func TestStageRecordRoundTrip(t *testing.T) {
	records := stageRecords{dir: filepath.Join(t.TempDir(), "stage")}
	want := iscsiStageRecord{VolumeId: "vol-1", Iqn: recordedIqn, MappingIndex: 2}

	if got, err := records.read("vol-1"); err != nil || got != nil {
		t.Fatalf("expected no record yet, got %v, %v", got, err)
	}
	// The directory is the driver's to create.
	if err := records.write(want); err != nil {
		t.Fatal(err)
	}
	// Writing again must replace it.
	if err := records.write(want); err != nil {
		t.Fatal(err)
	}

	got, err := records.read("vol-1")
	if err != nil || got == nil || *got != want {
		t.Fatalf("read back %v, %v; want %v", got, err, want)
	}

	info, err := os.Stat(records.path("vol-1"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode is %v, want 0600", info.Mode().Perm())
	}

	entries, err := os.ReadDir(records.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the record in its directory, found %v", entries)
	}

	if err := records.remove("vol-1"); err != nil {
		t.Fatal(err)
	}
	if err := records.remove("vol-1"); err != nil {
		t.Errorf("removing a record that is already gone must not fail, got %v", err)
	}
}

// The volume ID is attacker-adjacent input to a file name.
func TestStageRecordPathStaysInTheDirectoryAndIsInjective(t *testing.T) {
	records := stageRecords{dir: "/records"}
	seen := map[string]string{}
	for _, id := range []string{"vol-1", "a/b", "a%2Fb", "a_b", "a-b", ".", "..", ".hidden", "../../etc/passwd", "a.b", "é"} {
		path := records.path(id)
		if filepath.Dir(path) != "/records" {
			t.Errorf("record of %q escapes the directory: %s", id, path)
		}
		if other, dup := seen[path]; dup {
			t.Errorf("volumes %q and %q share the record %s", id, other, path)
		}
		seen[path] = id
	}
}

// A crash between creating a temporary file and renaming it leaves the file
// behind; nothing else would ever remove it.
func TestStaleTemporaryRecordFilesAreSweptAtStartup(t *testing.T) {
	records := stageRecords{dir: t.TempDir()}
	if err := records.write(iscsiStageRecord{VolumeId: "vol-1", Iqn: recordedIqn}); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(records.dir, stageRecordTempPrefix+"killed")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	records.sweep()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temporary file was not removed: %v", err)
	}
	if got, err := records.read("vol-1"); err != nil || got == nil {
		t.Errorf("sweeping must leave the records alone, read %v, %v", got, err)
	}
}

func TestZeroStageRecordsKeepNothing(t *testing.T) {
	var records stageRecords
	if err := records.write(iscsiStageRecord{VolumeId: "vol-1", Iqn: recordedIqn}); err == nil {
		t.Error("expected an error writing with no directory configured")
	}
	if got, err := records.read("vol-1"); got != nil || err != nil {
		t.Errorf("read %v, %v; want nothing", got, err)
	}
	if err := records.remove("vol-1"); err != nil {
		t.Error(err)
	}
}

func TestStageRecordDirDefaultsToTheSocketDirectory(t *testing.T) {
	prev := StageRecordDir
	t.Cleanup(func() { StageRecordDir = prev })

	StageRecordDir = ""
	for endpoint, want := range map[string]string{
		"unix://csi/csi.sock": "/csi/stage",
		"unix:///var/lib/kubelet/plugins/" + DriverName + "/csi.sock": "/var/lib/kubelet/plugins/" + DriverName + "/stage",
		"tcp://127.0.0.1:10000": "/var/lib/kubelet/plugins/" + DriverName + "/stage",
	} {
		if got := stageRecordDir(endpoint); got != want {
			t.Errorf("stageRecordDir(%q) = %q, want %q", endpoint, got, want)
		}
	}

	StageRecordDir = "/elsewhere"
	if got := stageRecordDir("unix://csi/csi.sock"); got != "/elsewhere" {
		t.Errorf("the flag must win, got %q", got)
	}
}

func TestStageRecordRefusesAnUnusableTarget(t *testing.T) {
	records := stageRecords{dir: t.TempDir()}
	for _, rec := range []iscsiStageRecord{
		{VolumeId: "vol-1"},
		{VolumeId: "vol-1", Iqn: "nas.pvc-x"},
		{VolumeId: "vol-1", Iqn: recordedIqn, MappingIndex: -1},
		{Iqn: recordedIqn},
	} {
		if err := records.write(rec); err == nil {
			t.Errorf("expected %+v not to be recorded", rec)
		}
	}
}

func TestIscsiTargetOfDeviceRefusesLegsThatDisagree(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", recordedIqn)
	other, _ := fakeSysfs(t, "sdy", "session8", "7:0:0:1", sysfsIqn)

	// Graft the second leg into the first tree.
	canonical, err := filepath.EvalSymlinks(filepath.Join(other, "block", "sdy", "device"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "sdy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(sysfs, "block", "sdy", "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysfs, "class", "iscsi_session", "session8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysfs, "class", "iscsi_session", "session8", "targetname"), []byte(sysfsIqn), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, leg := range []string{"sdx", "sdy"} {
		if err := os.MkdirAll(filepath.Join(sysfs, "block", "dm-0", "slaves", leg), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err = iscsiTargetOfDevice(filepath.Join(sysfs, "dm-0"), sysfs)
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("expected the disagreeing legs to be refused, got %v", err)
	}
}
