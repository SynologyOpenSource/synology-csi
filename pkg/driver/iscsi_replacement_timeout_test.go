// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

const (
	updateReplacementTimeout = "--name node.session.timeo.replacement_timeout --value 600"
	otherVendorIqn           = "iqn.2003-01.org.linux-iscsi.host:sn.abc"
	// Same vendor prefix as the driver's targets, but not created by the driver.
	sameVendorForeignIqn = "iqn.2000-01.com.synology:nas.backups"
)

// sessionSysfs is a fabricated /sys that the package's sysfsRoot points at for the
// duration of a test.
type sessionSysfs struct {
	t    *testing.T
	root string
}

func newSessionSysfs(t *testing.T) *sessionSysfs {
	t.Helper()
	old := sysfsRoot
	root := t.TempDir()
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = old })
	return &sessionSysfs{t: t, root: root}
}

func (f *sessionSysfs) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sessionSysfs) sessionPath(sid int, attr string) string {
	return filepath.Join("class", "iscsi_session", "session"+strconv.Itoa(sid), attr)
}

// session adds a logged in session with the 120s the kernel starts with.
func (f *sessionSysfs) session(sid int, iqn string) *sessionSysfs {
	f.write(f.sessionPath(sid, "recovery_tmo"), "120\n")
	f.write(f.sessionPath(sid, "targetname"), iqn+"\n")
	return f
}

func (f *sessionSysfs) removeSession(sid int) {
	if err := os.RemoveAll(filepath.Join(f.root, f.sessionPath(sid, ""))); err != nil {
		f.t.Fatal(err)
	}
}

// disk adds a SCSI disk below the session, where the kernel puts it.
func (f *sessionSysfs) disk(sid int, name string) *sessionSysfs {
	f.write(filepath.Join(f.sessionPath(sid, "device"), "target2:0:0", "2:0:0:1", "block", name, "dev"), "8:0\n")
	return f
}

// holder makes the device-mapper device dm hold the disk, with the given dm uuid.
func (f *sessionSysfs) holder(disk, dm, uuid string) *sessionSysfs {
	f.write(filepath.Join("block", disk, "holders", dm, "x"), "")
	f.write(filepath.Join("block", dm, "dm", "uuid"), uuid+"\n")
	return f
}

func (f *sessionSysfs) recoveryTmo(sid int) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, f.sessionPath(sid, "recovery_tmo")))
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func (f *sessionSysfs) setTargetName(sid int, iqn string) {
	f.write(f.sessionPath(sid, "targetname"), iqn+"\n")
}

// fakeClock replaces the time source of the retries: every wait fires at once
// and is recorded.
type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	// onWait runs when a wait is requested, before it fires.
	onWait func(n int)
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	n := len(c.waits)
	c.mu.Unlock()
	if c.onWait != nil {
		c.onWait(n)
	}
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func (c *fakeClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// busyFirst makes the first n writes fail with EBUSY, like the kernel does for
// a session that is not logged in, and counts the writes.
type busyFirst struct {
	n     int32
	calls int32
}

func (b *busyFirst) write(path, value string) error {
	if atomic.AddInt32(&b.calls, 1) <= b.n {
		return &fs.PathError{Op: "write", Path: path, Err: syscall.EBUSY}
	}
	return writeSysfsAttribute(path, value)
}

func (b *busyFirst) count() int { return int(atomic.LoadInt32(&b.calls)) }

const retryInterval = 7 * time.Second

// newTimeoutTools returns tools whose retries never fire on their own and stop
// with the test.
func newTimeoutTools(t *testing.T, e *recordingExecutor, timeout int) tools {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tl := NewTools(e, WithIscsiReplacementTimeout(timeout), WithContext(ctx))
	tl.tuning.retryInterval = retryInterval
	tl.tuning.after = func(time.Duration) <-chan time.Time { return nil }
	return tl
}

func newTimeoutInitiator(t *testing.T, e *recordingExecutor, timeout int) *initiatorDriver {
	t.Helper()
	return &initiatorDriver{tools: newTimeoutTools(t, e, timeout)}
}

func sessionOutput(lines ...string) string { return strings.Join(lines, "\n") }

func sessionLineFor(sid int, iqn string) string {
	return "tcp: [" + strconv.Itoa(sid) + "] " + testPortal + ",1 " + iqn + " (non-flash)"
}

// The record only reaches the session through login, so the update has to land
// between discovery (which creates the record) and login (which reads it).
func TestLoginSetsReplacementTimeoutBetweenDiscoveryAndLogin(t *testing.T) {
	newSessionSysfs(t)
	e := &recordingExecutor{}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}

	discover := e.find("--discover")
	update := e.find("--op update --name node.session.timeo.replacement_timeout")
	login := e.find("--login")
	if discover < 0 || update < 0 || login < 0 || !(discover < update && update < login) {
		t.Fatalf("want discovery < update < login, got %v", e.ran)
	}
	want := "iscsiadm -m node --targetname " + testIqn + " --portal " + testPortal +
		" --op update " + updateReplacementTimeout
	if e.ran[update] != want {
		t.Errorf("update command = %q, want %q", e.ran[update], want)
	}
}

// 0 is the upstream behaviour: leave whatever the host configures alone.
func TestLoginLeavesReplacementTimeoutAloneWhenUnset(t *testing.T) {
	newSessionSysfs(t)
	e := &recordingExecutor{}
	if err := newTimeoutInitiator(t, e, 0).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	if i := e.find("replacement_timeout"); i >= 0 {
		t.Errorf("unexpected command %q", e.ran[i])
	}
	if e.find("--login") < 0 {
		t.Errorf("login not issued: %v", e.ran)
	}
}

