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

	"github.com/cenkalti/backoff/v4"
	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type nodeServer struct {
	Driver     *Driver
	Mounter    *mount.SafeFormatAndMount
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
	k8sVolume := ns.dsmService.GetVolume(volumeId)

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

	if k8sVolume == nil || k8sVolume.Protocol != utils.ProtocolIscsi {
		return
	}

	ns.Initiator.logout(k8sVolume.Target.Iqn, k8sVolume.DsmIp)
}

func checkGidPresentInMountFlags(volumeMountGroup string, mountFlags []string) (bool, error) {
	gidPresentInMountFlags := false
	for _, mountFlag := range mountFlags {
		if strings.HasPrefix(mountFlag, "gid") {
			gidPresentInMountFlags = true
			kvpair := strings.Split(mountFlag, "=")
			if volumeMountGroup != "" && len(kvpair) == 2 && !strings.EqualFold(volumeMountGroup, kvpair[1]) {
				return false, status.Error(codes.InvalidArgument, fmt.Sprintf("gid(%s) in storageClass and pod fsgroup(%s) are not equal", kvpair[1], volumeMountGroup))
			}
		}
	}
	return gidPresentInMountFlags, nil
}

func (ns *nodeServer) mountSensitiveWithRetry(sourcePath string, targetPath string, fsType string, options []string, sensitiveOptions []string) error {
	mountBackoff := backoff.NewExponentialBackOff()
	mountBackoff.InitialInterval = 1 * time.Second
	mountBackoff.Multiplier = 2
	mountBackoff.RandomizationFactor = 0.1
	mountBackoff.MaxElapsedTime = 5 * time.Second

	checkFinished := func() error {
		if err := ns.Mounter.MountSensitive(sourcePath, targetPath, fsType, options, sensitiveOptions); err != nil {
			return err
		}

		return nil
	}

	mountNotify := func(err error, duration time.Duration) {
		log.Infof("Retry MountSensitive, waiting %3.2f seconds .....", float64(duration.Seconds()))
	}

	if err := backoff.RetryNotify(checkFinished, mountBackoff, mountNotify); err != nil {
		log.Errorf("Could not finish mount after %3.2f seconds.", float64(mountBackoff.MaxElapsedTime.Seconds()))
		return err
	}

	log.Debugf("Mount successfully. source: %s, target: %s", sourcePath, targetPath)
	return nil
}

func (ns *nodeServer) setSMBVolumePermission(sourcePath string, userName string, authType utils.AuthType) error {
	s := strings.Split(strings.TrimPrefix(sourcePath, "//"), "/")
	if len(s) != 2 {
		return fmt.Errorf("Failed to parse dsmIp and shareName from source path")
	}
	dsmIp, shareName := s[0], s[1]

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		return fmt.Errorf("Failed to get DSM[%s]", dsmIp)
	}

	permission := webapi.SharePermission{
		Name: userName,
	}
	switch authType {
	case utils.AuthTypeReadWrite:
		permission.IsWritable = true
	case utils.AuthTypeReadOnly:
		permission.IsReadonly = true
	case utils.AuthTypeNoAccess:
		permission.IsDeny = true
	default:
		return fmt.Errorf("Unknown auth type: %s", string(authType))
	}

	permissions := append([]*webapi.SharePermission{}, &permission)
	spec := webapi.SharePermissionSetSpec{
		Name: shareName,
		UserGroupType: models.UserGroupTypeLocalUser,
		Permissions: permissions,
	}

	return dsm.SharePermissionSet(spec)
}

