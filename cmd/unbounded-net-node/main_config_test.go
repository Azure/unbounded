// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func newNodeConfigTestCommand(cfg *config) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	flags := cmd.Flags()
	flags.DurationVar(&cfg.InformerResyncPeriod, "informer-resync-period", 3600*time.Second, "")
	flags.StringVar(&cfg.NodeName, "node-name", "", "")
	flags.StringVar(&cfg.CNIConfDir, "cni-conf-dir", "/etc/cni/net.d", "")
	flags.StringVar(&cfg.CNIConfFile, "cni-conf-file", "10-unbounded.conflist", "")
	flags.StringVar(&cfg.BridgeName, "bridge-name", "cbr0", "")
	flags.StringVar(&cfg.WireGuardDir, "wireguard-dir", "/etc/wireguard", "")
	flags.IntVar(&cfg.WireGuardPort, "wireguard-port", 51820, "")
	flags.BoolVar(&cfg.EnablePolicyRouting, "enable-policy-routing", true, "")
	flags.IntVar(&cfg.MTU, "mtu", 0, "")
	flags.IntVar(&cfg.HealthPort, "health-port", 9998, "")
	flags.BoolVar(&cfg.StatusPushEnabled, "status-push-enabled", true, "")
	flags.StringVar(&cfg.StatusPushURL, "status-push-url", "", "")
	flags.DurationVar(&cfg.StatusPushInterval, "status-push-interval", 10*time.Second, "")
	flags.DurationVar(&cfg.StatusPushAPIServerInterval, "status-push-apiserver-interval", 30*time.Second, "")
	flags.BoolVar(&cfg.StatusPushDelta, "status-push-delta", true, "")
	flags.BoolVar(&cfg.StatusWSEnabled, "status-ws-enabled", true, "")
	flags.StringVar(&cfg.StatusWSURL, "status-ws-url", "", "")
	flags.StringVar(&cfg.StatusWSAPIServerMode, "status-ws-apiserver-mode", statusWSAPIServerModeFallback, "")
	flags.StringVar(&cfg.StatusWSAPIServerURL, "status-ws-apiserver-url", "", "")
	flags.DurationVar(&cfg.StatusWSAPIServerStartupDelay, "status-ws-apiserver-startup-delay", 60*time.Second, "")
	flags.DurationVar(&cfg.StatusWSKeepaliveInterval, "status-ws-keepalive-interval", 10*time.Second, "")
	flags.IntVar(&cfg.StatusWSKeepaliveFailureCount, "status-ws-keepalive-failure-count", 2, "")
	flags.BoolVar(&cfg.RemoveWireGuardOnShutdown, "shutdown-remove-wireguard-configuration", false, "")
	flags.BoolVar(&cfg.CleanupNetlinkOnShutdown, "shutdown-remove-ip-routes", false, "")
	flags.BoolVar(&cfg.RemoveMasqueradeOnShutdown, "shutdown-remove-masquerade-rules", false, "")
	flags.DurationVar(&cfg.CriticalDeltaEvery, "status-critical-interval", time.Second, "")
	flags.DurationVar(&cfg.StatsDeltaEvery, "status-stats-interval", 15*time.Second, "")

	return cmd
}

