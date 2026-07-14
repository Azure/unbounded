// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package clusterinfo discovers the external Kubernetes API server endpoint and
// trust anchor from the standard cluster-info ConfigMap in the kube-public
// namespace. Every conformant cluster (kubeadm, kind, AKS, ...) publishes this
// ConfigMap, making it the canonical way to discover the externally-reachable
// API server URL without requiring it to be configured explicitly.
package clusterinfo

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// ClusterInfo holds the API server URL and CA certificate discovered from the
// standard cluster-info ConfigMap in kube-public.
type ClusterInfo struct {
	// ApiserverURL is the external API server URL (e.g. "https://10.0.0.1:6443").
	ApiserverURL string
	// CACertPEM is the PEM-encoded cluster CA certificate.
	CACertPEM []byte
}

// Resolve reads the standard cluster-info ConfigMap from the kube-public
// namespace and returns both the API server URL and the CA certificate from the
// embedded kubeconfig. Every conformant cluster publishes this ConfigMap,
// making it the canonical way to discover the external API server endpoint and
// trust anchor.
func Resolve(ctx context.Context, clientset kubernetes.Interface) (*ClusterInfo, error) {
	cm, err := clientset.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get cluster-info ConfigMap from kube-public: %w", err)
	}

	kubeconfig, ok := cm.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig key not found in cluster-info ConfigMap")
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig from cluster-info ConfigMap: %w", err)
	}

	if cfg.Host == "" {
		return nil, fmt.Errorf("cluster-info kubeconfig has no server URL")
	}

	if len(cfg.CAData) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig has no CA certificate")
	}

	return &ClusterInfo{
		ApiserverURL: cfg.Host,
		CACertPEM:    cfg.CAData,
	}, nil
}

// ResolveApiserverURL reads the standard cluster-info ConfigMap from the
// kube-public namespace and returns the Kubernetes API server URL contained in
// the embedded kubeconfig.
//
// Deprecated: Use Resolve instead, which also returns the CA certificate from
// the same kubeconfig.
func ResolveApiserverURL(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	info, err := Resolve(ctx, clientset)
	if err != nil {
		return "", err
	}

	return info.ApiserverURL, nil
}
