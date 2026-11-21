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
	// failErr is the error a failing command returns; a plain error if nil.
	failErr error
	// failOut is the output a failing command prints; "boom" if empty.
	failOut string
	// sessionOutput is what `iscsiadm -m session` reports.
	sessionOutput string
}

func (e *recordingExecutor) Command(cmd string, args ...string) utilexec.Cmd {
	line := strings.TrimSpace(cmd + " " + strings.Join(args, " "))
	e.ran = append(e.ran, line)

	out := ""
	if cmd == "iscsiadm" && len(args) >= 2 && args[0] == "-m" && args[1] == "session" {
		out = e.sessionOutput
	}

	fake := &testingexec.FakeCmd{
		CombinedOutputScript: []testingexec.FakeAction{
			func() ([]byte, []byte, error) {
				if e.fail != "" && strings.Contains(line, e.fail) {
					err := e.failErr
					if err == nil {
						err = fmt.Errorf("command failed")
					}
					failOut := e.failOut
					if failOut == "" {
						failOut = "boom"
					}
					return []byte(failOut), nil, err
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
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err != nil {
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
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.find("-o delete") < 0 {
		t.Errorf("expected the stale node record to be removed with no session present, ran: %v", e.ran)
	}
	if e.find("--logout") >= 0 {
		t.Errorf("did not expect a logout when there is no session, ran: %v", e.ran)
	}
}

// A record that cannot be removed keeps iscsid retrying the login, so the
// unstage is reported as failed and kubelet calls again.
func TestLogoutReportsAFailedRecordRemoval(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "-o delete"}
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err == nil {
		t.Error("a record that could not be removed should fail the logout")
	}
}

// There being no record to remove is what the cleanup wants, not a failure.
func TestLogoutIgnoresAMissingRecord(t *testing.T) {
	tests := []struct {
		name string
		err  error
		out  string
	}{
		{"exit status 21", testingexec.FakeExitError{Status: 21}, "iscsiadm: No records found"},
		{"output only", nil, "iscsiadm: No records found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "-o delete", failErr: tt.err, failOut: tt.out}
			if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err != nil {
				t.Errorf("a missing node record should not fail the logout, got %v", err)
			}
		})
	}
}

// Not being able to list the sessions is not the same as having none: reading
// it as none would skip the logout and report the volume unstaged.
func TestLogoutReportsASessionListingFailure(t *testing.T) {
	e := &recordingExecutor{fail: "-m session"}
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err == nil {
		t.Fatal("expected an error when the sessions cannot be listed")
	}
	if e.find("--logout") >= 0 || e.find("-o delete") >= 0 {
		t.Errorf("expected neither a logout nor a record removal, ran: %v", e.ran)
	}
}

// iscsiadm exits with 21 when there are no sessions, which is a normal state.
func TestLogoutTreatsNoActiveSessionsAsEmpty(t *testing.T) {
	e := &recordingExecutor{fail: "-m session", failErr: testingexec.FakeExitError{Status: 21}, failOut: "iscsiadm: No active sessions."}
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.find("--logout") >= 0 {
		t.Errorf("did not expect a logout with no sessions, ran: %v", e.ran)
	}
	if e.find("-o delete") < 0 {
		t.Errorf("expected the node record to be removed, ran: %v", e.ran)
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
