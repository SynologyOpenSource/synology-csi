// Copyright 2021 Synology Inc.

package helmtest

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	sourceArg = "--nfs-client-address-source="
	keyArg    = "--nfs-client-address-annotation-key="
	source    = "node.nfsClientAddress.source="
	key       = "node.nfsClientAddress.annotationKey="
)

func helmBinary(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required to test chart rendering")
	}
	return helm
}

func helmTemplate(helm string, values ...string) ([]byte, error) {
	args := []string{"template", "synology-csi", "../../deploy/helm", "--namespace", "synology-csi", "--kube-version", "1.20.0"}
	for _, value := range values {
		args = append(args, "--set-string", value)
	}
	return exec.Command(helm, args...).CombinedOutput()
}

func helmResource[T any](t *testing.T, manifests []byte, kind, name string) T {
	t.Helper()
	var resource T
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			t.Fatalf("rendered chart is missing %s %q", kind, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		var metadata struct {
			metav1.TypeMeta
			ObjectMeta metav1.ObjectMeta `json:"metadata"`
		}
		if len(raw) == 0 {
			continue
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Kind == kind && metadata.ObjectMeta.Name == name {
			if err := json.Unmarshal(raw, &resource); err != nil {
				t.Fatal(err)
			}
			return resource
		}
	}
}

func TestNFSClientAddressHelmRendering(t *testing.T) {
	helm := helmBinary(t)
	const (
		customKey     = "Storage.Example.org/NFS_Client.IP"
		defaultKey    = "nfs-client-ip"
		iscsiOverride = "--iscsiadm-path=/usr/local/sbin/iscsiadm"
		logOverride   = "--log-level=debug"
	)
	tests := []struct {
		name     string
		values   []string
		wantArgs []string
	}{
		{"default", nil, []string{sourceArg + "internalIP"}},
		{"explicit InternalIP", []string{source + "internalIP"}, []string{sourceArg + "internalIP"}},
		{"annotation", []string{source + "annotation", key + defaultKey}, []string{sourceArg + "annotation", keyArg + defaultKey}},
		{
			"custom key and extraArgs",
			[]string{source + "annotation", key + customKey, "node.extraArgs[0]=" + iscsiOverride, "node.extraArgs[1]=" + logOverride},
			[]string{sourceArg + "annotation", keyArg + customKey, iscsiOverride, logOverride},
		},
		{
			"raw extraArgs",
			[]string{"node.extraArgs[0]=" + sourceArg + "annotation", "node.extraArgs[1]=" + keyArg + customKey},
			[]string{sourceArg + "internalIP", sourceArg + "annotation", keyArg + customKey},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifests, err := helmTemplate(helm, tc.values...)
			if err != nil {
				t.Fatalf("helm template failed: %v\n%s", err, manifests)
			}
			node := helmResource[appsv1.DaemonSet](t, manifests, "DaemonSet", "synology-csi-node")
			foundPlugin := false
			for _, container := range node.Spec.Template.Spec.Containers {
				if container.Name == "csi-plugin" {
					foundPlugin = true
					index := slices.IndexFunc(container.Args, func(arg string) bool {
						return strings.HasPrefix(arg, "--nfs-client-address-")
					})
					if index < 0 || !slices.Equal(container.Args[index:], tc.wantArgs) {
						t.Errorf("CSI node plugin arguments = %v, want suffix %v", container.Args, tc.wantArgs)
					}
				} else {
					for _, arg := range container.Args {
						if strings.HasPrefix(arg, "--nfs-client-address-") {
							t.Errorf("node sidecar %q got NFS configuration %q", container.Name, arg)
						}
					}
				}
			}
			if !foundPlugin {
				t.Fatal("rendered node DaemonSet is missing the CSI plugin")
			}

			storageClass := helmResource[storagev1.StorageClass](t, manifests, "StorageClass", "synology-iscsi-storage")
			if storageClass.Provisioner != "csi.san.synology.com" ||
				storageClass.Annotations["storageclass.kubernetes.io/is-default-class"] != "false" ||
				storageClass.ReclaimPolicy == nil || *storageClass.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain ||
				storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
				t.Errorf("unexpected default StorageClass configuration: %+v", storageClass)
			}

			account := helmResource[corev1.ServiceAccount](t, manifests, "ServiceAccount", "synology-csi-node")
			role := helmResource[rbacv1.ClusterRole](t, manifests, "ClusterRole", account.Name)
			binding := helmResource[rbacv1.ClusterRoleBinding](t, manifests, "ClusterRoleBinding", role.Name)
			if node.Spec.Template.Spec.ServiceAccountName != account.Name || binding.RoleRef.Name != role.Name ||
				!slices.Contains(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: account.Name, Namespace: "synology-csi"}) {
				t.Error("node DaemonSet ServiceAccount is missing its ClusterRole binding")
			}
			if !slices.ContainsFunc(role.Rules, func(rule rbacv1.PolicyRule) bool {
				return slices.Equal(rule.APIGroups, []string{""}) &&
					slices.Equal(rule.Resources, []string{"nodes"}) &&
					slices.Equal(rule.Verbs, []string{"get", "list", "update"})
			}) {
				t.Errorf("node ClusterRole lost its upstream Node permissions: %v", role.Rules)
			}
		})
	}
}

func TestNFSClientAddressInvalidHelmValues(t *testing.T) {
	helm := helmBinary(t)
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"missing annotation key", []string{source + "annotation"}, "annotationKey"},
		{"invalid source", []string{source + "hostname"}, "internalIP"},
		{"key in InternalIP mode", []string{key + "nfs-ip"}, "annotationKey"},
		{"invalid key", []string{source + "annotation", key + "storage.example.org/bad key"}, "must be a valid Kubernetes annotation key"},
		{"long prefix", []string{source + "annotation", key + strings.Repeat("n", 254) + "/ip"}, "must be a valid Kubernetes annotation key"},
		{"long name", []string{source + "annotation", key + strings.Repeat("n", 64)}, "must be a valid Kubernetes annotation key"},
		{
			"conflicting extraArgs",
			[]string{source + "annotation", key + "nfs-ip", "node.extraArgs[0]=" + sourceArg + "internalIP"},
			"either node.nfsClientAddress or node.extraArgs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, err := helmTemplate(helm, tc.values...)
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("helm template error = %v, output = %s, want %q", err, output, tc.want)
			}
		})
	}
}
