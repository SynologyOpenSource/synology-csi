// Copyright 2021 Synology Inc.

package models

import (
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

type CreateK8sVolumeSpec struct {
	DsmIp            string
	K8sVolumeName    string
	LunName          string
	Location         string
	Size             int64
	Type             string
	ThinProvisioning bool
	TargetName       string
	TargetIqn        string
	MultipleSession  bool
	SourceSnapshotId string
	SourceVolumeId   string
}

type ListK8sVolumeRespSpec struct {
	DsmIp            string
	Lun              webapi.LunInfo
	Target           webapi.TargetInfo
}

type CreateK8sVolumeSnapshotSpec struct {
	K8sVolumeId  string
	SnapshotName string
	Description  string
	TakenBy      string
	IsLocked     bool
}