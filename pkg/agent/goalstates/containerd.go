// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"net"
	"path/filepath"
	"strings"
)

// Containerd describes the containerd configuration goal state.
type Containerd struct {
	SandboxImage      string
	ContainerdBinPath string
	RuncBinaryPath    string
	CNIBinDir         string
	CNIConfDir        string
	MetricsAddress    string
	NvidiaRuntime     NvidiaRuntime
	RegistryHosts     []ContainerdRegistryHost
}

// ContainerdRegistryHost describes a containerd certs.d host entry.
type ContainerdRegistryHost struct {
	Host   string
	Server string
}

// ResolveContainerd returns the containerd configuration goal state.
func ResolveContainerd(ociImage string) Containerd {
	return Containerd{
		SandboxImage:      SandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime:     resolveNvidiaRuntime(),
		RegistryHosts:     resolveContainerdRegistryHosts(ociImage),
	}
}

func resolveContainerdRegistryHosts(ociImage string) []ContainerdRegistryHost {
	host, ok := imageRegistryHost(ociImage)
	if !ok || !plainHTTPRegistryHost(host) {
		return nil
	}

	return []ContainerdRegistryHost{{
		Host:   host,
		Server: "http://" + host,
	}}
}

func imageRegistryHost(ref string) (string, bool) {
	registry, _, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok {
		return "", false
	}

	if strings.Contains(registry, ".") || strings.Contains(registry, ":") || registry == "localhost" {
		return registry, true
	}

	return "", false
}

func plainHTTPRegistryHost(host string) bool {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}

	if hostname == "localhost" {
		return true
	}

	ip := net.ParseIP(hostname)

	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
