package hostexec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestWrapEnv_TalosChroot_NoEnv covers issue #130: Talos chroots do not ship
// /usr/bin/env, so wrapEnv must not prepend it. Instead the binary is resolved
// via defaultSearchPath inside the chroot and the chroot-relative path is
// returned so wrapChroot can execute it directly.
func TestWrapEnv_TalosChroot_NoEnv(t *testing.T) {
	tmpDir := t.TempDir()

	// Lay down usr/local/sbin/iscsiadm inside the fake chroot.
	iscsiadmDir := filepath.Join(tmpDir, "usr/local/sbin")
	if err := os.MkdirAll(iscsiadmDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(iscsiadmDir, "iscsiadm"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile iscsiadm: %v", err)
	}
	// Deliberately do NOT create usr/bin/env.

	h := &hostexec{chrootDir: tmpDir}
	cmd, args := h.wrapEnv("iscsiadm", "-m", "node")

	if cmd != "usr/local/sbin/iscsiadm" {
		t.Errorf("wrapEnv() cmd = %q, want %q (chroot-relative path, no /usr/bin/env)", cmd, "usr/local/sbin/iscsiadm")
	}
	if !reflect.DeepEqual(args, []string{"-m", "node"}) {
		t.Errorf("wrapEnv() args = %v, want [-m node]", args)
	}
	// The returned path must contain a slash so that wrapChroot hands chroot a
	// path (rather than a bare binary name that would need PATH lookup).
	if !strings.Contains(cmd, "/") {
		t.Errorf("returned path must be chroot-relative with slashes, got %q", cmd)
	}
}

// TestWrapEnv_TalosChroot_WithEnv keeps the upstream behavior when
// /usr/bin/env IS present in the chroot.
func TestWrapEnv_TalosChroot_WithEnv(t *testing.T) {
	tmpDir := t.TempDir()

	envDir := filepath.Join(tmpDir, "usr/bin")
	if err := os.MkdirAll(envDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "env"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile env: %v", err)
	}

	iscsiadmDir := filepath.Join(tmpDir, "usr/local/sbin")
	if err := os.MkdirAll(iscsiadmDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(iscsiadmDir, "iscsiadm"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile iscsiadm: %v", err)
	}

	h := &hostexec{chrootDir: tmpDir}
	cmd, args := h.wrapEnv("iscsiadm", "-m", "node")

	if cmd != "/usr/bin/env" {
		t.Errorf("wrapEnv() cmd = %q, want /usr/bin/env (upstream behavior when env exists)", cmd)
	}
	if len(args) < 3 || args[0] != "-i" || !strings.HasPrefix(args[1], "PATH=") || args[2] != "iscsiadm" {
		t.Errorf("wrapEnv() args = %v, want [-i PATH=... iscsiadm -m node]", args)
	}
	if !reflect.DeepEqual(args, []string{"-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "iscsiadm", "-m", "node"}) {
		t.Errorf("wrapEnv() args = %v, want full env-style args", args)
	}
}

// TestWrap_TalosChroot_NoEnv_FullChain verifies the full wrap() pipeline
// produces a chroot invocation that does not go through /usr/bin/env.
func TestWrap_TalosChroot_NoEnv_FullChain(t *testing.T) {
	tmpDir := t.TempDir()

	iscsiadmDir := filepath.Join(tmpDir, "usr/local/sbin")
	if err := os.MkdirAll(iscsiadmDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(iscsiadmDir, "iscsiadm"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile iscsiadm: %v", err)
	}

	h := &hostexec{chrootDir: tmpDir}
	cmd, args := h.wrap("iscsiadm", "-m", "node")

	if cmd != "/usr/sbin/chroot" {
		t.Fatalf("wrap() cmd = %q, want /usr/sbin/chroot", cmd)
	}
	if len(args) < 3 {
		t.Fatalf("wrap() args too short: %v", args)
	}
	if args[0] != tmpDir {
		t.Errorf("wrap() args[0] = %q, want chrootDir %q", args[0], tmpDir)
	}
	if args[1] != "usr/local/sbin/iscsiadm" {
		t.Errorf("wrap() args[1] = %q, want usr/local/sbin/iscsiadm (no env in chain)", args[1])
	}
	if !reflect.DeepEqual(args[2:], []string{"-m", "node"}) {
		t.Errorf("wrap() trailing args = %v, want [-m node]", args[2:])
	}
}
