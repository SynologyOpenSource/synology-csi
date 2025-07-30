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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
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
	Client     clientset.Interface
	tools      tools
}

func waitForDevicePathToExist(path string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			exists, err := mount.PathExists(path)
			if err != nil {
				return err
			}
			if exists == true {
				return nil
			}
			log.Warnf("Device path [%s] doesn't exists yet, retrying in 1 second", path)
		case <-timer.C:
			return os.ErrNotExist
		}
	}
}

// for unstage, resize volume
func (t *tools) getExistedVolumeMountPath(targetIqn string, mappingIndex int) string {
	paths := []string{}

	sessions := t.listSessionsByIqn(targetIqn)
	for _, session := range sessions {
		paths = append(paths, fmt.Sprintf("%sip-%s-iscsi-%s-lun-%d", "/dev/disk/by-path/", session.Portal, targetIqn, mappingIndex))
	}

	return getVolumeMountPath(paths)
}

// for publish, stage volume
func getVolumeMountPath(iscsiDevPaths []string) string {
	var path string

	if len(iscsiDevPaths) > 1 { // check multipath exist
		devices, err := lsblk(iscsiDevPaths, true)
		if err != nil {
			log.Errorf("Failed to lsblk for iscsi devices: %v", err)
			return ""
		}

		multipathDevice, err := GetMultipathDevice(devices)
		if err != nil {
			log.Error(err)
			return ""
		}
		path = filepath.Join("/dev/mapper", multipathDevice.Name)
	} else if len(iscsiDevPaths) == 1 {
		path = iscsiDevPaths[0]
	} else {
		return ""
	}

	if err := waitForDevicePathToExist(path); err != nil {
		log.Errorf("Can't find device path [%s],: %v", path, err)
		return ""
	}

	return path
}

func createTargetMountPathNFS(mounter mount.Interface, mountPath string, mountPermissionsUint uint64) (bool, error) {
	notMount, err := mounter.IsLikelyNotMountPoint(mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(mountPath, os.FileMode(mountPermissionsUint)); err != nil {
				return notMount, err
			}
			notMount = true
		} else {
			return false, err
		}
	}
	return notMount, nil
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

func (ns *nodeServer) getPortals(dsmIp string) []string {
	portals := []string{}

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		log.Errorf("Failed to get DSM[%s]", dsmIp)
		return portals
	}

	ips, err := utils.LookupIPv4(dsmIp)
	if err != nil {
		log.Error(err)
		portals = append(portals, fmt.Sprintf("%s:%d", dsmIp, ISCSIPort))
	} else {
		portals = append(portals, fmt.Sprintf("%s:%d", ips[0], ISCSIPort)) //get the first ip
	}

	if dsm.IsUC() && ns.tools.IsMultipathEnabled() {
		dsm2, err := dsm.GetAnotherController()
		if err != nil {
			log.Errorf("[%s] UC failed to get another controller: %v", err)
		} else {
			portals = append(portals, fmt.Sprintf("%s:%d", dsm2.Ip, ISCSIPort))
		}
	}
	return portals
}

func (ns *nodeServer) loginTarget(volumeId string) ([]string, error) {
	paths := []string{}
	k8sVolume := ns.dsmService.GetVolume(volumeId)

	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	portals := ns.getPortals(k8sVolume.DsmIp)
	if len(portals) == 0 {
		return nil, status.Errorf(codes.Internal, fmt.Sprintf("Failed to get portals"))
	}

	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex
	for _, portal := range portals {
		if err := ns.Initiator.login(k8sVolume.Target.Iqn, portal); err != nil {
			return nil, status.Errorf(codes.Internal,
				fmt.Sprintf("Failed to login with target iqn [%s], err: %v", k8sVolume.Target.Iqn, err))
		}

		path := fmt.Sprintf("%sip-%s-iscsi-%s-lun-%d", "/dev/disk/by-path/", portal, k8sVolume.Target.Iqn, mappingIndex)
		if err := waitForDevicePathToExist(path); err != nil {
			log.Errorf("Can't find device path [%s]: %v", path, err)
			return nil, status.Errorf(codes.Internal, fmt.Sprintf("Can't find device path [%s]: %v", path, err))
		}

		paths = append(paths, path)
	}

	return paths, nil
}

