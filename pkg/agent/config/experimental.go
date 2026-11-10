// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// ExperimentalConfig opts a host in to agent features that are not yet
// supported for production use.
type ExperimentalConfig struct {
	// ContainerIsolation allows Isolation "container", the OCI container
	// backend. The agent refuses to bootstrap in, or change a running node to,
	// that mode unless this is true.
	ContainerIsolation bool `json:"ContainerIsolation,omitempty"`
}

// ContainerIsolationEnabled reports whether the configuration opts in to the
// experimental container isolation mode.
func (a *AgentConfig) ContainerIsolationEnabled() bool {
	return a.Experimental != nil && a.Experimental.ContainerIsolation
}

// CheckModeEnabled returns an error when mode is experimental and this
// configuration does not opt in to it. It says nothing about whether mode is a
// known mode; ParseMode answers that.
func (a *AgentConfig) CheckModeEnabled(mode isolation.Mode) error {
	if mode == isolation.ModeContainer && !a.ContainerIsolationEnabled() {
		return fmt.Errorf("isolation mode %q is experimental and not enabled on this host; "+
			"set Experimental.ContainerIsolation to true in the agent configuration to use it", mode)
	}

	return nil
}

func (a *AgentConfig) validateExperimental() error {
	mode, err := a.IsolationMode()
	if err != nil {
		// Validate reports the invalid mode itself.
		return nil //nolint:nilerr // reported by the Isolation check
	}

	if err := a.CheckModeEnabled(mode); err != nil {
		return fmt.Errorf("invalid Isolation: %w", err)
	}

	return nil
}
