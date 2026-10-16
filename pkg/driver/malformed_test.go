// Copyright 2026 Synology Inc.

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestC4_ParseSessionsMalformedIQN verifies that parseSessions does not
// panic when an iSCSI session entry has no colon in the IQN (e.g. an
// eui.* or naa.* identifier), and skips that entry cleanly.
//
// With the current code (initiator.go:88 does strings.Split(e[3], ":")[1]
// without checking the slice length), this test panics and crashes the test
// binary.
func TestC4_ParseSessionsMalformedIQN(t *testing.T) {
	// Real-shaped iSCSI session line where the "IQN" is a non-IQN identifier
	// with no colon. Under the current code, this line triggers
	// strings.Split(e[3], ":")[1] -> index out of range -> panic.
	raw := "tcp: [1] 10.10.16.11:3260,1 eui.0123456789ABCDEF\n" +
		"tcp: [2] 10.10.16.12:3260,1 iqn.2000-01.com.synology:target-1\n"

	var got []iscsiSession
	var panicked interface{}
	func() {
		defer func() { panicked = recover() }()
		got = parseSessions(raw)
	}()
	if panicked != nil {
		t.Fatalf("parseSessions panicked on IQN without colon: %v", panicked)
	}

	// The second line must still be parsed correctly.
	if len(got) < 1 {
		t.Fatalf("expected at least one parsed session, got %d", len(got))
	}
	var foundIQN bool
	for _, s := range got {
		if s.Iqn == "iqn.2000-01.com.synology:target-1" {
			foundIQN = true
			if s.Name != "target-1" {
				t.Errorf("well-formed IQN Name = %q, want %q", s.Name, "target-1")
			}
		}
	}
	if !foundIQN {
		t.Errorf("well-formed IQN was dropped from the output: %+v", got)
	}
}

// TestC4_ParseSessionsTotallyGarbled verifies that parseSessions tolerates
// completely garbled lines (missing fields, wrong shape) without panicking,
// AND exercises the real edge case the `len(e) < 4` guard was added for:
// an IQN field that is a single trailing colon (e.g. "target:") which would
// yield an empty Name if split unconditionally. The parser must skip the
// trailing-colon case gracefully rather than producing a session with Name="".
func TestC4_ParseSessionsTotallyGarbled(t *testing.T) {
	raw := "garbage-with-no-fields\n" +
		"tcp: [1] only-one-field\n" +
		"\n" +
		"::::\n" +
		// IQN-shaped field with a single trailing colon. Under the original
		// `strings.Split(e[3], ":")[1]` code this produced Name="" silently.
		// With the fix it must be parsed into a session whose Name is the
		// full field (no split), because there is no target suffix after ':'.
		"tcp: [3] 10.10.16.11:3260,1 target:\n"

	var panicked interface{}
	var got []iscsiSession
	func() {
		defer func() { panicked = recover() }()
		got = parseSessions(raw)
	}()
	if panicked != nil {
		t.Fatalf("parseSessions panicked on garbled input: %v", panicked)
	}

	// The trailing-colon IQN must be kept (not dropped), and its Name must
	// equal the whole field (no empty-string split).
	var found bool
	for _, s := range got {
		if s.Iqn == "target:" {
			found = true
			if s.Name != "target:" {
				t.Errorf("trailing-colon IQN Name = %q, want %q (full field, no empty split)", s.Name, "target:")
			}
		}
	}
	if !found {
		t.Errorf("trailing-colon IQN was dropped from the output: %+v", got)
	}
}

// TestC4_LsblkMalformedColumns verifies that the lsblk parser does not panic
// when a line has fewer than 7 columns and skips the malformed line instead
// of crashing.
//
// With the current code (multipath.go:167-176 guards on < 5 columns but
// accesses columns[5] and columns[6] unconditionally, and uses
// strings.Split(line, " ") rather than strings.Fields), this test panics.
func TestC4_LsblkMalformedColumns(t *testing.T) {
	// Build a fake `lsblk` that prints a well-formed root device, a short
	// malformed line, and another well-formed child device. The hierarchy is
	// sda (root) -> sdb (child), with a garbled line in the middle.
	tmp := t.TempDir()
	fakeLsblk := filepath.Join(tmp, "lsblk")
	// Use single-space separators so strings.Split keeps every column. A
	// root device's empty PKNAME becomes an empty column at index 2 (two
	// adjacent spaces in the output), which the production parser relies on
	// to filter roots. The garbled line has 4 tokens separated by spaces
	// and must be skipped by the `len(columns) < 7` guard.
	script := "#!/bin/sh\n" +
		"printf 'sda sda  4:0:0:0 disk iscsi 1G\\n'\n" +
		"printf 'short line only four\\n'\n" +
		"printf 'sdb sdb sda 5:0:0:0 disk iscsi 2G\\n'\n"
	if err := os.WriteFile(fakeLsblk, []byte(script), 0755); err != nil {
		t.Fatalf("write fake lsblk: %v", err)
	}

	// Prepend the fake binary to PATH so utilexec finds it first.
	prevPath := os.Getenv("PATH")
	os.Setenv("PATH", tmp+":"+prevPath)
	defer os.Setenv("PATH", prevPath)

	// Sanity-check: the fake is what exec.LookPath returns.
	if got, err := exec.LookPath("lsblk"); err != nil || got != fakeLsblk {
		t.Fatalf("fake lsblk not on PATH: got %q err %v", got, err)
	}

	var panicked interface{}
	var devices []Device
	var parseErr error
	func() {
		defer func() { panicked = recover() }()
		devices, parseErr = lsblk([]string{"sda", "sdb"}, false)
	}()
	if panicked != nil {
		t.Fatalf("lsblk parser panicked on a 4-column line: %v", panicked)
	}
	if parseErr != nil {
		t.Fatalf("lsblk returned error instead of skipping the malformed line and continuing: %v", parseErr)
	}

	// After the fix: malformed lines should be skipped, the 2 well-formed
	// lines should parse into one root device (sda) with one child (sdb).
	if len(devices) != 1 {
		t.Errorf("parsed root device count = %d, want 1", len(devices))
	}
	if len(devices) == 1 && len(devices[0].Children) != 1 {
		t.Errorf("root device children count = %d, want 1", len(devices[0].Children))
	}
	for _, d := range devices {
		if strings.TrimSpace(d.Transport) == "" || strings.TrimSpace(d.Size) == "" {
			t.Errorf("device %+v has empty Transport or Size — parser read past the end of a short line", d)
		}
	}
}
