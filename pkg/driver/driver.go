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
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
)

const (
	DriverName    = "csi.san.synology.com" // CSI dirver name
	DriverVersion = "1.2.0"
)

var (
	MultipathEnabled      = true
	supportedProtocolList = []string{utils.ProtocolIscsi, utils.ProtocolSmb, utils.ProtocolNfs}
	allowedNfsVersionList = []string{"3", "4", "4.0", "4.1"}
)

type IDriver interface {
	Activate()
}

type Driver struct {
	// *csicommon.CSIDriver
	name       string
	nodeID     string
	version    string
	endpoint   string
	tools      tools
	csCap      []*csi.ControllerServiceCapability
	vCap       []*csi.VolumeCapability_AccessMode
	nsCap      []*csi.NodeServiceCapability
	DsmService interfaces.IDsmService
}

func NewControllerAndNodeDriver(nodeID string, endpoint string, dsmService interfaces.IDsmService, tools tools) (*Driver, error) {
	log.Debugf("NewControllerAndNodeDriver: DriverName: %v, DriverVersion: %v", DriverName, DriverVersion)

	// TODO version format and validation
	d := &Driver{
		name:       DriverName,
		version:    DriverVersion,
		nodeID:     nodeID,
		endpoint:   endpoint,
		DsmService: dsmService,
		tools:      tools,
	}

	d.addControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	})

	d.addVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	})

	d.addNodeServiceCapabilities([]csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		// csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
	})

	log.Infof("New driver created: name=%s, nodeID=%s, version=%s, endpoint=%s", d.name, d.nodeID, d.version, d.endpoint)
	return d, nil
}

// TODO: func NewNodeDriver() {}
// TODO: func NewControllerDriver() {}

func (d *Driver) Activate() {
	go func() {
		RunControllerandNodePublishServer(d.endpoint, d, NewControllerServer(d), NewNodeServer(d))
	}()
}

func (d *Driver) addControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	var csc []*csi.ControllerServiceCapability

	for _, c := range cl {
		log.Debugf("Enabling controller service capability: %v", c.String())
		csc = append(csc, NewControllerServiceCapability(c))
	}

	d.csCap = csc
	return
}

func (d *Driver) addVolumeCapabilityAccessModes(vc []csi.VolumeCapability_AccessMode_Mode) {
	var vca []*csi.VolumeCapability_AccessMode

	for _, c := range vc {
		log.Debugf("Enabling volume access mode: %v", c.String())
		vca = append(vca, NewVolumeCapabilityAccessMode(c))
	}

	d.vCap = vca
	return
}

func (d *Driver) addNodeServiceCapabilities(nsc []csi.NodeServiceCapability_RPC_Type) {
	var nca []*csi.NodeServiceCapability

	for _, c := range nsc {
		log.Debugf("Enabling node service capability: %v", c.String())
		nca = append(nca, NewNodeServiceCapability(c))
	}

	d.nsCap = nca
	return
}

func (d *Driver) getVolumeCapabilityAccessModes() []*csi.VolumeCapability_AccessMode { // for debugging
	return d.vCap
}

func isProtocolSupport(protocol string) bool {
	return utils.SliceContains(supportedProtocolList, protocol)
}

func isNfsVersionAllowed(ver string) bool {
	return utils.SliceContains(allowedNfsVersionList, ver)
}
