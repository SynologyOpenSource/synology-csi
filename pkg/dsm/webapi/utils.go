// Copyright 2022 Synology Inc.

package webapi

import (
	"fmt"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func (dsm *DSM) IsUC() bool {
	dsmSysInfo, err := dsm.DsmSystemInfoGet()
	if err != nil {
		log.Errorf("failed to get DSM[%s] system info", dsm.Ip)
		return false
	}
	return strings.Contains(dsmSysInfo.FirmwareVer, "DSM UC")
}

func (dsm *DSM) GetAnotherController() (*DSM, error) {
	anotherDsm := &DSM{
		Port:     dsm.Port,
		Username: dsm.Username,
		Password: dsm.Password,
		Https:    dsm.Https,
	}

	netListA, err := dsm.NetworkInterfaceList("node0")
	if err != nil {
		return nil, fmt.Errorf("failed to get DSM network list of controller A: %w", err)
	}

	netListB, err := dsm.NetworkInterfaceList("node1")
	if err != nil {
		return nil, fmt.Errorf("failed to get DSM network list of controller B: %w", err)
	}

	ips, err := utils.LookupIPv4(dsm.Ip) // because dsm.Ip may be a domain
	if err != nil {
		return nil, err
	}

	dsm.Controller = "A"
	anotherDsm.Controller = "B"
	anotherList := netListB
	for _, netIf := range netListB {
		if netIf.Ip == ips[0] {
			dsm.Controller = "B"
			anotherDsm.Controller = "A"
			anotherList = netListA
			break
		}
	}

	ipPrefix := ips[0]
	for i := 0; i < 4; i++ {
		dotPos := strings.LastIndex(ipPrefix, ".")

		if dotPos == -1 {
			ipPrefix = ""
		} else {
			ipPrefix = ipPrefix[:dotPos]
		}

		for _, netIf := range anotherList {
			if ipPrefix != "" && !strings.HasPrefix(netIf.Ip, ipPrefix) {
				continue
			}

			if netIf.Status == "connected" && CheckIpReachable(netIf.Ip, anotherDsm.Port) {
				anotherDsm.Ip = netIf.Ip
				return anotherDsm, nil
			}
		}
	}

	return nil, fmt.Errorf("failed to get reachable network of another controller")
}

func CheckIpReachable(ip string, port int) bool {
	_, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 5*time.Second)

	return err == nil
}