// A target logged in through several portals is multipathd's to tune.
func TestLoginSkipsReplacementTimeoutWhenNotTuning(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{}
	d := newTimeoutInitiator(t, e, 600)

	if err := d.login(testIqn, testPortal, false); err != nil {
		t.Fatalf("login: %v", err)
	}
	if i := e.find("replacement_timeout"); i >= 0 {
		t.Errorf("new login: unexpected command %q", e.ran[i])
	}
	if e.find("--login") < 0 {
		t.Errorf("login not issued: %v", e.ran)
	}

	e = &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	d = newTimeoutInitiator(t, e, 600)
	if err := d.login(testIqn, testPortal, false); err != nil {
		t.Fatalf("login: %v", err)
	}
	if e.find("--op update") >= 0 {
		t.Errorf("existing session: node record updated: %v", e.ran)
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("existing session: recovery_tmo = %q, want 120", got)
	}
}

func TestMayTuneReplacementTimeoutOnlyWithOnePortal(t *testing.T) {
	tl := newTimeoutTools(t, &recordingExecutor{}, 600)
	if !tl.mayTuneReplacementTimeout(testIqn, []string{"1.2.3.4:3260"}) {
		t.Error("one portal: want tuning")
	}
	if tl.mayTuneReplacementTimeout(testIqn, []string{"1.2.3.4:3260", "1.2.3.5:3260"}) {
		t.Error("two portals (multipath): want no tuning")
	}
}

// A tuning knob must not break staging.
func TestLoginContinuesWhenReplacementTimeoutUpdateFails(t *testing.T) {
	newSessionSysfs(t)
	e := &recordingExecutor{fail: "replacement_timeout"}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login must succeed when the update fails: %v", err)
	}
	if e.find("--login") < 0 {
		t.Errorf("login not issued after failed update: %v", e.ran)
	}
}

// Volumes staged before the flag was set: there is no login to carry the
// record's value, so the session itself has to be written.
func TestLoginExistingSessionUpdatesRecordAndRecoveryTmo(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, otherVendorIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(sessionLine(testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+otherVendorIqn+" (non-flash)")}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}

	if e.find("--login") >= 0 || e.find("--discover") >= 0 {
		t.Errorf("existing session must not be logged in again: %v", e.ran)
	}
	if e.find("--targetname "+testIqn+" --portal "+testPortal+" --op update "+updateReplacementTimeout) < 0 {
		t.Errorf("node record not updated: %v", e.ran)
	}
	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("session1 recovery_tmo = %q, want 600", got)
	}
	if got := fsys.recoveryTmo(2); got != "120" {
		t.Errorf("session2 belongs to another target, recovery_tmo = %q, want 120", got)
	}
}

func TestLoginExistingSessionUntouchedWhenUnset(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	if err := newTimeoutInitiator(t, e, 0).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	if e.find("replacement_timeout") >= 0 {
		t.Errorf("unexpected update: %v", e.ran)
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
}

// A missing sysfs entry or failed record update is logged, not returned, and a
// session that is gone must not leave a file behind.
func TestLoginExistingSessionToleratesFailures(t *testing.T) {
	fsys := newSessionSysfs(t)
	e := &recordingExecutor{fail: "replacement_timeout", sessionOutput: sessionLine(testIqn)}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsys.root, "class")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("writing to a missing session created files: %v", err)
	}
}

// The kernel refuses the write while the session is down; staging must not
// block on that, and the value has to arrive once the session is back.
func TestLoginExistingSessionRecoveringIsRetriedInBackground(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	d := newTimeoutInitiator(t, e, 600)

	busy := &busyFirst{n: 3}
	d.tools.tuning.writeSysfs = busy.write
	gate := make(chan time.Time)
	var waited int32
	d.tools.tuning.after = func(time.Duration) <-chan time.Time {
		atomic.AddInt32(&waited, 1)
		return gate
	}

	if err := d.login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login must succeed while the session is recovering: %v", err)
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Fatalf("recovery_tmo = %q, want 120 until the retry", got)
	}

	// Staging more volumes of the same session must not pile up retries.
	d.tools.applyReplacementTimeout(mustFindSessions(t, &d.tools, testIqn, testPortal), false)
	d.tools.applyReplacementTimeout(mustFindSessions(t, &d.tools, testIqn, testPortal), false)

	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for fsys.recoveryTmo(1) != "600" {
		if time.Now().After(deadline) {
			t.Fatalf("recovery_tmo = %q, retry never wrote 600", fsys.recoveryTmo(1))
		}
		time.Sleep(time.Millisecond)
	}
	for {
		d.tools.tuning.mu.Lock()
		n := len(d.tools.tuning.retrying)
		d.tools.tuning.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&waited); got != 1 {
		t.Errorf("%d retries ran for one session, want 1", got)
	}
	if got := busy.count(); got != 4 {
		// 3 refused by the kernel (login and two applies), 1 from the retry.
		t.Errorf("sysfs writes = %d, want 4", got)
	}
}

func newRetryTools(t *testing.T, clock *fakeClock, busy *busyFirst) tools {
	t.Helper()
	return newRetryToolsWith(t, &recordingExecutor{sessionOutput: sessionLine(testIqn)}, clock, busy)
}