func (ns *nodeServer) logoutTarget(volumeId string) {
	k8sVolume := ns.dsmService.GetVolume(volumeId)

	if k8sVolume == nil || k8sVolume.Protocol != utils.ProtocolIscsi {
		return
	}

	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex
	volumeMountPath := ns.tools.getExistedVolumeMountPath(k8sVolume.Target.Iqn, mappingIndex)

	if strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled() {
		if err := ns.tools.multipath_flush(volumeMountPath); err != nil {
			log.Errorf("Failed to remove multipath device in path %s. err: %v", volumeMountPath, err)
		}
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

func getNodeAddress(ctx context.Context, client clientset.Interface) ([]string, error) {
	ips := []string{}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Errorf("Failed to list nodes, err: %v", err)
		return nil, err
	}

	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == "InternalIP" {
				ips = append(ips, address.Address)
			}
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("Empty results")
	}
	return ips, nil
}

func (ns *nodeServer) setNFSVolumePrivilege(sourcePath string, hostnames []string, authType utils.AuthType) error {
	// NFSTODO: fix the parsing rule
	s := strings.Split(strings.TrimPrefix(sourcePath, "//"), "/")
	if len(s) != 2 {
		return fmt.Errorf("Failed to parse dsmIp and shareName from source path")
	}
	dsmIp, shareName := s[0], s[1]

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		return fmt.Errorf("Failed to get DSM[%s]", dsmIp)
	}

	priv := webapi.SharePrivilege{
		ShareName: shareName,
	}

	for _, hostname := range hostnames {
		priv.Rule = append(priv.Rule, webapi.PrivilegeRule{
			Async:      true,
			Client:     hostname,
			Crossmnt:   true,
			Insecure:   true,
			Privilege:  string(authType),
			RootSquash: "root",
			SecurityFlavor: webapi.SecurityFlavor{
				Kerbros:          false,
				KerbrosIntegrity: false,
				KerbrosPrivacy:   false,
				Sys:              true,
			},
		})
	}

	err = dsm.ShareNfsPrivilegeSave(priv)
	if err != nil {
		log.Printf("Failed to save share NFS privilege. Priv:%v. %v", priv, err)
		return err
	}
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
		Name:          shareName,
		UserGroupType: models.UserGroupTypeLocalUser,
		Permissions:   permissions,
	}

	return dsm.SharePermissionSet(spec)
}

