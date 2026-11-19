// Copyright 2021 Synology Inc.

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestNFSClientAddressFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unsupported source", []string{"--nfs-client-address-source=hostname"}, "invalid --nfs-client-address-source"},
		{"empty source", []string{"--nfs-client-address-source="}, "invalid --nfs-client-address-source"},
		{"missing key", []string{"--nfs-client-address-source=annotation"}, "requires --nfs-client-address-annotation-key"},
		{"invalid key", []string{"--nfs-client-address-source=annotation", "--nfs-client-address-annotation-key=storage.example.org/bad key"}, "invalid --nfs-client-address-annotation-key"},
		{"key with default source", []string{"--nfs-client-address-annotation-key=nfs-ip"}, "requires --nfs-client-address-source=annotation"},
		{"key with explicit InternalIP", []string{"--nfs-client-address-source=internalIP", "--nfs-client-address-annotation-key=nfs-ip"}, "requires --nfs-client-address-source=annotation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCommand()
			cmd.SetErr(io.Discard)
			// An invalid configuration must fail before trying to load DSM credentials.
			args := []string{"--endpoint=unix:///unused.sock", "--client-info=" + filepath.Join(t.TempDir(), "missing.yml")}
			cmd.SetArgs(append(args, tc.args...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}
