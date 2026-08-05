/*
 * Copyright 2025 Synology Inc.
 */

package service

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func (service *DsmService) createMappingSubsystem(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, namespaceUuid string) (*webapi.SubsystemInfo, error) {
	genNqn := func() string {
		nqn := models.NqnPrefix + fmt.Sprintf("%s.%s", dsm.Hostname, spec.K8sVolumeName)
		nqn = strings.ReplaceAll(nqn, "_", "-")
		nqn = strings.ReplaceAll(nqn, "+", "p")

		if len(nqn) > models.MaxNqnLen {
			return nqn[:models.MaxNqnLen]
		}
		return nqn
	}
	subsystemSpec := webapi.SubsystemCreateSpec{
		Name: fmt.Sprintf("%s-%s", models.SubsystemPrefix, spec.K8sVolumeName),
		Nqn:  genNqn(),
	}

	log.Debugf("SubsystemCreate spec: %v", subsystemSpec)
	subsystemUuid, err := dsm.SubsystemCreate(subsystemSpec)

	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create subsystem with spec: %v, err: %v", subsystemSpec, err))
	}

	subsystemInfo, err := dsm.SubsystemGet(subsystemUuid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get subsystem with spec: %v, err: %v", subsystemSpec, err))
	}

	if err := dsm.SubsystemSetNamespaces(subsystemUuid, []string{namespaceUuid}); err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to map namespace [%s] to subsystem [%s], err: %v", spec.BackendName, subsystemInfo.Name, err))
	}

	return subsystemInfo, nil
}

func (service *DsmService) createNVMeVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcSnapshot *models.K8sSnapshotRespSpec) (*models.K8sVolumeRespSpec, error) {
	if spec.Size != 0 && spec.Size != srcSnapshot.SizeInBytes {
		return nil, status.Errorf(codes.OutOfRange, "Requested namespace size [%d] is not equal to snapshot size [%d]", spec.Size, srcSnapshot.SizeInBytes)
	}

	if !dsm.SupportNvmeof { // should not enter here
		return nil, status.Errorf(codes.Internal, "[BUG] [%s] volume protocol = nvme, but DSM doesn't support nmveof", dsm.Ip)
	}

	snapshotCloneSpec := webapi.SnapshotCloneSpec{
		Name:            spec.BackendName,
		SrcSnapshotUuid: srcSnapshot.Uuid,
	}

	if _, err := dsm.NamespaceSnapshotClone(snapshotCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source snapshot ID: %s, err: %v", srcSnapshot.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.BackendName, spec.Protocol); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	namespaceInfo, err := dsm.NamespaceGet(spec.BackendName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed nvme namespace with name: %s, err: %v", spec.BackendName, err))
	}

	subsystemInfo, err := service.createMappingSubsystem(dsm, spec, namespaceInfo.Uuid)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map subsystem, err: %v", err))
	}
	namespaceInfo.SubsystemUuid = subsystemInfo.Uuid

	log.Debugf("[%s] createVolumeBySnapshot Successfully. VolumeId: %s", dsm.Ip, namespaceInfo.Uuid)

	return DsmNamespaceToK8sVolume(dsm.Ip, *namespaceInfo, *subsystemInfo), nil
}

func (service *DsmService) createNVMeVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcNamespaceInfo webapi.NamespaceInfo) (*models.K8sVolumeRespSpec, error) {
	if spec.Size != 0 && spec.Size != int64(srcNamespaceInfo.Size) {
		return nil, status.Errorf(codes.OutOfRange, "Requested namespace size [%d] is not equal to src namespace size [%d]", spec.Size, srcNamespaceInfo.Size)
	}

	if !dsm.SupportNvmeof { // should not enter here
		return nil, status.Errorf(codes.Internal, "[BUG] [%s] volume protocol = nvme, but DSM doesn't support nmveof", dsm.Ip)
	}

	if spec.Location == "" {
		spec.Location = srcNamespaceInfo.Location
	}

	namespaceCloneSpec := webapi.NamespaceCloneSpec{
		Name:         spec.BackendName,
		SrcUuid:      srcNamespaceInfo.Uuid,
		Location:     spec.Location,
	}

	if _, err := dsm.NamespaceClone(namespaceCloneSpec); err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcNamespaceInfo.Uuid, err))
	}

	if err := waitCloneFinished(dsm, spec.BackendName, spec.Protocol); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	namespaceInfo, err := dsm.NamespaceGet(spec.BackendName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed nvme namespace with name: [%s], err: %v", spec.BackendName, err))
	}

	subsystemInfo, err := service.createMappingSubsystem(dsm, spec, namespaceInfo.Uuid)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map subsystem, err: %v", err))
	}
	namespaceInfo.SubsystemUuid = subsystemInfo.Uuid

	log.Debugf("[%s] createNVMeVolumeByVolume Successfully. VolumeId: %s", dsm.Ip, namespaceInfo.Uuid)

	return DsmNamespaceToK8sVolume(dsm.Ip, *namespaceInfo, *subsystemInfo), nil
}

