/*
 * Copyright 2022 Synology Inc.
 */

package service

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"time"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func GMTToUnixSecond(timeStr string) (int64) {
	t, err := time.Parse("GMT-07-2006.01.02-15.04.05", timeStr)
	if err != nil {
		log.Error(err)
		return -1
	}
	return t.Unix()
}

func (service *DsmService) createSMBVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcSnapshot *models.K8sSnapshotRespSpec) (*models.K8sVolumeRespSpec, error) {
	srcShareInfo, err := dsm.ShareGet(srcSnapshot.ParentName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get share: %s, err: %v", srcSnapshot.ParentName, err))
	}

	shareCloneSpec := webapi.ShareCloneSpec{
		Name: spec.ShareName,
		Snapshot: srcSnapshot.Time,
		ShareInfo: webapi.ShareInfo{
			Name:                spec.ShareName,
			VolPath:             srcSnapshot.RootPath,
			Desc:                "Cloned from [" + srcSnapshot.Time + "] by csi driver", // max: 64
			EnableRecycleBin:    srcShareInfo.EnableRecycleBin,
			RecycleBinAdminOnly: srcShareInfo.RecycleBinAdminOnly,
			NameOrg:             srcSnapshot.ParentName,
		},
	}

	if _, err := dsm.ShareClone(shareCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcShareInfo.Uuid, err))
	}

	shareInfo, err := dsm.ShareGet(spec.ShareName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed Share with name: [%s], err: %v", spec.ShareName, err))
	}

	newSizeInMB := utils.BytesToMBCeil(spec.Size)
	if shareInfo.QuotaValueInMB == 0 {
		// known issue for some DS, manually set quota to the new share
		if err := dsm.SetShareQuota(shareInfo, newSizeInMB); err != nil {
			msg := fmt.Sprintf("Failed to set quota [%d] to Share [%s], err: %v", newSizeInMB, shareInfo.Name, err)
			log.Error(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}

		shareInfo.QuotaValueInMB = newSizeInMB
	}

	if newSizeInMB != shareInfo.QuotaValueInMB {
		// FIXME: need to delete share
		return nil,
			status.Errorf(codes.OutOfRange, "Requested share quotaMB [%d] is not equal to snapshot restore quotaMB [%d]", newSizeInMB, shareInfo.QuotaValueInMB)
	}

	log.Debugf("[%s] createSMBVolumeBySnapshot Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid);

	return DsmShareToK8sVolume(dsm.Ip, shareInfo), nil
}

func (service *DsmService) createSMBVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcShareInfo webapi.ShareInfo) (*models.K8sVolumeRespSpec, error) {
	newSizeInMB := utils.BytesToMBCeil(spec.Size)
	if spec.Size != 0 && newSizeInMB != srcShareInfo.QuotaValueInMB {
		return nil,
			status.Errorf(codes.OutOfRange, "Requested share quotaMB [%d] is not equal to src share quotaMB [%d]", newSizeInMB, srcShareInfo.QuotaValueInMB)
	}

	shareCloneSpec := webapi.ShareCloneSpec{
		Name: spec.ShareName,
		Snapshot: "",
		ShareInfo: webapi.ShareInfo{
			Name:                spec.ShareName,
			VolPath:             srcShareInfo.VolPath, // must be same with srcShare location
			Desc:                "Cloned from [" + srcShareInfo.Name + "] by csi driver", // max: 64
			EnableRecycleBin:    srcShareInfo.EnableRecycleBin,
			RecycleBinAdminOnly: srcShareInfo.RecycleBinAdminOnly,
			NameOrg:             srcShareInfo.Name,
		},
	}

	if _, err := dsm.ShareClone(shareCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcShareInfo.Uuid, err))
	}

	shareInfo, err := dsm.ShareGet(spec.ShareName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed Share with name: [%s], err: %v", spec.ShareName, err))
	}

	if shareInfo.QuotaValueInMB == 0 {
		// known issue for some DS, manually set quota to the new share
		if err := dsm.SetShareQuota(shareInfo, newSizeInMB); err != nil {
			msg := fmt.Sprintf("Failed to set quota [%d] to Share [%s], err: %v", newSizeInMB, shareInfo.Name, err)
			log.Error(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}

		shareInfo.QuotaValueInMB = newSizeInMB
	}

	log.Debugf("[%s] createSMBVolumeByVolume Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid);

	return DsmShareToK8sVolume(dsm.Ip, shareInfo), nil
}

func (service *DsmService) createSMBVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	// TODO: Check if share name is allowable

	// 1. Find a available location
	if spec.Location == "" {
		vol, err := service.getFirstAvailableVolume(dsm, spec.Size)
		if err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get available location, err: %v", err))
		}
		spec.Location = vol.Path
	}

	// 2. Check if location exists
	_, err := dsm.VolumeGet(spec.Location)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unable to find location %s", spec.Location))
	}

	// 3. Create Share
	sizeInMB := utils.BytesToMBCeil(spec.Size)
	shareSpec := webapi.ShareCreateSpec{
		Name: spec.ShareName,
		ShareInfo: webapi.ShareInfo{
			Name:                spec.ShareName,
			VolPath:             spec.Location,
			Desc:                "Created by Synology K8s CSI",
			EnableShareCow:      false,
			EnableRecycleBin:    true,
			RecycleBinAdminOnly: true,
			Encryption:          0,
			QuotaForCreate:      &sizeInMB,
		},
	}

	log.Debugf("ShareCreate spec: %v", shareSpec)
	err = dsm.ShareCreate(shareSpec)
	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create share, err: %v", err))
	}

	shareInfo, err := dsm.ShareGet(spec.ShareName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed Share with name: %s, err: %v", spec.ShareName, err))
	}

	log.Debugf("[%s] createSMBVolumeByDsm Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid)

	return DsmShareToK8sVolume(dsm.Ip, shareInfo), nil
}

func (service *DsmService) listSMBVolumes(dsmIp string) (infos []*models.K8sVolumeRespSpec) {
	for _, dsm := range service.dsms {
		if dsmIp != "" && dsmIp != dsm.Ip {
			continue
		}

		if dsm.IsUC() {
			continue
		}

		shares, err := dsm.ShareList()
		if err != nil {
			log.Errorf("[%s] Failed to list shares: %v", dsm.Ip, err)
			continue
		}

		for _, share := range shares {
			if !strings.HasPrefix(share.Name, models.SharePrefix) {
				continue
			}
			infos = append(infos, DsmShareToK8sVolume(dsm.Ip, share))
		}
	}

	return infos
}

func (service *DsmService) listSMBSnapshotsByDsm(dsm *webapi.DSM) (infos []*models.K8sSnapshotRespSpec) {
	volumes := service.listSMBVolumes(dsm.Ip)
	for _, volume := range volumes {
		shareInfo := volume.Share
		shareSnaps, err := dsm.ShareSnapshotList(shareInfo.Name)
		if err != nil {
			log.Errorf("[%s] Failed to list share snapshots: %v", dsm.Ip, err)
			continue
		}
		for _, info := range shareSnaps {
			infos = append(infos, DsmShareSnapshotToK8sSnapshot(dsm.Ip, info, shareInfo))
		}
	}
	return infos
}

func (service *DsmService) getSMBSnapshot(snapshotUuid string) *models.K8sSnapshotRespSpec {
	for _, dsm := range service.dsms {
		snapshots := service.listSMBSnapshotsByDsm(dsm)
		for _, snap := range snapshots {
			if snap.Uuid == snapshotUuid {
				return snap
			}
		}
	}

	return nil
}
