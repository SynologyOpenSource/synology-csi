package main

import (
	"math"
	"testing"

	"github.com/spf13/cobra"
)

func TestValidateIscsiReplacementTimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		wantErr bool
	}{
		{0, false},
		{600, false},
		{-1, true},
		{math.MaxInt32, false},
		{math.MaxInt32 + 1, true},
		{math.MaxInt64, true},
		{math.MinInt64, true},
	} {
		err := validateIscsiReplacementTimeout(tc.seconds)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateIscsiReplacementTimeout(%d) error = %v, wantErr %v", tc.seconds, err, tc.wantErr)
		}
	}
}

// The check has to run on the real command path, not just as a helper.
func TestNegativeIscsiReplacementTimeoutRejectedAtStartup(t *testing.T) {
	old := iscsiReplacementTimeout
	defer func() { iscsiReplacementTimeout = old }()
	iscsiReplacementTimeout = -5

	if err := rootCmd.RunE(rootCmd, nil); err == nil {
		t.Fatal("negative --iscsi-replacement-timeout must fail startup")
	}
}

// The value comes from the command line as text; flag parsing is where an
// overflowing one has to be rejected or accepted.
func TestIscsiReplacementTimeoutFlagBounds(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		wantErr bool
	}{
		{"600", false},
		{"2147483647", false},
		{"2147483648", true},
		{"99999999999999999999", true}, // out of int64: rejected by flag parsing
		{"1e20", true},
		{"-1", true},
	} {
		old := iscsiReplacementTimeout
		cmd := &cobra.Command{Use: "test"}
		addFlags(cmd)
		err := cmd.PersistentFlags().Parse([]string{"--iscsi-replacement-timeout=" + tc.arg})
		if err == nil {
			err = validateIscsiReplacementTimeout(iscsiReplacementTimeout)
		}
		iscsiReplacementTimeout = old
		if (err != nil) != tc.wantErr {
			t.Errorf("--iscsi-replacement-timeout=%s: error = %v, wantErr %v", tc.arg, err, tc.wantErr)
		}
	}
}

func TestOverflowingIscsiReplacementTimeoutRejectedAtStartup(t *testing.T) {
	old := iscsiReplacementTimeout
	defer func() { iscsiReplacementTimeout = old }()
	iscsiReplacementTimeout = math.MaxInt32 + 1

	if err := rootCmd.RunE(rootCmd, nil); err == nil {
		t.Fatal("--iscsi-replacement-timeout above the kernel's int must fail startup")
	}
}
