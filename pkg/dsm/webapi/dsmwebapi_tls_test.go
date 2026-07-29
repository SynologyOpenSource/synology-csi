// Copyright 2024 Synology Inc.

package webapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDSMResponse is the minimal valid DSM login JSON response.
const fakeDSMResponse = `{"success":true,"data":{"sid":"test-sid"}}`

func fakeDSMHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fakeDSMResponse))
}

// serverHostPort parses host and port from a test server's listener address.
func serverHostPort(t *testing.T, server *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("cannot split host/port from %q: %v", server.Listener.Addr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("cannot parse port %q: %v", portStr, err)
	}
	return host, port
}

// generateCertWithSANs generates a unique ECDSA self-signed certificate with
// the given SANs, returning the PEM string and the tls.Certificate.
// Each call produces a distinct certificate, unlike httptest's shared default.
func generateCertWithSANs(t *testing.T, dnsNames []string, ips []net.IP) (certPEM string, tlsCert tls.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "synology-csi-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err = tls.X509KeyPair(certPEMBytes, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	return string(certPEMBytes), tlsCert
}

// generateSelfSignedCert generates a unique ECDSA self-signed certificate
// valid for 127.0.0.1.
func generateSelfSignedCert(t *testing.T) (certPEM string, tlsCert tls.Certificate) {
	t.Helper()
	return generateCertWithSANs(t, nil, []net.IP{net.IPv4(127, 0, 0, 1)})
}

// startTLSServer starts a TLS test server using the given certificate.
func startTLSServer(t *testing.T, handler http.Handler, tlsCert tls.Certificate) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// newTLSServerWithCert starts a TLS test server using a freshly generated
// self-signed certificate. Returns the server and its certificate PEM.
// Unlike httptest.NewTLSServer, every call produces a distinct certificate.
func newTLSServerWithCert(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	certPEM, tlsCert := generateSelfSignedCert(t)
	return startTLSServer(t, handler, tlsCert), certPEM
}

// TestHTTPS_DefaultRejectsUntrustedCert verifies that https: true without any
// TLS config now rejects a self-signed certificate (the vulnerability is fixed).
func TestHTTPS_DefaultRejectsUntrustedCert(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(fakeDSMHandler))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:       host,
		Port:     port,
		Username: "admin",
		Password: "secret",
		Https:    true,
		// InsecureSkipVerify: false (default)
		// TLSCACert: "" (not provided)
	}

	err := dsm.Login()
	if err == nil {
		t.Fatal("expected TLS certificate error, got nil — self-signed cert was accepted without InsecureSkipVerify or TLSCACert")
	}
	msg := err.Error()
	if !strings.Contains(msg, "certificate") && !strings.Contains(msg, "x509") && !strings.Contains(msg, "tls") {
		t.Fatalf("expected a TLS/certificate error, got: %v", err)
	}
}

// TestHTTPS_InsecureSkipVerify_ConnectsAndSendsCredentials verifies that
// insecureSkipVerify: true still allows connecting to an untrusted endpoint
// (explicit opt-in) and that credentials are delivered to the server.
func TestHTTPS_InsecureSkipVerify_ConnectsAndSendsCredentials(t *testing.T) {
	var gotAccount, gotPasswd string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount = r.URL.Query().Get("account")
		gotPasswd = r.URL.Query().Get("passwd")
		fakeDSMHandler(w, r)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:                 host,
		Port:               port,
		Username:           "test-user",
		Password:           "test-password",
		Https:              true,
		InsecureSkipVerify: true,
	}

	if err := dsm.Login(); err != nil {
		t.Fatalf("expected successful login with InsecureSkipVerify=true, got: %v", err)
	}
	if gotAccount != "test-user" {
		t.Errorf("server received account=%q, want %q", gotAccount, "test-user")
	}
	if gotPasswd != "test-password" {
		t.Errorf("server received passwd=%q, want %q", gotPasswd, "test-password")
	}
	if dsm.Sid != "test-sid" {
		t.Errorf("dsm.Sid=%q, want %q", dsm.Sid, "test-sid")
	}
}

