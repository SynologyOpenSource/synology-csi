/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type controllerServer struct {
	Driver     *Driver
	dsmService interfaces.IDsmService
	Initiator  *initiatorDriver
}

func getSizeByCapacityRange(capRange *csi.CapacityRange) (int64, error) {
	if capRange == nil {
		return 1 * utils.UNIT_GB, nil
	}

	minSize := capRange.GetRequiredBytes()
	maxSize := capRange.GetLimitBytes()
	if 0 < maxSize && maxSize < minSize {
		return 0, status.Error(codes.InvalidArgument, "Invalid input: limitBytes is smaller than requiredBytes")
	}
	if minSize < utils.UNIT_GB {
		return 0, status.Error(codes.InvalidArgument, "Invalid input: required bytes is smaller than 1G")
	}

	return int64(minSize), nil
}

func (cs *controllerServer) isVolumeAccessModeSupport(mode csi.VolumeCapability_AccessMode_Mode) bool {
	for _, accessMode := range cs.Driver.getVolumeCapabilityAccessModes() {
		if mode == accessMode.Mode {
			return true
		}
	}

	return false
}

func parseNfsVesrion(ops []string) string {
	for _, op := range ops {
		if strings.HasPrefix(op, "nfsvers") {
			kvpair := strings.Split(op, "=")
			if len(kvpair) == 2 {
				return kvpair[1]
			}
		}
	}
	return ""
}

