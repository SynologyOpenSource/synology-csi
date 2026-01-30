// Copyright 2021 Synology Inc.

package models

const (
	K8sCsiName       = "Kubernetes CSI"

	// ISCSI definitions
	FsTypeExt4       = "ext4"
	FsTypeBtrfs      = "btrfs"
	LunTypeFile      = "FILE"
	LunTypeThin      = "THIN"
	LunTypeAdv       = "ADV"
	LunTypeBlun      = "BLUN"               // thin provision, mapped to type 263
	LunTypeBlunThick = "BLUN_THICK"         // thick provision, mapped to type 259
	MaxIqnLen = 128

	// Share definitions
	MaxShareLen     = 32
	MaxShareDescLen = 64
	UserGroupTypeLocalUser  = "local_user"
	UserGroupTypeLocalGroup = "local_group"
	UserGroupTypeSystem     = "system"


	// CSI definitions
	TargetPrefix            = "k8s-csi"
	LunPrefix               = "k8s-csi"
	IqnPrefix               = "iqn.2000-01.com.synology:"
	SharePrefix             = "k8s-csi"
	ShareSnapshotDescPrefix = "(Do not change)"

	// CSI parameter definitions
	CSIPVCName    = "csi.storage.k8s.io/pvc/name"
	CSIUsePVCName = "use_pvc_name"
)
