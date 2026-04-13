// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cloudprovider

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// CommonDefaultLabels returns labels that must be applied to every node
// provisioned by unbounded, regardless of the detected cloud provider.
func CommonDefaultLabels() map[string]string {
	return map[string]string{}
}

// kubeletAllowedPrefixes are the kubernetes.io sub-domains that the kubelet
// is allowed to self-apply via --node-labels.
var kubeletAllowedPrefixes = []string{
	"kubelet.kubernetes.io/",
	"node.kubernetes.io/",
}

// kubeletAllowedLabels is the exact set of kubernetes.io / k8s.io labels
// that the kubelet may self-apply (legacy well-known labels).
var kubeletAllowedLabels = map[string]bool{
	"beta.kubernetes.io/arch":                  true,
	"beta.kubernetes.io/instance-type":         true,
	"beta.kubernetes.io/os":                    true,
	"failure-domain.beta.kubernetes.io/region": true,
	"failure-domain.beta.kubernetes.io/zone":   true,
	"kubernetes.io/arch":                       true,
	"kubernetes.io/hostname":                   true,
	"kubernetes.io/os":                         true,
	"node.kubernetes.io/instance-type":         true,
	"topology.kubernetes.io/region":            true,
	"topology.kubernetes.io/zone":              true,
}

// IsKubeletAllowedLabel reports whether the given label key may be passed to
// the kubelet via --node-labels. Labels outside the kubernetes.io and k8s.io
// namespaces are always allowed. Labels within those namespaces must be in
// the kubelet's allowed prefix set or exact allowed set.
func IsKubeletAllowedLabel(key string) bool {
	// Split on "/" to find the domain part. Labels without a "/" have no
	// domain and are always allowed.
	slash := strings.IndexByte(key, '/')
	if slash < 0 {
		return true
	}

	domain := key[:slash]

	// Labels outside the kubernetes.io/k8s.io namespaces are always allowed.
	if !strings.HasSuffix(domain, ".kubernetes.io") &&
		!strings.HasSuffix(domain, ".k8s.io") &&
		domain != "kubernetes.io" &&
		domain != "k8s.io" {
		return true
	}

	// Check the exact allowed set.
	if kubeletAllowedLabels[key] {
		return true
	}

	// Check allowed prefixes.
	for _, prefix := range kubeletAllowedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}

// PartitionNodeLabels splits labels into two maps: those that kubelet may
// self-apply via --node-labels, and those that require a controller to apply
// after the node has registered.
func PartitionNodeLabels(labels map[string]string) (kubeletLabels, controllerLabels map[string]string) {
	kubeletLabels = make(map[string]string, len(labels))
	controllerLabels = make(map[string]string)

	for k, v := range labels {
		if IsKubeletAllowedLabel(k) {
			kubeletLabels[k] = v
		} else {
			controllerLabels[k] = v
		}
	}

	return kubeletLabels, controllerLabels
}

// Provider represents a Kubernetes cluster provider and its default
// node configuration for unbounded machines.
type Provider interface {
	// ID returns a stable identifier for the provider (e.g. "microsoft-aks").
	ID() string

	// DefaultLabels returns labels that must be present on every node
	// provisioned in this cluster. These labels take precedence over
	// user-specified labels.
	DefaultLabels() map[string]string
}

// AKSProvider implements Provider for Azure Kubernetes Service clusters.
type AKSProvider struct {
	// ClusterName is the value of the kubernetes.azure.com/cluster label
	// read from a system-mode node.
	ClusterName string
}

func (p *AKSProvider) ID() string {
	return "microsoft-aks"
}

func (p *AKSProvider) DefaultLabels() map[string]string {
	return map[string]string{
		"kubernetes.azure.com/managed": "false",
		"kubernetes.azure.com/cluster": p.ClusterName,
	}
}

// DetectProvider probes the cluster to identify the Kubernetes provider.
// It returns nil when the provider cannot be determined (e.g. vanilla
// Kubernetes, on-prem, k3s). A non-nil error indicates a transient
// failure during detection, not an unknown provider.
func DetectProvider(ctx context.Context, k kubernetes.Interface) (Provider, error) {
	logger := ctrl.Log.WithName("provider-detection")

	// AKS: check for the aks-cluster-metadata ConfigMap in kube-public.
	_, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "aks-cluster-metadata", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		logger.Info("No known provider detected")
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get aks-cluster-metadata ConfigMap: %w", err)
	}

	logger.Info("AKS provider detected")

	// Read the kubernetes.azure.com/cluster label from a system-mode node.
	nodes, err := k.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.azure.com/mode=system",
		Limit:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("list system-mode nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("AKS detected but no nodes found with label kubernetes.azure.com/mode=system")
	}

	clusterName, ok := nodes.Items[0].Labels["kubernetes.azure.com/cluster"]
	if !ok || clusterName == "" {
		return nil, fmt.Errorf("AKS detected but system-mode node %q is missing kubernetes.azure.com/cluster label",
			nodes.Items[0].Name)
	}

	logger.Info("Resolved AKS cluster name", "clusterName", clusterName)

	return &AKSProvider{ClusterName: clusterName}, nil
}
