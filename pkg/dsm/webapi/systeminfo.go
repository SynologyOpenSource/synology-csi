// Copyright 2021 Synology Inc.

package webapi

import (
	"fmt"
	"net/url"
	"strings"
)

type DsmInfo struct {
	Hostname string `json:"hostname"`
}

type DsmSysInfo struct {
	Model       string `json:"model"`
	FirmwareVer string `json:"firmware_ver"`
	Serial      string `json:"serial"`
}

type NetworkInterface struct {
	Ifname     string `json:"ifname"`
	Ip         string `json:"ip"`
	Mask       string `json:"mask"`
	Speed      int    `json:"speed"`
	Status     string `json:"status"`
	Type       string `json:"type"`
	UseDhcp    bool   `json:"use_dhcp"`
}

func (dsm *DSM) DsmInfoGet() (*DsmInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.System")
	params.Add("method", "info")
	params.Add("version", "1")
	params.Add("type", "network")

	resp, err := dsm.sendRequest("", &DsmInfo{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, err
	}

	dsmInfo, ok := resp.Data.(*DsmInfo)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &DsmInfo{})
	}

	return dsmInfo, nil
}

func (dsm *DSM) DsmSystemInfoGet() (*DsmSysInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.System")
	params.Add("method", "info")
	params.Add("version", "1")

	resp, err := dsm.sendRequest("", &DsmSysInfo{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, err
	}

	dsmInfo, ok := resp.Data.(*DsmSysInfo)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &DsmSysInfo{})
	}

	return dsmInfo, nil
}


func (dsm *DSM) NetworkInterfaceList(relayNode string) ([]NetworkInterface, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Network.Interface")
	params.Add("method", "list")
	params.Add("version", "1")

	if relayNode != "" {
		params.Add("relay_node", relayNode)
	}

	ifaces := []NetworkInterface{}
	validIfaces := []NetworkInterface{}

	_, err := dsm.sendRequest("", &ifaces, params, "webapi/entry.cgi")
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		if strings.Contains(iface.Ifname, "eth") || strings.Contains(iface.Ifname, "bond") {
			validIfaces = append(validIfaces, iface)
		}
	}

	return validIfaces, nil
}