// newRetryToolsWith is newRetryTools with the executor the test controls, to
// change the session list while the retry runs.
func newRetryToolsWith(t *testing.T, e *recordingExecutor, clock *fakeClock, busy *busyFirst) tools {
	t.Helper()
	tl := newTimeoutTools(t, e, 600)
	tl.tuning.after = clock.after
	tl.tuning.writeSysfs = busy.write
	return tl
}

func TestRetryWritesOnceTheSessionIsBack(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	clock := &fakeClock{}
	busy := &busyFirst{n: 2}
	tl := newRetryTools(t, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("recovery_tmo = %q, want 600", got)
	}
	if busy.count() != 3 {
		t.Errorf("writes = %d, want 3 (two EBUSY, one success)", busy.count())
	}
	for _, w := range clock.recorded() {
		if w != retryInterval {
			t.Errorf("waited %v between attempts, want %v", w, retryInterval)
		}
	}
	if len(clock.recorded()) != 3 {
		t.Errorf("waits = %v, want one before each attempt", clock.recorded())
	}
}

// There is no cap on the number of attempts while the session stays down.
func TestRetryHasNoAttemptLimit(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	clock := &fakeClock{}
	busy := &busyFirst{n: 1000}
	tl := newRetryTools(t, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("recovery_tmo = %q, want 600 after 1000 busy attempts", got)
	}
}

func TestRetryStopsQuietlyWhenSessionIsGone(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	clock := &fakeClock{onWait: func(n int) {
		if n == 2 {
			fsys.removeSession(1)
		}
	}}
	busy := &busyFirst{n: 1000}
	tl := newRetryTools(t, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	if len(clock.recorded()) != 2 {
		t.Errorf("waits = %v, want the retry to stop at the second", clock.recorded())
	}
	if _, err := os.Stat(filepath.Join(fsys.root, fsys.sessionPath(1, ""))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("session directory was recreated: %v", err)
	}
}

// Same sid, different target: the session we were waiting for is gone and this
// one is not ours to touch.
func TestRetryStopsWhenSessionIdentityChanges(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	clock := &fakeClock{onWait: func(n int) {
		if n == 2 {
			fsys.setTargetName(1, sameVendorForeignIqn)
		}
	}}
	busy := &busyFirst{n: 1}
	tl := newRetryTools(t, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	// Attempt 1 (EBUSY) happened, attempt 2 must have been refused before writing.
	if busy.count() != 1 {
		t.Errorf("writes = %d, want 1", busy.count())
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo of another target = %q, want 120", got)
	}
}

func TestRetryStopsOnOtherErrors(t *testing.T) {
	newSessionSysfs(t).session(1, testIqn)
	clock := &fakeClock{}
	var writes int32
	tl := newTimeoutTools(t, &recordingExecutor{sessionOutput: sessionLine(testIqn)}, 600)
	tl.tuning.after = clock.after
	tl.tuning.writeSysfs = func(path, value string) error {
		atomic.AddInt32(&writes, 1)
		return &fs.PathError{Op: "write", Path: path, Err: syscall.EINVAL}
	}

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	if writes != 1 {
		t.Errorf("writes = %d, want 1: only EBUSY is retried", writes)
	}
}

func TestRetryStopsWhenCancelled(t *testing.T) {
	newSessionSysfs(t).session(1, testIqn)
	tl := newTimeoutTools(t, &recordingExecutor{}, 600) // after never fires
	ctx, cancel := context.WithCancel(context.Background())

	finished := make(chan struct{})
	go func() {
		tl.retryRecoveryTimeout(ctx, iscsiSession{Id: 1, Iqn: testIqn})
		close(finished)
	}()
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not stop when its context was cancelled")
	}
}

// multipathd may build its map on the path while we are waiting.
func TestRetryStopsWhenSessionBecomesMultipath(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).disk(1, "sdb")
	clock := &fakeClock{onWait: func(n int) {
		if n == 2 {
			fsys.holder("sdb", "dm-0", "mpath-3600140512345")
		}
	}}
	busy := &busyFirst{n: 1}
	tl := newRetryTools(t, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn})

	if busy.count() != 1 {
		t.Errorf("writes = %d, want 1", busy.count())
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
}

func TestSessionHeldByMultipath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(f *sessionSysfs)
		want  bool
	}{
		{"no disk yet", func(f *sessionSysfs) {}, false},
		{"plain disk", func(f *sessionSysfs) { f.disk(1, "sdb") }, false},
		{"held by multipath", func(f *sessionSysfs) {
			f.disk(1, "sdb").holder("sdb", "dm-3", "mpath-36001405abc")
		}, true},
		{"held by another dm target", func(f *sessionSysfs) {
			f.disk(1, "sdb").holder("sdb", "dm-3", "LVM-abcdef")
		}, false},
		{"holder without a uuid", func(f *sessionSysfs) {
			f.disk(1, "sdb")
			f.write(filepath.Join("block", "sdb", "holders", "dm-3", "x"), "")
		}, false},
		{"multipath on a disk of another session", func(f *sessionSysfs) {
			f.session(2, otherVendorIqn).disk(1, "sdb").disk(2, "sdc").holder("sdc", "dm-4", "mpath-1")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionSysfs(t).session(1, testIqn)
			tc.build(f)
			got, err := sessionHeldByMultipath(1)
			if err != nil {
				t.Fatalf("sessionHeldByMultipath: %v", err)
			}
			if got != tc.want {
				t.Errorf("sessionHeldByMultipath = %v, want %v", got, tc.want)
			}
		})
	}
}

