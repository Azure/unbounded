// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package clusterinfo discovers the external Kubernetes API server endpoint and
// trust anchor without requiring them to be configured explicitly.
//
// The primary source is the standard cluster-info ConfigMap in the kube-public
// namespace, which kubeadm-provisioned clusters (including kind) publish. Some
// managed control planes (notably AKS) do not publish it; for those, Discover
// falls back to the KUBERNETES_SERVICE_HOST/PORT the kubelet injects, but only
// when it resolves to an external FQDN rather than the in-cluster ClusterIP (on
// AKS the kubernetes.azure.com/set-kube-service-host-fqdn pod label makes it the
// public API FQDN), pairing it with the in-cluster service-account CA.
package clusterinfo

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// inClusterCAPath is the projected service-account CA bundle the kubelet mounts
// into every pod. It is a package variable so tests can point it at a fixture.
var inClusterCAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// inClusterServiceDNSNames are the in-cluster DNS names for the API server
// Service. They resolve only from inside the cluster, so they are never a valid
// endpoint to advertise to a node that is still joining.
var inClusterServiceDNSNames = map[string]struct{}{
	"kubernetes":                           {},
	"kubernetes.default":                   {},
	"kubernetes.default.svc":               {},
	"kubernetes.default.svc.cluster.local": {},
}

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
// embedded kubeconfig. Clusters provisioned by kubeadm (including kind) publish
// this ConfigMap; Discover layers a fallback on top for those that do not.
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

// Discover resolves the external API server URL and CA certificate, preferring
// the kube-public/cluster-info ConfigMap and falling back to the
// KUBERNETES_SERVICE_HOST FQDN paired with the in-cluster service-account CA.
//
// The fallback exists for managed control planes (e.g. AKS) that do not publish
// cluster-info. It is only taken when KUBERNETES_SERVICE_HOST is an external
// FQDN (see KubeServiceHostEndpoint); an in-cluster ClusterIP is rejected so we
// never advertise an endpoint a joining node cannot reach.
func Discover(ctx context.Context, clientset kubernetes.Interface) (*ClusterInfo, error) {
	info, cmErr := Resolve(ctx, clientset)
	if cmErr == nil {
		return info, nil
	}

	endpoint, ok := KubeServiceHostEndpoint()
	if !ok {
		return nil, fmt.Errorf("cluster-info discovery failed (%w) and no external KUBERNETES_SERVICE_HOST FQDN is available", cmErr)
	}

	caPEM, caErr := InClusterCA()
	if caErr != nil {
		return nil, fmt.Errorf("cluster-info discovery failed (%w); reading in-cluster CA for the KUBERNETES_SERVICE_HOST fallback: %w", cmErr, caErr)
	}

	return &ClusterInfo{ApiserverURL: endpoint, CACertPEM: caPEM}, nil
}

// KubeServiceHostEndpoint builds the API server URL from the
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT_HTTPS/PORT environment
// variables the kubelet injects into pods.
//
// It returns ok=false unless the host is an external FQDN: an empty value, an IP
// literal (the in-cluster ClusterIP), or an in-cluster Service DNS name
// (kubernetes.default.svc, ...) is rejected, since none of those are reachable
// by a node that is still joining the cluster. On AKS the
// kubernetes.azure.com/set-kube-service-host-fqdn pod label makes this env the
// public API FQDN, which is accepted.
func KubeServiceHostEndpoint() (string, bool) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return "", false
	}

	if net.ParseIP(host) != nil {
		return "", false
	}

	if _, ok := inClusterServiceDNSNames[strings.ToLower(strings.TrimSuffix(host, "."))]; ok {
		return "", false
	}

	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}

	if port == "" {
		port = "443"
	}

	return "https://" + net.JoinHostPort(host, port), true
}

// InClusterCA reads the service-account CA bundle the kubelet mounts into every
// pod. It is the cluster CA and signs the API server serving certificate
// (including the AKS FQDN SAN), so it is the correct trust anchor for the
// KUBERNETES_SERVICE_HOST fallback when cluster-info is unavailable.
func InClusterCA() ([]byte, error) {
	caPEM, err := os.ReadFile(inClusterCAPath)
	if err != nil {
		return nil, fmt.Errorf("read in-cluster CA %q: %w", inClusterCAPath, err)
	}

	if len(caPEM) == 0 {
		return nil, fmt.Errorf("in-cluster CA %q is empty", inClusterCAPath)
	}

	return caPEM, nil
}
