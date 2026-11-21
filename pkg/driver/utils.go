/*
Copyright 2021 Synology Inc.

Copyright 2017 The Kubernetes Authors.

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
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
)

func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("Invalid endpoint: %v", ep)
}

func NewControllerServer(d *Driver) *controllerServer {
	return &controllerServer{
		Driver:     d,
		dsmService: d.DsmService,
	}
}

func getK8sClient() clientset.Interface {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to read in-cluster config: %v", err)
	}

	client, err := clientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	return client
}

// StageRecordDir is where the node keeps its stage records. Left empty it is
// stageRecordDir(endpoint).
var StageRecordDir string

// stageRecordDir is the directory for the stage records: StageRecordDir if set,
// and otherwise "stage" next to the CSI socket. The socket's directory is the
// plugin directory, which has to be a host path that outlives the pod for
// kubelet to reach the driver, so records kept there survive a restart of the
// driver with no mount of their own. An endpoint that is not a unix socket has
// no such directory, and the default plugin directory is assumed.
func stageRecordDir(endpoint string) string {
	if StageRecordDir != "" {
		return StageRecordDir
	}
	if proto, addr, err := ParseEndpoint(endpoint); err == nil && proto == "unix" {
		return filepath.Join("/", filepath.Dir(addr), "stage")
	}
	return filepath.Join("/var/lib/kubelet/plugins", DriverName, "stage")
}

func NewNodeServer(d *Driver) *nodeServer {
	records := stageRecords{dir: stageRecordDir(d.endpoint)}
	records.sweep()
	return &nodeServer{
		stageRecords: records,
		Driver:       d,
		dsmService:   d.DsmService,
		Mounter: &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      exec.New(),
		},
		Initiator: &initiatorDriver{
			chapUser:     "",
			chapPassword: "",
			tools:        d.tools,
		},
		Client: getK8sClient(),
		tools:  d.tools,
	}
}

func NewIdentityServer(d *Driver) *identityServer {
	return &identityServer{
		Driver: d,
	}
}

func NewVolumeCapabilityAccessMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability_AccessMode {
	return &csi.VolumeCapability_AccessMode{Mode: mode}
}

func NewControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

func NewNodeServiceCapability(cap csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

func RunControllerandNodePublishServer(endpoint string, d *Driver, cs csi.ControllerServer, ns csi.NodeServer) {
	ids := NewIdentityServer(d)

	s := NewNonBlockingGRPCServer()
	s.Start(endpoint, ids, cs, ns)
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	log.Infof("GRPC call: %s", info.FullMethod)
	log.Infof("GRPC request: %s", protosanitizer.StripSecrets(req))
	resp, err := handler(ctx, req)
	if err != nil {
		log.Errorf("GRPC error: %v", err)
	} else {
		log.Infof("GRPC response: %s", protosanitizer.StripSecrets(resp))
	}
	return resp, err
}
