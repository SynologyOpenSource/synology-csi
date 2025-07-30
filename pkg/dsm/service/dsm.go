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

func (service *DsmService) getFirstAvailableVolume(dsm *webapi.DSM, sizeInBytes int64, protocol string) (webapi.VolInfo, error) {
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

		if volInfo.FsType == models.FsTypeExt4 && (protocol == utils.ProtocolSmb || protocol == utils.ProtocolNfs) {
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

func (service *DsmService) createMappingTarget(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, lunUuid string) (webapi.TargetInfo, error) {
	dsmInfo, err := dsm.DsmInfoGet()

	if err != nil {
		return webapi.TargetInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s] info", dsm.Ip));
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
		Iqn:  genTargetIqn(),
	}

	log.Debugf("TargetCreate spec: %v", targetSpec)
	targetId, err := dsm.TargetCreate(targetSpec)

	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return webapi.TargetInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create target with spec: %v, err: %v", targetSpec, err))
	}

	targetInfo, err := dsm.TargetGet(targetSpec.Name)
	if err != nil {
		return webapi.TargetInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get target with spec: %v, err: %v", targetSpec, err))
	} else {
		targetId = strconv.Itoa(targetInfo.TargetId);
	}

	if spec.MultipleSession == true {
		if err := dsm.TargetSet(targetId, 0); err != nil {
			return webapi.TargetInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("Failed to set target [%s] max session, err: %v", spec.TargetName, err))
		}
	}

	if err := dsm.LunMapTarget([]string{targetId}, lunUuid); err != nil {
		return webapi.TargetInfo{}, status.Errorf(codes.Internal, fmt.Sprintf("Failed to map target [%s] to lun [%s], err: %v", spec.TargetName, lunUuid, err))
	}

	return targetInfo, nil
}

func (service *DsmService) createVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	// 1. Find a available location
	if spec.Location == "" {
		vol, err := service.getFirstAvailableVolume(dsm, spec.Size, spec.Protocol)
		if err != nil {
			return nil,
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to get available location, err: %v", err))
		}
		spec.Location = vol.Path
	}

	// 2. Check if location exists
	dsmVolInfo, err := dsm.VolumeGet(spec.Location)
	if err != nil {
		return nil,
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unable to find location %s", spec.Location))
	}

	lunType, err := getLunTypeByInputParams(spec.Type, spec.ThinProvisioning, dsmVolInfo.FsType)
	if err != nil {
		return nil,
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unknown volume fs type: %s, location: %s", dsmVolInfo.FsType, spec.Location))
	}

	devAttribs := []webapi.LunDevAttrib{}
	for key, value := range spec.DevAttribs {
		devAttribs = append(devAttribs, webapi.LunDevAttrib{
			DevAttrib: key,
			Enable:    utils.BoolToInt(value),
		})
	}

	// 3. Create LUN
	lunSpec := webapi.LunCreateSpec{
		Name:        spec.LunName,
		Description: spec.LunDescription,
		Location:    spec.Location,
		Size:        spec.Size,
		Type:        lunType,
		DevAttribs:  devAttribs,
	}

	log.Debugf("LunCreate spec: %v", lunSpec)
	_, err = dsm.LunCreate(lunSpec)

	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create LUN, err: %v", err))
	}

	// No matter lun existed or not, Get Lun by name
	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return nil,
			// discussion with log
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	// 4. Create Target and Map to Lun
	targetInfo, err := service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] CreateVolume Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid)

	return DsmLunToK8sVolume(dsm.Ip, lunInfo, targetInfo), nil
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

