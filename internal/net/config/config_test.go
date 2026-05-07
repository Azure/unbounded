// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"testing"
	"time"
)

// TestDefaultLeaderElectionConfig tests DefaultLeaderElectionConfig.
func TestDefaultLeaderElectionConfig(t *testing.T) {
	cfg := DefaultLeaderElectionConfig()
	if !cfg.Enabled {
		t.Fatalf("expected leader election enabled by default")
	}

	if cfg.LeaseDuration != 15*time.Second {
		t.Fatalf("unexpected lease duration: %v", cfg.LeaseDuration)
	}

	if cfg.RenewDeadline != 10*time.Second {
		t.Fatalf("unexpected renew deadline: %v", cfg.RenewDeadline)
	}

	if cfg.RetryPeriod != 2*time.Second {
		t.Fatalf("unexpected retry period: %v", cfg.RetryPeriod)
	}

	if cfg.ResourceNamespace != "kube-system" {
		t.Fatalf("unexpected resource namespace: %s", cfg.ResourceNamespace)
	}

	if cfg.ResourceName != "unbounded-net-controller" {
		t.Fatalf("unexpected resource name: %s", cfg.ResourceName)
	}
}

// TestConfigValidate tests ConfigValidate.
func TestConfigValidate(t *testing.T) {
	cfg := &Config{StatusWSKeepaliveFailureCount: 2}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected nil validation error, got %v", err)
	}
}

// TestConfigValidateRejectsInvalidStatusWSKeepaliveFailureCount tests ConfigValidateRejectsInvalidStatusWSKeepaliveFailureCount.
func TestConfigValidateRejectsInvalidStatusWSKeepaliveFailureCount(t *testing.T) {
	cfg := &Config{StatusWSKeepaliveFailureCount: 0}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for keepalive failure count <= 0")
	}
}