func parseDevAttribs(params map[string]string) (map[string]bool, error) {
	attribFlags := make(map[string]bool)

	if params["enableSpaceReclamation"] != "" {
		attribFlags["emulate_tpu"] = utils.StringToBoolean(params["enableSpaceReclamation"])
	}
	if params["enableFuaSyncCache"] != "" {
		enabled := utils.StringToBoolean(params["enableFuaSyncCache"])
		attribFlags["emulate_fua_write"] = enabled
		attribFlags["emulate_sync_cache"] = enabled
	}

	return attribFlags, nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	sizeInByte, err := getSizeByCapacityRange(req.GetCapacityRange())
	volName, volCap := req.GetName(), req.GetVolumeCapabilities()
	volContentSrc := req.GetVolumeContentSource()

	var srcSnapshotId string = ""
	var srcVolumeId string = ""
	var multiSession bool = false

	if err != nil {
		return nil, err
	}

	if volName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "No name is provided")
	}

	if volCap == nil {
		return nil, status.Errorf(codes.InvalidArgument, "No volume capabilities are provided")
	}
	var mountOptions []string
	for _, cap := range volCap {
		accessMode := cap.GetAccessMode().GetMode()

		if !cs.isVolumeAccessModeSupport(accessMode) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid volume capability access mode")
		}

		if accessMode == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			multiSession = false
		} else if accessMode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
			multiSession = true
		}

		if mount := cap.GetMount(); mount != nil {
			mountOptions = mount.GetMountFlags()
		}
	}

	if volContentSrc != nil {
		if srcSnapshot := volContentSrc.GetSnapshot(); srcSnapshot != nil {
			srcSnapshotId = srcSnapshot.SnapshotId
		} else if srcVolume := volContentSrc.GetVolume(); srcVolume != nil {
			srcVolumeId = srcVolume.VolumeId
		} else {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid volume content source")
		}
	}

	params := req.GetParameters()

	isThin := true
	if params["thin_provisioning"] != "" {
		isThin = utils.StringToBoolean(params["thin_provisioning"])
	}

	protocol := strings.ToLower(params["protocol"])
	if protocol == "" {
		protocol = utils.ProtocolDefault
	} else if !isProtocolSupport(protocol) {
		return nil, status.Error(codes.InvalidArgument, "Unsupported volume protocol")
	}

	// not needed during CreateVolume method
	// used only in NodeStageVolume through VolumeContext
	formatOptions := params["formatOptions"]
	mountPermissions := params["mountPermissions"]
	// check mountPermissions valid
	if mountPermissions != "" {
		if _, err := strconv.ParseUint(mountPermissions, 8, 32); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid mountPermissions %s in storage class", mountPermissions))
		}
	}

	devAttribs, err := parseDevAttribs(params)
	if err != nil {
		return nil, err
	}
	if enabled, exists := devAttribs["emulate_tpu"]; exists && enabled && !isThin {
		return nil, status.Error(codes.InvalidArgument, "Invalid provisioning type: space reclamation only supported for thin LUNs")
	}

	lunDescription := ""
	if _, ok := params["csi.storage.k8s.io/pvc/name"]; ok {
		// if the /pvc/name is present, the namespace is present too
		// as these parameters are reserved by external-provisioner
		pvcNamespace := params["csi.storage.k8s.io/pvc/namespace"]
		pvcName := params["csi.storage.k8s.io/pvc/name"]
		lunDescription = pvcNamespace + "/" + pvcName
	}

	nfsVer := parseNfsVesrion(mountOptions)
	if nfsVer != "" && !isNfsVersionAllowed(nfsVer) {
		return nil, status.Errorf(codes.InvalidArgument, "Unsupported nfsvers: %s", nfsVer)
	}

	spec := &models.CreateK8sVolumeSpec{
		DsmIp:            params["dsm"],
		K8sVolumeName:    volName,
		LunName:          models.GenLunName(volName),
		LunDescription:   lunDescription,
		ShareName:        models.GenShareName(volName),
		Location:         params["location"],
		Size:             sizeInByte,
		Type:             params["type"],
		ThinProvisioning: isThin,
		TargetName:       fmt.Sprintf("%s-%s", models.TargetPrefix, volName),
		MultipleSession:  multiSession,
		SourceSnapshotId: srcSnapshotId,
		SourceVolumeId:   srcVolumeId,
		Protocol:         protocol,
		NfsVersion:       nfsVer,
		DevAttribs:       devAttribs,
	}

	// idempotency
	// Note: an SMB PV may not be tested existed precisely because the share folder name was sliced from k8sVolumeName
	k8sVolume := cs.dsmService.GetVolumeByName(volName)
	if k8sVolume == nil {
		k8sVolume, err = cs.dsmService.CreateVolume(spec)
		if err != nil {
			return nil, err
		}
	} else {
		// already existed
		log.Debugf("Volume [%s] already exists in [%s], backing name: [%s]", volName, k8sVolume.DsmIp, k8sVolume.Name)
	}

	if (k8sVolume.Protocol == utils.ProtocolIscsi && k8sVolume.SizeInBytes != sizeInByte) ||
		(k8sVolume.Protocol == utils.ProtocolSmb && utils.BytesToMB(k8sVolume.SizeInBytes) != utils.BytesToMBCeil(sizeInByte)) ||
		(k8sVolume.Protocol == utils.ProtocolNfs && utils.BytesToMB(k8sVolume.SizeInBytes) != utils.BytesToMBCeil(sizeInByte)) {
		return nil, status.Errorf(codes.AlreadyExists, "Already existing volume name with different capacity")
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      k8sVolume.VolumeId,
			CapacityBytes: k8sVolume.SizeInBytes,
			ContentSource: volContentSrc,
			VolumeContext: map[string]string{
				"dsm":              k8sVolume.DsmIp,
				"protocol":         k8sVolume.Protocol,
				"source":           k8sVolume.Source,
				"formatOptions":    formatOptions,
				"mountPermissions": mountPermissions,
				"baseDir":          k8sVolume.BaseDir,
			},
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeId := req.GetVolumeId()
	if volumeId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "No volume id is provided")
	}

	if err := cs.dsmService.DeleteVolume(volumeId); err != nil {
		return nil, status.Errorf(codes.Internal,
			fmt.Sprintf("Failed to DeleteVolume(%s), err: %v", volumeId, err))
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volumeId, volCap := req.GetVolumeId(), req.GetVolumeCapabilities()
	if volumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "No volume capabilities are provided")
	}

	if cs.dsmService.GetVolume(volumeId) == nil {
		return nil, status.Errorf(codes.NotFound, "Volume[%s] does not exist", volumeId)
	}

	for _, cap := range volCap {
		if !cs.isVolumeAccessModeSupport(cap.GetAccessMode().GetMode()) {
			return nil, status.Errorf(codes.NotFound, "Driver does not support volume capabilities:%v", volCap)
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{}, nil
}

func (cs *controllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	maxEntries := req.GetMaxEntries()
	startingToken := req.GetStartingToken()

	var entries []*csi.ListVolumesResponse_Entry
	var nextToken string = ""

	if 0 > maxEntries {
		return nil, status.Error(codes.InvalidArgument, "Max entries can not be negative.")
	}

	pagingSkip := ("" != startingToken)
	infos := cs.dsmService.ListVolumes()

	sort.Sort(models.ByVolumeId(infos))

	var count int32 = 0
	for _, info := range infos {
		if info.VolumeId == startingToken {
			pagingSkip = false
		}

		if pagingSkip {
			continue
		}

		if maxEntries > 0 && count >= maxEntries {
			nextToken = info.VolumeId
			break
		}

		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      info.VolumeId,
				CapacityBytes: info.SizeInBytes,
				VolumeContext: map[string]string{
					"dsm":       info.DsmIp,
					"lunName":   info.Lun.Name,
					"targetIqn": info.Target.Iqn,
					"shareName": info.Share.Name,
					"protocol":  info.Protocol,
				},
			},
		})

		count++
	}

	if pagingSkip {
		return nil, status.Errorf(codes.Aborted, fmt.Sprintf("Invalid StartingToken(%s)", startingToken))
	}

	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (cs *controllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	params := req.GetParameters()

	volInfos, err := cs.dsmService.ListDsmVolumes(params["dsm"])

	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Failed to list dsm volumes")
	}

	var availableCapacity int64 = 0

	location := params["location"]
	for _, info := range volInfos {
		if location != "" && info.Path != location {
			continue
		}

		freeSize, err := strconv.ParseInt(info.Free, 10, 64)
		if err != nil {
			continue
		}

		availableCapacity += freeSize
	}

	return &csi.GetCapacityResponse{
		AvailableCapacity: availableCapacity,
	}, nil
}

