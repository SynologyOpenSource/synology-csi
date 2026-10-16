// Copyright 2026 Synology Inc.

package webapi

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	log "github.com/sirupsen/logrus"
)

// TestC1_ConcurrentLoginSidRace verifies that concurrent logins on the same
// DSM do not trigger a data race on DSM.Sid. The fake server returns a
// different sid per call, so concurrent writers + concurrent readers on the
// cookie path exercise both sides of the race.
//
// Must be run under `go test -race`. With the current code (no mutex on Sid),
// the race detector reports a data race on DSM.Sid.
func TestC1_ConcurrentLoginSidRace(t *testing.T) {
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"success":true,"data":{"sid":"sid-%d"}}`, n)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "secret",
		Https:    false,
	}

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N * 2)

	// Writers: N goroutines calling Login concurrently.
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = dsm.Login()
			}
		}()
	}

	// Readers: N goroutines hammering the cookie path by issuing API calls
	// that reach the Sid read at dsmwebapi.go:140-143.
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				// sendRequestWithoutConnectionCheck reads dsm.Sid to build
				// the cookie. Any endpoint that is not Login will do; we
				// use VolumeList which hits entry.cgi.
				_, _ = dsm.VolumeList()
			}
		}()
	}

	wg.Wait()
}

// TestC2_PasswordInPOSTBodyNotURL verifies that Login uses POST and
// delivers the password in the request body (form encoded), never in the URL.
//
// With the current code (GET + query string at dsmwebapi.go:211-217), this
// test fails because r.Method == "GET" and the passwd is in the URL.
func TestC2_PasswordInPOSTBodyNotURL(t *testing.T) {
	var gotMethod string
	var gotURL string
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotURL = r.URL.String()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"sid":"test-sid"}}`)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "super-secret-pw",
		Https:    false,
	}

	if err := dsm.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if gotMethod != "POST" {
		t.Errorf("Login HTTP method = %s, want POST (password must not travel in the URL)", gotMethod)
	}
	if strings.Contains(gotURL, "super-secret-pw") {
		t.Errorf("URL leaks password: %s", gotURL)
	}
	if !strings.Contains(gotBody, "super-secret-pw") {
		t.Errorf("password not found in request body: %q", gotBody)
	}
}

// TestC2_DebugLogDoesNotLeakPassword verifies that enabling WebapiDebug does
// not cause the DSM password to appear in log output.
//
// With the current code (log.Debugln(baseUrl.RawQuery) at dsmwebapi.go:130-132),
// this test fails because the raw query contains passwd=<password>.
func TestC2_DebugLogDoesNotLeakPassword(t *testing.T) {
	var logBuf bytes.Buffer
	savedOut := log.StandardLogger().Out
	savedLevel := log.StandardLogger().Level
	savedDebug := logger.WebapiDebug
	log.SetOutput(&logBuf)
	log.SetLevel(log.DebugLevel)
	logger.WebapiDebug = true
	defer func() {
		log.SetOutput(savedOut)
		log.SetLevel(savedLevel)
		logger.WebapiDebug = savedDebug
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"sid":"test-sid"}}`)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "leak-me-not-abc123",
		Https:    false,
	}

	if err := dsm.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if strings.Contains(logBuf.String(), "leak-me-not-abc123") {
		t.Errorf("password leaked into debug log:\n%s", logBuf.String())
	}
}

// TestC3_HTTPClientHasTimeout verifies that the http.Client built by both
// branches of newHTTPClient has a non-zero Timeout, so a hanging DSM cannot
// block a handler indefinitely.
//
// With the current code (dsmwebapi.go:59 and :87-90 build &http.Client{} with
// no Timeout), this test fails.
func TestC3_HTTPClientHasTimeout(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		dsm := &DSM{Ip: "127.0.0.1", Port: 5000, Https: false}
		c, err := dsm.newHTTPClient()
		if err != nil {
			t.Fatalf("newHTTPClient: %v", err)
		}
		if c.Timeout <= 0 {
			t.Errorf("plain http.Client.Timeout = %v, want > 0 (DSM call can hang indefinitely)", c.Timeout)
		}
	})
	t.Run("https", func(t *testing.T) {
		dsm := &DSM{Ip: "127.0.0.1", Port: 5001, Https: true, InsecureSkipVerify: true}
		c, err := dsm.newHTTPClient()
		if err != nil {
			t.Fatalf("newHTTPClient: %v", err)
		}
		if c.Timeout <= 0 {
			t.Errorf("https http.Client.Timeout = %v, want > 0", c.Timeout)
		}
	})
}