func (service *DsmService) createNVMeVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
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
	_, err := dsm.VolumeGet(spec.Location)
	if err != nil {
		return nil,
			status.Errorf(codes.InvalidArgument, fmt.Sprintf("Unable to find location %s", spec.Location))
	}

	// 3. Create Namespace
	namespaceSpec := webapi.NamespaceCreateSpec{
		Name:             spec.BackendName,
		Description:      spec.Description,
		Location:         spec.Location,
		Size:             uint64(spec.Size),
		ThinProvisioning: spec.ThinProvisioning,
		Reclaim:          spec.Reclaim,
	}

	log.Debugf("NamespaceCreate spec: %v", namespaceSpec)
	_, err = dsm.NamespaceCreate(namespaceSpec)
	if err != nil && !errors.Is(err, utils.AlreadyExistError("")) {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create namespace, err: %v", err))
	}

	namespaceInfo, err := dsm.NamespaceGet(spec.BackendName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed nvme namespace with name: %s, err: %v", spec.BackendName, err))
	}

	// 4. Create Subsystem and Map to Namespace
	subsystemInfo, err := service.createMappingSubsystem(dsm, spec, namespaceInfo.Uuid)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to create and map subsystem, err: %v", err))
	}
	namespaceInfo.SubsystemUuid = subsystemInfo.Uuid

	log.Debugf("[%s] createNVMeVolumeByDsm Successfully. VolumeId: %s", dsm.Ip, namespaceInfo.Uuid)

	return DsmNamespaceToK8sVolume(dsm.Ip, *namespaceInfo, *subsystemInfo), nil
}

func (service *DsmService) listNVMeVolumes(dsmIp string) (infos []*models.K8sVolumeRespSpec) {
	for _, dsm := range service.listDsms() {
		if dsmIp != "" && dsmIp != dsm.Ip {
			continue
		}
		if !dsm.SupportNvmeof {
			continue
		}

		namespaceInfos, err := dsm.NamespaceList()
		if err != nil {
			log.Errorf("[%s] Failed to list namespaces: %v", dsm.Ip, err)
			continue
		}

		for _, ns := range namespaceInfos {
			if !strings.HasPrefix(ns.Name, models.DevicePrefix) {
				continue
			}
			if ns.SubsystemUuid == "" {
				continue
			}

			subsystemInfo, err := dsm.SubsystemGet(ns.SubsystemUuid)
			if err != nil {
				log.Errorf("[%s] Failed to get Subsystem(%s): %v", dsm.Ip, ns.SubsystemUuid, err)
			}

			infos = append(infos, DsmNamespaceToK8sVolume(dsm.Ip, ns, *subsystemInfo))
		}
	}
	return infos
}

func (service *DsmService) listNVMeSnapshotsByDsm(dsm *webapi.DSM) (infos []*models.K8sSnapshotRespSpec) {
	if !dsm.SupportNvmeof {
		return infos
	}

	volumes := service.listNVMeVolumes(dsm.Ip)
	for _, volume := range volumes {
		nsInfo := volume.Namespace
		nsSnaps, err := dsm.NamespaceSnapshotList(nsInfo.Uuid)
		if err != nil {
			log.Errorf("[%s] Failed to list namespace snapshots: %v", dsm.Ip, err)
			continue
		}
		for _, info := range nsSnaps {
			infos = append(infos, DsmSanSnapshotToK8sSnapshot(dsm.Ip, info, utils.ProtocolNvme))
		}
	}
	return infos
}