// TestApplyNodeRuntimeConfig tests ApplyNodeRuntimeConfig.
func TestApplyNodeRuntimeConfig(t *testing.T) {
	cfg := &config{
		ConfigFile:                    "",
		InformerResyncPeriod:          3600 * time.Second,
		NodeName:                      "node-from-flag",
		CNIConfDir:                    "/etc/cni/net.d",
		CNIConfFile:                   "10-unbounded.conflist",
		BridgeName:                    "cbr0",
		WireGuardDir:                  "/etc/wireguard",
		WireGuardPort:                 51820,
		EnablePolicyRouting:           true,
		MTU:                           0,
		HealthPort:                    9998,
		StatusPushEnabled:             true,
		StatusPushInterval:            10 * time.Second,
		StatusPushAPIServerInterval:   30 * time.Second,
		StatusPushDelta:               true,
		StatusWSEnabled:               true,
		StatusWSAPIServerMode:         statusWSAPIServerModeFallback,
		StatusWSAPIServerStartupDelay: 60 * time.Second,
		StatusWSKeepaliveInterval:     10 * time.Second,
		StatusWSKeepaliveFailureCount: 2,
		CriticalDeltaEvery:            time.Second,
		StatsDeltaEvery:               15 * time.Second,
	}

	tmpPath := filepath.Join(".", "runtime-config-node-test.tmp")
	cfg.ConfigFile = tmpPath

	t.Cleanup(func() { _ = os.Remove(tmpPath) })

	runtimeYAML := []byte("node:\n" +
		"  informerResyncPeriod: 30s\n" +
		"  nodeName: node-from-config\n" +
		"  cniConfDir: /tmp/cni\n" +
		"  cniConfFile: 20-test.conflist\n" +
		"  bridgeName: cbr-test\n" +
		"  wireGuardDir: /tmp/wg\n" +
		"  wireGuardPort: 51888\n" +
		"  enablePolicyRouting: false\n" +
		"  mtu: 1450\n" +
		"  healthPort: 10001\n" +
		"  statusPushEnabled: false\n" +
		"  statusPushURL: http://controller/status/push\n" +
		"  statusPushInterval: 20s\n" +
		"  statusPushApiserverInterval: 45s\n" +
		"  statusPushDelta: false\n" +
		"  statusWebsocketEnabled: false\n" +
		"  statusWebsocketURL: ws://controller/status/nodews\n" +
		"  statusWebsocketApiserverMode: never\n" +
		"  statusWebsocketApiserverURL: wss://kubernetes.default.svc/apis/status.net.unbounded-kube.io/v1alpha1/status/nodews\n" +
		"  statusWebsocketApiserverStartupDelay: 75s\n" +
		"  statusWebsocketKeepaliveInterval: 0s\n" +
		"  statusWsKeepaliveFailureCount: 3\n" +
		"  shutdownRemoveWireGuardConfiguration: true\n" +
		"  shutdownRemoveIPRoutes: true\n" +
		"  shutdownRemoveMasqueradeRules: true\n" +
		"  criticalDeltaEvery: 2s\n" +
		"  statsDeltaEvery: 30s\n")
	if err := os.WriteFile(tmpPath, runtimeYAML, 0o644); err != nil {
		t.Fatalf("failed to write runtime config fixture: %v", err)
	}

	cmd := newNodeConfigTestCommand(cfg)
	if err := applyNodeRuntimeConfig(cmd, cfg); err != nil {
		t.Fatalf("applyNodeRuntimeConfig returned error: %v", err)
	}

	if cfg.InformerResyncPeriod != 30*time.Second || cfg.NodeName != "node-from-config" {
		t.Fatalf("expected resync period and node name from runtime config, got period=%s node=%q", cfg.InformerResyncPeriod, cfg.NodeName)
	}

	if cfg.CNIConfDir != "/tmp/cni" || cfg.CNIConfFile != "20-test.conflist" || cfg.BridgeName != "cbr-test" {
		t.Fatalf("expected CNI settings from runtime config, got dir=%q file=%q bridge=%q", cfg.CNIConfDir, cfg.CNIConfFile, cfg.BridgeName)
	}

	if cfg.WireGuardDir != "/tmp/wg" || cfg.WireGuardPort != 51888 || cfg.EnablePolicyRouting {
		t.Fatalf("expected wireguard settings from runtime config, got dir=%q port=%d policy=%v", cfg.WireGuardDir, cfg.WireGuardPort, cfg.EnablePolicyRouting)
	}

	if cfg.MTU != 1450 || cfg.HealthPort != 10001 {
		t.Fatalf("expected mtu/health port from runtime config, got mtu=%d health=%d", cfg.MTU, cfg.HealthPort)
	}

	if cfg.StatusPushEnabled || cfg.StatusPushDelta {
		t.Fatalf("expected HTTP status push toggles false from runtime config, got pushEnabled=%v pushDelta=%v", cfg.StatusPushEnabled, cfg.StatusPushDelta)
	}

	if cfg.StatusPushURL != "http://controller/status/push" {
		t.Fatalf("expected status push URL from runtime config, got push=%q", cfg.StatusPushURL)
	}

	if cfg.StatusPushInterval != 20*time.Second || cfg.StatusPushAPIServerInterval != 45*time.Second || cfg.CriticalDeltaEvery != 2*time.Second || cfg.StatsDeltaEvery != 30*time.Second {
		t.Fatalf("expected status intervals from runtime config, got push=%s apiserver=%s critical=%s stats=%s", cfg.StatusPushInterval, cfg.StatusPushAPIServerInterval, cfg.CriticalDeltaEvery, cfg.StatsDeltaEvery)
	}

	if cfg.StatusWSAPIServerMode != statusWSAPIServerModeNever {
		t.Fatalf("expected status websocket API server mode never from runtime config, got %q", cfg.StatusWSAPIServerMode)
	}

	if cfg.StatusWSAPIServerURL != "wss://kubernetes.default.svc/apis/status.net.unbounded-kube.io/v1alpha1/status/nodews" {
		t.Fatalf("expected status websocket API server URL from runtime config, got %q", cfg.StatusWSAPIServerURL)
	}

	if cfg.StatusWSAPIServerStartupDelay != 75*time.Second {
		t.Fatalf("expected status websocket API server startup delay 75s from runtime config, got %s", cfg.StatusWSAPIServerStartupDelay)
	}

	if cfg.StatusWSKeepaliveInterval != 0 {
		t.Fatalf("expected status websocket keepalive interval 0s from runtime config, got %s", cfg.StatusWSKeepaliveInterval)
	}

	if cfg.StatusWSKeepaliveFailureCount != 3 {
		t.Fatalf("expected status websocket keepalive failure count 3 from runtime config, got %d", cfg.StatusWSKeepaliveFailureCount)
	}

	if !cfg.RemoveWireGuardOnShutdown || !cfg.CleanupNetlinkOnShutdown || !cfg.RemoveMasqueradeOnShutdown {
		t.Fatalf("expected shutdown cleanup toggles true from runtime config, got wireguard=%v routes=%v masquerade=%v", cfg.RemoveWireGuardOnShutdown, cfg.CleanupNetlinkOnShutdown, cfg.RemoveMasqueradeOnShutdown)
	}
}

