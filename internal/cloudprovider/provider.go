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

// CommonDefaultLabels returns labels that must be applied to every node
// provisioned by unbounded, regardless of the detected cloud provider.
func CommonDefaultLabels() map[string]string {
	return map[string]string{}
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
type AKSProvider struct{}

func (p *AKSProvider) ID() string {
	return "microsoft-aks"
}

func (p *AKSProvider) DefaultLabels() map[string]string {
	return map[string]string{
		"kubernetes.azure.com/managed": "false",
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

	return &AKSProvider{}, nil
}
