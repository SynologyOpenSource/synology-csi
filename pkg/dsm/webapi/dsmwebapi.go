/*
 * Copyright 2021 Synology Inc.
 */

package webapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	log "github.com/sirupsen/logrus"
)

// dsmHTTPTimeout is the per-request timeout applied to every DSM HTTP call.
// It is a var (not a const) so tests can override it. The default of 30s
// prevents a hanging DSM from blocking a gRPC handler indefinitely.
var dsmHTTPTimeout = 30 * time.Second

type DSM struct {
	Ip                 string
	Port               int
	Username           string
	Password           string
	Sid                string
	Https              bool
	Controller         string
	TLSCACert          string
	TLSServerName      string
	InsecureSkipVerify bool
	SystemInfo

	// sidMu guards Sid. Login writes it, every handler reads it to build the
	// session cookie. Without a mutex concurrent logins and concurrent
	// request/send races were observed under -race.
	sidMu sync.RWMutex
}

type SystemInfo struct {
	Hostname      string
	SupportNvmeof bool
	FirmwareVer   string
}

type errData struct {
	Code int `json:"code"`
}

type dsmApiResp struct {
	Success bool    `json:"success"`
	Err     errData `json:"error"`
}

type Response struct {
	StatusCode int
	ErrorCode  int
	Success    bool
	Data       interface{}
}

func (dsm *DSM) newHTTPClient() (*http.Client, error) {
	if !dsm.Https {
		return &http.Client{Timeout: dsmHTTPTimeout}, nil
	}

	tlsCfg := &tls.Config{}

	if dsm.TLSServerName != "" {
		tlsCfg.ServerName = dsm.TLSServerName
	}

	if dsm.InsecureSkipVerify {
		if dsm.TLSCACert != "" {
			log.Warnf("tlsCACert is configured but will be ignored for DSM %s because insecureSkipVerify is true.", dsm.Ip)
		}
		log.Warnf("TLS certificate verification is disabled for DSM %s. "+
			"This is insecure; provide tlsCACert instead.", dsm.Ip)
		tlsCfg.InsecureSkipVerify = true
	} else if dsm.TLSCACert != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			log.Warnf("Failed to load system CA pool for DSM %s, falling back to empty pool: %v", dsm.Ip, err)
			pool = x509.NewCertPool()
		}
		if ok := pool.AppendCertsFromPEM([]byte(dsm.TLSCACert)); !ok {
			return nil, fmt.Errorf("failed to parse TLS CA certificate for DSM %s", dsm.Ip)
		}
		tlsCfg.RootCAs = pool
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg

	return &http.Client{Transport: tr, Timeout: dsmHTTPTimeout}, nil
}

func (dsm *DSM) sendRequest(data string, apiTemplate interface{}, params url.Values, cgiPath string) (Response, error) {
	resp, err := dsm.sendRequestWithoutConnectionCheck(data, apiTemplate, params, cgiPath)
	if err != nil && (resp.ErrorCode == 105 || resp.ErrorCode == 106 || resp.ErrorCode == 119) { // 105: WEBAPI_ERR_NO_PERMISSION, 106: session timeout, 119: WEBAPI_ERR_SID_NOT_FOUND
		// Re-login
		if err := dsm.Login(); err != nil {
			return Response{}, fmt.Errorf("Failed to re-login to DSM: [%s]. err: %v", dsm.Ip, err)
		}
		log.Info("Re-login succeeded.")
		return dsm.sendRequestWithoutConnectionCheck(data, apiTemplate, params, cgiPath)
	}

	return resp, err
}