func (service *DsmService) createVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcSnapshot *models.K8sSnapshotRespSpec) (*models.K8sVolumeRespSpec, error) {
	if spec.Size != 0 && spec.Size != srcSnapshot.SizeInBytes {
		return nil, status.Errorf(codes.OutOfRange, "Requested lun size [%d] is not equal to snapshot size [%d]", spec.Size, srcSnapshot.SizeInBytes)
	}

	snapshotCloneSpec := webapi.SnapshotCloneSpec{
		Name:            spec.LunName,
		SrcLunUuid:      srcSnapshot.ParentUuid,
		SrcSnapshotUuid: srcSnapshot.Uuid,
	}

	if _, err := dsm.SnapshotClone(snapshotCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source snapshot ID: %s, err: %v", srcSnapshot.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.LunName); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	targetInfo, err := service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] createVolumeBySnapshot Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid)

	return DsmLunToK8sVolume(dsm.Ip, lunInfo, targetInfo), nil
}

func (service *DsmService) createVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcLunInfo webapi.LunInfo) (*models.K8sVolumeRespSpec, error) {
	if spec.Size != 0 && spec.Size != int64(srcLunInfo.Size) {
		return nil, status.Errorf(codes.OutOfRange, "Requested lun size [%d] is not equal to src lun size [%d]", spec.Size, srcLunInfo.Size)
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
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcLunInfo.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.LunName); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	lunInfo, err := dsm.LunGet(spec.LunName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed LUN with name: %s, err: %v", spec.LunName, err))
	}

	targetInfo, err := service.createMappingTarget(dsm, spec, lunInfo.Uuid)
	if err != nil {
		// FIXME need to delete lun and target
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map target, err: %v", err))
	}

	log.Debugf("[%s] createVolumeByVolume Successfully. VolumeId: %s", dsm.Ip, lunInfo.Uuid)

	return DsmLunToK8sVolume(dsm.Ip, lunInfo, targetInfo), nil
}

func DsmShareToK8sVolume(dsmIp string, info webapi.ShareInfo, protocol string) *models.K8sVolumeRespSpec {
	var source, baseDir string
	if protocol == utils.ProtocolSmb {
		source = "//" + dsmIp + "/" + info.Name
	} else if protocol == utils.ProtocolNfs {
		source = "//" + dsmIp + "/" + info.Name
		baseDir = info.VolPath + "/" + info.Name
	}

	return &models.K8sVolumeRespSpec{
		DsmIp: dsmIp,
		VolumeId: info.Uuid,
		SizeInBytes: utils.MBToBytes(info.QuotaValueInMB),
		Location: info.VolPath,
		Name: info.Name,
		Source: source,
		Protocol: protocol,
		Share: info,
		BaseDir: baseDir,
	}
}

func DsmLunToK8sVolume(dsmIp string, info webapi.LunInfo, targetInfo webapi.TargetInfo) *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{
		DsmIp: dsmIp,
		VolumeId: info.Uuid,
		SizeInBytes: int64(info.Size),
		Location: info.Location,
		Name: info.Name,
		Source: "",
		Protocol: utils.ProtocolIscsi,
		Lun: info,
		Target: targetInfo,
	}
}

func isNfsVersionSupport(dsm *webapi.DSM, nfsVersion string) bool {
	major := 0
	minor := 0

	info, err := dsm.NfsGet()
	if err != nil {
		return false
	}

	if nfsVersion == "" {
		major = info.SupportMajorVer
		minor = info.SupportMinorVer
	} else if nfsVersion == "3" {
		major = 3
	} else if nfsVersion == "4" || nfsVersion == "4.0" || nfsVersion == "4.1" {
		major = 4
		if nfsVersion == "4.1" {
			minor = 1
		}
	} else {
		log.Infof("Input nfsVersion = %s, not supported!", nfsVersion)
		return false
	}

	if major > info.SupportMajorVer || (major == info.SupportMajorVer && minor > info.SupportMinorVer) {
		log.Infof("Dsm NFS version not supported")
		return false
	}

	// enable the highest NFS version the DSM supports
	if err := dsm.NfsSet(true, (info.SupportMajorVer == 4), info.SupportMinorVer); err != nil {
		log.Errorf("[%s] Failed to enable nfs: %v\n", dsm.Ip, err)
		return false
	}

	return true
}


