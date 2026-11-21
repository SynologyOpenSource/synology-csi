// Copyright 2021 Synology Inc.

package service

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

func TestCheckTargetNotInUse(t *testing.T) {
	session := func(iqn, ip string) webapi.ConncetedSession {
		return webapi.ConncetedSession{Iqn: iqn, Ip: ip}
	}

	t.Run("no sessions", func(t *testing.T) {
		if err := checkTargetNotInUse("vol-1", webapi.TargetInfo{Name: "k8s-csi-pvc-1"}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	tests := map[string][]webapi.ConncetedSession{
		"one session":                        {session("iqn.1993-08.org.debian:01:node1", "10.0.0.11")},
		"multipath, one portal still logged": {session("iqn.1993-08.org.debian:01:node1", "10.0.0.11")},
		"multipath, both portals": {
			session("iqn.1993-08.org.debian:01:node1", "10.0.0.11"),
			session("iqn.1993-08.org.debian:01:node1", "10.0.1.11"),
		},
	}
	for name, sessions := range tests {
		t.Run(name, func(t *testing.T) {
			err := checkTargetNotInUse("vol-1", webapi.TargetInfo{Name: "k8s-csi-pvc-1", ConnectedSessions: sessions})

			if got := status.Code(err); got != codes.FailedPrecondition {
				t.Fatalf("expected FailedPrecondition, got %v (%v)", got, err)
			}
			for _, s := range sessions {
				if !strings.Contains(err.Error(), s.Iqn) || !strings.Contains(err.Error(), s.Ip) {
					t.Errorf("error does not name initiator %s (%s): %v", s.Iqn, s.Ip, err)
				}
			}
		})
	}
}
