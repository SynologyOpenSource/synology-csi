// Copyright 2021 Synology Inc.

package interfaces

import (
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
)

// An interface for DSM service

type IDsmService interface {
	AddDsm(client common.ClientInfo) error
	RemoveAllDsms()
	GetDsm(ip string) (*webapi.DSM, error)
	GetDsmsCount() int
	ListDsmVolumes(ip string) ([]webapi.VolInfo, error)
	CreateVolume(spec *models.CreateK8sVolumeSpec) (webapi.LunInfo, string, error)
	DeleteVolume(volumeId string) error
	ListVolumes() []*models.ListK8sVolumeRespSpec
	GetVolume(lunUuid string) *models.ListK8sVolumeRespSpec
	ExpandLun(lunUuid string, newSize int64) error
	CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (string, error)
	DeleteSnapshot(snapshotUuid string) error
	ListAllSnapshots() ([]webapi.SnapshotInfo, error)
	ListSnapshots(lunUuid string) ([]webapi.SnapshotInfo, error)
	GetSnapshot(snapshotUuid string) (webapi.SnapshotInfo, error)
}