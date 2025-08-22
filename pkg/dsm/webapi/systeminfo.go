// Copyright 2021 Synology Inc.

package webapi

import (
	"fmt"
	"net/url"
	"strings"
)

type DsmSysInfo struct {
	Model         string `json:"model"`
	FirmwareVer   string `json:"firmware_ver"`
	Serial        string `json:"serial"`
	// type: network
	Hostname      string `json:"hostname"`
	// type: define
	SupportNvmeof string `json:"support_nvmeof"`
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

func (dsm *DSM) FillSystemInfo() error {
	info, err := dsm.DsmSystemInfoGet("define")
	if err != nil {
		return err
	}
	dsm.SupportNvmeof = (info.SupportNvmeof == "yes")

	info, err = dsm.DsmSystemInfoGet("network")
	if err != nil {
		return err
	}
	dsm.Hostname = info.Hostname

	info, err = dsm.DsmSystemInfoGet("")
	if err != nil {
		return err
	}
	dsm.FirmwareVer = info.FirmwareVer

	return nil
}

func (dsm *DSM) DsmSystemInfoGet(infoType string) (*DsmSysInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.System")
	params.Add("method", "info")
	params.Add("version", "1")
	if infoType != "" {
		params.Add("type", infoType)
	}

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
