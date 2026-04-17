// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// RuntimeConfig is the root YAML runtime configuration schema.
type RuntimeConfig struct {
	Common     CommonRuntimeConfig     `yaml:"common"`
	Controller ControllerRuntimeConfig `yaml:"controller"`
	Node       NodeRuntimeConfig       `yaml:"node"`
}

// CommonRuntimeConfig contains settings shared by controller and node binaries.
type CommonRuntimeConfig struct {
	AzureTenantID string `yaml:"azureTenantId"`
	LogLevel      *int   `yaml:"logLevel"`
	ApiserverURL  string `yaml:"apiserverURL"`
}

// ControllerRuntimeConfig contains controller-specific runtime settings.
type ControllerRuntimeConfig struct {
	InformerResyncPeriod        string                       `yaml:"informerResyncPeriod"`
	HealthPort                  *int                         `yaml:"healthPort"`
	NodeAgentHealthPort         *int                         `yaml:"nodeAgentHealthPort"`
	StatusStaleThreshold        string                       `yaml:"statusStaleThreshold"`
	StatusWSKeepaliveInterval   string                       `yaml:"statusWebsocketKeepaliveInterval"`
	StatusWSKeepaliveFailCount  *int                         `yaml:"statusWsKeepaliveFailureCount"`
	RegisterAggregatedAPIServer *bool                        `yaml:"registerAggregatedAPIServer"`
	RequireDashboardAuth        *bool                        `yaml:"requireDashboardAuth"`
	KubeProxyHealthInterval     string                       `yaml:"kubeProxyHealthInterval"`
	LeaderElection              ControllerLeaderElectionYAML `yaml:"leaderElection"`
}

// ControllerLeaderElectionYAML configures controller leader election behavior.
type ControllerLeaderElectionYAML struct {
	Enabled           *bool  `yaml:"enabled"`
	LeaseDuration     string `yaml:"leaseDuration"`
	RenewDeadline     string `yaml:"renewDeadline"`
	RetryPeriod       string `yaml:"retryPeriod"`
	ResourceNamespace string `yaml:"resourceNamespace"`
	ResourceName      string `yaml:"resourceName"`
}

// NodeRuntimeConfig contains node-agent runtime settings.
type NodeRuntimeConfig struct {
	InformerResyncPeriod string `yaml:"informerResyncPeriod"`
	NodeName             string `yaml:"nodeName"`
	CNIConfDir           string `yaml:"cniConfDir"`
	CNIConfFile          string `yaml:"cniConfFile"`
	BridgeName           string `yaml:"bridgeName"`
	WireGuardDir         string `yaml:"wireGuardDir"`
	WireGuardPort        *int   `yaml:"wireGuardPort"`
	// Deprecated: EnablePolicyRouting enables connmark/fwmark/ip-rule policy
	// routing on gateway interfaces. Replaced by per-interface FORWARD ACCEPT
	// rules. Defaults to false; retained for backward compatibility.
	EnablePolicyRouting                  *bool  `yaml:"enablePolicyRouting"`
	MTU                                  *int   `yaml:"mtu"`
	HealthPort                           *int   `yaml:"healthPort"`
	StatusPushEnabled                    *bool  `yaml:"statusPushEnabled"`
	StatusPushURL                        string `yaml:"statusPushURL"`
	StatusPushInterval                   string `yaml:"statusPushInterval"`
	StatusPushAPIServerInterval          string `yaml:"statusPushApiserverInterval"`
	StatusPushDelta                      *bool  `yaml:"statusPushDelta"`
	StatusWSEnabled                      *bool  `yaml:"statusWebsocketEnabled"`
	StatusWSURL                          string `yaml:"statusWebsocketURL"`
	StatusWSAPIServerMode                string `yaml:"statusWebsocketApiserverMode"`
	StatusWSAPIServerURL                 string `yaml:"statusWebsocketApiserverURL"`
	StatusWSAPIServerStartupDelay        string `yaml:"statusWebsocketApiserverStartupDelay"`
	StatusWSKeepaliveInterval            string `yaml:"statusWebsocketKeepaliveInterval"`
	StatusWSKeepaliveFailCount           *int   `yaml:"statusWsKeepaliveFailureCount"`
	RemoveConfigurationOnShutdown        *bool  `yaml:"removeConfigurationOnShutdown"`
	ShutdownRemoveWireGuardConfiguration *bool  `yaml:"shutdownRemoveWireGuardConfiguration"` // Deprecated: use RemoveConfigurationOnShutdown
	ShutdownRemoveIPRoutes               *bool  `yaml:"shutdownRemoveIPRoutes"`               // Deprecated: use RemoveConfigurationOnShutdown
	ShutdownRemoveMasqueradeRules        *bool  `yaml:"shutdownRemoveMasqueradeRules"`        // Deprecated: use RemoveConfigurationOnShutdown
	CriticalDeltaEvery                   string `yaml:"criticalDeltaEvery"`
	StatsDeltaEvery                      string `yaml:"statsDeltaEvery"`
	FullSyncEvery                        string `yaml:"fullSyncEvery"`
	PreferredPrivateNetworkEncapsulation string `yaml:"preferredPrivateNetworkEncapsulation"`
	PreferredPublicNetworkEncapsulation  string `yaml:"preferredPublicNetworkEncapsulation"`
	HealthFlapMaxBackoff                 string `yaml:"healthFlapMaxBackoff"`
	KubeProxyHealthInterval              string `yaml:"kubeProxyHealthInterval"`
	RouteTableID                         *int   `yaml:"routeTableId"`
	NetlinkResyncPeriod                  string `yaml:"netlinkResyncPeriod"`
	TunnelDataplane                      string `yaml:"tunnelDataplane"`
	TunnelDataplaneMapSize               *int   `yaml:"tunnelDataplaneMapSize"`
	TunnelIPFamily                       string `yaml:"tunnelIPFamily"`
	VXLANSrcPortLow                      *int   `yaml:"vxlanSrcPortLow"`
	VXLANSrcPortHigh                     *int   `yaml:"vxlanSrcPortHigh"`
}

// LoadRuntimeConfig reads and parses a runtime config YAML file.
func LoadRuntimeConfig(path string) (*RuntimeConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime config %q: %w", path, err)
	}

	cfg := &RuntimeConfig{}
	if err := yaml.Unmarshal(content, cfg); err != nil {
		return nil, fmt.Errorf("parse runtime config %q: %w", path, err)
	}

	return cfg, nil
}

// ParseDurationField parses a duration field and annotates parse errors.
func ParseDurationField(raw, fieldName string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %w", fieldName, err)
	}

	return value, nil
}
