/*
 * Copyright 2021 Synology Inc.
 */

package service

import (
	"errors"
	"fmt"

	"github.com/cenkalti/backoff/v4"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strconv"
	"time"
	"strings"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type DsmService struct {
	dsms map[string]*webapi.DSM
}

func NewDsmService() *DsmService {
	return &DsmService{
		dsms: make(map[string]*webapi.DSM),
	}
}

func (service *DsmService) AddDsm(client common.ClientInfo) error {
	// TODO: use sn or other identifiers as key
	if _, ok := service.dsms[client.Host]; ok {
		log.Infof("Adding DSM [%s] already present.", client.Host)
		return nil
	}

	dsm := &webapi.DSM{
		Ip:       client.Host,
		Port:     client.Port,
		Username: client.Username,
		Password: client.Password,
		Https:    client.Https,
	}
	err := dsm.Login()
	if err != nil {
		return fmt.Errorf("Failed to login to DSM: [%s]. err: %v", dsm.Ip, err)
	}
	service.dsms[dsm.Ip] = dsm
	log.Infof("Add DSM [%s].", dsm.Ip)
	return nil
}

func (service *DsmService) RemoveAllDsms() {
	for _, dsm := range service.dsms {
		log.Infof("Going to logout DSM [%s]", dsm.Ip)

		for i := 0; i < 3; i++ {
			err := dsm.Logout()
			if err == nil {
				break
			}
			log.Debugf("Retry to logout DSM [%s], retry: %d", dsm.Ip, i)
		}
	}
	return
}

func (service *DsmService) GetDsm(ip string) (*webapi.DSM, error) {
	dsm, ok := service.dsms[ip]
	if !ok {
		return nil, fmt.Errorf("Requested dsm [%s] does not exist", ip)
	}
	return dsm, nil
}

func (service *DsmService) GetDsmsCount() int {
	return len(service.dsms)
}

func (service *DsmService) ListDsmVolumes(ip string) ([]webapi.VolInfo, error) {
	var allVolInfos []webapi.VolInfo

	for _, dsm := range service.dsms {
		if ip != "" && dsm.Ip != ip {
			continue
		}

		volInfos, err := dsm.VolumeList()
		if err != nil {
			continue
		}

		allVolInfos = append(allVolInfos, volInfos...)
	}

	return allVolInfos, nil
}

func (service *DsmService) getFirstAvailableVolume(dsm *webapi.DSM, sizeInBytes int64) (webapi.VolInfo, error) {
	volInfos, err := dsm.VolumeList()
	if err != nil {
		return webapi.VolInfo{}, err
	}

	for _, volInfo := range volInfos {
		free, err := strconv.ParseInt(volInfo.Free, 10, 64)
		if err != nil {
			return webapi.VolInfo{}, err
		}
		if free < utils.UNIT_GB {
			continue
		}
		if volInfo.Status == "crashed" || volInfo.Status == "read_only" || volInfo.Status == "deleting" || free <= sizeInBytes {
			continue
		}
		// ignore esata disk
		if volInfo.Container == "external" && volInfo.Location == "sata" {
			continue
		}
		return volInfo, nil
	}
	return webapi.VolInfo{}, fmt.Errorf("Cannot find any available volume")
}

func getLunTypeByInputParams(lunType string, isThin bool, locationFsType string) (string, error) {
	log.Debugf("Input lunType: %v, isThin: %v, locationFsType: %v", lunType, isThin, locationFsType)
	if lunType != "" {
		return lunType, nil
	}

	if locationFsType == models.FsTypeExt4 {
		if isThin {
			return models.LunTypeAdv, nil // ADV
		} else {
			return models.LunTypeFile, nil // FILE
		}
	} else if locationFsType == models.FsTypeBtrfs {
		if isThin {
			return models.LunTypeBlun, nil // BLUN
		} else {
			return models.LunTypeBlunThick, nil // BLUN_THICK
		}
	}

	return "", fmt.Errorf("Unknown volume fs type: %s", locationFsType)
}

func (service *DsmService) createMappingTarget(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, lunUuid string) error {
	dsmInfo, err := dsm.DsmInfoGet()

	if err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s] info", dsm.Ip));
	}

	genTargetIqn := func() string {
		iqn := models.IqnPrefix + fmt.Sprintf("%s.%s", dsmInfo.Hostname, spec.K8sVolumeName)
		iqn = strings.ReplaceAll(iqn, "_", "-")
		iqn = strings.ReplaceAll(iqn, "+", "p")

		if len(iqn) > models.MaxIqnLen {
			return iqn[:models.MaxIqnLen]
		}
		return iqn
	}
	targetSpec := webapi.TargetCreateSpec{
		Name: spec.TargetName,
		Iqn: genTargetIqn(),
	}

	log.Debugf("TargetCreate spec: %v", targetSpec)
	targetId, err := dsm.TargetCreate(targetSpec)

	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to create target with spec: %v, err: %v", targetSpec, err))
	}

	if targetInfo, err := dsm.TargetGet(targetSpec.Name); err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to get target with spec: %v, err: %v", targetSpec, err))
	} else {
		targetId = strconv.Itoa(targetInfo.TargetId);
	}

	if spec.MultipleSession == true {
		if err := dsm.TargetSet(targetId, 0); err != nil {
			return status.Errorf(codes.Internal, fmt.Sprintf("Failed to set target [%s] max session, err: %v", spec.TargetName, err))
		}
	}

	err = dsm.LunMapTarget([]string{targetId}, lunUuid)
	if err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to map target [%s] to lun [%s], err: %v", spec.TargetName, lunUuid, err))
	}

	return nil
}

