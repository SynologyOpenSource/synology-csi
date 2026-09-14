// Copyright 2021 Synology Inc.

package service

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func TestCheckRestoreSize(t *testing.T) {
	const source = 1 << 30 // 1 GiB

	cases := []struct {
		name      string
		requested int64
		wantErr   bool
	}{
		{
			// What the driver used to reject. Kubernetes expects it to work and
			// the external storage suite has a test for it.
			name:      "larger than the source is allowed",
			requested: 2 << 30,
		},
		{
			name:      "equal to the source is allowed",
			requested: source,
		},
		{
			// A copy starts out the size of its source; there would be nowhere
			// to put the data that no longer fits.
			name:      "smaller than the source is refused",
			requested: 512 << 20,
			wantErr:   true,
		},
		{
			name:      "no size requested takes the source's",
			requested: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRestoreSize(tc.requested, source, "lun")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %d < %d to be refused", tc.requested, source)
				}
				if code := status.Code(err); code != codes.OutOfRange {
					t.Errorf("expected OutOfRange, got %v", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected %d to be accepted against a source of %d, got %v",
					tc.requested, source, err)
			}
		})
	}
}

func TestNeedsExpandTo(t *testing.T) {
	const cloned = 1 << 30

	cases := []struct {
		name      string
		requested int64
		want      int64
	}{
		{
			name:      "grow to the requested size",
			requested: 2 << 30,
			want:      2 << 30,
		},
		{
			name:      "already the right size, nothing to do",
			requested: cloned,
			want:      0,
		},
		{
			// checkRestoreSize has already refused this; expanding must not try
			// to shrink it as a side effect.
			name:      "never shrinks",
			requested: 512 << 20,
			want:      0,
		},
		{
			name:      "no size requested, nothing to do",
			requested: 0,
			want:      0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsExpandTo(tc.requested, cloned); got != tc.want {
				t.Errorf("needsExpandTo(%d, %d) = %d, want %d",
					tc.requested, cloned, got, tc.want)
			}
		})
	}
}

// The size the caller asks for is in bytes and a share's quota is in whole MB,
// so the rounding has to go up: rounding down would hand back a PVC that is
// smaller than requested and the restore would silently under-deliver.
func TestShareQuotaRoundingNeverUnderDelivers(t *testing.T) {
	const oneMB = 1 << 20

	cases := []int64{1, oneMB - 1, oneMB, oneMB + 1, 3*oneMB + 7}
	for _, size := range cases {
		quotaMB := utils.BytesToMBCeil(size)
		if quotaMB*oneMB < size {
			t.Errorf("a %d byte request became a %d MB quota, which is smaller than asked for",
				size, quotaMB)
		}
	}
}
