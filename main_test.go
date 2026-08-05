/*
 * Copyright 2021 Synology Inc.
 */

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/service"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
)

// fakeDsmService fails the login of every host in failures the given number of
// times before letting it through. The embedded interface leaves the methods
// loginDsms does not touch unimplemented - they panic if ever called.
type fakeDsmService struct {
	interfaces.IDsmService
	failures map[string]int
	added    map[string]bool
}

func newFakeDsmService(failures map[string]int) *fakeDsmService {
	return &fakeDsmService{failures: failures, added: make(map[string]bool)}
}

func (f *fakeDsmService) AddDsm(client common.ClientInfo) error {
	if f.added[client.Host] {
		return nil
	}
	if f.failures[client.Host] > 0 {
		f.failures[client.Host]--
		return fmt.Errorf("dial tcp %s:5000: connect: connection refused", client.Host)
	}
	f.added[client.Host] = true
	return nil
}

func (f *fakeDsmService) GetDsmsCount() int {
	return len(f.added)
}

func testBackoff() backoff.BackOff {
	return backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 5)
}

func TestLoginDsmsRetriesUntilTheDsmAnswers(t *testing.T) {
	svc := newFakeDsmService(map[string]int{"10.0.0.1": 3})
	clients := []common.ClientInfo{{Host: "10.0.0.1"}}

	if err := loginDsms(svc, clients, testBackoff()); err != nil {
		t.Fatalf("expected the DSM to be added after its 3 failures, got: %v", err)
	}
	if svc.GetDsmsCount() != 1 {
		t.Fatalf("expected 1 DSM added, got %d", svc.GetDsmsCount())
	}
}

func TestLoginDsmsFailsWhenNoDsmAnswers(t *testing.T) {
	svc := newFakeDsmService(map[string]int{"10.0.0.1": 99})
	clients := []common.ClientInfo{{Host: "10.0.0.1"}}

	if err := loginDsms(svc, clients, testBackoff()); err == nil {
		t.Fatal("expected an error when no DSM could be logged in, got nil")
	}
	if svc.GetDsmsCount() != 0 {
		t.Fatalf("expected 0 DSMs added, got %d", svc.GetDsmsCount())
	}
}

func TestLoginDsmsKeepsTheDsmsThatAnswered(t *testing.T) {
	svc := newFakeDsmService(map[string]int{"10.0.0.2": 99})
	clients := []common.ClientInfo{{Host: "10.0.0.1"}, {Host: "10.0.0.2"}}

	if err := loginDsms(svc, clients, testBackoff()); err == nil {
		t.Fatal("expected an error naming the DSM that never answered, got nil")
	}
	// The reachable DSM stays registered and serving, whatever its peer does.
	if svc.GetDsmsCount() != 1 {
		t.Fatalf("expected 1 DSM added, got %d", svc.GetDsmsCount())
	}
	// The reachable DSM must not be re-added on every retry round.
	if svc.failures["10.0.0.2"] != 99-6 {
		t.Fatalf("expected 6 login attempts against the unreachable DSM, got %d", 99-svc.failures["10.0.0.2"])
	}
}

func TestLoginDsmsWithNoClientsConfigured(t *testing.T) {
	svc := newFakeDsmService(nil)

	if err := loginDsms(svc, nil, testBackoff()); err != nil {
		t.Fatalf("expected no error with no clients configured, got: %v", err)
	}
}

// fakeDsm serves just enough of the DSM web API for AddDsm to succeed: the login
// and the three SYNO.Core.System calls FillSystemInfo makes. The first failCount
// requests are refused, standing in for a NAS that is still starting up.
func fakeDsm(t *testing.T, failCount int32) common.ClientInfo {
	t.Helper()

	var seen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&seen, 1) <= failCount {
			http.Error(w, "DSM is still starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"sid":"fake-sid","hostname":"fake-nas","firmware_ver":"DSM 7.2","support_nvmeof":"no"}}`)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return common.ClientInfo{Host: u.Hostname(), Port: port}
}

// End to end against a real DsmService: the login retries a NAS that is refusing
// connections, and registers it once it answers, while the read paths a gRPC handler
// would take are being exercised alongside it.
//
// The lock those reads depend on is covered by
// service.TestDsmServiceReadsAreSafeWhileDsmsAreAdded - a single AddDsm write is too
// narrow a target for -race to hit reliably.
func TestLoginDsmsRegistersALateDsmWhileTheDriverIsServing(t *testing.T) {
	dsmService := service.NewDsmService()
	clients := []common.ClientInfo{fakeDsm(t, 20)}
	bo := backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 50)

	done := make(chan error, 1)
	go func() {
		done <- loginDsms(dsmService, clients, bo)
	}()

	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("expected the DSM to be added once it started answering, got: %v", err)
			}
			if dsmService.GetDsmsCount() != 1 {
				t.Fatalf("expected 1 DSM added, got %d", dsmService.GetDsmsCount())
			}
			return
		default:
			// the read paths a gRPC handler would take
			dsmService.GetDsmsCount()
			dsmService.ListVolumes()
			dsmService.ListAllSnapshots()
		}
	}
}
