// Copyright 2021 Synology Inc.

package models

import (
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

type CreateK8sVolumeSpec struct {
	DsmIp            string
	K8sVolumeName    string
	LunName          string
	LunDescription	 string
	ShareName        string
	Location         string
	Size             int64
	Type             string
	ThinProvisioning bool
	TargetName       string
	MultipleSession  bool
	SourceSnapshotId string
	SourceVolumeId   string
	Protocol         string
	NfsVersion       string
	DevAttribs       map[string]bool
}

type K8sVolumeRespSpec struct {
	DsmIp             string
	VolumeId          string
	SizeInBytes       int64
	Location          string
	Name              string
	Source            string
	Lun               webapi.LunInfo
	Target            webapi.TargetInfo
	Share             webapi.ShareInfo
	Protocol          string
	BaseDir           string
}

type K8sSnapshotRespSpec struct {
	DsmIp             string
	Name              string
	Uuid              string
	ParentName        string
	ParentUuid        string
	Status            string
	SizeInBytes       int64
	CreateTime        int64
	Time              string // only for share snapshot delete
	RootPath          string
	Protocol          string
}

type CreateK8sVolumeSnapshotSpec struct {
	K8sVolumeId  string
	SnapshotName string
	Description  string
	TakenBy      string
	IsLocked     bool
}

type NodeStageVolumeSpec struct {
	VolumeId          string
	StagingTargetPath string
	VolumeCapability  *csi.VolumeCapability
	Dsm               string
	Source            string
	FormatOptions     string
}

type ByVolumeId []*K8sVolumeRespSpec
func (a ByVolumeId) Len() int           { return len(a) }
func (a ByVolumeId) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByVolumeId) Less(i, j int) bool { return a[i].VolumeId < a[j].VolumeId }

type BySnapshotAndParentUuid []*K8sSnapshotRespSpec
func (a BySnapshotAndParentUuid) Len() int           { return len(a) }
func (a BySnapshotAndParentUuid) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a BySnapshotAndParentUuid) Less(i, j int) bool {
	return fmt.Sprintf("%s/%s", a[i].ParentUuid, a[i].Uuid) < fmt.Sprintf("%s/%s", a[j].ParentUuid, a[j].Uuid)
}
