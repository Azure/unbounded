// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadRuntimeConfig tests LoadRuntimeConfig.
func TestLoadRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.yaml")

	content := []byte(`
common:
  azureTenantId: tenant-1
controller:
  informerResyncPeriod: 30s
  healthPort: 9080
node:
  nodeName: node-a
  wireGuardPort: 51820
  statusPushEnabled: true
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig returned error: %v", err)
	}

	if cfg.Common.AzureTenantID != "tenant-1" {
		t.Fatalf("unexpected common.azureTenantId: %q", cfg.Common.AzureTenantID)
	}

	if cfg.Controller.HealthPort == nil || *cfg.Controller.HealthPort != 9080 {
		t.Fatalf("unexpected controller.healthPort: %#v", cfg.Controller.HealthPort)
	}

	if cfg.Node.NodeName != "node-a" {
		t.Fatalf("unexpected node.nodeName: %q", cfg.Node.NodeName)
	}

	if cfg.Node.WireGuardPort == nil || *cfg.Node.WireGuardPort != 51820 {
		t.Fatalf("unexpected node.wireGuardPort: %#v", cfg.Node.WireGuardPort)
	}

	if cfg.Node.StatusPushEnabled == nil || !*cfg.Node.StatusPushEnabled {
		t.Fatalf("unexpected node.statusPushEnabled: %#v", cfg.Node.StatusPushEnabled)
	}
}

// TestLoadRuntimeConfigErrors tests LoadRuntimeConfigErrors.
func TestLoadRuntimeConfigErrors(t *testing.T) {
	if _, err := LoadRuntimeConfig("/definitely/missing/runtime.yaml"); err == nil {
		t.Fatalf("expected missing file error")
	}

	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("common: ["), 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}

	if _, err := LoadRuntimeConfig(path); err == nil || !strings.Contains(err.Error(), "parse runtime config") {
		t.Fatalf("expected parse runtime config error, got: %v", err)
	}
}

// TestParseDurationField tests ParseDurationField.
func TestParseDurationField(t *testing.T) {
	d, err := ParseDurationField("", "field")
	if err != nil || d != 0 {
		t.Fatalf("expected empty duration to return 0,nil; got %v,%v", d, err)
	}

	d, err = ParseDurationField("15s", "field")
	if err != nil || d != 15*time.Second {
		t.Fatalf("expected parsed duration 15s; got %v,%v", d, err)
	}

	if _, err := ParseDurationField("bad-duration", "node.statusPushInterval"); err == nil || !strings.Contains(err.Error(), "invalid duration for node.statusPushInterval") {
		t.Fatalf("expected invalid duration error with field name, got: %v", err)
	}
}