// TestHTTPS_TLSCACert_ValidCert_ConnectsSuccessfully verifies that providing
// the server's own certificate as tlsCACert allows a successful login.
func TestHTTPS_TLSCACert_ValidCert_ConnectsSuccessfully(t *testing.T) {
	server, certPEM := newTLSServerWithCert(t, http.HandlerFunc(fakeDSMHandler))

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:        host,
		Port:      port,
		Username:  "admin",
		Password:  "secret",
		Https:     true,
		TLSCACert: certPEM,
	}

	if err := dsm.Login(); err != nil {
		t.Fatalf("expected successful login with correct TLSCACert, got: %v", err)
	}
	if dsm.Sid != "test-sid" {
		t.Errorf("dsm.Sid=%q, want %q", dsm.Sid, "test-sid")
	}
}

// TestHTTPS_TLSCACert_WrongCert_RejectsConnection verifies that providing a
// CA certificate that does not sign the server's certificate causes rejection.
func TestHTTPS_TLSCACert_WrongCert_RejectsConnection(t *testing.T) {
	// Each call to newTLSServerWithCert generates a distinct certificate,
	// so certPEM2 is guaranteed to be unrelated to server1's certificate.
	server1, _ := newTLSServerWithCert(t, http.HandlerFunc(fakeDSMHandler))
	_, certPEM2 := newTLSServerWithCert(t, http.HandlerFunc(fakeDSMHandler))

	host, port := serverHostPort(t, server1)
	dsm := &DSM{
		Ip:        host,
		Port:      port,
		Username:  "admin",
		Password:  "secret",
		Https:     true,
		TLSCACert: certPEM2, // intentionally wrong CA
	}

	err := dsm.Login()
	if err == nil {
		t.Fatal("expected TLS certificate error with wrong CA cert, got nil")
	}
}

// TestHTTPS_TLSServerName_MatchesDNSSAN_ConnectsSuccessfully covers the
// documented DSM self-signed scenario: the certificate only has a DNS SAN,
// the client connects by IP, and tlsServerName bridges the mismatch.
func TestHTTPS_TLSServerName_MatchesDNSSAN_ConnectsSuccessfully(t *testing.T) {
	certPEM, tlsCert := generateCertWithSANs(t, []string{"synology"}, nil)
	server := startTLSServer(t, http.HandlerFunc(fakeDSMHandler), tlsCert)

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:            host, // an IP address; the certificate has no IP SANs
		Port:          port,
		Username:      "admin",
		Password:      "secret",
		Https:         true,
		TLSCACert:     certPEM,
		TLSServerName: "synology",
	}

	if err := dsm.Login(); err != nil {
		t.Fatalf("expected successful login with TLSServerName matching the DNS SAN, got: %v", err)
	}
	if dsm.Sid != "test-sid" {
		t.Errorf("dsm.Sid=%q, want %q", dsm.Sid, "test-sid")
	}
}

// TestHTTPS_TLSServerName_DNSSANOnly_RejectedWithoutOverride verifies that the
// same setup fails when tlsServerName is not set: the connecting IP is not in
// the certificate's SANs, so verification must reject even with a trusted CA.
func TestHTTPS_TLSServerName_DNSSANOnly_RejectedWithoutOverride(t *testing.T) {
	certPEM, tlsCert := generateCertWithSANs(t, []string{"synology"}, nil)
	server := startTLSServer(t, http.HandlerFunc(fakeDSMHandler), tlsCert)

	host, port := serverHostPort(t, server)
	dsm := &DSM{
		Ip:        host,
		Port:      port,
		Username:  "admin",
		Password:  "secret",
		Https:     true,
		TLSCACert: certPEM,
		// TLSServerName not set: verification uses the IP, which has no SAN
	}

	if err := dsm.Login(); err == nil {
		t.Fatal("expected hostname verification error when connecting by IP to a DNS-SAN-only cert without TLSServerName, got nil")
	}
}
