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

// Detaching succeeded; a leftover record is noise, not a failure to report.
func TestLogoutSucceedsWhenTheRecordCannotBeRemoved(t *testing.T) {
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "-o delete"}
	if err := newTestInitiator(e).logout(testIqn, "1.2.3.4"); err != nil {
		t.Errorf("a record that could not be removed should not fail the unstage, got %v", err)
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
