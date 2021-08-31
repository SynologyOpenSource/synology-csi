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
	"os"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type nodeServer struct {
	Driver *Driver
	Mounter *mount.SafeFormatAndMount
	dsmService interfaces.IDsmService
	Initiator  *initiatorDriver
}

func getExistedDevicePath(paths []string) string {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			for _, path := range paths {
				exists, err := mount.PathExists(path)
				if err == nil && exists == true {
					return path
				} else {
					log.Errorf("Can't find device path [%s], err: %v", path, err)
				}
			}
		case <-timer.C:
			return ""
		}
	}
}

func (ns *nodeServer) getVolumeMountPath(volumeId string) string {
	paths := []string{}

	k8sVolume := ns.dsmService.GetVolume(volumeId)
	if k8sVolume == nil {
		log.Errorf("Failed to get Volume id:%d.")
		return ""
	}
	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex

	ips, err := utils.LookupIPv4(k8sVolume.DsmIp)
	if err != nil {
		log.Errorf("Failed to lookup ipv4 for host: %s", k8sVolume.DsmIp)
		paths = append(paths, fmt.Sprintf("%sip-%s:3260-iscsi-%s-lun-%d", "/dev/disk/by-path/", k8sVolume.DsmIp, k8sVolume.Target.Iqn, mappingIndex))
	} else {
		for _, ipv4 := range ips {
			paths = append(paths, fmt.Sprintf("%sip-%s:3260-iscsi-%s-lun-%d", "/dev/disk/by-path/", ipv4, k8sVolume.Target.Iqn, mappingIndex))
		}
	}

	path := getExistedDevicePath(paths)
	if path == "" {
		log.Errorf("Volume mount path is not exist.")
		return ""
	}

	return path
}

func createTargetMountPath(mounter mount.Interface, mountPath string, isBlock bool) (bool, error) {
	notMount, err := mount.IsNotMountPoint(mounter, mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if isBlock {
				pathFile, err := os.OpenFile(mountPath, os.O_CREATE|os.O_RDWR, 0750)
				if err != nil {
					log.Errorf("Failed to create mountPath:%s with error: %v", mountPath, err)
					return notMount, err
				}
				if err = pathFile.Close(); err != nil {
					log.Errorf("Failed to close mountPath:%s with error: %v", mountPath, err)
					return notMount, err
				}
			} else {
				err = os.MkdirAll(mountPath, 0750)
				if err != nil {
					return notMount, err
				}
			}
			notMount = true
		} else {
			return false, err
		}
	}
	return notMount, nil
}

func (ns *nodeServer) loginTarget(volumeId string) error {
	k8sVolume := ns.dsmService.GetVolume(volumeId);

	if k8sVolume == nil {
		return status.Error(codes.NotFound, fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	if err := ns.Initiator.login(k8sVolume.Target.Iqn, k8sVolume.DsmIp); err != nil {
		return status.Errorf(codes.Internal,
			fmt.Sprintf("Failed to login with target iqn [%s], err: %v", k8sVolume.Target.Iqn, err))
	}

	return nil
}

func (ns *nodeServer) logoutTarget(volumeId string) {
	k8sVolume := ns.dsmService.GetVolume(volumeId)

	if k8sVolume == nil {
		return
	}

	ns.Initiator.logout(k8sVolume.Target.Iqn, k8sVolume.DsmIp)
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeId, stagingTargetPath, volumeCapability :=
		req.GetVolumeId(), req.GetStagingTargetPath(), req.GetVolumeCapability()

	if volumeId == "" || stagingTargetPath == "" || volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument,
		 "InvalidArgument: Please check volume ID, staging target path and volume capability.")
	}

	// if block mode, skip mount
	if volumeCapability.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if err := ns.loginTarget(volumeId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeMountPath := ns.getVolumeMountPath(volumeId)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	notMount, err := ns.Mounter.Interface.IsLikelyNotMountPoint(stagingTargetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if !notMount {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fsType := volumeCapability.GetMount().GetFsType()
	mountFlags := volumeCapability.GetMount().GetMountFlags()
	options := append([]string{"rw"}, mountFlags...)

	if err = ns.Mounter.FormatAndMount(volumeMountPath, stagingTargetPath, fsType, options); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" { // Useless, just for sanity check
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	stagingTargetPath := req.GetStagingTargetPath()

	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, stagingTargetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		err = ns.Mounter.Interface.Unmount(stagingTargetPath)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeId, targetPath, stagingTargetPath := req.GetVolumeId(), req.GetTargetPath(), req.GetStagingTargetPath()
	isBlock := req.GetVolumeCapability().GetBlock() != nil

	if volumeId == "" || targetPath == "" || stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument,
			"InvalidArgument: Please check volume ID, target path and staging target path.")
	}

	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, isBlock)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err := ns.loginTarget(volumeId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeMountPath := ns.getVolumeMountPath(volumeId)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	options := []string{"bind"}
	if req.GetReadonly() {
		options = append(options, "ro")
	}

	if isBlock {
		err = ns.Mounter.Interface.Mount(volumeMountPath, targetPath, "", options)
	} else {
		fsType := req.GetVolumeCapability().GetMount().GetFsType()
		err = ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, fsType, options)
	}

	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeId, targetPath := req.GetVolumeId(), req.GetTargetPath()
	if volumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err){
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, targetPath)

	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if notMount {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	needToLogout := true

	list, err := ns.Mounter.Interface.GetMountRefs(targetPath)

	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	for _, path := range list {
		filePrefix := "/var/lib/kubelet/pods/"
		blkPrefix := "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/"

		if strings.HasPrefix(path, filePrefix) || strings.HasPrefix(path, blkPrefix) {
			needToLogout = false
			break
		}
	}

	if err := ns.Mounter.Interface.Unmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.Remove(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to remove target path.")
	}

	if needToLogout {
		ns.logoutTarget(volumeId)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	log.Debugf("Using default NodeGetInfo, ns.Driver.nodeID = [%s]", ns.Driver.nodeID)

	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nsCap,
	}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeId, volumePath := req.GetVolumeId(), req.GetVolumePath()
	if volumeId == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Invalid Argument")
	}

	k8sVolume := ns.dsmService.GetVolume(volumeId)
	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound,
			fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, volumePath)
	if err != nil || notMount {
		return nil, status.Error(codes.NotFound,
			fmt.Sprintf("Volume[%s] does not exist on the %s", volumeId, volumePath))
	}

	lun := k8sVolume.Lun

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			&csi.VolumeUsage{
				Available: int64(lun.Size - lun.Used),
				Total:     int64(lun.Size),
				Used:      int64(lun.Used),
				Unit:      csi.VolumeUsage_BYTES,
			},
		},
	}, nil
}

func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeId, volumePath := req.GetVolumeId(), req.GetVolumePath()
	sizeInByte, err := getSizeByCapacityRange(req.GetCapacityRange())
	if volumeId == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "InvalidArgument: Please check volume ID and volume path.")
	}

	k8sVolume := ns.dsmService.GetVolume(volumeId)
	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	if err := ns.Initiator.rescan(k8sVolume.Target.Iqn); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to rescan. err: %v", err))
	}

	isBlock := req.GetVolumeCapability() != nil && req.GetVolumeCapability().GetBlock() != nil
	if isBlock {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
	}

	volumeMountPath := ns.getVolumeMountPath(volumeId)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	ok, err := mount.NewResizeFs(ns.Mounter.Exec).Resize(volumeMountPath, volumePath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Error(codes.Internal, "Failed to expand volume filesystem")
	}
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: sizeInByte}, nil
}