func (service *DsmService) createVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (webapi.LunInfo, error) {
	// 1. Find a available location
	if spec.Location == "" {
		vol, err := service.getFirstAvailableVolume(dsm, spec.Size)
		if err != nil {
			return webapi.LunInfo{},
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to get available location, err: %v", err))
		}
		spec.Location = vol.Path
	}

	// 2. Check if location exists
	dsmVolInfo, err := dsm.VolumeGet(spec.Location)
	if err != nil {
		return webapi.LunInfo{},
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unable to find location %s", spec.Location))
	}

	lunType, err := getLunTypeByInputParams(spec.Type, spec.ThinProvisioning, dsmVolInfo.FsType)
	if err != nil {
		return webapi.LunInfo{},
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unknown volume fs type: %s, location: %s", dsmVolInfo.FsType, spec.Location))
	}

	// 3. Create LUN
	lunSpec := webapi.LunCreateSpec{
		Name:     spec.LunName,
		Location: spec.Location,
		Size:     spec.Size,
		Type:     lunType,
	}

	log.Debugf("LunCreate spec: %v", lunSpec)
	_, err = dsm.LunCreate(lunSpec)

	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create Volume with name: %s, err: %v", spec.K8sVolumeName, err))
	}

	// No matter lun existed or not, Get Lun by name
	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return webapi.LunInfo{},
			// discussion with log
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	// 4. Create Target and Map to Lun
	err = service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] CreateVolume Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid);

	return lunInfo, nil
}

func waitCloneFinished(dsm *webapi.DSM, lunName string) error {
	cloneBackoff := backoff.NewExponentialBackOff()
	cloneBackoff.InitialInterval = 1 * time.Second
	cloneBackoff.Multiplier = 2
	cloneBackoff.RandomizationFactor = 0.1
	cloneBackoff.MaxElapsedTime = 20 * time.Second

	checkFinished := func() error {
		lunInfo, err := dsm.LunGet(lunName)
		if err != nil {
			return backoff.Permanent(fmt.Errorf("Failed to get existed LUN with name: %s, err: %v", lunName, err))
		}

		if lunInfo.IsActionLocked != false {
			return fmt.Errorf("Clone not yet completed. Lun: %s", lunName)
		}
		return nil
	}

	cloneNotify := func(err error, duration time.Duration) {
		log.Infof("Lun is being locked for lun clone, waiting %3.2f seconds .....", float64(duration.Seconds()))
	}

	if err := backoff.RetryNotify(checkFinished, cloneBackoff, cloneNotify); err != nil {
		log.Errorf("Could not finish clone after %3.2f seconds. err: %v", float64(cloneBackoff.MaxElapsedTime.Seconds()), err)
		return err
	}

	log.Debugf("Clone successfully. Lun: %v", lunName)
	return nil
}

func (service *DsmService) createVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, snapshotInfo webapi.SnapshotInfo) (webapi.LunInfo, error) {
	if spec.Size != 0 && spec.Size != snapshotInfo.TotalSize {
		return webapi.LunInfo{}, status.Errorf(codes.OutOfRange, "Lun size [%d] is not equal to snapshot size [%d]", spec.Size, snapshotInfo.TotalSize)
	}

	snapshotCloneSpec := webapi.SnapshotCloneSpec{
		Name:            spec.LunName,
		SrcLunUuid:      snapshotInfo.ParentUuid,
		SrcSnapshotUuid: snapshotInfo.Uuid,
	}

	if _, err := dsm.SnapshotClone(snapshotCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source snapshot ID: %s, err: %v", snapshotInfo.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.LunName); err != nil {
		return webapi.LunInfo{}, status.Errorf(codes.Internal, err.Error())
	}

	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	err = service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] createVolumeBySnapshot Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid);

	return lunInfo, nil
}

