// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/net/config"
)

func newControllerConfigTestCommand(cfg *config.Config) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	flags := cmd.Flags()
	flags.DurationVar(&cfg.InformerResyncPeriod, "informer-resync-period", 300*time.Second, "")
	flags.IntVar(&cfg.HealthPort, "health-port", 9999, "")
	flags.IntVar(&cfg.NodeAgentHealthPort, "node-agent-health-port", 9998, "")
	flags.DurationVar(&cfg.StatusStaleThreshold, "status-stale-threshold", 40*time.Second, "")
	flags.DurationVar(&cfg.StatusWSKeepaliveInterval, "status-ws-keepalive-interval", 10*time.Second, "")
	flags.IntVar(&cfg.StatusWSKeepaliveFailureCount, "status-ws-keepalive-failure-count", 2, "")
	flags.BoolVar(&cfg.RegisterAggregatedAPIServer, "register-aggregated-apiserver", true, "")
	flags.BoolVar(&cfg.LeaderElection.Enabled, "leader-elect", true, "")
	flags.DurationVar(&cfg.LeaderElection.LeaseDuration, "leader-elect-lease-duration", 15*time.Second, "")
	flags.DurationVar(&cfg.LeaderElection.RenewDeadline, "leader-elect-renew-deadline", 5*time.Second, "")
	flags.DurationVar(&cfg.LeaderElection.RetryPeriod, "leader-elect-retry-period", 10*time.Second, "")
	flags.StringVar(&cfg.LeaderElection.ResourceNamespace, "leader-elect-resource-namespace", "kube-system", "")
	flags.StringVar(&cfg.LeaderElection.ResourceName, "leader-elect-resource-name", "unbounded-net-controller", "")

	return cmd
}

// TestApplyControllerRuntimeConfig tests ApplyControllerRuntimeConfig.
func TestApplyControllerRuntimeConfig(t *testing.T) {
	tmpPath := filepath.Join(".", "runtime-config-controller-test.tmp")

	t.Cleanup(func() { _ = os.Remove(tmpPath) })

	runtimeYAML := []byte("common:\n" +
		"  azureTenantId: tenant-from-config\n" +
		"controller:\n" +
		"  informerResyncPeriod: 45s\n" +
		"  healthPort: 12000\n" +
		"  nodeAgentHealthPort: 12001\n" +
		"  statusStaleThreshold: 55s\n" +
		"  statusWebsocketKeepaliveInterval: 8s\n" +
		"  statusWsKeepaliveFailureCount: 3\n" +
		"  registerAggregatedAPIServer: false\n" +
		"  leaderElection:\n" +
		"    enabled: false\n" +
		"    leaseDuration: 20s\n" +
		"    renewDeadline: 12s\n" +
		"    retryPeriod: 3s\n" +
		"    resourceNamespace: lease-ns\n" +
		"    resourceName: lease-name\n")
	if err := os.WriteFile(tmpPath, runtimeYAML, 0o644); err != nil {
		t.Fatalf("failed to write runtime config fixture: %v", err)
	}

	cfg := &config.Config{
		LeaderElection:                config.DefaultLeaderElectionConfig(),
		InformerResyncPeriod:          300 * time.Second,
		HealthPort:                    9999,
		NodeAgentHealthPort:           9998,
		StatusStaleThreshold:          40 * time.Second,
		StatusWSKeepaliveInterval:     10 * time.Second,
		StatusWSKeepaliveFailureCount: 2,
		AzureTenantID:                 "tenant-from-env",
		RegisterAggregatedAPIServer:   true,
	}
	cmd := newControllerConfigTestCommand(cfg)

	if err := applyControllerRuntimeConfig(cmd, cfg, tmpPath); err != nil {
		t.Fatalf("applyControllerRuntimeConfig returned error: %v", err)
	}

	if cfg.InformerResyncPeriod != 45*time.Second {
		t.Fatalf("expected informer resync period from runtime config, got %s", cfg.InformerResyncPeriod)
	}

	if cfg.HealthPort != 12000 || cfg.NodeAgentHealthPort != 12001 {
		t.Fatalf("expected runtime ports to apply, got health=%d nodeAgent=%d", cfg.HealthPort, cfg.NodeAgentHealthPort)
	}

	if cfg.StatusStaleThreshold != 55*time.Second {
		t.Fatalf("expected stale threshold 55s, got %s", cfg.StatusStaleThreshold)
	}

	if cfg.StatusWSKeepaliveInterval != 8*time.Second {
		t.Fatalf("expected websocket keepalive interval 8s, got %s", cfg.StatusWSKeepaliveInterval)
	}

	if cfg.StatusWSKeepaliveFailureCount != 3 {
		t.Fatalf("expected websocket keepalive failure count 3, got %d", cfg.StatusWSKeepaliveFailureCount)
	}

	if cfg.RegisterAggregatedAPIServer {
		t.Fatalf("expected aggregated API registration disabled from runtime config")
	}

	if cfg.LeaderElection.Enabled {
		t.Fatalf("expected leader election enabled=false from runtime config")
	}

	if cfg.LeaderElection.LeaseDuration != 20*time.Second || cfg.LeaderElection.RenewDeadline != 12*time.Second || cfg.LeaderElection.RetryPeriod != 3*time.Second {
		t.Fatalf("unexpected leader election durations: lease=%s renew=%s retry=%s", cfg.LeaderElection.LeaseDuration, cfg.LeaderElection.RenewDeadline, cfg.LeaderElection.RetryPeriod)
	}

	if cfg.LeaderElection.ResourceNamespace != "lease-ns" || cfg.LeaderElection.ResourceName != "lease-name" {
		t.Fatalf("unexpected leader election identity values: ns=%q name=%q", cfg.LeaderElection.ResourceNamespace, cfg.LeaderElection.ResourceName)
	}

	if cfg.AzureTenantID != "tenant-from-config" {
		t.Fatalf("expected Azure tenant id from runtime config, got %q", cfg.AzureTenantID)
	}
}

