// Copyright 2021 Synology Inc.

package webapi

import (
	"fmt"
	"strconv"
	"net/url"
)

type VolInfo struct {
	Name      string `json:"display_name"`
	Path      string `json:"volume_path"`
	Status    string `json:"status"`
	FsType    string `json:"fs_type"`
	Size      string `json:"size_total_byte"`
	Free      string `json:"size_free_byte"`
	Container string `json:"container"`
	Location  string `json:"location"`
}

func (dsm *DSM) VolumeList() ([]VolInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Storage.Volume")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("offset", "0")
	params.Add("limit", "-1")
	params.Add("location", "all")

	type VolInfos struct {
		Vols []VolInfo `json:"volumes"`
	}

	resp, err := dsm.sendRequest("", &VolInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, err
	}

	volInfos, ok := resp.Data.(*VolInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &VolInfos{})
	}

	return volInfos.Vols, nil
}

func (dsm *DSM) VolumeGet(name string) (VolInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Storage.Volume")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("volume_path", strconv.Quote(name))

	type Info struct {
		Volume VolInfo `json:"volume"`
	}
	info := Info{}

	_, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return VolInfo{}, err
	}

	return info.Volume, nil
}