// multipathd's failover policy wins: neither the record nor the live session
// is written.
func TestApplySkipsSessionsHeldByMultipath(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).disk(1, "sdb").holder("sdb", "dm-0", "mpath-36001405abc")
	fsys.session(2, "iqn.2000-01.com.synology:single").disk(2, "sdc")
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		sessionLineFor(2, "iqn.2000-01.com.synology:single"),
	)}
	tl := newTimeoutTools(t, e, 600)

	tl.applyReplacementTimeout([]iscsiSession{
		{Id: 1, Iqn: testIqn, Portal: testPortal},
		{Id: 2, Iqn: "iqn.2000-01.com.synology:single", Portal: testPortal},
	}, true)

	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("multipath session recovery_tmo = %q, want 120", got)
	}
	if got := fsys.recoveryTmo(2); got != "600" {
		t.Errorf("single path session recovery_tmo = %q, want 600", got)
	}
	for _, line := range e.ran {
		if strings.Contains(line, testIqn) {
			t.Errorf("node record of the multipath target touched: %q", line)
		}
	}
	if e.find("single") < 0 {
		t.Errorf("node record of the single path target not updated: %v", e.ran)
	}
}

func TestLoginExistingSessionHeldByMultipathIsLeftAlone(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).disk(1, "sdb").holder("sdb", "dm-0", "mpath-1")
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	if e.find("--op update") >= 0 {
		t.Errorf("node record updated: %v", e.ran)
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
}

// During boot, or while multipathd rebuilds its maps, both sessions of a
// multipath target exist before their disks have a device-mapper holder. They
// have to be left alone anyway, or the value written here overwrites the
// shorter recovery_tmo multipathd sets once it has assembled the map.
func TestApplySkipsTargetWithSeveralSessionsBeforeHolders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second string
	}{
		{"two portals", "tcp: [2] 5.6.7.8:3260,1 " + testIqn + " (non-flash)"},
		{"two sessions to one portal", sessionLineFor(2, testIqn)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn).disk(1, "sdb").disk(2, "sdc")
			e := &recordingExecutor{sessionOutput: sessionOutput(sessionLine(testIqn), tc.second)}
			tl := newTimeoutTools(t, e, 600)

			tl.applyReplacementTimeout(mustFindSessions(t, &tl, testIqn, ""), false)

			for _, sid := range []int{1, 2} {
				if got := fsys.recoveryTmo(sid); got != "120" {
					t.Errorf("session%d recovery_tmo = %q, want 120", sid, got)
				}
			}
			if e.find("--op update") >= 0 {
				t.Errorf("node record updated: %v", e.ran)
			}
		})
	}
}

// Staging a volume whose target already has a session through the other
// portal, with no holder yet, is the same case reached through login.
func TestLoginExistingSessionOfTargetWithSeveralSessionsIsLeftAlone(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(sessionLine(testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)")}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	if e.find("--op update") >= 0 {
		t.Errorf("node record updated: %v", e.ran)
	}
	for _, sid := range []int{1, 2} {
		if got := fsys.recoveryTmo(sid); got != "120" {
			t.Errorf("session%d recovery_tmo = %q, want 120", sid, got)
		}
	}
}

// The node record of a new login is not written either when the target is
// already logged in through another portal.
func TestLoginNewPortalOfLoggedInTargetLeavesRecordAlone(t *testing.T) {
	newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{sessionOutput: "tcp: [1] 5.6.7.8:3260,1 " + testIqn + " (non-flash)"}
	if err := newTimeoutInitiator(t, e, 600).login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	if e.find("--login") < 0 {
		t.Errorf("login not issued: %v", e.ran)
	}
	if e.find("--op update --name node.session.timeo.replacement_timeout") >= 0 {
		t.Errorf("node record updated: %v", e.ran)
	}
}

// An apply for a session iscsiadm does not list can't be shown to be tunable.
func TestApplySkipsSessionsIscsiadmDoesNotList(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{}
	tl := newTimeoutTools(t, e, 600)

	tl.applyReplacementTimeout([]iscsiSession{{Id: 1, Iqn: testIqn, Portal: testPortal}}, true)

	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
	if e.find("--op update") >= 0 {
		t.Errorf("node record updated: %v", e.ran)
	}
}

// The second session of the target shows up while the first one is waiting for
// its retry: nothing may be written from then on.
func TestRetryStopsWhenTargetGainsASession(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	clock := &fakeClock{onWait: func(n int) {
		if n == 2 {
			e.sessionOutput = sessionOutput(sessionLine(testIqn),
				"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)")
		}
	}}
	busy := &busyFirst{n: 1000}
	tl := newRetryToolsWith(t, e, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn, Portal: testPortal})

	if len(clock.recorded()) != 2 {
		t.Errorf("waits = %v, want the retry to stop at the second", clock.recorded())
	}
	if busy.count() != 1 {
		t.Errorf("writes = %d, want 1 (the second attempt must not write)", busy.count())
	}
	for _, sid := range []int{1, 2} {
		if got := fsys.recoveryTmo(sid); got != "120" {
			t.Errorf("session%d recovery_tmo = %q, want 120", sid, got)
		}
	}
}

