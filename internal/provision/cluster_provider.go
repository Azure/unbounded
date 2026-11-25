// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterProvider is the managed Kubernetes service or distribution that hosts
// the control plane, and what it requires of every node that joins it.
type ClusterProvider struct {
	// Name identifies the provider in logs, e.g. "microsoft-aks".
	Name string

	// NodeLabels must be on every node. They override the Machine's own
	// labels; see BuildAgentConfigParams.ProviderLabels.
	NodeLabels map[string]string
}

// A clusterProviderDetector returns its provider when the cluster runs on it,
// and nil when it does not.
type clusterProviderDetector func(context.Context, kubernetes.Interface) (*ClusterProvider, error)

// clusterProviderDetectors lists the known providers. The first match wins.
// To add a provider, add its detector here. A detector that reads a ConfigMap
// also needs that ConfigMap's name allowed in the metalman and machine-ops
// RBAC, which name the kube-public ConfigMaps they may read.
var clusterProviderDetectors = []clusterProviderDetector{detectAKS}

// DetectClusterProvider returns the provider hosting the cluster, or nil when
// it is unknown (e.g. kubeadm or k3s). An error means detection failed, not
// that the provider is unknown.
func DetectClusterProvider(ctx context.Context, k kubernetes.Interface) (*ClusterProvider, error) {
	return detectClusterProvider(ctx, k, clusterProviderDetectors)
}

func detectClusterProvider(ctx context.Context, k kubernetes.Interface, detectors []clusterProviderDetector) (*ClusterProvider, error) {
	for _, detect := range detectors {
		provider, err := detect(ctx, k)
		if err != nil {
			return nil, err
		}

		if provider != nil {
			return provider, nil
		}
	}

	return nil, nil
}

// detectAKS matches clusters that publish the aks-cluster-metadata ConfigMap
// in kube-public.
func detectAKS(ctx context.Context, k kubernetes.Interface) (*ClusterProvider, error) {
	_, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "aks-cluster-metadata", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("detect AKS: get aks-cluster-metadata ConfigMap: %w", err)
	}

	return &ClusterProvider{
		Name:       "microsoft-aks",
		NodeLabels: map[string]string{"kubernetes.azure.com/managed": "false"},
	}, nil
}
