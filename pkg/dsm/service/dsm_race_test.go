// Copyright 2026 Synology Inc.

package service

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

// fakeDSMForService stands up an httptest server that satisfies the sequence
// of calls AddDsm performs (Login + 3x FillSystemInfo) and returns 200/JSON
// for anything else.
func fakeDSMForService(t *testing.T) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Small delay to increase the window where readers are making HTTP
		// calls while writers are adding DSMs to the map.
		// time.Sleep(10 * time.Microsecond)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/webapi/auth.cgi":
			fmt.Fprint(w, `{"success":true,"data":{"sid":"test-sid"}}`)
		case "/webapi/entry.cgi":
			// FillSystemInfo hits this endpoint three times with type=define,
			// type=network, and no type. Answer every shape permissively.
			fmt.Fprint(w, `{"success":true,"data":{"model":"fake","firmware_ver":"7.0","serial":"X","hostname":"fake","support_nvmeof":"no","ip":"127.0.0.1","mask":"255.255.255.0","speed":1000,"status":"ready","type":"lan","use_dhcp":true}}`)
		default:
			fmt.Fprint(w, `{"success":true,"data":{}}`)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestC1_DeterministicMapRace is the deterministic race detector for C1.
// It directly accesses service.dsms without any HTTP calls, so the race
// window is not masked by network I/O. Readers iterate the map in a tight
// loop (no lock in the buggy version), writers insert entries in a tight
// loop. Under `go test -race`, this MUST detect the race when the lock is
// missing, and MUST pass when the lock is present.
//
// This test is INTERNAL (package service, not service_test) so it can
// access the unexported dsms field directly.
func TestC1_DeterministicMapRace(t *testing.T) {
	svc := NewDsmService()
	const N = 32
	iterations := 10000

	// Writers: directly insert into service.dsms without going through
	// AddDsm (which does HTTP calls). This exercises the raw map write.
	var writerWg sync.WaitGroup
	writerWg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer writerWg.Done()
			for j := 0; j < iterations; j++ {
				key := fmt.Sprintf("writer-%d-iter-%d", i, j)
				// Simulate the write pattern from AddDsm: take the lock,
				// write, release. In the buggy version (no lock in readers),
				// this write races with the unlocked iteration below.
				svc.dsmsMu.Lock()
				svc.dsms[key] = &webapi.DSM{Ip: key}
				svc.dsmsMu.Unlock()
			}
		}()
	}

	// Readers: directly iterate service.dsms in a tight loop. In the buggy
	// version (no snapshotDsms), this iterates WITHOUT holding dsmsMu.RLock,
	// which races with the writer's locked write above. The race detector
	// flags ANY concurrent map read/write, even if one side holds a lock —
	// because the unlocked side is not synchronized.
	var readerWg sync.WaitGroup
	readerWg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer readerWg.Done()
			for j := 0; j < iterations; j++ {
				// In the FIXED version, this should call snapshotDsms()
				// (which takes RLock). In the BUGGY version, it iterates
				// service.dsms directly without any lock.
				_ = svc.snapshotDsms()
			}
		}()
	}

	writerWg.Wait()
	readerWg.Wait()
}

// TestC1_ConcurrentDsmServiceMapRace verifies that concurrent AddDsm/GetDsm
// on the same DsmService do not race on the dsms map, exercising the REAL
// list paths (listSMBorNFSVolumes, listNVMeVolumes, findSMBorNFSVolume,
// getSMBorNFSSnapshot). This test validates that the conversions to
// snapshotDsms() don't break the real code paths, but it does NOT reliably
// detect the race (the HTTP calls in the list methods mask the race window).
// For deterministic race detection, see TestC1_DeterministicMapRace above.
func TestC1_ConcurrentDsmServiceMapRace(t *testing.T) {
	srv := fakeDSMForService(t)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	basePort, _ := strconv.Atoi(portStr)

	svc := NewDsmService()
	const N = 16

	// Pre-populate the map with a few DSMs so readers have something to
	// iterate. This increases the chance of overlapping with the writers'
	// map assignments.
	for i := 0; i < 4; i++ {
		ci := common.ClientInfo{
			Host:     fmt.Sprintf("pre-host-%d", i),
			Port:     basePort,
			Https:    false,
			Username: "admin",
			Password: "secret",
		}
		_ = svc.AddDsm(ci)
	}

	// Writers complete when every AddDsm has returned. Readers must run
	// until that happens, so the race window is fully covered.
	writersDone := make(chan struct{})

	var wg sync.WaitGroup
	var readsCompleted int64

	// Concurrent writers: AddDsm on distinct keys so the map write at
	// service/dsm.go:59 is hit every iteration (the "already present"
	// short-circuit would otherwise mask the race).
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				ci := common.ClientInfo{
					Host:     fmt.Sprintf("host-%d-%d", i, j),
					Port:     basePort,
					Https:    false,
					Username: "admin",
					Password: "secret",
				}
				_ = svc.AddDsm(ci)
			}
		}()
	}

	// Close writersDone when all AddDsm have returned.
	go func() {
		wg.Wait()
		close(writersDone)
	}()

	// Concurrent readers: hammer the 4 list paths that iterate service.dsms
	// (findSMBorNFSVolume, listSMBorNFSVolumes, listNVMeVolumes,
	// getSMBorNFSSnapshot). Each reader loops until writersDone closes, so
	// every reader is guaranteed to be running while at least one AddDsm
	// is mid-flight. Under -race, a missing lock on service.dsms is
	// reported on the very first overlap.
	const readers = N * 2
	var readerWg sync.WaitGroup
	readerWg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-writersDone:
					return
				default:
				}
				// Exercise each of the 4 previously-unlocked sites.
				_, _ = svc.listSMBorNFSVolumes("")
				_, _ = svc.listNVMeVolumes("")
				_, _ = svc.findSMBorNFSVolume(func(_ webapi.ShareInfo) bool { return false })
				_ = svc.getSMBorNFSSnapshot("nonexistent-uuid")
				atomic.AddInt64(&readsCompleted, 1)
				// Force a context switch every 100 iterations to increase
				// the chance of overlapping with the writer's map assignment.
				if atomic.LoadInt64(&readsCompleted)%100 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}

	readerWg.Wait()
	t.Logf("completed %d reader iterations across %d goroutines", atomic.LoadInt64(&readsCompleted), readers)
}