// Retry that starts when the target already has two sessions, before any holder.
func TestRetryDoesNotWriteWhenTargetHasSeveralSessions(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(sessionLine(testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)")}
	clock := &fakeClock{}
	busy := &busyFirst{n: 0}
	tl := newRetryToolsWith(t, e, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn, Portal: testPortal})

	if busy.count() != 0 {
		t.Errorf("writes = %d, want 0", busy.count())
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
}

// iscsiadm failing once must not end a retry for a session that still exists.
func TestRetryKeepsGoingWhenIscsiadmDoesNotListTheSession(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{}
	clock := &fakeClock{onWait: func(n int) {
		if n == 2 {
			e.sessionOutput = sessionLine(testIqn)
		}
	}}
	busy := &busyFirst{n: 0}
	tl := newRetryToolsWith(t, e, clock, busy)

	tl.retryRecoveryTimeout(context.Background(), iscsiSession{Id: 1, Iqn: testIqn, Portal: testPortal})

	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("recovery_tmo = %q, want 600", got)
	}
	if len(clock.recorded()) != 2 {
		t.Errorf("waits = %v, want 2", clock.recorded())
	}
}

// The startup reconcile reads the session list while the node is still
// assembling its multipath maps: two sessions to an owned target, no holders.
func TestReconcileSkipsTargetWithSeveralSessionsBeforeHolders(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn).
		session(3, "iqn.2000-01.com.synology:single").disk(1, "sdb").disk(2, "sdc").disk(3, "sdd")
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)",
		sessionLineFor(3, "iqn.2000-01.com.synology:single"),
	)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{
		iscsiVolume(testIqn), iscsiVolume("iqn.2000-01.com.synology:single")}}}}

	newReconcileTools(t, e, &fakeClock{}).reconcileIscsiSessions(dsm.owned)

	for _, sid := range []int{1, 2} {
		if got := fsys.recoveryTmo(sid); got != "120" {
			t.Errorf("session%d of the two-session target: recovery_tmo = %q, want 120", sid, got)
		}
	}
	if got := fsys.recoveryTmo(3); got != "600" {
		t.Errorf("session3 of a single-session target: recovery_tmo = %q, want 600", got)
	}
	for _, line := range e.ran {
		if strings.Contains(line, "--op update") && strings.Contains(line, testIqn) {
			t.Errorf("node record of the two-session target updated: %q", line)
		}
	}
}

// A retry that the reconcile started for a recovering session must also give up
// once the target turns out to have more than one session.
func TestReconcileRetryHonoursTargetExclusion(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}}}}
	clock := &fakeClock{}
	tl := newReconcileTools(t, e, clock)
	busy := &busyFirst{n: 1000}
	tl.tuning.writeSysfs = busy.write
	waited := make(chan struct{}, 1)
	tl.tuning.after = func(d time.Duration) <-chan time.Time {
		// The second session of the target appears before the first retry.
		e.sessionOutput = sessionOutput(sessionLine(testIqn),
			"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)")
		select {
		case waited <- struct{}{}:
		default:
		}
		return clock.after(d)
	}

	tl.reconcileIscsiSessions(dsm.owned)
	<-waited
	deadline := time.Now().Add(5 * time.Second)
	for {
		tl.tuning.mu.Lock()
		n := len(tl.tuning.retrying)
		tl.tuning.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	if busy.count() != 1 {
		t.Errorf("writes = %d, want only the first, refused one", busy.count())
	}
	for _, sid := range []int{1, 2} {
		if got := fsys.recoveryTmo(sid); got != "120" {
			t.Errorf("session%d recovery_tmo = %q, want 120", sid, got)
		}
	}
}

// fakeDsm answers only the listing the reconcile asks for.
type fakeDsm struct {
	interfaces.IDsmService
	lists []fakeListing
	calls int
}

type fakeListing struct {
	volumes []*models.K8sVolumeRespSpec
	err     error
}

func (d *fakeDsm) ListVolumes() ([]*models.K8sVolumeRespSpec, error) {
	l := d.lists[min(d.calls, len(d.lists)-1)]
	d.calls++
	return l.volumes, l.err
}

func iscsiVolume(iqn string) *models.K8sVolumeRespSpec {
	v := &models.K8sVolumeRespSpec{Protocol: utils.ProtocolIscsi}
	v.Target.Iqn = iqn
	return v
}

func (d *fakeDsm) owned() (map[string]struct{}, error) { return ownedIscsiTargets(d) }

func TestOwnedIscsiTargets(t *testing.T) {
	boom := errors.New("DSM[1.2.3.4] failed to list targets")
	d := &fakeDsm{lists: []fakeListing{{
		volumes: []*models.K8sVolumeRespSpec{
			iscsiVolume(testIqn),
			{Protocol: utils.ProtocolNfs},
			{Protocol: utils.ProtocolIscsi}, // no target
			iscsiVolume("iqn.2000-01.com.synology:other.target"),
		},
		err: boom,
	}}}

	got, err := d.owned()
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the listing error passed on", err)
	}
	if len(got) != 2 {
		t.Fatalf("owned = %v, want the two iSCSI targets", got)
	}
	for _, iqn := range []string{testIqn, "iqn.2000-01.com.synology:other.target"} {
		if _, ok := got[iqn]; !ok {
			t.Errorf("%s missing from %v", iqn, got)
		}
	}
}

func newReconcileTools(t *testing.T, e *recordingExecutor, clock *fakeClock) *tools {
	t.Helper()
	tl := newTimeoutTools(t, e, 600)
	tl.tuning.after = clock.after
	return &tl
}

