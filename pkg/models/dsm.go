// Copyright 2021 Synology Inc.

package models

import (
	"fmt"
)

const (
	K8sCsiName       = "Kubernetes CSI"

	// ISCSI definitions
	FsTypeExt4       = "ext4"
	FsTypeBtrfs      = "btrfs"
	LunTypeFile      = "FILE"
	LunTypeAdv       = "ADV"
	LunTypeBlun      = "BLUN"               // thin provision, mapped to type 263
	LunTypeBlunThick = "BLUN_THICK"         // thick provision, mapped to type 259
	MaxIqnLen = 128

	// NVMe definitions
	MaxNqnLen = 128

	// Share definitions
	MaxShareLen     = 32
	MaxShareDescLen = 64
	UserGroupTypeLocalUser  = "local_user"
	UserGroupTypeLocalGroup = "local_group"
	UserGroupTypeSystem     = "system"


	// CSI definitions
	TargetPrefix            = "k8s-csi"
	SubsystemPrefix         = TargetPrefix
	DevicePrefix            = "k8s-csi"
	IqnPrefix               = "iqn.2000-01.com.synology:"
	NqnPrefix               = "nqn.2000-01.com.synology:"
	SharePrefix             = "k8s-csi"
	ShareSnapshotDescPrefix = "(Do not change)"
)

func GenBackendName(volName string) string {
	return fmt.Sprintf("%s-%s", DevicePrefix, volName)
}

func GenShareName(volName string) string {
	shareName := fmt.Sprintf("%s-%s", SharePrefix, volName)
	if len(shareName) > MaxShareLen {
		return shareName[:MaxShareLen]
	}
	return shareName
}
