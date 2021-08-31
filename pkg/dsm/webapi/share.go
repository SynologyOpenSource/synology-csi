// Copyright 2021 Synology Inc.

package webapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

type ShareInfo struct {
	Name                string `json:"name"`
	VolPath             string `json:"vol_path"`
	Desc                string `json:"desc"`
	EnableShareCow      bool   `json:"enable_share_cow"`
	EnableRecycleBin    bool   `json:"enable_recycle_bin"`
	RecycleBinAdminOnly bool   `json:"recycle_bin_admin_only"`
	Encryption          int    `json:"encryption"`
}

type ShareCreateSpec struct {
	Name      string    `json:"name"`
	ShareInfo ShareInfo `json:"shareinfo"`
}

func (dsm *DSM) ShareList() ([]ShareInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("additional", "[\"encryption\"]")

	type ShareInfos struct {
		Shares []ShareInfo `json:"shares"`
	}

	resp, err := dsm.sendRequest("", &ShareInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, err
	}

	infos, ok := resp.Data.(*ShareInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &ShareInfos{})
	}

	return infos.Shares, nil
}

func (dsm *DSM) ShareCreate(spec ShareCreateSpec) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))

	js, err := json.Marshal(spec.ShareInfo)
	if err != nil {
		return err
	}
	params.Add("shareinfo", string(js))

	// response : {"data":{"name":"Share-2"},"success":true}
	_, err = dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return err
	}

	return nil
}

func (dsm *DSM) ShareDelete(shareName string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(shareName))

	// response : {"success":true}
	_, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return err
	}

	return nil
}