func (service *DsmService) CreateVolume(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	if spec.SourceVolumeId != "" {
		/* Create volume by exists volume (Clone) */
		k8sVolume := service.GetVolume(spec.SourceVolumeId)
		if k8sVolume == nil {
			return nil, status.Errorf(codes.NotFound, fmt.Sprintf("No such volume id: %s", spec.SourceVolumeId))
		}

		dsm, err := service.GetDsm(k8sVolume.DsmIp)
		if err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
		}

		if spec.Protocol == utils.ProtocolIscsi {
			return service.createVolumeByVolume(dsm, spec, k8sVolume.Lun)
		} else if spec.Protocol == utils.ProtocolSmb || spec.Protocol == utils.ProtocolNfs {
			return service.createSMBorNFSVolumeByVolume(dsm, spec, k8sVolume.Share)
		}
		return nil, status.Error(codes.InvalidArgument, "Unknown protocol")
	}

	if spec.SourceSnapshotId != "" {
		/* Create volume by snapshot */
		snapshot := service.GetSnapshotByUuid(spec.SourceSnapshotId)
		if snapshot == nil {
			return nil, status.Errorf(codes.NotFound, fmt.Sprintf("No such snapshot id: %s", spec.SourceSnapshotId))
		}

		// found source by snapshot id, check allowable
		if spec.DsmIp != "" && spec.DsmIp != snapshot.DsmIp {
			msg := fmt.Sprintf("The source PVC and destination PVCs must be on the same DSM for cloning from snapshots. Source is on %s, but new PVC is on %s",
				snapshot.DsmIp, spec.DsmIp)
			return nil, status.Errorf(codes.InvalidArgument, msg)
		}
		if spec.Location != "" && spec.Location != snapshot.RootPath {
			msg := fmt.Sprintf("The source PVC and destination PVCs must be on the same location for cloning from snapshots. Source is on %s, but new PVC is on %s",
				snapshot.RootPath, spec.Location)
			return nil, status.Errorf(codes.InvalidArgument, msg)
		}

		log.Debugf("The source PVC protocol [%s] and the destination PVC protocol [%s]", snapshot.Protocol, spec.Protocol)
		if (spec.Protocol == utils.ProtocolIscsi || snapshot.Protocol == utils.ProtocolIscsi) &&
			spec.Protocol != snapshot.Protocol {
			msg := fmt.Sprintf("The source PVC and destination PVCs shouldn't have different protocols. Source is %s, but new PVC is %s",
				snapshot.Protocol, spec.Protocol)
			return nil, status.Errorf(codes.InvalidArgument, msg)
		}

		dsm, err := service.GetDsm(snapshot.DsmIp)
		if err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", snapshot.DsmIp))
		}

		if spec.Protocol == utils.ProtocolIscsi {
			return service.createVolumeBySnapshot(dsm, spec, snapshot)
		} else if spec.Protocol == utils.ProtocolSmb || spec.Protocol == utils.ProtocolNfs {
			return service.createSMBorNFSVolumeBySnapshot(dsm, spec, snapshot)
		}
		return nil, status.Error(codes.InvalidArgument, "Unknown protocol")
	}

	/* Find appropriate dsm to create volume */
	for _, dsm := range service.dsms {
		if spec.DsmIp != "" && spec.DsmIp != dsm.Ip {
			continue
		}

		var k8sVolume *models.K8sVolumeRespSpec
		var err error
		if spec.Protocol == utils.ProtocolIscsi {
			k8sVolume, err = service.createVolumeByDsm(dsm, spec)
		} else if spec.Protocol == utils.ProtocolSmb {
			k8sVolume, err = service.createSMBorNFSVolumeByDsm(dsm, spec)
		} else if spec.Protocol == utils.ProtocolNfs {
			if !isNfsVersionSupport(dsm, spec.NfsVersion) {
				continue
			}
			k8sVolume, err = service.createSMBorNFSVolumeByDsm(dsm, spec)
		}

		if err != nil {
			log.Errorf("[%s] Failed to create Volume: %v", dsm.Ip, err)
			continue
		}

		return k8sVolume, nil
	}

	return nil, status.Errorf(codes.Internal, fmt.Sprintf("Couldn't find any host available to create Volume"))
}

