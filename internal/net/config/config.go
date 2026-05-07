// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package config provides configuration types for the unbounded-net-controller.
package config

import (
	"fmt"
	"time"
)

// Config holds the controller configuration.
type Config struct {
	// ConfigFile is the path to the runtime YAML config file, used for dynamic reloading.
	ConfigFile string
	// KubeconfigPath is the path to the kubeconfig file. Empty for in-cluster config.
	KubeconfigPath string
	// ApiserverURL overrides the Kubernetes API server URL. When set, this URL
	// is used instead of the in-cluster service host. Empty means use the default.
	ApiserverURL string
	// AzureTenantID is surfaced by the status UI for Azure portal links.
	AzureTenantID string
	// DryRun causes the controller to run a single evaluation and print proposed changes.
	DryRun bool
	// HealthPort is the port for the health check HTTP server. 0 disables the server.
	HealthPort int
	// NodeAgentHealthPort is the port where node agents serve their health/status endpoints.
	NodeAgentHealthPort int
	// InformerResyncPeriod is the resync period for informers.
	InformerResyncPeriod time.Duration
	// LeaderElection contains leader election configuration.
	LeaderElection LeaderElectionConfig
	// StatusStaleThreshold is the duration after which a node's pushed status is considered stale.
	// When stale, the controller falls back to pulling status directly from the node.
	StatusStaleThreshold time.Duration
	// RegisterAggregatedAPIServer controls whether the controller serves aggregated API status endpoints.
	RegisterAggregatedAPIServer bool
	// StatusWSKeepaliveInterval controls websocket ping cadence for node status streams.
	// Set to 0 to disable controller-side websocket keepalive pings.
	StatusWSKeepaliveInterval time.Duration
	// StatusWSKeepaliveFailureCount is the number of sequential websocket keepalive ping failures
	// before the controller closes a node status websocket connection.
	StatusWSKeepaliveFailureCount int
	// RequireDashboardAuth controls whether the status dashboard and JSON
	// endpoints require authentication and SubjectAccessReview authorization.
	RequireDashboardAuth bool
	// NodeMTU is the configured node MTU from the shared configmap (node.mtu).
	// Used to validate that no node's detected WireGuard MTU is lower than this value.
	// A value of 0 means the check is skipped.
	NodeMTU int
	// KubeProxyHealthInterval is the interval between kube-proxy health checks on the controller node.
	// Set to 0 to disable the check.
	KubeProxyHealthInterval time.Duration
	// NetlinkResyncPeriod is the interval between full netlink cache resyncs on node agents.
	NetlinkResyncPeriod time.Duration
	// NodeTokenLifetime is the lifetime of HMAC tokens issued to node agents.
	NodeTokenLifetime time.Duration
	// ViewerTokenLifetime is the lifetime of HMAC tokens issued to dashboard viewers.
	ViewerTokenLifetime time.Duration
}

// LeaderElectionConfig holds leader election configuration.
type LeaderElectionConfig struct {
	// Enabled indicates whether leader election is enabled.
	Enabled bool
	// LeaseDuration is the duration that non-leader candidates will wait to force acquire leadership.
	LeaseDuration time.Duration
	// RenewDeadline is the duration that the acting leader will retry refreshing leadership before giving up.
	RenewDeadline time.Duration
	// RetryPeriod is the duration the LeaderElector clients should wait between tries of actions.
	RetryPeriod time.Duration
	// ResourceNamespace is the namespace in which the leader election resource will be created.
	ResourceNamespace string
	// ResourceName is the name of the leader election resource.
	ResourceName string
}

// DefaultLeaderElectionConfig returns the default leader election configuration.
func DefaultLeaderElectionConfig() LeaderElectionConfig {
	return LeaderElectionConfig{
		Enabled:           true,
		LeaseDuration:     15 * time.Second,
		RenewDeadline:     10 * time.Second,
		RetryPeriod:       2 * time.Second,
		ResourceNamespace: "kube-system",
		ResourceName:      "unbounded-net-controller",
	}
}

// Validate validates the configuration and returns an error if invalid.
// It also sets default mask sizes if not specified:
// - IPv4: defaults to /24
// - IPv6: defaults to (first CIDR prefix size + 16), e.g., /64 -> /80
func (c *Config) Validate() error {
	if c.StatusWSKeepaliveFailureCount < 1 {
		return fmt.Errorf("status websocket keepalive failure count must be >= 1")
	}

	return nil
}