func (dsm *DSM) sendRequestWithoutConnectionCheck(data string, apiTemplate interface{}, params url.Values, cgiPath string) (Response, error) {
	client, err := dsm.newHTTPClient()
	if err != nil {
		return Response{}, err
	}

	var req *http.Request
	var cgiUrl string

	// Ex: http://10.12.12.14:5000/webapi/auth.cgi
	if dsm.Https {
		cgiUrl = fmt.Sprintf("https://%s:%d/%s", dsm.Ip, dsm.Port, cgiPath)
	} else {
		cgiUrl = fmt.Sprintf("http://%s:%d/%s", dsm.Ip, dsm.Port, cgiPath)
	}

	baseUrl, err := url.Parse(cgiUrl)
	if err != nil {
		return Response{}, err
	}

	baseUrl.RawQuery = params.Encode()

	if logger.WebapiDebug {
		log.Debugln(redactSecrets(baseUrl.RawQuery))
	}

	if data != "" {
		req, err = http.NewRequest("POST", baseUrl.String(), nil)
	} else {
		req, err = http.NewRequest("GET", baseUrl.String(), nil)
	}

	dsm.sidMu.RLock()
	sid := dsm.Sid
	dsm.sidMu.RUnlock()
	if sid != "" {
		cookie := http.Cookie{Name: "id", Value: sid}
		req.AddCookie(&cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	// For debug print text body
	var bodyText []byte
	if logger.WebapiDebug {
		bodyText, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return Response{}, err
		}
		s := string(bodyText)
		log.Debugln(s)
	}

	if resp.StatusCode != 200 && resp.StatusCode != 302 {
		return Response{}, fmt.Errorf("Bad response status code: %d", resp.StatusCode)
	}

	// Strip data json data from response
	type envelop struct {
		dsmApiResp
		Data json.RawMessage `json:"data"`
	}

	e := envelop{}
	var outResp Response

	if logger.WebapiDebug {
		if err := json.Unmarshal(bodyText, &e); err != nil {
			return Response{}, err
		}
	} else {
		decoder := json.NewDecoder(resp.Body)

		if err := decoder.Decode(&e); err != nil {
			return Response{}, err
		}
	}
	outResp.Success = e.Success
	outResp.ErrorCode = e.Err.Code
	outResp.StatusCode = resp.StatusCode

	if !e.Success {
		return outResp, fmt.Errorf("DSM Api error. Error code:%d", outResp.ErrorCode)
	}

	if e.Data != nil {
		if err := json.Unmarshal(e.Data, apiTemplate); err != nil {
			return Response{}, err
		}
	}

	outResp.Data = apiTemplate

	if err != nil {
		return Response{}, err
	}

	return outResp, nil
}

// redactSecrets replaces passwd=...& and passwd=...$ occurrences in a query
// string / URL body with [REDACTED]. Defense in depth: after C2 the password
// no longer appears in the URL at all, but any future caller that adds a
// sensitive parameter must not accidentally log it either.
func redactSecrets(s string) string {
	// Match passwd= up to the next & or end-of-string.
	re := regexp.MustCompile(`passwd=[^&\s]*`)
	return re.ReplaceAllString(s, "passwd=[REDACTED]")
}

// setSid writes the session id under the write lock.
func (dsm *DSM) setSid(s string) {
	dsm.sidMu.Lock()
	dsm.Sid = s
	dsm.sidMu.Unlock()
}

// Login by given user name and password.
//
// Sends the credentials as an application/x-www-form-urlencoded POST body —
// never in the URL — so the password does not leak through access logs,
// browser history, or the debug output of sendRequestWithoutConnectionCheck.
// Concurrent logins are serialized via sidMu (write-held for the duration).
// Note: sidMu is held across the full HTTP round-trip (up to dsmHTTPTimeout,
// typically 30s) — this is intentional, as releasing it mid-flight would let
// a concurrent Login or cookie-read observe a half-updated session state.
// Any DSM call that goes through sendRequestWithoutConnectionCheck will
// block on sidMu.RLock() until Login returns, which is the correct
// serialization for "no API call may use the session before the handshake
// has completed".
func (dsm *DSM) Login() error {
	dsm.sidMu.Lock()
	defer dsm.sidMu.Unlock()

	form := url.Values{}
	form.Add("api", "SYNO.API.Auth")
	form.Add("method", "login")
	form.Add("version", "3")
	form.Add("account", dsm.Username)
	form.Add("passwd", dsm.Password)
	form.Add("format", "sid")
	body := strings.NewReader(form.Encode())

	var cgiUrl string
	if dsm.Https {
		cgiUrl = fmt.Sprintf("https://%s:%d/webapi/auth.cgi", dsm.Ip, dsm.Port)
	} else {
		cgiUrl = fmt.Sprintf("http://%s:%d/webapi/auth.cgi", dsm.Ip, dsm.Port)
	}

	client, err := dsm.newHTTPClient()
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", cgiUrl, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("login to DSM [%s] failed: %v", dsm.Ip, err)
	}
	defer resp.Body.Close()

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 302 {
		return fmt.Errorf("Bad response status code: %d", resp.StatusCode)
	}

	type LoginResp struct {
		Sid string `json:"sid"`
	}
	type envelop struct {
		dsmApiResp
		Data json.RawMessage `json:"data"`
	}
	e := envelop{}
	if err := json.Unmarshal(respBody, &e); err != nil {
		return err
	}
	if !e.Success {
		return fmt.Errorf("DSM Api error. Error code:%d", e.Err.Code)
	}
	loginResp := &LoginResp{}
	if e.Data != nil {
		if err := json.Unmarshal(e.Data, loginResp); err != nil {
			return err
		}
	}
	if loginResp.Sid == "" {
		// DSM answered success:true but with no data (or data without a sid).
		// Treating that as a successful login would silently publish Sid=""
		// and leave every subsequent API call unauthenticated.
		return fmt.Errorf("DSM login succeeded but returned no session id")
	}
	dsm.Sid = loginResp.Sid

	return nil
}

// Logout on current IP and reset the synoToken
func (dsm *DSM) Logout() error {
	params := url.Values{}
	params.Add("api", "SYNO.API.Auth")
	params.Add("method", "logout")
	params.Add("version", "1")

	_, err := dsm.sendRequestWithoutConnectionCheck("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return err
	}
	dsm.setSid("")

	return nil
}
