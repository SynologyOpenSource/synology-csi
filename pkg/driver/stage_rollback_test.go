// Copyright 2025 Synology Inc.

package driver

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// loginCreatesSession makes the executor report a session for iqn from the
// moment iscsiadm is asked to log in, the way a login that works does.
func loginCreatesSession(e *recordingExecutor, iqn string) {
	e.onCommand = func(line string) {
		if strings.Contains(line, "--login") {
			e.sessionOutput = sessionLine(iqn)
		}
	}
}

// kubelet takes an Internal error from NodeStageVolume as final and does not
// have to call NodeUnstageVolume, so a stage that logged in and then failed has
// to end that session itself.
func TestFailedStageEndsTheSessionItCreated(t *testing.T) {
	e := &recordingExecutor{}
	loginCreatesSession(e, recordedIqn)
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	wantCode(t, v.stage(), codes.Internal) // logged in, but no device appears to mount
	wantLoggedOut(t, e, recordedIqn)
	if v.recordExists() {
		t.Error("the record has to be gone once the stage has undone its login")
	}
}

func TestFailedStageKeepsTheRecordWhenItCannotEndItsSession(t *testing.T) {
	e := &recordingExecutor{fail: "--logout"}
	loginCreatesSession(e, recordedIqn)
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	wantCode(t, v.stage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the session is still there")
	}
}

func TestFailedStageKeepsTheRecordWhenItCannotRemoveTheNodeRecordsOfItsSession(t *testing.T) {
	e := &recordingExecutor{fail: "-o delete"}
	loginCreatesSession(e, recordedIqn)
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	wantCode(t, v.stage(), codes.Internal)
	if !v.recordExists() {
		t.Error("the record was removed although the node records are still there")
	}
}

// A session that exists already may be another stage's: the volume lock only
// serializes operations on one volume, and a target can carry another's.
func TestFailedStageDoesNotEndASessionThatPredatedIt(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(recordedIqn)}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	wantCode(t, v.stage(), codes.Internal)
	if e.find("--logout") >= 0 || e.find("-o delete") >= 0 {
		t.Errorf("ended a session the stage did not create, ran: %v", e.ran)
	}
}

// Discovery files a node record for every target the NAS advertises, and the
// login for this one then fails. With no session there is still a record of the
// target, which iscsid would keep retrying, so it must go before the stage
// forgets which target it was.
func TestFailedLoginRemovesTheNodeRecordsOfItsTargetOnly(t *testing.T) {
	e := &recordingExecutor{fail: "--login"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail")
	}
	if e.find("--targetname "+recordedIqn+" -o delete") < 0 {
		t.Fatalf("the stage dropped its record without deleting discovery's node records, ran: %v", e.ran)
	}
	if e.find("--logout") >= 0 {
		t.Errorf("logged out although there was no session, ran: %v", e.ran)
	}
	for _, line := range e.ran {
		if strings.Contains(line, "-o delete") && !strings.Contains(line, "--targetname "+recordedIqn) {
			t.Errorf("deleted node records of another target: %s", line)
		}
	}
	if v.recordExists() {
		t.Error("the record has to be gone once the node records are")
	}
}

func TestFailedLoginKeepsTheRecordWhenTheNodeRecordsCannotBeRemoved(t *testing.T) {
	// Fails the login and the delete alike.
	e := &recordingExecutor{fail: "-m node --targetname"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail")
	}
	if e.find("-o delete") < 0 {
		t.Fatalf("expected an attempt to delete the node records, ran: %v", e.ran)
	}
	if !v.recordExists() {
		t.Error("the record was removed although the node records are still there")
	}
}

// iscsiadm reports "no records" as an error of its own, and that is what the
// cleanup was after.
func TestFailedLoginDropsTheRecordWhenThereAreNoNodeRecordsToRemove(t *testing.T) {
	e := &recordingExecutor{fail: "-m node --targetname", failStatus: 21, failOutput: "iscsiadm: No records found"}
	v := newStagedVolume(t, "", e, &volumeService{volume: iscsiVolume(recordedIqn)})

	if err := v.stage(); err == nil {
		t.Fatal("expected the stage to fail")
	}
	if v.recordExists() {
		t.Error("the record was kept although there were no node records to remove")
	}
}