// The host's session list includes other iSCSI users, and Synology LUNs that an
// admin attached by hand; only targets DSM says the driver manages are ours.
func TestReconcileOnlyTouchesTargetsTheDriverOwns(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, otherVendorIqn).
		session(3, "eui.0123456789abcdef").session(4, sameVendorForeignIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		sessionLineFor(2, otherVendorIqn),
		sessionLineFor(3, "eui.0123456789abcdef"),
		sessionLineFor(4, sameVendorForeignIqn),
	)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}}}}
	clock := &fakeClock{}

	newReconcileTools(t, e, clock).reconcileIscsiSessions(dsm.owned)

	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("driver session recovery_tmo = %q, want 600", got)
	}
	for _, sid := range []int{2, 3, 4} {
		if got := fsys.recoveryTmo(sid); got != "120" {
			t.Errorf("session%d is not the driver's, recovery_tmo = %q, want 120", sid, got)
		}
	}
	for _, line := range e.ran {
		if strings.Contains(line, "--op update") && !strings.Contains(line, testIqn) {
			t.Errorf("node record of a target the driver doesn't own updated: %q", line)
		}
	}
	if e.find("--op update") < 0 {
		t.Errorf("node record of the driver's target not updated: %v", e.ran)
	}
	if len(clock.recorded()) != 0 {
		t.Errorf("a complete listing needs no retry, waits = %v", clock.recorded())
	}
}

// Not knowing is not a reason to guess from the vendor prefix.
func TestReconcileWaitsForDsmAndNeverFallsBackToThePrefix(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(4, sameVendorForeignIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		sessionLineFor(4, sameVendorForeignIqn),
	)}
	down := errors.New("DSM unreachable")
	dsm := &fakeDsm{lists: []fakeListing{
		{err: down},
		{err: down},
		{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}},
	}}
	clock := &fakeClock{onWait: func(n int) {
		// Still nothing written while DSM can't be asked.
		if got := fsys.recoveryTmo(1); got != "120" {
			t.Errorf("wait %d: recovery_tmo = %q before DSM answered, want 120", n, got)
		}
	}}

	newReconcileTools(t, e, clock).reconcileIscsiSessions(dsm.owned)

	if dsm.calls != 3 {
		t.Errorf("DSM listed %d times, want 3", dsm.calls)
	}
	want := []time.Duration{reconcileBackoffMin, 2 * reconcileBackoffMin}
	if got := clock.recorded(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("waits = %v, want %v", got, want)
	}
	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("driver session recovery_tmo = %q, want 600", got)
	}
	if got := fsys.recoveryTmo(4); got != "120" {
		t.Errorf("same-vendor session recovery_tmo = %q, want 120", got)
	}
}

// A partial listing proves the targets it does contain, so those are done at
// once; the rest wait for a complete one and nothing is written twice.
func TestReconcileUsesPartialListingForWhatItProves(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, "iqn.2000-01.com.synology:second").
		session(4, sameVendorForeignIqn)
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		sessionLineFor(2, "iqn.2000-01.com.synology:second"),
		sessionLineFor(4, sameVendorForeignIqn),
	)}
	dsm := &fakeDsm{lists: []fakeListing{
		{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}, err: errors.New("NFS listing failed")},
		{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn), iscsiVolume("iqn.2000-01.com.synology:second")}},
	}}
	clock := &fakeClock{onWait: func(int) {
		if got := fsys.recoveryTmo(1); got != "600" {
			t.Errorf("provably owned session not done before the retry: %q", got)
		}
		if got := fsys.recoveryTmo(2); got != "120" {
			t.Errorf("unproven session touched before the retry: %q", got)
		}
	}}

	newReconcileTools(t, e, clock).reconcileIscsiSessions(dsm.owned)

	for sid, want := range map[int]string{1: "600", 2: "600", 4: "120"} {
		if got := fsys.recoveryTmo(sid); got != want {
			t.Errorf("session%d recovery_tmo = %q, want %s", sid, got, want)
		}
	}
	updates := 0
	for _, line := range e.ran {
		if strings.Contains(line, "--targetname "+testIqn+" ") && strings.Contains(line, "--op update") {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("node record of the first target updated %d times, want 1", updates)
	}
}

func TestReconcileBackoffIsCappedAndStopsWhenCancelled(t *testing.T) {
	newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	dsm := &fakeDsm{lists: []fakeListing{{err: errors.New("DSM unreachable")}}}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &fakeClock{onWait: func(n int) {
		if n == 10 {
			cancel()
		}
	}}
	tl := newReconcileTools(t, e, clock)
	tl.tuning.ctx = ctx

	tl.reconcileIscsiSessions(dsm.owned)

	waits := clock.recorded()
	if len(waits) != 10 {
		t.Fatalf("waits = %v, want the reconcile to stop when cancelled", waits)
	}
	if waits[0] != reconcileBackoffMin || waits[len(waits)-1] != reconcileBackoffMax {
		t.Errorf("waits = %v, want %v growing to %v", waits, reconcileBackoffMin, reconcileBackoffMax)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] || waits[i] > reconcileBackoffMax {
			t.Errorf("waits = %v, want non-decreasing and capped at %v", waits, reconcileBackoffMax)
			break
		}
	}
}

// Without sessions there is nothing to prove, so DSM isn't asked.
func TestReconcileWithoutSessionsDoesNotAskDsm(t *testing.T) {
	newSessionSysfs(t)
	dsm := &fakeDsm{lists: []fakeListing{{}}}
	newReconcileTools(t, &recordingExecutor{}, &fakeClock{}).reconcileIscsiSessions(dsm.owned)
	if dsm.calls != 0 {
		t.Errorf("DSM listed %d times, want 0", dsm.calls)
	}
}

func TestReconcileDoesNothingWhenUnset(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn)
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}}}}
	tl := newTimeoutTools(t, e, 0)

	tl.reconcileIscsiSessions(dsm.owned)

	if len(e.ran) != 0 || dsm.calls != 0 {
		t.Errorf("nothing expected, ran %v, DSM listed %d times", e.ran, dsm.calls)
	}
	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120", got)
	}
}

