// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

// recordingExecutor captures the commands the driver runs so a teardown can be
// checked for what it did and in which order.
type recordingExecutor struct {
	ran []string
	// fail makes any command whose joined form contains this substring return
	// an error.
	fail string
	// sessionOutput is what `iscsiadm -m session` reports.
	sessionOutput string
	// failStatus, if set, makes the failing command exit with this status, as
	// iscsiadm does for "nothing found" (21); failOutput is what it prints.
	failStatus int
	failOutput string
	// onCommand, if set, is called with each command as it is started.
	onCommand func(line string)
}

func (e *recordingExecutor) Command(cmd string, args ...string) utilexec.Cmd {
	line := strings.TrimSpace(cmd + " " + strings.Join(args, " "))
	e.ran = append(e.ran, line)
	if e.onCommand != nil {
		e.onCommand(line)
	}

	out := ""
	if cmd == "iscsiadm" && len(args) >= 2 && args[0] == "-m" && args[1] == "session" {
		out = e.sessionOutput
	}

	fake := &testingexec.FakeCmd{
		CombinedOutputScript: []testingexec.FakeAction{
			func() ([]byte, []byte, error) {
				if e.fail != "" && strings.Contains(line, e.fail) {
					if e.failStatus != 0 {
						return []byte(e.failOutput), nil, testingexec.FakeExitError{Status: e.failStatus}
					}
					return []byte("boom"), nil, fmt.Errorf("command failed")
				}
				return []byte(out), nil, nil
			},
		},
	}
	return fake
}

func (e *recordingExecutor) CommandContext(_ context.Context, cmd string, args ...string) utilexec.Cmd {
	return e.Command(cmd, args...)
}

func (e *recordingExecutor) find(substr string) int {
	for i, line := range e.ran {
		if strings.Contains(line, substr) {
			return i
		}
	}
	return -1
}

const (
	testIqn    = "iqn.2000-01.com.synology:test.target"
	testPortal = "1.2.3.4:3260"
)

func sessionLine(iqn string) string {
	return "tcp: [1] " + testPortal + ",1 " + iqn + " (non-flash)"
}

func newTestInitiator(e *recordingExecutor) *initiatorDriver {
	return &initiatorDriver{tools: NewTools(e)}
}

// The record outliving the session is the reported failure mode (#138): iscsid
// keeps trying to log in to a target DSM has already deleted.
func TestLogoutRemovesTheNodeRecord(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	if err := newTestInitiator(e).logout(testIqn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logout := e.find("--logout")
	del := e.find("-o delete")
	if logout < 0 {
		t.Fatalf("expected a logout, ran: %v", e.ran)
	}
	if del < 0 {
		t.Fatalf("expected the node record to be deleted, ran: %v", e.ran)
	}
	if del < logout {
		t.Errorf("expected the record to be removed after the logout, ran: %v", e.ran)
	}
}

// The case that actually strands a record is the one where the session is
// already gone, so an early return on "no session" would skip the cleanup
// exactly when it matters.
func TestNodeRecordIsRemovedEvenWithNoSession(t *testing.T) {
	e := &recordingExecutor{sessionOutput: ""} // no sessions at all
	if err := newTestInitiator(e).logout(testIqn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.find("-o delete") < 0 {
		t.Errorf("expected the stale node record to be removed with no session present, ran: %v", e.ran)
	}
	if e.find("--logout") >= 0 {
		t.Errorf("did not expect a logout when there is no session, ran: %v", e.ran)
	}
}

// A node record left behind has iscsid retrying a login for good, and nothing
// would ever tell the caller, so it is a failure of the logout.
func TestLogoutFailsWhenTheRecordCannotBeRemoved(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "-o delete"}
	if err := newTestInitiator(e).logout(testIqn); err == nil {
		t.Error("a node record that could not be removed must fail the logout, or the unstage reports done with it still there")
	}
}

// No records is the state the logout was after, as when the login never got as
// far as creating one.
func TestLogoutToleratesThereBeingNoNodeRecord(t *testing.T) {
	for name, e := range map[string]*recordingExecutor{
		"exit status 21": {sessionOutput: sessionLine(testIqn), fail: "-o delete", failStatus: 21},
		"message alone":  {sessionOutput: sessionLine(testIqn), fail: "-o delete", failStatus: 1, failOutput: "iscsiadm: No records found"},
	} {
		if err := newTestInitiator(e).logout(testIqn); err != nil {
			t.Errorf("%s: no records found should not fail the logout, got %v", name, err)
		}
	}
}

func TestLogoutFailsWhenTheSessionCannotBeEnded(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "--logout"}
	if err := newTestInitiator(e).logout(testIqn); err == nil {
		t.Fatal("expected the failed logout to be reported")
	}
	if e.find("-o delete") >= 0 {
		t.Errorf("the record must outlive a logout that failed, ran: %v", e.ran)
	}
}

// Not being able to list the sessions is not the same as there being none:
// going on as if there were would skip the logout of a session that is there.
func TestLogoutFailsWhenTheSessionsCannotBeListed(t *testing.T) {
	e := &recordingExecutor{fail: "-m session"}
	if err := newTestInitiator(e).logout(testIqn); err == nil {
		t.Fatal("expected the failure to list sessions to be reported")
	}
	if e.find("--logout") >= 0 || e.find("-o delete") >= 0 {
		t.Errorf("nothing may be torn down on a guess about the sessions, ran: %v", e.ran)
	}
}

func TestNoActiveSessionsIsNotAnError(t *testing.T) {
	e := &recordingExecutor{fail: "-m session", failStatus: 21, failOutput: "iscsiadm: No active sessions."}
	tools := NewTools(e)
	sessions, err := tools.iscsiadm_session()
	if err != nil || len(sessions) != 0 {
		t.Errorf("got %v, %v; want no sessions and no error", sessions, err)
	}
}

// With no target name iscsiadm acts on every target on the node.
func TestIscsiadmIsNeverAskedToActOnNoTarget(t *testing.T) {
	e := &recordingExecutor{}
	tools := NewTools(e)
	if err := tools.iscsiadm_delete_node(""); err == nil {
		t.Error("expected the delete of an empty target to be refused")
	}
	if err := tools.iscsiadm_logout(""); err == nil {
		t.Error("expected the logout of an empty target to be refused")
	}
	if len(e.ran) != 0 {
		t.Errorf("expected iscsiadm not to be run, ran: %v", e.ran)
	}
}

func TestFlushWritesOutTheDeviceBeforeItGoesAway(t *testing.T) {
	e := &recordingExecutor{}
	tools := NewTools(e)

	if err := tools.blockdev_flushbufs("/dev/sdx"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.find("blockdev --flushbufs /dev/sdx") < 0 {
		t.Errorf("expected the device to be flushed, ran: %v", e.ran)
	}
}

func TestFlushReportsFailure(t *testing.T) {
	e := &recordingExecutor{fail: "flushbufs"}
	tools := NewTools(e)

	if err := tools.blockdev_flushbufs("/dev/sdx"); err == nil {
		t.Error("expected an error when the flush fails; silently continuing to logout " +
			"is what loses the writes still held in memory")
	}
}
