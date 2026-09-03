// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// machineNameEnv is the environment variable that overrides the resolved
// MachineName when the config does not set one. It lets a single bootstrap
// payload (e.g. cloud-init for an Azure VMSS or AWS Auto Scaling group) be
// reused across many instances, each resolving its own name at startup.
const machineNameEnv = "AGENT_MACHINE_NAME"

// ResolveMachineName resolves and stores the MachineName on cfg once. It
// returns the source of the resolved value ("config", "env", or "hostname") so
// callers can log the resolution; the source is "config" when an explicit value
// was already present. Resolution order:
//  1. An explicit, valid MachineName already in the config is kept as-is.
//  2. The AGENT_MACHINE_NAME environment variable.
//  3. The host hostname.
//
// Values resolved from the environment or the hostname are normalized (trimmed
// and lowercased) before validation. This lives in internal/provision (the
// unbounded-specific config layer) rather than the shared pkg/agent library so
// the resolution behavior is not imposed on external consumers of that library.
//
// Callers must invoke ResolveMachineName before AgentConfig.BackfillNodeName
// because the latter falls back to MachineName.
func ResolveMachineName(cfg *AgentConfig) (string, error) {
	if name := strings.TrimSpace(cfg.MachineName); name != "" {
		if !isValidMachineName(name) {
			return "", fmt.Errorf("machine name %q is not a valid Kubernetes node name", cfg.MachineName)
		}

		cfg.MachineName = name

		return "config", nil
	}

	if env := strings.TrimSpace(os.Getenv(machineNameEnv)); env != "" {
		name, err := normalizeDerivedMachineName(env)
		if err != nil {
			return "", fmt.Errorf("%s: %w", machineNameEnv, err)
		}

		cfg.MachineName = name

		return "env", nil
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve machine name from hostname: %w", err)
	}

	name, err := normalizeDerivedMachineName(hostname)
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}

	cfg.MachineName = name

	return "hostname", nil
}

// normalizeDerivedMachineName trims and lowercases a machine name derived from
// the environment or the host hostname, then validates it as a Kubernetes node
// name (DNS-1123 subdomain). It is kept separate from ResolveMachineName so it
// can be unit-tested without depending on os.Hostname.
func normalizeDerivedMachineName(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if !isValidMachineName(normalized) {
		return "", fmt.Errorf("derived machine name %q is not a valid Kubernetes node name", name)
	}

	return normalized, nil
}

// isValidMachineName reports whether name is a valid Kubernetes node name
// (DNS-1123 subdomain), which is the constraint the MachineName must satisfy
// because it becomes the Machine CR name.
func isValidMachineName(name string) bool {
	if name == "" {
		return false
	}

	return len(validation.IsDNS1123Subdomain(name)) == 0
}
