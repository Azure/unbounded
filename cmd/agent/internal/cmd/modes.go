// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/nspawnops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// agentMode is everything the agent needs to operate one isolation mode: its
// backend, the daemon's node operator for it, and its bootstrap plan.
type agentMode struct {
	// newBackend builds the mode's isolation backend bound to one agent
	// configuration.
	newBackend daemon.BackendFactory
	// newOperator builds the daemon's node operator for the mode from the
	// mode's backend factory.
	newOperator func(daemon.BackendFactory) daemon.ModeOperator
	// newPlan builds the mode's bootstrap plan. It is called once per bootstrap
	// run.
	newPlan func() stagePlan
}

// agentModeTable maps each isolation mode to its components.
type agentModeTable map[isolation.Mode]agentMode

// agentModes is the mode composition table: the one place an isolation mode is
// wired into the agent. Adding a mode is one entry here, with the mode's
// backend, its daemon operator and its bootstrap plan in their own files;
// nothing else (the daemon's dispatcher, bootstrap, start) changes. A mode
// without an entry is one this agent cannot operate, and the daemon and
// bootstrap refuse it before they touch the host.
//
// It is a function rather than a package variable so no test can swap it: tests
// inject a plan through agentStages and an operator through the daemon's run
// options.
func agentModes() agentModeTable {
	return agentModeTable{
		isolation.ModeNSpawn: {
			newBackend: func(log *slog.Logger, cfg *provision.AgentConfig) isolation.Backend {
				return nspawnops.Backend(nspawnops.Params{Log: log, Config: cfg})
			},
			newOperator: func(newBackend daemon.BackendFactory) daemon.ModeOperator {
				return daemon.NewNSpawnOperator(newBackend)
			},
			newPlan: func() stagePlan { return nspawnPlan{} },
		},
	}
}

// lookup returns the components of mode. The empty mode is the default mode. A
// mode with no entry, or an entry that is missing a component, is an error.
func (t agentModeTable) lookup(mode isolation.Mode) (agentMode, error) {
	if mode == "" {
		mode = isolation.DefaultMode
	}

	m, ok := t[mode]
	if !ok {
		return agentMode{}, fmt.Errorf("this agent cannot operate isolation mode %q", mode)
	}

	if m.newBackend == nil || m.newOperator == nil || m.newPlan == nil {
		return agentMode{}, fmt.Errorf("isolation mode %q is not fully wired: it needs a backend, a node operator and a bootstrap plan", mode)
	}

	return m, nil
}

// operators builds the daemon's node operator of every mode in the table, each
// bound to its own mode's backend.
func (t agentModeTable) operators() (daemon.ModeOperators, error) {
	ops := make(daemon.ModeOperators, len(t))

	for mode := range t {
		m, err := t.lookup(mode)
		if err != nil {
			return nil, err
		}

		ops[mode] = m.newOperator(m.newBackend)
	}

	return ops, nil
}
