// Copyright 2026 Synology Inc.

package webapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTargetSetAuth_SendsChapCredentials verifies that TargetSetAuth sends the
// correct parameters to the DSM API: auth_type, chap_user, and chap_pass.
// The DSM API expects auth_type "1" for CHAP authentication.
func TestTargetSetAuth_SendsChapCredentials(t *testing.T) {
	var gotParams url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			// For GET requests, parse the query string
			gotParams = r.URL.Query()
		} else {
			gotParams = r.Form
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "secret",
		Https:    false,
		Sid:      "test-sid",
	}

	err := dsm.TargetSetAuth("123", "myuser", "mypassword")
	if err != nil {
		t.Fatalf("TargetSetAuth returned error: %v", err)
	}

	// Verify the API parameters
	if got := gotParams.Get("api"); got != "SYNO.Core.ISCSI.Target" {
		t.Errorf("api = %q, want %q", got, "SYNO.Core.ISCSI.Target")
	}
	if got := gotParams.Get("method"); got != "set" {
		t.Errorf("method = %q, want %q", got, "set")
	}
	if got := gotParams.Get("version"); got != "1" {
		t.Errorf("version = %q, want %q", got, "1")
	}
	if got := gotParams.Get("target_id"); got != `"123"` {
		t.Errorf("target_id = %q, want %q", got, `"123"`)
	}
	if got := gotParams.Get("auth_type"); got != "1" {
		t.Errorf("auth_type = %q, want %q", got, "1")
	}
	if got := gotParams.Get("chap_user"); got != "myuser" {
		t.Errorf("chap_user = %q, want %q", got, "myuser")
	}
	if got := gotParams.Get("chap_pass"); got != "mypassword" {
		t.Errorf("chap_pass = %q, want %q", got, "mypassword")
	}
}

// TestTargetSetAuth_SpecialCharactersInPassword verifies that passwords with
// special characters are properly URL-encoded and reach the DSM API intact.
func TestTargetSetAuth_SpecialCharactersInPassword(t *testing.T) {
	var gotPass string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			gotPass = r.FormValue("chap_pass")
		} else {
			gotPass = r.URL.Query().Get("chap_pass")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "secret",
		Https:    false,
		Sid:      "test-sid",
	}

	// Password with special characters that need URL encoding
	testPassword := "p@ss&word=test+123"
	err := dsm.TargetSetAuth("456", "user", testPassword)
	if err != nil {
		t.Fatalf("TargetSetAuth returned error: %v", err)
	}

	if gotPass != testPassword {
		t.Errorf("chap_pass = %q, want %q", gotPass, testPassword)
	}
}

// TestTargetSetAuth_ApiError verifies that DSM API errors are properly propagated.
func TestTargetSetAuth_ApiError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":false,"error":{"code":18990001}}`))
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "secret",
		Https:    false,
		Sid:      "test-sid",
	}

	err := dsm.TargetSetAuth("789", "user", "pass")
	if err == nil {
		t.Fatal("expected error from DSM API, got nil")
	}
	if !strings.Contains(err.Error(), "18990001") {
		t.Errorf("error should contain error code 18990001, got: %v", err)
	}
}