func (service *DsmService) createVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcLunInfo webapi.LunInfo) (webapi.LunInfo, error) {
	if spec.Size != 0 && spec.Size != int64(srcLunInfo.Size) {
		return webapi.LunInfo{}, status.Errorf(codes.OutOfRange, "Lun size [%d] is not equal to src lun size [%d]", spec.Size, srcLunInfo.Size)
	}

	if spec.Location == "" {
		spec.Location = srcLunInfo.Location
	}

	lunCloneSpec := webapi.LunCloneSpec{
		Name:            spec.LunName,
		SrcLunUuid:      srcLunInfo.Uuid,
		Location:        spec.Location,
	}

	if _, err := dsm.LunClone(lunCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcLunInfo.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.LunName); err != nil {
		return webapi.LunInfo{}, status.Errorf(codes.Internal, err.Error())
	}

	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	err = service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return webapi.LunInfo{},
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] createVolumeByVolume Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid);

	return lunInfo, nil
}

func (service *DsmService) CreateVolume(spec *models.CreateK8sVolumeSpec) (webapi.LunInfo, string, error) {
	if spec.SourceVolumeId != "" {
		/* Create volume by exists volume (Clone) */
		k8sVolume := service.GetVolume(spec.SourceVolumeId)
		if k8sVolume == nil {
			return webapi.LunInfo{}, "", status.Errorf(codes.NotFound, fmt.Sprintf("No such volume id: %s", spec.SourceVolumeId))
		}

		dsm, err := service.GetDsm(k8sVolume.DsmIp)
		if err != nil {
			return webapi.LunInfo{}, "", status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
		}

		lunInfo, err := service.createVolumeByVolume(dsm, spec, k8sVolume.Lun)
		return lunInfo, dsm.Ip, err
	} else if spec.SourceSnapshotId != "" {
		/* Create volume by snapshot */
		for _, dsm := range service.dsms {
			snapshotInfo, err := dsm.SnapshotGet(spec.SourceSnapshotId)
			if err != nil {
				continue
			}

			// found source by snapshot id, check allowable
			if spec.DsmIp != "" && spec.DsmIp != dsm.Ip {
				msg := fmt.Sprintf("The source PVC and destination PVCs must be on the same DSM for cloning from snapshots. Source is on %s, but new PVC is on %s",
					dsm.Ip, spec.DsmIp)
				return webapi.LunInfo{}, "", status.Errorf(codes.InvalidArgument, msg)
			}
			if spec.Location != "" && spec.Location != snapshotInfo.RootPath {
				msg := fmt.Sprintf("The source PVC and destination PVCs must be on the same location for cloning from snapshots. Source is on %s, but new PVC is on %s",
					snapshotInfo.RootPath, spec.Location)
				return webapi.LunInfo{}, "", status.Errorf(codes.InvalidArgument, msg)
			}

			lunInfo, err := service.createVolumeBySnapshot(dsm, spec, snapshotInfo)
			return lunInfo, dsm.Ip, err
		}
		return webapi.LunInfo{}, "", status.Errorf(codes.NotFound, fmt.Sprintf("No such snapshot id: %s", spec.SourceSnapshotId))
	} else if spec.DsmIp != "" {
		/* Create volume by specific dsm ip */
		dsm, err := service.GetDsm(spec.DsmIp)
		if err != nil {
			return webapi.LunInfo{}, "", status.Errorf(codes.Internal, fmt.Sprintf("%v", err))
		}
		lunInfo, err := service.createVolumeByDsm(dsm, spec)
		return lunInfo, dsm.Ip, err
	} else {
		/* Find appropriate dsm to create volume */
		for _, dsm := range service.dsms {
			lunInfo, err := service.createVolumeByDsm(dsm, spec)
			if err != nil {
				log.Errorf("[%s] Failed to create Volume: %v", dsm.Ip, err)
				continue
			}
			return lunInfo, dsm.Ip, nil
		}
		return webapi.LunInfo{}, "", status.Errorf(codes.Internal, fmt.Sprintf("Couldn't find any host available to create Volume"))
	}
}

func (service *DsmService) DeleteVolume(volumeId string) error {
	k8sVolume := service.GetVolume(volumeId)
	if k8sVolume == nil {
		log.Infof("Skip delete volume[%s] that is no exist", volumeId)
		return nil
	}

	lun, target := k8sVolume.Lun, k8sVolume.Target
	dsm, err := service.GetDsm(k8sVolume.DsmIp)

	if err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
	}

	if err := dsm.LunDelete(lun.Uuid); err != nil {
		return err
	}

	if len(target.MappedLuns) != 1 {
		log.Infof("Skip deletes target[%s] that was mapped with lun. DSM[%s]", target.Name, dsm.Ip)
		return nil
	}

	if err := dsm.TargetDelete(strconv.Itoa(target.TargetId)); err != nil {
		return err
	}

	return nil
}