// One bad session must not stop the rest from being updated.
func TestReconcileContinuesAfterFailure(t *testing.T) {
	fsys := newSessionSysfs(t).session(2, "iqn.2000-01.com.synology:other") // session1 has no sysfs entry
	e := &recordingExecutor{fail: "--portal 1.2.3.4:3260", sessionOutput: sessionOutput(
		"tcp: [1] 1.2.3.4:3260,1 "+testIqn+" (non-flash)",
		"tcp: [2] 5.6.7.8:3260,1 iqn.2000-01.com.synology:other (non-flash)",
	)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{
		iscsiVolume(testIqn), iscsiVolume("iqn.2000-01.com.synology:other"),
	}}}}

	newReconcileTools(t, e, &fakeClock{}).reconcileIscsiSessions(dsm.owned)

	if got := fsys.recoveryTmo(2); got != "600" {
		t.Errorf("session2 recovery_tmo = %q, want 600", got)
	}
}

func TestParseSessionsToleratesNamesWithoutColon(t *testing.T) {
	got := parseSessions("tcp: [3] 1.2.3.4:3260,1 eui.0123456789abcdef (non-flash)")
	if len(got) != 1 || got[0].Iqn != "eui.0123456789abcdef" || got[0].Id != 3 {
		t.Errorf("parseSessions = %+v", got)
	}
}

func mustFindSessions(t *testing.T, tl *tools, iqn, portal string) []iscsiSession {
	t.Helper()
	sessions, err := tl.findSessions(iqn, portal)
	if err != nil {
		t.Fatalf("findSessions: %v", err)
	}
	return sessions
}

// hookExecutor runs a function before every command, to change the world while
// a command is "running", the way a blocking iscsiadm call lets it change.
type hookExecutor struct {
	*recordingExecutor
	before func(args []string)
	// sessionExit makes `iscsiadm -m session` fail with this exit status.
	sessionExit int
}

func (e *hookExecutor) Command(cmd string, args ...string) utilexec.Cmd {
	if e.before != nil {
		e.before(args)
	}
	if e.sessionExit != 0 && len(args) >= 2 && args[0] == "-m" && args[1] == "session" {
		e.ran = append(e.ran, cmd+" "+strings.Join(args, " "))
		return &testingexec.FakeCmd{
			CombinedOutputScript: []testingexec.FakeAction{
				func() ([]byte, []byte, error) {
					return nil, nil, testingexec.FakeExitError{Status: e.sessionExit}
				},
			},
		}
	}
	return e.recordingExecutor.Command(cmd, args...)
}

func (e *hookExecutor) CommandContext(_ context.Context, cmd string, args ...string) utilexec.Cmd {
	return e.Command(cmd, args...)
}

func updatesReplacementTimeout(args []string) bool {
	return strings.Contains(strings.Join(args, " "), "--name node.session.timeo.replacement_timeout")
}

func TestStartupWithSeveralPortalsAndNoMapLeavesSessionsAlone(t *testing.T) {
	f := newSessionSysfs(t).session(1, testIqn).session(2, testIqn).disk(1, "sdb").disk(2, "sdc")
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)",
	)}
	dsm := &fakeDsm{lists: []fakeListing{{volumes: []*models.K8sVolumeRespSpec{iscsiVolume(testIqn)}}}}

	newReconcileTools(t, e, &fakeClock{}).reconcileIscsiSessions(dsm.owned)

	for _, sid := range []int{1, 2} {
		if got := f.recoveryTmo(sid); got != "120" {
			t.Errorf("session %d of a target with two portals was tuned before a multipath holder appeared: recovery_tmo=%q", sid, got)
		}
	}
	if e.find("replacement_timeout") >= 0 {
		t.Errorf("node records of a target with two portals were tuned: %v", e.ran)
	}
}

func TestRetryWithSeveralPortalsAndNoMapLeavesSessionAlone(t *testing.T) {
	f := newSessionSysfs(t).session(1, testIqn).session(2, testIqn).disk(1, "sdb").disk(2, "sdc")
	e := &recordingExecutor{sessionOutput: sessionOutput(
		sessionLineFor(1, testIqn),
		"tcp: [2] 5.6.7.8:3260,1 "+testIqn+" (non-flash)",
	)}
	tl := newTimeoutTools(t, e, 600)

	if !tl.retryRecoveryTimeoutOnce(iscsiSession{Id: 1, Iqn: testIqn, Portal: testPortal}) {
		t.Error("the retry should end for a target with two portals")
	}
	if got := f.recoveryTmo(1); got != "120" {
		t.Errorf("retry tuned a target with two portals before a multipath holder appeared: recovery_tmo=%q", got)
	}
}

// The node record update blocks; a multipath map can appear while it runs, and
// multipathd's recovery_tmo must not be overwritten.
func TestApplyChecksAgainBeforeWritingRecoveryTmo(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).disk(1, "sdb")
	e := &hookExecutor{recordingExecutor: &recordingExecutor{sessionOutput: sessionLine(testIqn)}}
	e.before = func(args []string) {
		if updatesReplacementTimeout(args) {
			fsys.holder("sdb", "dm-0", "mpath-test")
			fsys.write(fsys.sessionPath(1, "recovery_tmo"), "5\n")
		}
	}
	tl := NewTools(e, WithIscsiReplacementTimeout(600))

	tl.applyReplacementTimeout([]iscsiSession{{Id: 1, Iqn: testIqn, Portal: testPortal}}, true)

	if got := fsys.recoveryTmo(1); got != "5" {
		t.Errorf("recovery_tmo = %q, want multipath's 5 left alone after a holder appeared during the node update", got)
	}
}

