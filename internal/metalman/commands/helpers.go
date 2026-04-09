// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
)

const siteLabel = "unbounded-kube.io/site"

// ANSI color/style codes for terminal output.
const (
	bold  = "\033[1m"
	dim   = "\033[2m"
	reset = "\033[0m"
	green = "\033[32m"
	cyan  = "\033[36m"
)

// StatusUpdater implements attestation.StatusUpdater using a controller-runtime client.
type StatusUpdater struct {
	Client client.Client
}

func (u *StatusUpdater) Update(ctx context.Context, node *v1alpha3.Machine) error {
	return u.Client.Status().Update(ctx, node)
}

func BuildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func SiteSelector(site string) (labels.Selector, error) {
	var (
		op   selection.Operator
		vals []string
	)

	if site != "" {
		op = selection.Equals
		vals = []string{site}
	} else {
		op = selection.DoesNotExist
	}

	req, err := labels.NewRequirement(siteLabel, op, vals)
	if err != nil {
		return nil, err
	}

	return labels.NewSelector().Add(*req), nil
}

func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".unbounded", "metalman", "cache")
	}

	return filepath.Join(home, ".unbounded", "metalman", "cache")
}

func LeaderElectionID(site string) string {
	if site == "" {
		return "metalman"
	}

	return "metalman-" + site
}

func PrintStep(msg string) {
	fmt.Printf("  %s-->%s %s\n", cyan, reset, msg)
}

func PrintConfig(key, value string) {
	fmt.Printf("  %s%-18s%s %s\n", dim, key, reset, value)
}

func PrintReady() {
	fmt.Printf("\n  %s%sready%s\n\n", green, bold, reset)
}

func PrintService(protocol, address string) {
	fmt.Printf("  %s%-8s%s %s\n", bold, protocol, reset, address)
}

func OutboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // Best-effort close of UDP probe connection.

	return conn.LocalAddr().(*net.UDPAddr).IP, nil //nolint:errcheck // Type is guaranteed by net.Dial("udp", ...).
}

// InterfaceForIP returns the name of the network interface that holds the given IP address.
func InterfaceForIP(ip net.IP) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("listing network interfaces: %w", err)
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			slog.Warn("unable to read addresses from interface %q: %s", iface.Name, err)
			continue
		}

		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			if ipnet.IP.Equal(ip) {
				return iface.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no interface found for IP %s", ip)
}

// ClusterInfo holds the API server URL and CA certificate discovered from
// the standard cluster-info ConfigMap in kube-public.
type ClusterInfo struct {
	// ApiserverURL is the external API server URL (e.g. "https://10.0.0.1:6443").
	ApiserverURL string
	// CACertPEM is the PEM-encoded cluster CA certificate.
	CACertPEM []byte
}

// ResolveClusterInfo reads the standard cluster-info ConfigMap from the
// kube-public namespace and returns both the API server URL and the CA
// certificate from the embedded kubeconfig. Every conformant cluster
// publishes this ConfigMap, making it the canonical way to discover the
// external API server endpoint and trust anchor.
func ResolveClusterInfo(ctx context.Context, clientset kubernetes.Interface) (*ClusterInfo, error) {
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

	if len(cfg.TLSClientConfig.CAData) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig has no CA certificate")
	}

	return &ClusterInfo{
		ApiserverURL: cfg.Host,
		CACertPEM:    cfg.TLSClientConfig.CAData,
	}, nil
}

// ResolveApiserverURL reads the standard cluster-info ConfigMap from the
// kube-public namespace and returns the Kubernetes API server URL contained
// in the embedded kubeconfig. Every conformant cluster publishes this
// ConfigMap, making it the canonical way to discover the external API
// server endpoint.
//
// Deprecated: Use ResolveClusterInfo instead, which also returns the CA
// certificate from the same kubeconfig.
func ResolveApiserverURL(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	info, err := ResolveClusterInfo(ctx, clientset)
	if err != nil {
		return "", err
	}

	return info.ApiserverURL, nil
}

func InterfaceIPv4(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4, nil
		}
	}

	return nil, fmt.Errorf("no IPv4 address on interface %s", name)
}