func (service *DsmService) DeleteVolume(volId string) error {
	k8sVolume := service.GetVolume(volId)
	if k8sVolume == nil {
		log.Infof("Skip delete volume[%s] that is no exist", volId)
		return nil
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
	}

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		if err := dsm.ShareDelete(k8sVolume.Share.Name); err != nil {
			log.Errorf("[%s] Failed to delete Share(%s): %v", dsm.Ip, k8sVolume.Share.Name, err)
			return err
		}
	} else {
		lun, target := k8sVolume.Lun, k8sVolume.Target

		if err := dsm.LunDelete(lun.Uuid); err != nil {
			if  _, err := dsm.LunGet(lun.Uuid); err != nil && errors.Is(err, utils.NoSuchLunError("")) {
				return nil
			}
			log.Errorf("[%s] Failed to delete LUN(%s): %v", dsm.Ip, lun.Uuid, err)
			return err
		}

		if len(target.MappedLuns) != 1 {
			log.Infof("Skip deletes target[%s] that was mapped with lun. DSM[%s]", target.Name, dsm.Ip)
			return nil
		}

		if err := dsm.TargetDelete(strconv.Itoa(target.TargetId)); err != nil {
			if  _, err := dsm.TargetGet(strconv.Itoa(target.TargetId)); err != nil {
				return nil
			}
			log.Errorf("[%s] Failed to delete target(%d): %v", dsm.Ip, target.TargetId, err)
			return err
		}
	}

	return nil
}

func (service *DsmService) listISCSIVolumes(dsmIp string) (infos []*models.K8sVolumeRespSpec) {
	for _, dsm := range service.dsms {
		if dsmIp != "" && dsmIp != dsm.Ip {
			continue
		}

		targetInfos, err := dsm.TargetList()
		if err != nil {
			log.Errorf("[%s] Failed to list targets: %v", dsm.Ip, err)
			continue
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

				// FIXME: filter same LUN mapping to two target
				infos = append(infos, DsmLunToK8sVolume(dsm.Ip, lun, target))
			}
		}
	}
	return infos
}

func (service *DsmService) ListVolumes() (infos []*models.K8sVolumeRespSpec) {
	infos = append(infos, service.listISCSIVolumes("")...)
	infos = append(infos, service.listSMBorNFSVolumes("")...)

	return infos
}

func (service *DsmService) GetVolume(volId string) *models.K8sVolumeRespSpec {
	volumes := service.ListVolumes()
	for _, volume := range volumes {
		if volume.VolumeId == volId {
			return volume
		}
	}

	return nil
}

func (service *DsmService) GetVolumeByName(volName string) *models.K8sVolumeRespSpec {
	volumes := service.ListVolumes()
	for _, volume := range volumes {
		if volume.Name == models.GenLunName(volName) ||
			volume.Name == models.GenShareName(volName) {
			return volume
		}
	}

	return nil
}

func (service *DsmService) GetSnapshotByName(snapshotName string) *models.K8sSnapshotRespSpec {
	snaps := service.ListAllSnapshots()
	for _, snap := range snaps {
		if snap.Name == snapshotName {
			return snap
		}
	}
	return nil
}

func (service *DsmService) ExpandVolume(volId string, newSize int64) (*models.K8sVolumeRespSpec, error) {
	k8sVolume := service.GetVolume(volId);
	if k8sVolume == nil {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Can't find volume[%s].", volId))
	}

	if k8sVolume.SizeInBytes > newSize {
		return nil, status.Errorf(codes.InvalidArgument,
			fmt.Sprintf("Failed to expand volume[%s], because expand size[%d] smaller than before[%d].",
				volId, newSize, k8sVolume.SizeInBytes))
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get DSM[%s]", k8sVolume.DsmIp))
	}

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		newSizeInMB := utils.BytesToMBCeil(newSize) // round up to MB
		if err := dsm.SetShareQuota(k8sVolume.Share, newSizeInMB); err != nil {
			log.Errorf("[%s] Failed to set quota [%d (MB)] to Share [%s]: %v",
				dsm.Ip, newSizeInMB, k8sVolume.Share.Name, err)
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to expand volume[%s]. err: %v", volId, err))
		}
		// convert MB to bytes, may be diff from the input newSize
		k8sVolume.SizeInBytes = utils.MBToBytes(newSizeInMB)
	} else {
		spec := webapi.LunUpdateSpec{
			Uuid: volId,
			NewSize: uint64(newSize),
		}
		if err := dsm.LunUpdate(spec); err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to expand volume[%s]. err: %v", volId, err))
		}
		k8sVolume.SizeInBytes = newSize
	}

	return k8sVolume, nil
}