func (cs *controllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.csCap,
	}, nil
}

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	srcVolId := req.GetSourceVolumeId()
	snapshotName := req.GetName() // snapshot-XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX
	params := req.GetParameters()

	if srcVolId == "" {
		return nil, status.Error(codes.InvalidArgument, "Source volume id is empty.")
	}

	if snapshotName == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot name is empty.")
	}

	// idempotency
	orgSnap := cs.dsmService.GetSnapshotByName(snapshotName)
	if orgSnap != nil {
		// already existed
		if orgSnap.ParentUuid != srcVolId {
			return nil, status.Errorf(codes.AlreadyExists, fmt.Sprintf("Snapshot [%s] already exists but volume id is incompatible", snapshotName))
		}
		if orgSnap.CreateTime < 0 {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Bad create time: %v", orgSnap.CreateTime))
		}
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SizeBytes:      orgSnap.SizeInBytes,
				SnapshotId:     orgSnap.Uuid,
				SourceVolumeId: orgSnap.ParentUuid,
				CreationTime:   timestamppb.New(time.Unix(orgSnap.CreateTime, 0)),
				ReadyToUse:     (orgSnap.Status == "Healthy"),
			},
		}, nil
	}

	// not exist, going to create a new snapshot
	spec := &models.CreateK8sVolumeSnapshotSpec{
		K8sVolumeId:  srcVolId,
		SnapshotName: snapshotName,
		Description:  params["description"],
		TakenBy:      models.K8sCsiName,
		IsLocked:     utils.StringToBoolean(params["is_locked"]),
	}

	snapshot, err := cs.dsmService.CreateSnapshot(spec)
	if err != nil {
		log.Errorf("Failed to CreateSnapshot, snapshotName: %s, srcVolId: %s, err: %v", snapshotName, srcVolId, err)
		return nil, err
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      snapshot.SizeInBytes,
			SnapshotId:     snapshot.Uuid,
			SourceVolumeId: snapshot.ParentUuid,
			CreationTime:   timestamppb.New(time.Unix(snapshot.CreateTime, 0)),
			ReadyToUse:     (snapshot.Status == "Healthy"),
		},
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotId := req.GetSnapshotId()

	if snapshotId == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot id is empty.")
	}

	err := cs.dsmService.DeleteSnapshot(snapshotId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to DeleteSnapshot(%s), err: %v", snapshotId, err))
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	srcVolId := req.GetSourceVolumeId()
	snapshotId := req.GetSnapshotId()
	maxEntries := req.GetMaxEntries()
	startingToken := req.GetStartingToken()

	var entries []*csi.ListSnapshotsResponse_Entry
	var nextToken string = ""

	if 0 > maxEntries {
		return nil, status.Error(codes.InvalidArgument, "Max entries can not be negative.")
	}

	pagingSkip := ("" != startingToken)
	var snapshots []*models.K8sSnapshotRespSpec

	if srcVolId != "" {
		snapshots = cs.dsmService.ListSnapshots(srcVolId)
	} else {
		snapshots = cs.dsmService.ListAllSnapshots()
	}

	sort.Sort(models.BySnapshotAndParentUuid(snapshots))

	var count int32 = 0
	for _, snapshot := range snapshots {
		if snapshot.Uuid == startingToken {
			pagingSkip = false
		}

		if pagingSkip {
			continue
		}

		if snapshotId != "" && snapshot.Uuid != snapshotId {
			continue
		}

		if maxEntries > 0 && count >= maxEntries {
			nextToken = snapshot.Uuid
			break
		}
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SizeBytes:      snapshot.SizeInBytes,
				SnapshotId:     snapshot.Uuid,
				SourceVolumeId: snapshot.ParentUuid,
				CreationTime:   timestamppb.New(time.Unix(snapshot.CreateTime, 0)),
				ReadyToUse:     (snapshot.Status == "Healthy"),
			},
		})

		count++
	}

	if pagingSkip {
		return nil, status.Errorf(codes.Aborted, fmt.Sprintf("Invalid StartingToken(%s)", startingToken))
	}

	return &csi.ListSnapshotsResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeId, capRange := req.GetVolumeId(), req.GetCapacityRange()

	if volumeId == "" || capRange == nil {
		return nil, status.Error(codes.InvalidArgument,
			"InvalidArgument: Please check volume ID and capacity range.")
	}

	sizeInByte, err := getSizeByCapacityRange(capRange)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"InvalidArgument: Please check CapacityRange[%v]", capRange)
	}

	k8sVolume, err := cs.dsmService.ExpandVolume(volumeId, sizeInByte)
	if err != nil {
		return nil, err
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         k8sVolume.SizeInBytes,
		NodeExpansionRequired: (k8sVolume.Protocol == utils.ProtocolIscsi),
	}, nil
}

func (cs *controllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
