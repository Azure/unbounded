// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the controller configuration.
type Config struct {
	APIServerEndpoint       string        `yaml:"apiServerEndpoint"`
	MetricsAddr             string        `yaml:"metricsAddr"`
	ProbeAddr               string        `yaml:"probeAddr"`
	EnableLeaderElection    bool          `yaml:"enableLeaderElection"`
	MaxConcurrentReconciles int           `yaml:"maxConcurrentReconciles"`
	ProvisioningTimeout     time.Duration `yaml:"provisioningTimeout"`
}

// DefaultConfig returns a config with default values.
func DefaultConfig() Config {
	return Config{
		MetricsAddr:             ":8080",
		ProbeAddr:               ":8081",
		EnableLeaderElection:    false,
		MaxConcurrentReconciles: 10,
		ProvisioningTimeout:     ProvisioningTimeout,
	}
}

// LoadConfig loads configuration from a YAML file.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file: %w", err)
	}

	return cfg, nil
}
