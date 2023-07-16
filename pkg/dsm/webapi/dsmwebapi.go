/*
 * Copyright 2021 Synology Inc.
 */

package webapi

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"

	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	log "github.com/sirupsen/logrus"
)

type DSM struct {
	Ip         string
	Port       int
	Username   string
	Password   string
	Sid        string
	Https      bool
	Controller string //new
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
	client := &http.Client{}
	var req *http.Request
	var err error
	var cgiUrl string

	// Ex: http://10.12.12.14:5000/webapi/auth.cgi
	if dsm.Https {
		// TODO: input CA certificate and fill in tls config
		// Skip Verify when https
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client = &http.Client{Transport: tr}
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
		log.Debugln(baseUrl.RawQuery)
	}

	if data != "" {
		req, err = http.NewRequest("POST", baseUrl.String(), nil)
	} else {
		req, err = http.NewRequest("GET", baseUrl.String(), nil)
	}

	if dsm.Sid != "" {
		cookie := http.Cookie{Name: "id", Value: dsm.Sid}
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

// Login by given user name and password
func (dsm *DSM) Login() error {
	params := url.Values{}
	params.Add("api", "SYNO.API.Auth")
	params.Add("method", "login")
	params.Add("version", "3")
	params.Add("account", dsm.Username)
	params.Add("passwd", dsm.Password)
	params.Add("format", "sid")

	type LoginResp struct {
		Sid string `json:"sid"`
	}

	resp, err := dsm.sendRequestWithoutConnectionCheck("", &LoginResp{}, params, "webapi/auth.cgi")
	if err != nil {
		r, _ := regexp.Compile("passwd=.*&")
		temp := r.ReplaceAllString(err.Error(), "")

		return fmt.Errorf("%s", temp)
	}

	loginResp, ok := resp.Data.(*LoginResp)
	if !ok {
		return fmt.Errorf("Failed to assert response to %T", &LoginResp{})
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
	dsm.Sid = ""

	return nil
}
