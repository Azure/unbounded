// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cloudprovider

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// ExcludeFromCloudProviderLabel prevents the cloud controller manager
	// from trying to manage a node that was not provisioned by the cloud
	// provider. This label must be set on every node added by unbounded.
	ExcludeFromCloudProviderLabel = "node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider"
)

// CommonDefaultLabels returns labels that must be applied to every node
// provisioned by unbounded, regardless of the detected cloud provider.
func CommonDefaultLabels() map[string]string {
	return map[string]string{
		ExcludeFromCloudProviderLabel: "true",
	}
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