func (ns *nodeServer) nodeStageISCSIVolume(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	// if block mode, skip mount
	if spec.VolumeCapability.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if err := ns.loginTarget(spec.VolumeId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeMountPath := ns.getVolumeMountPath(spec.VolumeId)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	notMount, err := ns.Mounter.Interface.IsLikelyNotMountPoint(spec.StagingTargetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if !notMount {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fsType := spec.VolumeCapability.GetMount().GetFsType()
	options := append([]string{"rw"}, spec.VolumeCapability.GetMount().GetMountFlags()...)

	if err = ns.Mounter.FormatAndMount(volumeMountPath, spec.StagingTargetPath, fsType, options); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) nodeStageSMBVolume(ctx context.Context, spec *models.NodeStageVolumeSpec, secrets map[string]string) (*csi.NodeStageVolumeResponse, error) {
	if spec.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("SMB protocol only allows 'mount' access type"))
	}

	if spec.Source == "" { //"//<host>/<shareName>"
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Missing 'source' field"))
	}

	if secrets == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Missing secrets for node staging volume"))
	}

	username := strings.TrimSpace(secrets["username"])
	password := strings.TrimSpace(secrets["password"])
	domain := strings.TrimSpace(secrets["domain"])

	// set permission to access the share
	if err := ns.setSMBVolumePermission(spec.Source, username, utils.AuthTypeReadWrite); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to set permission, source: %s, err: %v", spec.Source, err))
	}

	// create mount point if not exists
	targetPath := spec.StagingTargetPath
	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, false)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		log.Infof("NodeStageVolume: %s is already mounted", targetPath)
		return &csi.NodeStageVolumeResponse{}, nil // already mount
	}

	fsType := "cifs"
	options := spec.VolumeCapability.GetMount().GetMountFlags()

	volumeMountGroup := spec.VolumeCapability.GetMount().GetVolumeMountGroup()
	gidPresent, err := checkGidPresentInMountFlags(volumeMountGroup, options)
	if err != nil {
		return nil, err
	}
	if !gidPresent && volumeMountGroup != "" {
		options = append(options, fmt.Sprintf("gid=%s", volumeMountGroup))
	}


	if domain != "" {
		options = append(options, fmt.Sprintf("%s=%s", "domain", domain))
	}
	var sensitiveOptions = []string{fmt.Sprintf("%s=%s,%s=%s", "username", username, "password", password)}
	if err := ns.mountSensitiveWithRetry(spec.Source, targetPath, fsType, options, sensitiveOptions); err != nil {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Volume[%s] failed to mount %q on %q. err: %v", spec.VolumeId, spec.Source, targetPath, err))
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeId, stagingTargetPath, volumeCapability :=
		req.GetVolumeId(), req.GetStagingTargetPath(), req.GetVolumeCapability()

	if volumeId == "" || stagingTargetPath == "" || volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument,
		 "InvalidArgument: Please check volume ID, staging target path and volume capability.")
	}

	if volumeCapability.GetBlock() != nil && volumeCapability.GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot mix block and mount capabilities")
	}

	spec := &models.NodeStageVolumeSpec{
		VolumeId: volumeId,
		StagingTargetPath: stagingTargetPath,
		VolumeCapability: volumeCapability,
		Dsm: req.VolumeContext["dsm"],
		Source: req.VolumeContext["source"], // filled by CreateVolume response
	}

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		return ns.nodeStageSMBVolume(ctx, spec, req.GetSecrets())
	default:
		return ns.nodeStageISCSIVolume(ctx, spec)
	}
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, stagingTargetPath := req.GetVolumeId(), req.GetStagingTargetPath()

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
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

	ns.logoutTarget(volumeID)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeId, targetPath, stagingTargetPath := req.GetVolumeId(), req.GetTargetPath(), req.GetStagingTargetPath()

	if volumeId == "" || targetPath == "" || stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument,
			"InvalidArgument: Please check volume ID, target path and staging target path.")
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}

	isBlock := req.GetVolumeCapability().GetBlock() != nil // raw block, only for iscsi protocol
	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, isBlock)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	options := []string{"bind"}
	if req.GetReadonly() {
		options = append(options, "ro")
	}

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		if err := ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, "", options); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	default:
		if err := ns.loginTarget(volumeId); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		volumeMountPath := ns.getVolumeMountPath(volumeId)
		if volumeMountPath == "" {
			return nil, status.Error(codes.Internal, "Can't get volume mount path")
		}

		if isBlock {
			err = ns.Mounter.Interface.Mount(volumeMountPath, targetPath, "", options)
		} else {
			err = ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, fsType, options)
		}
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" { // Not needed, but still a mandatory field
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
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

	if err := ns.Mounter.Interface.Unmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.Remove(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to remove target path.")
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


	if k8sVolume.Protocol == utils.ProtocolSmb {
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				&csi.VolumeUsage{
					Total:     k8sVolume.SizeInBytes,
					Unit:      csi.VolumeUsage_BYTES,
				},
			},
		}, nil
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

	if k8sVolume.Protocol == utils.ProtocolSmb {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
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