func (service *DsmService) CreateSnapshot(spec *models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error) {
	srcVolId := spec.K8sVolumeId

	k8sVolume := service.GetVolume(srcVolId);
	if k8sVolume == nil {
		return nil, status.Errorf(codes.NotFound, fmt.Sprintf("Can't find volume[%s].", srcVolId))
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Failed to get dsm: %v", err))
	}

	if k8sVolume.Protocol == utils.ProtocolIscsi {
		snapshotSpec := webapi.SnapshotCreateSpec{
			Name:    spec.SnapshotName,
			LunUuid: srcVolId,
			Description: spec.Description,
			TakenBy: spec.TakenBy,
			IsLocked: spec.IsLocked,
		}

		snapshotUuid, err := dsm.SnapshotCreate(snapshotSpec)
		if err != nil {
			if err == utils.OutOfFreeSpaceError("") || err == utils.SnapshotReachMaxCountError("") {
				return nil,status.Errorf(codes.ResourceExhausted, fmt.Sprintf("Failed to SnapshotCreate(%s), err: %v", srcVolId, err))
			}
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to SnapshotCreate(%s), err: %v", srcVolId, err))
		}

		if snapshot := service.getISCSISnapshot(snapshotUuid); snapshot != nil {
			return snapshot, nil
		}

		return nil, status.Errorf(codes.NotFound, fmt.Sprintf("Failed to get iscsi snapshot (%s). Not found", snapshotUuid))
	} else if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		snapshotSpec := webapi.ShareSnapshotCreateSpec{
			ShareName: k8sVolume.Share.Name,
			Desc:      models.ShareSnapshotDescPrefix + spec.SnapshotName, // limitations: don't change the desc by DSM
			IsLocked:  spec.IsLocked,
		}

		snapshotTime, err := dsm.ShareSnapshotCreate(snapshotSpec)
		if err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to ShareSnapshotCreate(%s), err: %v", srcVolId, err))
		}

		snapshots := service.listSMBorNFSSnapshotsByDsm(dsm)
		for _, snapshot := range snapshots {
			if snapshot.Time == snapshotTime && snapshot.ParentUuid == srcVolId {
				return snapshot, nil
			}
		}
		return nil, status.Errorf(codes.NotFound, fmt.Sprintf("Failed to get %s snapshot (%s, %s). Not found",
			k8sVolume.Protocol, snapshotTime, srcVolId))
	}

	return nil, status.Error(codes.InvalidArgument, "Unsupported volume protocol")
}

func (service *DsmService) GetSnapshotByUuid(snapshotUuid string) *models.K8sSnapshotRespSpec {
	snaps := service.ListAllSnapshots()
	for _, snap := range snaps {
		if snap.Uuid == snapshotUuid {
			return snap
		}
	}
	return nil
}

func (service *DsmService) DeleteSnapshot(snapshotUuid string) error {
	snapshot := service.GetSnapshotByUuid(snapshotUuid)
	if snapshot == nil {
		return nil
	}
	dsm, err := service.GetDsm(snapshot.DsmIp)
	if err != nil {
		return err
	}

	if snapshot.Protocol == utils.ProtocolSmb || snapshot.Protocol == utils.ProtocolNfs {
		if err := dsm.ShareSnapshotDelete(snapshot.Time, snapshot.ParentName); err != nil {
			if snapshot := service.getSMBorNFSSnapshot(snapshotUuid); snapshot == nil { // idempotency
				return nil
			}

			log.Errorf("Failed to delete Share snapshot [%s]. err: %v", snapshotUuid, err)
			return err
		}
	} else if snapshot.Protocol == utils.ProtocolIscsi {
		if err := dsm.SnapshotDelete(snapshotUuid); err != nil {
			if _, err := dsm.SnapshotGet(snapshotUuid); err != nil { // idempotency
				return nil
			}

			log.Errorf("Failed to delete LUN snapshot [%s]. err: %v", snapshotUuid, err)
			return err
		}
	}

	return nil
}

