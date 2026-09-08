// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MetricsAddr          string `yaml:"metricsAddr"`
	ProbeAddr            string `yaml:"probeAddr"`
	EnableLeaderElection bool   `yaml:"enableLeaderElection"`
}

func DefaultConfig() Config {
	return Config{
		MetricsAddr:          ":8080",
		ProbeAddr:            ":8081",
		EnableLeaderElection: false,
	}
}

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
