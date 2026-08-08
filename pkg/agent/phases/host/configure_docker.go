// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const daemonConfigPath = "/etc/docker/daemon.json"

type configureDocker struct{}

// ConfigureDocker returns a task that prevents Docker from manipulating
// iptables rules, which would conflict with Kubernetes networking.
func ConfigureDocker(*slog.Logger) phases.Task {
	return configureDocker{}
}

func (configureDocker) Name() string { return "configure-docker" }

func (configureDocker) Do(context.Context) error {
	if err := ensureDaemonConfigAt(daemonConfigPath); err != nil {
		return fmt.Errorf("configuring docker daemon: %w", err)
	}

	return nil
}

// ensureDaemonConfigAt ensures the daemon.json at the given path contains
// "iptables": false. Existing configuration is otherwise preserved.
func ensureDaemonConfigAt(path string) error {
	config := map[string]any{}

	existing, err := os.ReadFile(path) //#nosec G304 -- trusted path
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("reading %s: %w", path, err)
	default:
		if err := json.Unmarshal(existing, &config); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	if enabled, ok := config["iptables"].(bool); ok && !enabled {
		return nil
	}

	config["iptables"] = false

	content, err := marshalDaemonConfig(config)
	if err != nil {
		return err
	}

	return utilio.WriteFile(path, content, 0o644)
}

func marshalDaemonConfig(config map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("marshaling daemon config: %w", err)
	}

	return append(data, '\n'), nil
}
