// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import "time"

const (
	ArchitectureAMD64 = "amd64"
	ArchitectureARM64 = "arm64"
)

type Config struct {
	ListenAddr        string
	Namespace         string
	ServiceName       string
	TLSSecretName     string
	Image             string
	ImagePullPolicy   string
	ServiceAccount    string
	AllocationTTL     time.Duration
	ReconcileInterval time.Duration

	WireGuardHostPortStart int32
	WireGuardHostPortEnd   int32
	EndpointListenPort     int
	RedfishPort            int
	VXLANPort              int
	GuestCIDR              string
	GuestDNS               []string
	DefaultDiskSize        string
	DefaultMemory          string
	DefaultCPUs            int
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:             ":8443",
		Namespace:              "playpen",
		ServiceName:            "playpen-operator",
		TLSSecretName:          "playpen-operator-tls",
		Image:                  "ghcr.io/azure/playpen:latest",
		ImagePullPolicy:        "Always",
		ServiceAccount:         "playpen-operator",
		AllocationTTL:          30 * time.Minute,
		ReconcileInterval:      30 * time.Second,
		WireGuardHostPortStart: 51820,
		WireGuardHostPortEnd:   51899,
		EndpointListenPort:     51820,
		RedfishPort:            8443,
		VXLANPort:              4789,
		GuestCIDR:              "192.168.200.0/24",
		GuestDNS:               []string{"1.1.1.1", "8.8.8.8"},
		DefaultDiskSize:        "40Gi",
		DefaultMemory:          "2Gi",
		DefaultCPUs:            2,
	}
}