func (service *DsmService) listISCSISnapshotsByDsm(dsm *webapi.DSM) (infos []*models.K8sSnapshotRespSpec) {
	volumes := service.listISCSIVolumes(dsm.Ip)
	for _, volume := range volumes {
		lunInfo := volume.Lun
		lunSnaps, err := dsm.SnapshotList(lunInfo.Uuid)
		if err != nil {
			log.Errorf("[%s] Failed to list LUN[%s] snapshots: %v", dsm.Ip, lunInfo.Uuid, err)
			continue
		}

		for _, info := range lunSnaps {
			infos = append(infos, DsmLunSnapshotToK8sSnapshot(dsm.Ip, info, lunInfo))
		}
	}
	return
}

func (service *DsmService) ListAllSnapshots() []*models.K8sSnapshotRespSpec {
	var allInfos []*models.K8sSnapshotRespSpec

	for _, dsm := range service.dsms {
		allInfos = append(allInfos, service.listISCSISnapshotsByDsm(dsm)...)
		allInfos = append(allInfos, service.listSMBorNFSSnapshotsByDsm(dsm)...)
	}

	return allInfos
}

func (service *DsmService) ListSnapshots(volId string) []*models.K8sSnapshotRespSpec {
	var allInfos []*models.K8sSnapshotRespSpec

	k8sVolume := service.GetVolume(volId);
	if k8sVolume == nil {
		return nil
	}

	dsm, err := service.GetDsm(k8sVolume.DsmIp)
	if err != nil {
		log.Errorf("Failed to get DSM[%s]", k8sVolume.DsmIp)
		return nil
	}

	if k8sVolume.Protocol == utils.ProtocolIscsi {
		infos, err := dsm.SnapshotList(volId)
		if err != nil {
			log.Errorf("Failed to SnapshotList[%s]", volId)
			return nil
		}
		for _, info := range infos {
			allInfos = append(allInfos, DsmLunSnapshotToK8sSnapshot(dsm.Ip, info, k8sVolume.Lun))
		}
	} else {
		infos, err := dsm.ShareSnapshotList(k8sVolume.Share.Name)
		if err != nil {
			log.Errorf("Failed to ShareSnapshotList[%s]", k8sVolume.Share.Name)
			return nil
		}
		for _, info := range infos {
			allInfos = append(allInfos, DsmShareSnapshotToK8sSnapshot(dsm.Ip, info, k8sVolume.Share, k8sVolume.Protocol))
		}
	}

	return allInfos
}

func DsmShareSnapshotToK8sSnapshot(dsmIp string, info webapi.ShareSnapshotInfo, shareInfo webapi.ShareInfo, protocol string) *models.K8sSnapshotRespSpec {
	return &models.K8sSnapshotRespSpec{
		DsmIp: dsmIp,
		Name: strings.ReplaceAll(info.Desc, models.ShareSnapshotDescPrefix, ""), // snapshot-XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX
		Uuid: info.Uuid,
		ParentName: shareInfo.Name,
		ParentUuid: shareInfo.Uuid,
		Status: "Healthy", // share snapshot always Healthy
		SizeInBytes: utils.MBToBytes(shareInfo.QuotaValueInMB), // unable to get snapshot quota, return parent quota instead
		CreateTime: GMTToUnixSecond(info.Time),
		Time: info.Time,
		RootPath: shareInfo.VolPath,
		Protocol: protocol,
	}
}

func DsmLunSnapshotToK8sSnapshot(dsmIp string, info webapi.SnapshotInfo, lunInfo webapi.LunInfo) *models.K8sSnapshotRespSpec {
	return &models.K8sSnapshotRespSpec{
		DsmIp: dsmIp,
		Name: info.Name,
		Uuid: info.Uuid,
		ParentName: lunInfo.Name, // it can be empty for iscsi
		ParentUuid: info.ParentUuid,
		Status: info.Status,
		SizeInBytes: info.TotalSize,
		CreateTime: info.CreateTime,
		Time: "",
		RootPath: info.RootPath,
		Protocol: utils.ProtocolIscsi,
	}
}

func (service *DsmService) getISCSISnapshot(snapshotUuid string) *models.K8sSnapshotRespSpec {
	for _, dsm := range service.dsms {
		snapshots := service.listISCSISnapshotsByDsm(dsm)
		for _, snap := range snapshots {
			if snap.Uuid == snapshotUuid {
				return snap
			}
		}
	}

	return nil
}
