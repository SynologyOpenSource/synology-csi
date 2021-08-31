// Copyright 2021 Synology Inc.

package utils

import (
	"fmt"
	"net"
	"strings"
)

const UNIT_GB = 1024 * 1024 * 1024

func StringToBoolean(value string) bool {
	value = strings.ToLower(value)
	return value == "yes" || value == "true" || value == "1"
}

// Haven't supported IPv6 yet.
func LookupIPv4(name string) ([]string, error) {
	ips, _ := net.LookupIP(name)

	retIps := []string{}
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			retIps = append(retIps, ipv4.String())
		}
	}
	if len(retIps) > 0 {
		return retIps, nil
	}

	return nil, fmt.Errorf("Failed to LookupIPv4 by local resolver for: %s", name)
}