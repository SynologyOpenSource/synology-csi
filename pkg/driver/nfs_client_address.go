// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	clientset "k8s.io/client-go/kubernetes"
)

const NFSClientAddressInternalIP = "internalIP"

// NFSClientAddressConfig selects the addresses used for DSM NFS privileges.
// Its zero value uses Kubernetes Node InternalIP addresses.
type NFSClientAddressConfig struct {
	annotationKey string
}

// NewNFSClientAddressConfig validates the configured address source and annotation key.
func NewNFSClientAddressConfig(source, annotationKey string) (NFSClientAddressConfig, error) {
	switch source {
	case NFSClientAddressInternalIP:
		if annotationKey != "" {
			return NFSClientAddressConfig{}, fmt.Errorf("--nfs-client-address-annotation-key requires --nfs-client-address-source=annotation")
		}
		return NFSClientAddressConfig{}, nil
	case "annotation":
		if annotationKey == "" {
			return NFSClientAddressConfig{}, fmt.Errorf("--nfs-client-address-source=annotation requires --nfs-client-address-annotation-key")
		}
		// The Kubernetes API validates annotation keys in lowercase but stores
		// them with the original spelling, which we use for the lookup.
		if problems := validation.IsQualifiedName(strings.ToLower(annotationKey)); len(problems) != 0 {
			return NFSClientAddressConfig{}, fmt.Errorf("invalid --nfs-client-address-annotation-key %q: %s", annotationKey, strings.Join(problems, ", "))
		}
		return NFSClientAddressConfig{annotationKey: annotationKey}, nil
	default:
		return NFSClientAddressConfig{}, fmt.Errorf("invalid --nfs-client-address-source %q: expected internalIP or annotation", source)
	}
}

func (cfg NFSClientAddressConfig) nodeAddresses(ctx context.Context, client clientset.Interface) ([]string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Kubernetes Nodes: %w", err)
	}
	if cfg.annotationKey == "" {
		var ips []string
		for _, node := range nodes.Items {
			for _, address := range node.Status.Addresses {
				if address.Type == corev1.NodeInternalIP {
					ips = append(ips, address.Address)
				}
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no Kubernetes Node InternalIP addresses found")
		}
		return ips, nil
	}

	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no Kubernetes Nodes found for NFS client address discovery")
	}
	ips := make([]string, 0, len(nodes.Items))
	seen := make(map[netip.Addr]struct{}, len(nodes.Items))
	for _, node := range nodes.Items {
		value, ok := node.Annotations[cfg.annotationKey]
		if !ok {
			return nil, fmt.Errorf("Node %q is missing NFS client address annotation %q", node.Name, cfg.annotationKey)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("Node %q has an empty NFS client address annotation %q", node.Name, cfg.annotationKey)
		}
		parsed, err := netip.ParseAddr(value)
		ip := parsed.Unmap()
		if err != nil || parsed.Zone() != "" || !ip.IsGlobalUnicast() {
			return nil, fmt.Errorf("Node %q annotation %q must contain a single unscoped unicast IPv4 or IPv6 address", node.Name, cfg.annotationKey)
		}
		if _, exists := seen[ip]; !exists {
			seen[ip] = struct{}{}
			ips = append(ips, ip.String())
		}
	}
	sort.Strings(ips)
	return ips, nil
}
