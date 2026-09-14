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
	// A non-nil error means the listing was incomplete: absence from the result
	// does not prove a volume is gone.
	ListVolumes() ([]*models.K8sVolumeRespSpec, error)
	// Returns (nil, nil) only when the volume is known not to exist.
	GetVolume(volId string) (*models.K8sVolumeRespSpec, error)
	ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error)
	CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error)
	DeleteSnapshot(snapshotUuid string) error
	ListAllSnapshots() []*models.K8sSnapshotRespSpec
	ListSnapshots(volId string) []*models.K8sSnapshotRespSpec
	GetVolumeByName(volName string) (*models.K8sVolumeRespSpec, error)
	GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec
}