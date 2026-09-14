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

func (service *DsmService) createMappingSubsystem(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, namespaceUuid string) (subsystem *webapi.SubsystemInfo, retErr error) {
	// The subsystem is this function's to clean up; the caller owns the namespace.
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

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

	if err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create subsystem with spec: %v, err: %v", subsystemSpec, err))
		}
		// Left by an earlier attempt, so not ours to remove.
	} else {
		createdUuid := subsystemUuid
		rollback.add(fmt.Sprintf("subsystem(%s)", subsystemSpec.Name), func() error { return dsm.SubsystemDelete(createdUuid) })
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

func (service *DsmService) createNVMeVolumeBySnapshot(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcSnapshot *models.K8sSnapshotRespSpec) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

	if err := checkRestoreSize(spec.Size, srcSnapshot.SizeInBytes, "namespace"); err != nil {
		return nil, err
	}

	if !dsm.SupportNvmeof { // should not enter here
		return nil, status.Errorf(codes.Internal, "[BUG] [%s] volume protocol = nvme, but DSM doesn't support nmveof", dsm.Ip)
	}

	snapshotCloneSpec := webapi.SnapshotCloneSpec{
		Name:            spec.BackendName,
		SrcSnapshotUuid: srcSnapshot.Uuid,
	}

	if _, err := dsm.NamespaceSnapshotClone(snapshotCloneSpec); err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil,
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source snapshot ID: %s, err: %v", srcSnapshot.Uuid, err))
		}
	} else {
		backendName := spec.BackendName
		rollback.add(fmt.Sprintf("namespace(%s)", backendName), func() error {
			ns, err := dsm.NamespaceGet(backendName)
			if err != nil {
				return err
			}
			return dsm.NamespaceDelete(ns.Uuid)
		})
	}

	if err := waitCloneFinished(dsm, spec.BackendName, spec.Protocol); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	namespaceInfo, err := dsm.NamespaceGet(spec.BackendName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed nvme namespace with name: %s, err: %v", spec.BackendName, err))
	}

	// A clone comes back the size of its source; grow it if the PVC asked for
	// more. Failing here rolls the namespace back rather than returning a
	// volume smaller than promised.
	if newSize := needsExpandTo(spec.Size, int64(namespaceInfo.Size)); newSize > 0 {
		if err := dsm.NamespaceSet(webapi.NamespaceSetSpec{Uuid: namespaceInfo.Uuid, NewSize: uint64(newSize)}); err != nil {
			return nil, status.Errorf(codes.Internal,
				"Failed to expand the restored namespace[%s] from [%d] to [%d], err: %v",
				namespaceInfo.Uuid, namespaceInfo.Size, newSize, err)
		}
		namespaceInfo.Size = uint64(newSize)
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

func (service *DsmService) createNVMeVolumeByVolume(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec, srcNamespaceInfo webapi.NamespaceInfo) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

	if err := checkRestoreSize(spec.Size, int64(srcNamespaceInfo.Size), "namespace"); err != nil {
		return nil, err
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

	if _, err := dsm.NamespaceClone(namespaceCloneSpec); err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil,
				status.Errorf(codes.Internal, fmt.Sprintf("Failed to create volume with source volume ID: %s, err: %v", srcNamespaceInfo.Uuid, err))
		}
	} else {
		backendName := spec.BackendName
		rollback.add(fmt.Sprintf("namespace(%s)", backendName), func() error {
			ns, err := dsm.NamespaceGet(backendName)
			if err != nil {
				return err
			}
			return dsm.NamespaceDelete(ns.Uuid)
		})
	}

	if err := waitCloneFinished(dsm, spec.BackendName, spec.Protocol); err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	namespaceInfo, err := dsm.NamespaceGet(spec.BackendName)
	if err != nil {
		return nil,
			status.Errorf(codes.Internal, fmt.Sprintf("Failed to get existed nvme namespace with name: [%s], err: %v", spec.BackendName, err))
	}

	// A clone comes back the size of its source; grow it if the PVC asked for
	// more. Failing here rolls the namespace back rather than returning a
	// volume smaller than promised.
	if newSize := needsExpandTo(spec.Size, int64(namespaceInfo.Size)); newSize > 0 {
		if err := dsm.NamespaceSet(webapi.NamespaceSetSpec{Uuid: namespaceInfo.Uuid, NewSize: uint64(newSize)}); err != nil {
			return nil, status.Errorf(codes.Internal,
				"Failed to expand the restored namespace[%s] from [%d] to [%d], err: %v",
				namespaceInfo.Uuid, namespaceInfo.Size, newSize, err)
		}
		namespaceInfo.Size = uint64(newSize)
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

func (service *DsmService) createNVMeVolumeByDsm(dsm *webapi.DSM, spec *models.CreateK8sVolumeSpec) (volume *models.K8sVolumeRespSpec, retErr error) {
	var rollback createRollback
	defer func() {
		if retErr != nil {
			rollback.run(dsm.Ip)
		}
	}()

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
	if err != nil {
		if !errors.Is(err, utils.AlreadyExistError("")) {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to create namespace, err: %v", err))
		}
	} else {
		backendName := spec.BackendName
		rollback.add(fmt.Sprintf("namespace(%s)", backendName), func() error {
			ns, err := dsm.NamespaceGet(backendName)
			if err != nil {
				return err
			}
			return dsm.NamespaceDelete(ns.Uuid)
		})
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

// listNVMeVolumes returns the namespaces it could enumerate. A non-nil error
// means the list is incomplete, so absence from it does not prove a volume is
// gone.
func (service *DsmService) listNVMeVolumes(dsmIp string) (infos []*models.K8sVolumeRespSpec, listErr error) {
	for _, dsm := range service.dsms {
		if dsmIp != "" && dsmIp != dsm.Ip {
			continue
		}
		if !dsm.SupportNvmeof {
			continue
		}

		namespaceInfos, err := dsm.NamespaceList()
		if err != nil {
			log.Errorf("[%s] Failed to list namespaces: %v", dsm.Ip, err)
			listErr = errors.Join(listErr, fmt.Errorf("DSM[%s] failed to list namespaces: %w", dsm.Ip, err))
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
				// SubsystemGet returns a nil pointer on failure, so this must
				// not fall through to the dereference below.
				log.Errorf("[%s] Failed to get Subsystem(%s): %v", dsm.Ip, ns.SubsystemUuid, err)
				listErr = errors.Join(listErr, fmt.Errorf("DSM[%s] failed to get subsystem(%s): %w", dsm.Ip, ns.SubsystemUuid, err))
				continue
			}

			infos = append(infos, DsmNamespaceToK8sVolume(dsm.Ip, ns, *subsystemInfo))
		}
	}
	return infos, listErr
}

func (service *DsmService) listNVMeSnapshotsByDsm(dsm *webapi.DSM) (infos []*models.K8sSnapshotRespSpec) {
	if !dsm.SupportNvmeof {
		return infos
	}

	volumes, err := service.listNVMeVolumes(dsm.Ip)
	if err != nil {
		log.Errorf("[%s] Namespace list was incomplete while listing snapshots: %v", dsm.Ip, err)
	}
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
