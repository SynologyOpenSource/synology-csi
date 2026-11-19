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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func TestWithXfsMountOptions(t *testing.T) {
	tests := []struct {
		name    string
		fsType  string
		options []string
		want    []string
	}{
		{
			name:    "xfs gets nouuid so clones can be mounted next to their source",
			fsType:  "xfs",
			options: []string{"rw"},
			want:    []string{"rw", "nouuid"},
		},
		{
			name:    "nouuid is not repeated when the user already asked for it",
			fsType:  "xfs",
			options: []string{"rw", "nouuid"},
			want:    []string{"rw", "nouuid"},
		},
		{
			name:    "ext4 is left untouched",
			fsType:  "ext4",
			options: []string{"rw"},
			want:    []string{"rw"},
		},
		{
			name:    "an unset fsType is left untouched",
			fsType:  "",
			options: []string{"rw"},
			want:    []string{"rw"},
		},
		{
			name:    "other user options are preserved",
			fsType:  "xfs",
			options: []string{"rw", "noatime"},
			want:    []string{"rw", "noatime", "nouuid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withXfsMountOptions(tt.fsType, tt.options)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("withXfsMountOptions(%q, %v) = %v, want %v",
					tt.fsType, tt.options, got, tt.want)
			}
		})
	}
}

const testNFSShareName = "test-share"

type nfsTestDSMService struct {
	interfaces.IDsmService
	dsm *webapi.DSM
}

func (s nfsTestDSMService) GetDsm(ip string) (*webapi.DSM, error) {
	if ip != s.dsm.Ip {
		return nil, fmt.Errorf("unexpected DSM address %q", ip)
	}
	return s.dsm, nil
}

// nfsTestPrivileges emulates the DSM rule load, save, and read-back calls.
// A successful save replaces the share's list of rules.
type nfsTestPrivileges struct {
	mu    sync.Mutex
	rules []webapi.PrivilegeRule
	saves int
	loads int
}

func (p *nfsTestPrivileges) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("api") != "SYNO.Core.FileServ.NFS.SharePrivilege" ||
		query.Get("share_name") != strconv.Quote(testNFSShareName) {
		http.Error(w, "unexpected DSM request", http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch query.Get("method") {
	case "load":
		p.loads++
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    webapi.SharePrivilege{ShareName: testNFSShareName, Rule: p.rules},
		})
	case "save":
		var rules []webapi.PrivilegeRule
		if err := json.Unmarshal([]byte(query.Get("rule")), &rules); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.saves++
		p.rules = rules
		fmt.Fprint(w, `{"success":true}`)
	default:
		http.Error(w, "unexpected NFS privilege method", http.StatusBadRequest)
	}
}

func (p *nfsTestPrivileges) assertRules(t *testing.T, saves, loads int, clients ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saves != saves || p.loads != loads {
		t.Errorf("DSM privilege calls = %d saves, %d loads, want %d saves, %d loads", p.saves, p.loads, saves, loads)
	}
	var got []string
	for _, rule := range p.rules {
		got = append(got, rule.Client)
		if rule.Privilege != string(utils.AuthTypeReadWrite) {
			t.Errorf("privilege for %s = %q, want read-write", rule.Client, rule.Privilege)
		}
	}
	if !slices.Equal(got, clients) {
		t.Errorf("DSM NFS clients = %v, want %v", got, clients)
	}
}

func newNFSPrivilegeTestService(t *testing.T, privileges *nfsTestPrivileges) nfsTestDSMService {
	t.Helper()
	server := httptest.NewServer(privileges)
	t.Cleanup(server.Close)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return nfsTestDSMService{dsm: &webapi.DSM{Ip: host, Port: port}}
}

func newNFSNodeServerForTest(t *testing.T, nodeID string, config NFSClientAddressConfig, client *fake.Clientset, service nfsTestDSMService) *nodeServer {
	t.Helper()
	driver, err := NewControllerAndNodeDriverWithConfig(nodeID, "", service, tools{}, config)
	if err != nil {
		t.Fatal(err)
	}
	return &nodeServer{Driver: driver, dsmService: service, Client: client}
}

