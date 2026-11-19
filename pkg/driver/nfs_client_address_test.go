// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const testNFSAnnotationKey = "storage.example.org/nfs-client-ip"

func nfsTestNode(name, key, value string, internalIPs ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if key != "" {
		node.Annotations = map[string]string{key: value}
	}
	for _, ip := range internalIPs {
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	return node
}

func testNFSAddressConfig(t *testing.T, source, key string) NFSClientAddressConfig {
	t.Helper()
	config, err := NewNFSClientAddressConfig(source, key)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestNFSClientAddressesInternalIP(t *testing.T) {
	node := nfsTestNode("worker-1", testNFSAnnotationKey, "192.0.2.21", "2001:0db8::1", "198.51.100.21", "198.51.100.21")
	node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.21"})
	client := fake.NewSimpleClientset(node, nfsTestNode("worker-2", "", ""), nfsTestNode("worker-3", "", "", "198.51.100.10"))
	want := []string{"2001:0db8::1", "198.51.100.21", "198.51.100.21", "198.51.100.10"}

	for _, tc := range []struct {
		name   string
		config NFSClientAddressConfig
	}{
		{"default", NFSClientAddressConfig{}},
		{"explicit", testNFSAddressConfig(t, NFSClientAddressInternalIP, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.config.nodeAddresses(context.Background(), client)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("nodeAddresses() = %v, %v, want %v", got, err, want)
			}
		})
	}
}

func TestNFSClientAddressesFromAnnotation(t *testing.T) {
	// Kubernetes validates the lowercase form but preserves the key as written.
	const key = "Storage.Example.org/NFS_Client.IP"
	config := testNFSAddressConfig(t, "annotation", key)
	node := nfsTestNode("worker-1", key, " 192.0.2.22 ", "198.51.100.21")
	node.Annotations[testNFSAnnotationKey] = "192.0.2.99"
	nodes := []corev1.Node{
		*node,
		*nfsTestNode("worker-2", key, "2001:0DB8:0:0:0:0:0:21"),
		*nfsTestNode("worker-3", key, "2001:db8::21"),
		*nfsTestNode("worker-4", key, "::ffff:192.0.2.22"),
		*nfsTestNode("worker-5", key, "192.0.2.21"),
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.NodeList{Items: nodes}, nil
	})

	want := []string{"192.0.2.21", "192.0.2.22", "2001:db8::21"}
	for _, order := range []string{"original order", "reversed order"} {
		t.Run(order, func(t *testing.T) {
			got, err := config.nodeAddresses(context.Background(), client)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("nodeAddresses() = %v, %v, want %v", got, err, want)
			}
		})
		slices.Reverse(nodes)
	}
}

func TestNFSClientAddressesRejectInvalidAnnotations(t *testing.T) {
	const key = "nfs-client-ip"
	config := testNFSAddressConfig(t, "annotation", key)
	tests := []struct {
		name, key, value, want string
	}{
		{"missing", "", "", "missing"},
		{"empty", key, " \t ", "empty"},
		{"malformed", key, "192.0.2.256", "single unscoped unicast"},
		{"CIDR", key, "192.0.2.0/24", "single unscoped unicast"},
		{"wildcard", key, "*", "single unscoped unicast"},
		{"multiple IPs", key, "192.0.2.21,192.0.2.22", "single unscoped unicast"},
		{"hostname", key, "worker.example.org", "single unscoped unicast"},
		{"scoped IPv6", key, "2001:db8::21%eth0", "single unscoped unicast"},
		{"scoped mapped IPv6", key, "::ffff:192.0.2.21%eth0", "single unscoped unicast"},
		{"IPv6 with port", key, "[2001:db8::21]:2049", "single unscoped unicast"},
		{"unspecified IPv4", key, "0.0.0.0", "single unscoped unicast"},
		{"unspecified IPv6", key, "::", "single unscoped unicast"},
		{"multicast", key, "ff02::1", "single unscoped unicast"},
		{"link-local", key, "fe80::1", "single unscoped unicast"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(
				nfsTestNode("worker-1", key, "192.0.2.21"),
				nfsTestNode("worker-2", tc.key, tc.value, "198.51.100.22"),
			)
			got, err := config.nodeAddresses(context.Background(), client)
			if len(got) != 0 || err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "worker-2") || !strings.Contains(err.Error(), key) {
				t.Fatalf("nodeAddresses() = %v, %v, want an error for worker-2 and annotation %s (%s)", got, err, key, tc.want)
			}
		})
	}
}

func TestNFSClientAddressesNoEligibleNodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config NFSClientAddressConfig
		client *fake.Clientset
		want   string
	}{
		{"empty cluster", NFSClientAddressConfig{}, fake.NewSimpleClientset(), "no Kubernetes Node InternalIP addresses"},
		{"no InternalIPs", NFSClientAddressConfig{}, fake.NewSimpleClientset(nfsTestNode("worker-1", "", "")), "no Kubernetes Node InternalIP addresses"},
		{"annotation empty cluster", testNFSAddressConfig(t, "annotation", testNFSAnnotationKey), fake.NewSimpleClientset(), "no Kubernetes Nodes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.config.nodeAddresses(context.Background(), tc.client)
			if len(got) != 0 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("nodeAddresses() = %v, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestNFSClientAddressesNodeListFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	listErr := errors.New("node list denied")
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, listErr
	})
	config := testNFSAddressConfig(t, "annotation", testNFSAnnotationKey)
	if got, err := config.nodeAddresses(context.Background(), client); len(got) != 0 || !errors.Is(err, listErr) {
		t.Fatalf("nodeAddresses() = %v, %v, want wrapped list error", got, err)
	}
}