// TestApplyNodeRuntimeConfigRespectsChangedFlags tests ApplyNodeRuntimeConfigRespectsChangedFlags.
func TestApplyNodeRuntimeConfigRespectsChangedFlags(t *testing.T) {
	cfg := &config{ConfigFile: filepath.Join(".", "runtime-config-node-flags-test.tmp"), NodeName: "from-flag", HealthPort: 9998}

	t.Cleanup(func() { _ = os.Remove(cfg.ConfigFile) })

	runtimeYAML := []byte("node:\n" +
		"  nodeName: from-config\n" +
		"  healthPort: 11000\n" +
		"  statusWsKeepaliveFailureCount: 6\n")
	if err := os.WriteFile(cfg.ConfigFile, runtimeYAML, 0o644); err != nil {
		t.Fatalf("failed to write runtime config fixture: %v", err)
	}

	cmd := newNodeConfigTestCommand(cfg)
	if err := cmd.Flags().Set("node-name", "from-flag"); err != nil {
		t.Fatalf("failed setting node-name flag: %v", err)
	}

	if err := cmd.Flags().Set("health-port", "12000"); err != nil {
		t.Fatalf("failed setting health-port flag: %v", err)
	}

	if err := cmd.Flags().Set("status-ws-keepalive-failure-count", "4"); err != nil {
		t.Fatalf("failed setting status-ws-keepalive-failure-count flag: %v", err)
	}

	if err := applyNodeRuntimeConfig(cmd, cfg); err != nil {
		t.Fatalf("applyNodeRuntimeConfig returned error: %v", err)
	}

	if cfg.NodeName != "from-flag" || cfg.HealthPort != 12000 {
		t.Fatalf("expected changed flags to win over runtime config, got node=%q health=%d", cfg.NodeName, cfg.HealthPort)
	}

	if cfg.StatusWSKeepaliveFailureCount != 4 {
		t.Fatalf("expected changed keepalive failure count flag to win over runtime config, got %d", cfg.StatusWSKeepaliveFailureCount)
	}
}
