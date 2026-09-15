// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"fmt"
	"net"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const (
	NodeExporterVersion       = "1.9.1"
	NodeExporterPort          = 9100
	NodeExporterBinaryPath    = "/usr/local/bin/node_exporter"
	NodeExporterConfigDir     = "/etc/unbounded/node-exporter"
	NodeExporterWebConfigPath = NodeExporterConfigDir + "/web-config.yml"
	NodeExporterServiceUnit   = "node-exporter.service"
)

// NodeExporter is the fully resolved node exporter goal state.
type NodeExporter struct {
	Enabled       bool
	Version       string
	ListenAddress string
	ExtraArgs     []string
	TLS           NodeExporterTLS
}

// NodeExporterTLS contains user-managed native TLS file paths.
type NodeExporterTLS struct {
	Enabled         bool
	CertificateFile string
	PrivateKeyFile  string
	ClientCAFile    string
}

func resolveNodeExporter(cfg *config.AgentConfig, downloads *DownloadOverrides) (NodeExporter, error) {
	if cfg.NodeExporter == nil || !cfg.NodeExporter.Enabled {
		return NodeExporter{}, nil
	}

	if err := cfg.Validate(); err != nil {
		return NodeExporter{}, err
	}

	version := NodeExporterVersion
	if override := nodeExporterDownloadOverride(downloads); override != nil && strings.TrimSpace(override.Version) != "" {
		version = strings.TrimPrefix(strings.TrimSpace(override.Version), "v")
	}

	address, err := resolveNodeExporterAddress(
		cfg.NodeExporter.ListenAddress,
		cfg.Kubelet.NodeIP,
		cfg.NodeName,
		defaultLocalDNSMetricsDeps(),
	)
	if err != nil {
		return NodeExporter{}, err
	}

	resolved := NodeExporter{
		Enabled:       true,
		Version:       version,
		ListenAddress: address,
		ExtraArgs:     append([]string(nil), cfg.NodeExporter.ExtraArgs...),
	}
	if cfg.NodeExporter.TLS != nil && cfg.NodeExporter.TLS.Enabled {
		resolved.TLS = NodeExporterTLS{
			Enabled:         true,
			CertificateFile: cfg.NodeExporter.TLS.CertificateFile,
			PrivateKeyFile:  cfg.NodeExporter.TLS.PrivateKeyFile,
			ClientCAFile:    cfg.NodeExporter.TLS.ClientCAFile,
		}
	}

	return resolved, nil
}

func resolveNodeExporterAddress(configured, nodeIPs, nodeName string, deps localDNSMetricsDeps) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		host, _, err := net.SplitHostPort(configured)
		if err != nil {
			return "", fmt.Errorf("parse NodeExporter.ListenAddress: %w", err)
		}

		if err := validateLocalDNSHostIP(net.ParseIP(host), deps.interfaceAddrs); err != nil {
			return "", fmt.Errorf("resolve NodeExporter.ListenAddress: %w", err)
		}

		return configured, nil
	}

	return resolveNodeServiceAddress("", nodeIPs, nodeName, "node exporter listen", NodeExporterPort, deps)
}

func validateNodeExporterListener(nodeExporter NodeExporter, localDNS LocalDNS, containerd Containerd) error {
	if !nodeExporter.Enabled {
		return nil
	}

	for description, address := range map[string]string{
		"containerd metrics": containerd.MetricsAddress,
		"kubelet":            KubeletBindAddress,
		"LocalDNS metrics":   localDNS.MetricsAddress,
	} {
		if address != "" && nodeServiceListenersConflict(nodeExporter.ListenAddress, address) {
			return fmt.Errorf("node exporter listen address %s conflicts with %s address %s", nodeExporter.ListenAddress, description, address)
		}
	}

	return nil
}

func nodeServiceListenersConflict(first, second string) bool {
	firstHost, firstPort, firstErr := net.SplitHostPort(first)

	secondHost, secondPort, secondErr := net.SplitHostPort(second)
	if firstErr != nil || secondErr != nil || firstPort != secondPort {
		return false
	}

	firstIP := net.ParseIP(firstHost)
	secondIP := net.ParseIP(secondHost)

	return firstIP != nil && secondIP != nil && (firstIP.IsUnspecified() || secondIP.IsUnspecified() || firstIP.Equal(secondIP))
}

func nodeExporterDownloadOverride(downloads *DownloadOverrides) *DownloadSource {
	if downloads == nil {
		return nil
	}

	return downloads.NodeExporter
}
