package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ClusterInfo holds cluster-level values resolved once at startup and passed
// to bootstrap scripts as environment variables.
type ClusterInfo struct {
	// APIServer is the HTTPS endpoint of the Kubernetes API server
	// (e.g. "my-cluster-dns-abc123.hcp.eastus.azmk8s.io:443").
	APIServer string

	// CACertBase64 is the base64-encoded cluster CA certificate from the
	// kube-root-ca.crt ConfigMap in the kube-public namespace.
	CACertBase64 string

	// ClusterDNS is the ClusterIP of the kube-dns Service in kube-system.
	ClusterDNS string

	// Provider is the detected cluster provider (e.g. AKS). It is nil
	// when the provider cannot be determined.
	Provider Provider

	// KubeVersion is the Kubernetes server version (e.g. "v1.34.2") obtained
	// from the API server's /version endpoint. It is used as the default
	// KUBE_VERSION for bootstrap scripts unless the Machine's
	// Spec.Kubernetes.Version overrides it.
	KubeVersion string
}

// ResolveClusterInfo queries the Kubernetes API to populate all four dynamic
// values needed by the bootstrap script. It is intended to be called once at
// controller startup so the results can be reused across reconcile loops.
func ResolveClusterInfo(ctx context.Context, cfg Config, k kubernetes.Interface) (*ClusterInfo, error) {
	logger := ctrl.Log.WithName("cluster-info")
	info := &ClusterInfo{}

	if cfg.APIServerEndpoint == "" {
		return nil, fmt.Errorf("API server endpoint not set in config")
	}

	cfg.APIServerEndpoint = strings.TrimPrefix(cfg.APIServerEndpoint, "https://")
	info.APIServer = cfg.APIServerEndpoint

	logger.Info("Resolved API server", "apiServer", info.APIServer)

	// ---------------------------------------------------------------
	// CA_CERT_BASE64 – from kube-root-ca.crt ConfigMap in kube-public.
	// ---------------------------------------------------------------
	cm, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-root-ca.crt ConfigMap from kube-public: %w", err)
	}

	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("ca.crt key not found in kube-root-ca.crt ConfigMap")
	}

	info.CACertBase64 = base64.StdEncoding.EncodeToString([]byte(caCert))
	logger.Info("Resolved CA certificate", "base64Length", len(info.CACertBase64))

	// ---------------------------------------------------------------
	// CLUSTER_DNS – ClusterIP of the kube-dns Service in kube-system.
	// ---------------------------------------------------------------
	svc, err := k.CoreV1().Services(metav1.NamespaceSystem).Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-dns Service from kube-system: %w", err)
	}

	if svc.Spec.ClusterIP == "" {
		return nil, fmt.Errorf("kube-dns Service has no ClusterIP")
	}

	info.ClusterDNS = svc.Spec.ClusterIP
	logger.Info("Resolved cluster DNS", "clusterDNS", info.ClusterDNS)

	// ---------------------------------------------------------------
	// Provider – detect the cluster provider (e.g. AKS).
	// ---------------------------------------------------------------
	provider, err := detectProvider(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("detect provider: %w", err)
	}

	info.Provider = provider

	// ---------------------------------------------------------------
	// KUBE_VERSION – Kubernetes server version from the /version endpoint.
	// Used as the default for bootstrap scripts; Machine's
	// Spec.Kubernetes.Version can override it.
	// ---------------------------------------------------------------
	sv, err := k.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}

	info.KubeVersion = sv.GitVersion
	logger.Info("Resolved Kubernetes version", "kubeVersion", info.KubeVersion)

	// ---------------------------------------------------------------
	// Summary
	// ---------------------------------------------------------------
	providerID := "none"
	if info.Provider != nil {
		providerID = info.Provider.ID()
	}

	logger.Info("All cluster info resolved successfully",
		"apiServer", info.APIServer,
		"caCertBase64Length", len(info.CACertBase64),
		"clusterDNS", info.ClusterDNS,
		"provider", providerID,
		"kubeVersion", info.KubeVersion,
	)

	return info, nil
}