// TestApplyControllerRuntimeConfigRespectsExplicitFlags tests ApplyControllerRuntimeConfigRespectsExplicitFlags.
func TestApplyControllerRuntimeConfigRespectsExplicitFlags(t *testing.T) {
	tmpPath := filepath.Join(".", "runtime-config-controller-flag-test.tmp")

	t.Cleanup(func() { _ = os.Remove(tmpPath) })

	runtimeYAML := []byte("controller:\n" +
		"  healthPort: 18000\n" +
		"  statusWsKeepaliveFailureCount: 6\n" +
		"  leaderElection:\n" +
		"    enabled: false\n")
	if err := os.WriteFile(tmpPath, runtimeYAML, 0o644); err != nil {
		t.Fatalf("failed to write runtime config fixture: %v", err)
	}

	cfg := &config.Config{
		LeaderElection: config.DefaultLeaderElectionConfig(),
		HealthPort:     9999,
	}
	cmd := newControllerConfigTestCommand(cfg)

	if err := cmd.Flags().Set("health-port", "17000"); err != nil {
		t.Fatalf("failed setting health-port flag: %v", err)
	}

	if err := cmd.Flags().Set("leader-elect", "true"); err != nil {
		t.Fatalf("failed setting leader-elect flag: %v", err)
	}

	if err := cmd.Flags().Set("register-aggregated-apiserver", "true"); err != nil {
		t.Fatalf("failed setting register-aggregated-apiserver flag: %v", err)
	}

	if err := cmd.Flags().Set("status-ws-keepalive-failure-count", "4"); err != nil {
		t.Fatalf("failed setting status-ws-keepalive-failure-count flag: %v", err)
	}

	if err := applyControllerRuntimeConfig(cmd, cfg, tmpPath); err != nil {
		t.Fatalf("applyControllerRuntimeConfig returned error: %v", err)
	}

	if cfg.HealthPort != 17000 {
		t.Fatalf("expected explicit flag value to be retained, got %d", cfg.HealthPort)
	}

	if !cfg.LeaderElection.Enabled {
		t.Fatalf("expected explicit leader-elect flag to be retained as true")
	}

	if !cfg.RegisterAggregatedAPIServer {
		t.Fatalf("expected explicit register-aggregated-apiserver flag to be retained as true")
	}

	if cfg.StatusWSKeepaliveFailureCount != 4 {
		t.Fatalf("expected explicit status-ws-keepalive-failure-count flag to be retained, got %d", cfg.StatusWSKeepaliveFailureCount)
	}
}
