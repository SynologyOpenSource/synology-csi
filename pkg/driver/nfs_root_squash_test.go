// Copyright 2021 Synology Inc.

package driver

import "testing"

// nfsRootSquashValue is the pure function under test. It does not exist yet in
// the production code — this is the deliberate RED of TDD. The implementation
// step will introduce it in nfs_utils.go (or a sibling file) with the
// signature below: reads CSI_NFS_ROOT_SQUASH via os.LookupEnv each call (so
// t.Setenv is observed), returns "root" when the env var is absent, and the
// env value when it is present (even when empty).
//
// Until then this file does not compile, which is the desired red state.

func TestNfsRootSquashValue_EnvAbsentReturnsRoot(t *testing.T) {
	// Do NOT touch CSI_NFS_ROOT_SQUASH — assume a clean environment.
	// t.Setenv guarantees cleanup between tests, so prior tests in this file
	// leave the variable unset by the time this one runs. If the user's own
	// shell has CSI_NFS_ROOT_SQUASH set, this test correctly surfaces that
	// the function sees the user's value rather than the default.
	got := nfsRootSquashValue()
	if got != "root" {
		t.Errorf("nfsRootSquashValue() with env absent = %q, want %q", got, "root")
	}
}

func TestNfsRootSquashValue_EnvPresentUsesValue(t *testing.T) {
	t.Setenv("CSI_NFS_ROOT_SQUASH", "all")
	got := nfsRootSquashValue()
	if got != "all" {
		t.Errorf("nfsRootSquashValue() with env=\"all\" = %q, want %q", got, "all")
	}
}

func TestNfsRootSquashValue_EnvPresentButEmptyUsesEmpty(t *testing.T) {
	// Explicit empty string must be honored — "present" is what matters, not
	// "non-empty". Operators may want to disable squashing entirely by
	// setting CSI_NFS_ROOT_SQUASH="".
	t.Setenv("CSI_NFS_ROOT_SQUASH", "")
	got := nfsRootSquashValue()
	if got != "" {
		t.Errorf("nfsRootSquashValue() with env=\"\" = %q, want %q", got, "")
	}
}

func TestNfsRootSquashValue_EnvReadOnEachCall(t *testing.T) {
	// The ask: "lue via os.LookupEnv à chaque appel — pas à l'init, pour
	// être testable avec t.Setenv". Flipping the env between calls must
	// change the answer, which would not be the case if the value were
	// captured once at package init.
	t.Setenv("CSI_NFS_ROOT_SQUASH", "first")
	if got := nfsRootSquashValue(); got != "first" {
		t.Fatalf("first call = %q, want %q", got, "first")
	}

	t.Setenv("CSI_NFS_ROOT_SQUASH", "second")
	if got := nfsRootSquashValue(); got != "second" {
		t.Fatalf("second call after env change = %q, want %q", got, "second")
	}
}
