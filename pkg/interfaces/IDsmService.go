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
	CreateVolume(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error)
	DeleteVolume(volId string) error
	ListVolumes() []*models.K8sVolumeRespSpec
	GetVolume(volId string) *models.K8sVolumeRespSpec
	ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error)
	CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error)
	DeleteSnapshot(snapshotUuid string) error
	ListAllSnapshots() []*models.K8sSnapshotRespSpec
	ListSnapshots(volId string) []*models.K8sSnapshotRespSpec
	GetVolumeByName(volName string) *models.K8sVolumeRespSpec
	GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec
}