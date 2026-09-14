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

func (service *DsmService) createSMBorNFSVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcSnapshot *models.K8sSnapshotRespSpec) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

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

	if _, err := dsm.ShareClone(shareCloneSpec); err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil,
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcShareInfo.Uuid, err))
		}
		// The share is from an earlier attempt, so it is not ours to remove.
	} else {
		shareName := spec.ShareName
		rollback.add(fmt.Sprintf("share(%s)", shareName), func() error { return dsm.ShareDelete(shareName) })
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

	if err := checkRestoreSize(int64(newSizeInMB), int64(shareInfo.QuotaValueInMB), "share quotaMB"); err != nil {
		return nil, err
	}

	// The restored share carries the snapshot's quota; raise it if the PVC asked
	// for more. Failing here rolls the share back rather than returning a volume
	// smaller than promised.
	if newSizeInMB > shareInfo.QuotaValueInMB {
		if err := dsm.SetShareQuota(shareInfo, newSizeInMB); err != nil {
			return nil, status.Errorf(codes.Internal,
				"Failed to raise the quota of restored share [%s] from [%d] to [%d] MB, err: %v",
				shareInfo.Name, shareInfo.QuotaValueInMB, newSizeInMB, err)
		}
		shareInfo.QuotaValueInMB = newSizeInMB
	}

	log.Debugf("[%s] createSMBorNFSVolumeBySnapshot Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid)

	return DsmShareToK8sVolume(dsm.Ip, shareInfo, spec.Protocol), nil
}

func (service *DsmService) createSMBorNFSVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcShareInfo webapi.ShareInfo) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

	newSizeInMB := utils.BytesToMBCeil(spec.Size)
	if spec.Size != 0 {
		if err := checkRestoreSize(int64(newSizeInMB), int64(srcShareInfo.QuotaValueInMB), "share quotaMB"); err != nil {
			return nil, err
		}
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

	if _, err := dsm.ShareClone(shareCloneSpec); err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil,
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcShareInfo.Uuid, err))
		}
	} else {
		shareName := spec.ShareName
		rollback.add(fmt.Sprintf("share(%s)", shareName), func() error { return dsm.ShareDelete(shareName) })
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
	} else if newSizeInMB > shareInfo.QuotaValueInMB {
		// The clone inherits the source's quota; raise it if the PVC asked for
		// more. Failing here rolls the share back rather than returning a volume
		// smaller than promised.
		if err := dsm.SetShareQuota(shareInfo, newSizeInMB); err != nil {
			return nil, status.Errorf(codes.Internal,
				"Failed to raise the quota of cloned share [%s] from [%d] to [%d] MB, err: %v",
				shareInfo.Name, shareInfo.QuotaValueInMB, newSizeInMB, err)
		}
		shareInfo.QuotaValueInMB = newSizeInMB
	}

	log.Debugf("[%s] createSMBorNFSVolumeByVolume Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid)

	return DsmShareToK8sVolume(dsm.Ip, shareInfo, spec.Protocol), nil
}

func (service *DsmService) createSMBorNFSVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

	// TODO: Check if share name is allowable

	// 1. Find a available location
	if spec.Location == "" {
		vol, err := service.getFirstAvailableVolume(dsm, spec.Size, spec.Protocol)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				fmt.Sprintf("Failed to get available location, err: %v", err))
		}
		spec.Location = vol.Path
	}

	// 2. Check if location exists
	dsmVolInfo, err := dsm.VolumeGet(spec.Location)
	if err != nil {
		return nil,
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unable to find location %s", spec.Location))
	}

	if dsmVolInfo.FsType == models.FsTypeExt4 {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Location: %s with ext4 fstype was not supported for creating smb/nfs protocol's K8s volume", spec.Location))
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
	if err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create share, err: %v", err))
		}
	} else {
		shareName := spec.ShareName
		rollback.add(fmt.Sprintf("share(%s)", shareName), func() error { return dsm.ShareDelete(shareName) })
	}

	shareInfo, err := dsm.ShareGet(spec.ShareName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed Share with name: %s, err: %v", spec.ShareName, err))
	}

	log.Debugf("[%s] createSMBorNFSVolumeByDsm Successfully. VolumeId: %s", dsm.Ip, shareInfo.Uuid)

	return DsmShareToK8sVolume(dsm.Ip, shareInfo, spec.Protocol), nil
}