func (ns *nodeServer) nodeStageISCSIVolume(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	// if block mode, skip mount
	if spec.VolumeCapability.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	iscsiDevPaths, err := ns.loginTarget(spec.VolumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeMountPath := getVolumeMountPath(iscsiDevPaths)
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

	formatOptions := utils.StringToSlice(spec.FormatOptions)

	if err = ns.Mounter.FormatAndMountSensitiveWithFormatOptions(volumeMountPath, spec.StagingTargetPath, fsType, options, nil, formatOptions); err != nil {
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

func (ns *nodeServer) nodeStageNFSVolume(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	nodeIps, err := getNodeAddress(ctx, ns.Client)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to get node IPs for NFS privilege setting, err: %v", err))
	}

	if err := ns.setNFSVolumePrivilege(spec.Source, nodeIps, utils.AuthTypeReadWrite); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to set NFS privilege rule, source: %s, err: %v", spec.Source, err))
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
		VolumeId:          volumeId,
		StagingTargetPath: stagingTargetPath,
		VolumeCapability:  volumeCapability,
		Dsm:               req.VolumeContext["dsm"],
		Source:            req.VolumeContext["source"], // filled by CreateVolume response
		FormatOptions:     req.VolumeContext["formatOptions"],
	}

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		return ns.nodeStageSMBVolume(ctx, spec, req.GetSecrets())
	case utils.ProtocolNfs:
		return ns.nodeStageNFSVolume(ctx, spec)
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
	options := []string{}
	if req.GetReadonly() {
		options = append(options, "ro")
	}

	// nfs
	if req.VolumeContext["protocol"] == utils.ProtocolNfs {
		options = append(options, req.GetVolumeCapability().GetMount().GetMountFlags()...)

		var server, baseDir string             //NFSTODO: subDir
		var mountPermissionsUint uint64 = 0750 // default
		for k, v := range req.GetVolumeContext() {
			switch k {
			case "dsm":
				server = v
			case "baseDir":
				baseDir = v
			case "mountPermissions":
				if v != "" {
					var err error
					mountPermissionsUint, err = strconv.ParseUint(v, 8, 32)
					if err != nil {
						return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid mountPermissions %s", v))
					}
				}
			}
		}

		if server == "" || baseDir == "" {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid inputs: server(dsm) and baseDir are required."))
		}
		source := fmt.Sprintf("%s:%s", server, baseDir)

		notMount, err := createTargetMountPathNFS(ns.Mounter.Interface, targetPath, mountPermissionsUint)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if !notMount {
			log.Infof("NodePublishVolume: %s is already mounted", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}

		log.Debugf("NodePublishVolume: volumeId(%v) source(%s) targetPath(%s) mountflags(%v)", volumeId, source, targetPath, options)
		err = ns.Mounter.Mount(source, targetPath, "nfs", options)
		if err != nil {
			if os.IsPermission(err) {
				return nil, status.Error(codes.PermissionDenied, err.Error())
			}
			if strings.Contains(err.Error(), "invalid argument") {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			return nil, status.Error(codes.Internal, err.Error())
		}

		if mountPermissionsUint > 0 {
			if err := chmodIfPermissionMismatch(targetPath, os.FileMode(mountPermissionsUint)); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}

		log.Debugf("NFS volume(%s) mount %s on %s succeeded", volumeId, source, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// iscsi & smb
	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, isBlock)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	options = append(options, "bind")

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		if err := ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, "", options); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	default:
		iscsiDevPaths, err := ns.loginTarget(volumeId)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		volumeMountPath := getVolumeMountPath(iscsiDevPaths)
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

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				&csi.VolumeUsage{
					Total: k8sVolume.SizeInBytes,
					Unit:  csi.VolumeUsage_BYTES,
				},
			},
		}, nil
	}

	// If we are dealing with a LUN use statfs
	statfs := &unix.Statfs_t{}
	err = unix.Statfs(volumePath, statfs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get fs info on path %s: %v", req.VolumePath, err)
	}

	// Available is blocks available * fragment size
	available := int64(statfs.Bavail) * int64(statfs.Bsize)

	// Capacity is total block count * fragment size
	capacity := int64(statfs.Blocks) * int64(statfs.Bsize)

	// Usage is block being used * fragment size (aka block size).
	usage := (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize)

	inodes := int64(statfs.Files)
	inodesFree := int64(statfs.Ffree)
	inodesUsed := inodes - inodesFree

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      usage,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
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

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
	}

	if err := ns.Initiator.rescan(k8sVolume.Target.Iqn); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to rescan. err: %v", err))
	}

	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex
	volumeMountPath := ns.tools.getExistedVolumeMountPath(k8sVolume.Target.Iqn, mappingIndex)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	if strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled() {
		if err := ns.tools.multipath_resize(filepath.Base(volumeMountPath)); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to resize multipath device in %s. err: %v", volumeMountPath, err))
		}
	}

	isBlock := req.GetVolumeCapability() != nil && req.GetVolumeCapability().GetBlock() != nil
	if isBlock {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
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
