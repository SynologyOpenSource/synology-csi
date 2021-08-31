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
	"time"
	"strconv"

	"github.com/golang/protobuf/ptypes"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
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

	spec := &models.CreateK8sVolumeSpec{
		DsmIp:            params["dsm"],
		K8sVolumeName:    volName,
		LunName:          fmt.Sprintf("%s-%s", models.LunPrefix, volName),
		Location:         params["location"],
		Size:             sizeInByte,
		Type:             params["type"],
		ThinProvisioning: isThin,
		TargetName:       fmt.Sprintf("%s-%s", models.LunPrefix, volName),
		MultipleSession:  multiSession,
		SourceSnapshotId: srcSnapshotId,
		SourceVolumeId:   srcVolumeId,
	}

	lunInfo, dsmIp, err := cs.dsmService.CreateVolume(spec)
	if err != nil {
		return nil, err
	}

	if int64(lunInfo.Size) != sizeInByte {
		return nil , status.Errorf(codes.AlreadyExists, "Already existing volume name with different capacity")
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId: lunInfo.Uuid,
			CapacityBytes: int64(lunInfo.Size),
			ContentSource: volContentSrc,
			VolumeContext: map[string]string{
				"dsm": dsmIp,
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

	var count int32 = 0
	for _, info := range infos {
		if info.Lun.Uuid == startingToken {
			pagingSkip = false
		}

		if pagingSkip {
			continue
		}

		if maxEntries > 0 && count >= maxEntries {
			nextToken = info.Lun.Uuid
			break
		}

		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      info.Lun.Uuid,
				CapacityBytes: int64(info.Lun.Size),
				VolumeContext: map[string]string{
					"dsm":       info.DsmIp,
					"lunName":   info.Lun.Name,
					"targetIqn": info.Target.Iqn,
				},
			},
		})

		count++
	}

	if pagingSkip {
		return nil, status.Errorf(codes.Aborted, fmt.Sprintf("Invalid StartingToken(%s)", startingToken))
	}

	return &csi.ListVolumesResponse{
		Entries: entries,
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
	snapshotName := req.GetName()
	params := req.GetParameters()

	if srcVolId == "" {
		return nil, status.Error(codes.InvalidArgument, "Source volume id is empty.")
	}

	if snapshotName == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot name is empty.")
	}

	snapshotInfos, err := cs.dsmService.ListAllSnapshots()

	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to ListAllSnapshots(), err: %v", err))
	}

	// idempotency
	for _, snapshotInfo := range snapshotInfos {
		if snapshotInfo.Name == snapshotName {
			if snapshotInfo.ParentUuid != srcVolId {
				return nil, status.Errorf(codes.AlreadyExists, fmt.Sprintf("Snapshot [%s] already exists but volume id is incompatible", snapshotName))
			}

			createTime, err := ptypes.TimestampProto(time.Unix(snapshotInfo.CreateTime, 0))

			if err != nil {
				return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to convert create time, err: %v", err))
			}

			return &csi.CreateSnapshotResponse{
				Snapshot: &csi.Snapshot{
					SizeBytes: snapshotInfo.TotalSize,
					SnapshotId: snapshotInfo.Uuid,
					SourceVolumeId: snapshotInfo.ParentUuid,
					CreationTime: createTime,
					ReadyToUse: (snapshotInfo.Status == "Healthy"),
				},
			}, nil
		}
	}

	spec := &models.CreateK8sVolumeSnapshotSpec{
		K8sVolumeId:  srcVolId,
		SnapshotName: snapshotName,
		Description:  params["description"],
		TakenBy:      models.K8sCsiName,
		IsLocked:     utils.StringToBoolean(params["is_locked"]),
	}

	snapshotId, err := cs.dsmService.CreateSnapshot(spec)

	if err != nil {
		if err == utils.OutOfFreeSpaceError("") || err == utils.SnapshotReachMaxCountError("") {
			return nil,status.Errorf(codes.ResourceExhausted, fmt.Sprintf("Failed to CreateSnapshot(%s), err: %v", srcVolId, err))
		} else {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to CreateSnapshot(%s), err: %v", srcVolId, err))
		}
	}

	snapshotInfo, err := cs.dsmService.GetSnapshot(snapshotId)

	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to GetSnapshot(%s), err: %v", snapshotId, err))
	}

	createTime, err := ptypes.TimestampProto(time.Unix(snapshotInfo.CreateTime, 0))

	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to convert create time, err: %v", err))
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes: snapshotInfo.TotalSize,
			SnapshotId: snapshotInfo.Uuid,
			SourceVolumeId: snapshotInfo.ParentUuid,
			CreationTime: createTime,
			ReadyToUse: (snapshotInfo.Status == "Healthy"),
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
	var snapshotInfos []webapi.SnapshotInfo
	var err error

	if (srcVolId != "") {
		snapshotInfos, err = cs.dsmService.ListSnapshots(srcVolId)
	} else {
		snapshotInfos, err = cs.dsmService.ListAllSnapshots()
	}

	if err != nil {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to ListSnapshots(%s), err: %v", srcVolId, err))
	}

	var count int32 = 0
	for _, snapshotInfo := range snapshotInfos {
		if snapshotInfo.Uuid == startingToken {
			pagingSkip = false
		}

		if pagingSkip {
			continue
		}

		if snapshotId != "" && snapshotInfo.Uuid != snapshotId {
			continue
		}

		if maxEntries > 0 && count >= maxEntries {
			nextToken = snapshotInfo.Uuid
			break
		}

		createTime, err := ptypes.TimestampProto(time.Unix(snapshotInfo.CreateTime, 0))

		if err != nil {
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to convert create time, err: %v", err))
		}

		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SizeBytes: snapshotInfo.TotalSize,
				SnapshotId: snapshotInfo.Uuid,
				SourceVolumeId: snapshotInfo.ParentUuid,
				CreationTime: createTime,
				ReadyToUse: (snapshotInfo.Status == "Healthy"),
			},
		})

		count++
	}

	if pagingSkip {
		return nil, status.Errorf(codes.Aborted, fmt.Sprintf("Invalid StartingToken(%s)", startingToken))
	}

	return &csi.ListSnapshotsResponse{
		Entries: entries,
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

	if err := cs.dsmService.ExpandLun(volumeId, sizeInByte); err != nil {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to expand volume [%s], err: %v", volumeId, err))
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes: sizeInByte,
		NodeExpansionRequired: true,
	}, nil
}

func (cs *controllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}