// shareToVolume decides whether a share is an NFS or an SMB volume.
//
// The only way to tell them apart is whether the share has an NFS export rule,
// and reading that rule costs a call to the privilege API. That call is the
// expensive part of listing: on the DSM side it takes a shared lock on
// smb.conf, which creating and deleting shares hold exclusively, so asking
// about shares we are not interested in makes the driver compete with itself.
// Callers that are looking for one particular share should use
// findSMBorNFSVolume rather than classifying everything.
func (service *DsmService) shareToVolume(dsm *webapi.DSM, share webapi.ShareInfo) (*models.K8sVolumeRespSpec, error) {
	sharePrivilege, err := dsm.ShareNfsPrivilegeLoad(share.Name)
	if err != nil {
		return nil, err
	}

	// if share has set nfs rule, deal it as NFS
	if len(sharePrivilege.Rule) > 0 {
		return DsmShareToK8sVolume(dsm.Ip, share, utils.ProtocolNfs), nil
	}
	return DsmShareToK8sVolume(dsm.Ip, share, utils.ProtocolSmb), nil
}

// findSMBorNFSVolume returns the one share matching pred, classifying only that
// share. It reports (nil, nil) when no share matched and every DSM could be
// listed, so the caller may then conclude the volume is not a share at all.
func (service *DsmService) findSMBorNFSVolume(pred func(webapi.ShareInfo) bool) (volume *models.K8sVolumeRespSpec, listErr error) {
	for _, dsm := range service.dsms {
		if dsm.IsUC() {
			continue
		}

		shares, err := dsm.ShareList()
		if err != nil {
			log.Errorf("[%s] Failed to list shares: %v", dsm.Ip, err)
			listErr = errors.Join(listErr, fmt.Errorf("DSM[%s] failed to list shares: %w", dsm.Ip, err))
			continue
		}

		for _, share := range shares {
			if !strings.HasPrefix(share.Name, models.SharePrefix) || !pred(share) {
				continue
			}

			info, err := service.shareToVolume(dsm, share)
			if err != nil {
				log.Errorf("[%s] Failed to load share nfs privilege: %v", dsm.Ip, err)
				return nil, fmt.Errorf("DSM[%s] failed to load NFS privilege of share(%s): %w", dsm.Ip, share.Name, err)
			}
			return info, nil
		}
	}

	return nil, listErr
}

// listSMBorNFSVolumes returns the shares it could enumerate. A non-nil error
// means the list is incomplete. This matters more here than for the other
// protocols: DSM rejects the privilege API while it is busy, and treating a
// share that could not be classified as one that no longer exists is what makes
// DeleteVolume report success without deleting anything.
func (service *DsmService) listSMBorNFSVolumes(dsmIp string) (infos []*models.K8sVolumeRespSpec, listErr error) {
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
			listErr = errors.Join(listErr, fmt.Errorf("DSM[%s] failed to list shares: %w", dsm.Ip, err))
			continue
		}

		for _, share := range shares {
			if !strings.HasPrefix(share.Name, models.SharePrefix) {
				continue
			}
			info, err := service.shareToVolume(dsm, share)
			if err != nil {
				log.Errorf("[%s] Failed to load share nfs privilege: %v", dsm.Ip, err)
				listErr = errors.Join(listErr, fmt.Errorf("DSM[%s] failed to load NFS privilege of share(%s): %w", dsm.Ip, share.Name, err))
				continue
			}
			infos = append(infos, info)
		}
	}

	return infos, listErr
}

func (service *DsmService) listSMBorNFSSnapshotsByDsm(dsm *webapi.DSM) (infos []*models.K8sSnapshotRespSpec) {
	volumes, err := service.listSMBorNFSVolumes(dsm.Ip)
	if err != nil {
		// Snapshot listing is best-effort; log the gap rather than hiding it.
		log.Errorf("[%s] Share list was incomplete while listing snapshots: %v", dsm.Ip, err)
	}
	for _, volume := range volumes {
		shareInfo := volume.Share
		shareSnaps, err := dsm.ShareSnapshotList(shareInfo.Name)
		if err != nil {
			log.Errorf("[%s] Failed to list share snapshots: %v", dsm.Ip, err)
			continue
		}
		for _, info := range shareSnaps {
			infos = append(infos, DsmShareSnapshotToK8sSnapshot(dsm.Ip, info, shareInfo, volume.Protocol))
		}
	}
	return infos
}

func (service *DsmService) getSMBorNFSSnapshot(snapshotUuid string) *models.K8sSnapshotRespSpec {
	for _, dsm := range service.dsms {
		snapshots := service.listSMBorNFSSnapshotsByDsm(dsm)
		for _, snap := range snapshots {
			if snap.Uuid == snapshotUuid {
				return snap
			}
		}
	}

	return nil
}
