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
		defaultNodeServiceAddressResolver(),
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

func resolveNodeExporterAddress(configured, nodeIPs, nodeName string, resolver nodeServiceAddressResolver) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		host, _, err := net.SplitHostPort(configured)
		if err != nil {
			return "", fmt.Errorf("parse NodeExporter.ListenAddress: %w", err)
		}

		if err := resolver.validateHostIP(net.ParseIP(host)); err != nil {
			return "", fmt.Errorf("resolve NodeExporter.ListenAddress: %w", err)
		}

		return configured, nil
	}

	return resolver.resolve(nodeServiceAddressParams{
		nodeIPs:     nodeIPs,
		nodeName:    nodeName,
		description: "node exporter listen",
		port:        NodeExporterPort,
	})
}

func nodeExporterDownloadOverride(downloads *DownloadOverrides) *DownloadSource {
	if downloads == nil {
		return nil
	}

	return downloads.NodeExporter
}