func TestNodeStageNFSVolumeRefreshesClientAddresses(t *testing.T) {
	ctx := context.Background()
	first := nfsTestNode("worker-1", testNFSAnnotationKey, "192.0.2.21", "198.51.100.21")
	client := fake.NewSimpleClientset(first)
	config := testNFSAddressConfig(t, "annotation", testNFSAnnotationKey)
	privileges := &nfsTestPrivileges{}
	service := newNFSPrivilegeTestService(t, privileges)
	ns := newNFSNodeServerForTest(t, first.Name, config, client, service)

	stage := func(ns *nodeServer) error {
		source := "//" + service.dsm.Ip + "/" + testNFSShareName
		_, err := ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          "existing-nfs-volume",
			StagingTargetPath: t.TempDir(),
			VolumeCapability:  &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{}},
			VolumeContext:     map[string]string{"protocol": utils.ProtocolNfs, "source": source},
		})
		return err
	}
	if err := stage(ns); err != nil {
		t.Fatal(err)
	}
	privileges.assertRules(t, 1, 2, "192.0.2.21")
	if err := stage(ns); err != nil {
		t.Fatal(err)
	}
	privileges.assertRules(t, 1, 3, "192.0.2.21")

	nodes := client.CoreV1().Nodes()
	second, err := nodes.Create(ctx, nfsTestNode("worker-2", testNFSAnnotationKey, "192.0.2.22", "198.51.100.22"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns = newNFSNodeServerForTest(t, second.Name, config, client, service)
	if err := stage(ns); err != nil {
		t.Fatal(err)
	}
	privileges.assertRules(t, 2, 5, "192.0.2.21", "192.0.2.22")

	second.Annotations[testNFSAnnotationKey] = "192.0.2.23"
	if _, err := nodes.Update(ctx, second, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stage(ns); err != nil {
		t.Fatal(err)
	}
	privileges.assertRules(t, 3, 7, "192.0.2.21", "192.0.2.23")

	if err := nodes.Delete(ctx, first.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stage(ns); err != nil {
		t.Fatal(err)
	}
	// Existing DSM rules can outlive node removal when remaining clients are already allowed.
	privileges.assertRules(t, 3, 8, "192.0.2.21", "192.0.2.23")

	if _, err := nodes.Create(ctx, nfsTestNode("worker-3", "", "", "198.51.100.23"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stage(ns); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "worker-3") {
		t.Fatalf("staging error = %v, want missing annotation error for worker-3", err)
	}
	privileges.assertRules(t, 3, 8, "192.0.2.21", "192.0.2.23")
}

type nfsRecoveryMounter struct {
	*mount.FakeMounter
	mounts int
}

func (m *nfsRecoveryMounter) Mount(source, target, fsType string, options []string) error {
	m.mounts++
	if m.mounts == 1 {
		return errors.New("mount.nfs: No such file or directory")
	}
	return m.FakeMounter.Mount(source, target, fsType, options)
}

func TestNodePublishNFSVolumeRestoresMissingExport(t *testing.T) {
	const (
		internalIP = "198.51.100.21"
		clientIP   = "2001:db8::21"
	)
	for _, tc := range []struct {
		name        string
		config      NFSClientAddressConfig
		key         string
		wantClient  string
		expectError bool
	}{
		{"legacy", NFSClientAddressConfig{}, testNFSAnnotationKey, internalIP, false},
		{"annotation", testNFSAddressConfig(t, "annotation", testNFSAnnotationKey), testNFSAnnotationKey, clientIP, false},
		{"missing annotation", testNFSAddressConfig(t, "annotation", testNFSAnnotationKey), "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := nfsTestNode("worker-1", tc.key, "2001:0DB8:0:0:0:0:0:21", internalIP)
			privileges := &nfsTestPrivileges{}
			if !tc.expectError {
				// Recovery must save even when these rules already exist in DSM.
				privileges.rules = []webapi.PrivilegeRule{{Client: tc.wantClient, Privilege: string(utils.AuthTypeReadWrite)}}
			}
			service := newNFSPrivilegeTestService(t, privileges)
			ns := newNFSNodeServerForTest(t, node.Name, tc.config, fake.NewSimpleClientset(node), service)
			mounter := &nfsRecoveryMounter{FakeMounter: mount.NewFakeMounter(nil)}
			ns.Mounter = &mount.SafeFormatAndMount{Interface: mounter}
			_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId:          "existing-nfs-volume",
				StagingTargetPath: t.TempDir(),
				TargetPath:        t.TempDir(),
				VolumeCapability:  &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{}},
				VolumeContext: map[string]string{
					"protocol":         utils.ProtocolNfs,
					"dsm":              service.dsm.Ip,
					"baseDir":          "/volume1/" + testNFSShareName,
					"mountPermissions": "0",
				},
			})
			if tc.expectError {
				if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "worker-1") || !strings.Contains(err.Error(), testNFSAnnotationKey) {
					t.Fatalf("publish error = %v, want missing annotation error for worker-1", err)
				}
				if mounter.mounts != 1 {
					t.Errorf("mount attempts = %d, want 1", mounter.mounts)
				}
				privileges.assertRules(t, 0, 0)
				return
			}
			if err != nil || mounter.mounts != 2 {
				t.Fatalf("publish error = %v after %d mount attempts, want success after 2 attempts", err, mounter.mounts)
			}
			privileges.assertRules(t, 1, 1, tc.wantClient)
		})
	}
}