// A later target must be judged on the sessions there are when its turn comes,
// not on the list from before the earlier targets' node updates.
func TestApplyListsSessionsAgainForEachTarget(t *testing.T) {
	other := "iqn.2000-01.com.synology:second"
	fsys := newSessionSysfs(t).session(1, testIqn).session(2, other).disk(1, "sdb").disk(2, "sdc")
	rec := &recordingExecutor{sessionOutput: sessionOutput(sessionLine(testIqn), sessionLineFor(2, other))}
	e := &hookExecutor{recordingExecutor: rec}
	e.before = func(args []string) {
		line := strings.Join(args, " ")
		if strings.Contains(line, "--targetname "+testIqn) && updatesReplacementTimeout(args) {
			fsys.session(3, other).disk(3, "sdd")
			rec.sessionOutput = sessionOutput(sessionLine(testIqn), sessionLineFor(2, other),
				"tcp: [3] 5.6.7.8:3260,1 "+other+" (non-flash)")
		}
	}
	tl := NewTools(e, WithIscsiReplacementTimeout(600))

	tl.applyReplacementTimeout([]iscsiSession{
		{Id: 1, Iqn: testIqn, Portal: testPortal},
		{Id: 2, Iqn: other, Portal: testPortal},
	}, true)

	if got := fsys.recoveryTmo(1); got != "600" {
		t.Errorf("first target: recovery_tmo = %q, want 600", got)
	}
	if got := fsys.recoveryTmo(2); got != "120" {
		t.Errorf("second target gained a session during the first target's update but was tuned: recovery_tmo = %q, want 120", got)
	}
	for _, line := range rec.ran {
		if strings.Contains(line, "--targetname "+other) && strings.Contains(line, "replacement_timeout") {
			t.Errorf("second target's node record was updated from a stale session list: %s", line)
		}
	}
}

func TestApplyLeavesSessionsAloneWhenTheyCantBeListed(t *testing.T) {
	fsys := newSessionSysfs(t).session(1, testIqn).disk(1, "sdb")
	e := &recordingExecutor{sessionOutput: sessionLine(testIqn), fail: "-m session"}
	tl := newTimeoutTools(t, e, 600)

	tl.applyReplacementTimeout([]iscsiSession{{Id: 1, Iqn: testIqn, Portal: testPortal}}, true)

	if got := fsys.recoveryTmo(1); got != "120" {
		t.Errorf("recovery_tmo = %q, want 120 when the sessions can't be listed", got)
	}
	if e.find("replacement_timeout") >= 0 {
		t.Errorf("node record was updated when the sessions can't be listed: %v", e.ran)
	}
}

// Listing the sessions fails closed for a new login: if the target's other
// sessions can't be seen, multipath can't be ruled out, so nothing is tuned.
// Login itself carries on.
func TestLoginNewPortalDoesNotTuneWhenSessionsCantBeListed(t *testing.T) {
	newSessionSysfs(t).session(1, testIqn).disk(1, "sdb").holder("sdb", "dm-0", "mpath-test")
	rec := &recordingExecutor{sessionOutput: "tcp: [1] 5.6.7.8:3260,1 " + testIqn + " (non-flash)"}
	e := &hookExecutor{recordingExecutor: rec}
	lists := 0
	e.before = func(args []string) {
		if strings.Join(args, " ") == "-m session" {
			lists++
			if lists == 2 {
				rec.fail = "-m session"
			}
		}
	}
	d := &initiatorDriver{tools: NewTools(e, WithIscsiReplacementTimeout(600))}

	if err := d.login(testIqn, testPortal, true); err != nil {
		t.Fatalf("login must not fail because the sessions can't be listed: %v", err)
	}
	if rec.find("replacement_timeout") >= 0 {
		t.Errorf("node record was tuned though the target's sessions can't be listed: %v", rec.ran)
	}
	if rec.find("--login") < 0 {
		t.Errorf("login did not happen: %v", rec.ran)
	}
}

// iscsiadm exits 21 when there are no sessions. That is a successful, empty
// answer, unlike a failure to ask, so the first login of a target is tuned.
func TestLoginNewTargetIsTunedWhenIscsiadmReportsNoSessions(t *testing.T) {
	e := &hookExecutor{recordingExecutor: &recordingExecutor{}, sessionExit: 21}
	d := &initiatorDriver{tools: NewTools(e, WithIscsiReplacementTimeout(600))}

	if err := d.login(testIqn, testPortal, true); err != nil {
		t.Fatal(err)
	}
	if e.find(updateReplacementTimeout) < 0 {
		t.Errorf("node record was not tuned for a target without sessions: %v", e.ran)
	}
}

func TestIscsiadmSessionDistinguishesNoSessionsFromFailure(t *testing.T) {
	none := NewTools(&hookExecutor{recordingExecutor: &recordingExecutor{}, sessionExit: 21})
	if sessions, err := none.iscsiadm_session(); err != nil || len(sessions) != 0 {
		t.Errorf("exit status 21 = (%v, %v), want no sessions and no error", sessions, err)
	}

	broken := NewTools(&hookExecutor{recordingExecutor: &recordingExecutor{}, sessionExit: 1})
	if _, err := broken.iscsiadm_session(); err == nil {
		t.Error("exit status 1 must be an error")
	}
}
