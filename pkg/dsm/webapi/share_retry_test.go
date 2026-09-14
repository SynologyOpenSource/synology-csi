// Copyright 2021 Synology Inc.

package webapi

import (
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// retryShareOp mirrors the retry decision made in shareRequest so it can be
// exercised without a DSM to talk to.
func retryShareOp(op func() error, b backoff.BackOff) error {
	return backoff.Retry(func() error {
		err := op()
		if err == nil {
			return nil
		}
		if errors.Is(err, utils.ShareSystemBusyError("")) {
			return err
		}
		return backoff.Permanent(err)
	}, b)
}

func testBackOff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Millisecond
	b.MaxInterval = 2 * time.Millisecond
	b.MaxElapsedTime = 200 * time.Millisecond
	return b
}

func TestShareBusyIsRetriedUntilItClears(t *testing.T) {
	calls := 0
	err := retryShareOp(func() error {
		calls++
		if calls < 3 {
			return utils.ShareSystemBusyError("")
		}
		return nil
	}, testBackOff())

	if err != nil {
		t.Fatalf("expected the operation to succeed once DSM stopped being busy, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestOtherErrorsAreNotRetried(t *testing.T) {
	calls := 0
	err := retryShareOp(func() error {
		calls++
		return utils.NoSuchShareError("")
	}, testBackOff())

	if calls != 1 {
		t.Errorf("expected a single attempt for a non-transient error, got %d", calls)
	}
	// The original error must survive so callers can still match on its type.
	if !errors.Is(err, utils.NoSuchShareError("")) {
		t.Errorf("expected the original error type to be preserved, got %v", err)
	}
}

func TestAlreadyExistStaysMatchableAfterRetryWrapping(t *testing.T) {
	// Several callers treat "already exists" as success; the retry wrapper must
	// not turn it into something errors.Is no longer recognises.
	err := retryShareOp(func() error {
		return utils.AlreadyExistError("")
	}, testBackOff())

	if !errors.Is(err, utils.AlreadyExistError("")) {
		t.Errorf("expected AlreadyExistError to be matchable, got %v", err)
	}
}

func TestBusyGivesUpAfterTheDeadline(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryShareOp(func() error {
		calls++
		return utils.ShareSystemBusyError("")
	}, testBackOff())

	if err == nil {
		t.Fatal("expected an error once the retry deadline passed")
	}
	if !errors.Is(err, utils.ShareSystemBusyError("")) {
		t.Errorf("expected the busy error to be reported to the caller, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected more than one attempt before giving up, got %d", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("retrying took %v, which would outlast the caller's timeout", elapsed)
	}
}

// retryNfsPrivilegeOp mirrors the retry decision in nfsPrivilegeRequest: that
// API namespace has no typed errors, so the DSM error code is matched directly.
func retryNfsPrivilegeOp(op func() (int, error), b backoff.BackOff) error {
	return backoff.Retry(func() error {
		code, err := op()
		if err == nil {
			return nil
		}
		if code == nfsPrivilegeShareLoadFailErrCode {
			return err
		}
		return backoff.Permanent(err)
	}, b)
}

func TestNfsPrivilegeShareLoadFailIsRetried(t *testing.T) {
	calls := 0
	err := retryNfsPrivilegeOp(func() (int, error) {
		calls++
		if calls < 3 {
			return nfsPrivilegeShareLoadFailErrCode, errors.New("DSM Api error. Error code:2370")
		}
		return 0, nil
	}, testBackOff())

	if err != nil {
		t.Fatalf("expected success once DSM could look the share up again, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

// The retry budgets are bounded by what the callers allow and by what retrying
// can actually achieve, so record both here: nothing else in the package
// enforces them.
func TestRetryBudgetsFitTheirCallers(t *testing.T) {
	// A controller RPC is cut off by whichever CSI sidecar issued it, so a retry
	// that outlasts that deadline is wasted work: the sidecar has already given
	// up and will call again from the start. Our manifests pass --timeout to the
	// provisioner, resizer and snapshotter; keep this in step with them, and note
	// that the sidecars default to as little as 10s when it is left unset.
	const sidecarRPCTimeout = 120 * time.Second
	for _, budget := range []struct {
		name  string
		value time.Duration
	}{
		{"share", shareBusyRetryTimeout},
		{"NFS privilege", nfsPrivilegeRetryTimeout},
	} {
		if budget.value >= sidecarRPCTimeout {
			t.Errorf("a %s call retrying for %v outlasts the sidecar's %v RPC timeout",
				budget.name, budget.value, sidecarRPCTimeout)
		}
	}

	// The share budget is the larger one because 3328 really is a busy subsystem
	// that clears on its own. 2370 mostly means the share is gone, which no
	// amount of waiting fixes, so its budget only has to cover a share being
	// deleted while we ask about it.
	if nfsPrivilegeRetryTimeout >= shareBusyRetryTimeout {
		t.Errorf("expected the privilege budget to stay below the share budget of %v, got %v; "+
			"waiting longer for a share that no longer exists only delays the error",
			shareBusyRetryTimeout, nfsPrivilegeRetryTimeout)
	}
}

func TestOtherNfsPrivilegeErrorsAreNotRetried(t *testing.T) {
	calls := 0
	_ = retryNfsPrivilegeOp(func() (int, error) {
		calls++
		return 402, errors.New("DSM Api error. Error code:402")
	}, testBackOff())

	if calls != 1 {
		t.Errorf("expected a single attempt for a non-transient code, got %d", calls)
	}
}