func (service *DsmService) listVolumesByDsm(dsm *webapi.DSM, infos *[]*models.ListK8sVolumeRespSpec) {
	targetInfos, err := dsm.TargetList()
	if err != nil {
		log.Errorf("[%s] Failed to list targets: %v", dsm.Ip, err)
	}

	for _, target := range targetInfos {
		// TODO: use target.ConnectedSessions to filter targets
		for _, mapping := range target.MappedLuns {
			lun, err := dsm.LunGet(mapping.LunUuid)
			if err != nil {
				log.Errorf("[%s] Failed to get LUN(%s): %v", dsm.Ip, mapping.LunUuid, err)
			}

			if !strings.HasPrefix(lun.Name, models.LunPrefix) {
				continue
			}

			*infos = append(*infos, &models.ListK8sVolumeRespSpec{
				DsmIp:  dsm.Ip,
				Target: target,
				Lun:    lun,
			})
		}
	}
	return
}

func (service *DsmService) ListVolumes() []*models.ListK8sVolumeRespSpec {
	var infos []*models.ListK8sVolumeRespSpec

	for _, dsm := range service.dsms {
		service.listVolumesByDsm(dsm, &infos)
	}

	return infos
}

func (service *DsmService) GetVolume(lunUuid string) *models.ListK8sVolumeRespSpec {
	volumes := service.ListVolumes()

	for _, volume := range volumes {
		if volume.Lun.Uuid == lunUuid {
			return volume
		}
	}

	return nil
}

func (service *DsmService) ExpandLun(lunUuid string, newSize int64) error {
	k8sVolume := service.GetVolume(lunUuid);
	if k8sVolume == nil {
		return status.Errorf(codes.InvalidArgument, fmt.Sprintf("Can't find volume[%s].", lunUuid))
	}

	if int64(k8sVolume.Lun.Size) > newSize {
		return status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("Failed to expand volume[%s], because expand size[%d] bigger than before[%d].",
				lunUuid, newSize, k8sVolume.Lun.Size))
	}

	spec := webapi.LunUpdateSpec{
		Uuid: lunUuid,
		NewSize: uint64(newSize),
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
	}

	if err := dsm.LunUpdate(spec); err != nil {
		return status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("Failed to expand volume[%s]. err: %v", lunUuid, err))
	}

	return nil
}

func (service *DsmService) CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (string, error) {
	k8sVolume := service.GetVolume(spec.K8sVolumeId);
	if k8sVolume == nil {
		return "", status.Errorf(codes.NotFound, fmt.Sprintf("Can't find volume[%s].", spec.K8sVolumeId))
	}

	snapshotSpec := webapi.SnapshotCreateSpec{
		Name:    spec.SnapshotName,
		LunUuid: spec.K8sVolumeId,
		Description: spec.Description,
		TakenBy: spec.TakenBy,
		IsLocked: spec.IsLocked,
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, fmt.Sprintf("Failed to get dsm: %v", err))
	}

	snapshotUuid, err := dsm.SnapshotCreate(snapshotSpec)
	if err != nil {
		return "", err
	}

	return snapshotUuid, nil
}

func (service *DsmService) DeleteSnapshot(snapshotUuid string) error {
	for _, dsm := range service.dsms {
		_, err := dsm.SnapshotGet(snapshotUuid)
		if err != nil {
			continue
		}

		err = dsm.SnapshotDelete(snapshotUuid)
		if err != nil {
			return status.Errorf(codes.Internal, fmt.Sprintf("Failed to delete snapshot [%s]. err: %v", snapshotUuid, err))
		}

		return nil
	}

	return nil
}

func (service *DsmService) ListAllSnapshots() ([]webapi.SnapshotInfo, error) {
	var allInfos []webapi.SnapshotInfo

	for _, dsm := range service.dsms {
		infos, err := dsm.SnapshotList("")
		if err != nil {
			continue
		}

		allInfos = append(allInfos, infos...)
	}

	return allInfos, nil
}

func (service *DsmService) ListSnapshots(lunUuid string) ([]webapi.SnapshotInfo, error) {
	k8sVolume := service.GetVolume(lunUuid);
	if k8sVolume == nil {
		return []webapi.SnapshotInfo{}, nil // return empty when the volume does not exist
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Failed to get dsm: %v", err))
	}

	infos, err := dsm.SnapshotList(lunUuid)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			fmt.Sprintf("Failed to list snapshot on lun [%s]. err: %v", lunUuid, err))
	}

	return infos, nil
}

func (service *DsmService) GetSnapshot(snapshotUuid string) (webapi.SnapshotInfo, error) {
	for _, dsm := range service.dsms {
		info, err := dsm.SnapshotGet(snapshotUuid)
		if err != nil {
			continue
		}

		return info, nil
	}

	return webapi.SnapshotInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("No such snapshot uuid [%s]", snapshotUuid))
}
