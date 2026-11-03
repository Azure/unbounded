// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nspawnops is the composition root for the nspawn isolation backend.
// The backend package cannot import the phase packages that implement its
// lifecycle operations, so this package binds them to an agent configuration
// and injects them as nspawn.Ops.
package nspawnops

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Params bind the operations to one machine's configuration.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides

	// Resolve overrides goal state resolution. Tests use it; the default is
	// goalstates.ResolveMachine.
	Resolve func(slot string) (*goalstates.MachineGoalState, error)
}

// New returns the registered nspawn backend with its operations wired.
func New(p Params) (isolation.Backend, error) {
	return isolation.New(isolation.ModeNSpawn, Backend(p))
}

// Backend returns an nspawn backend whose lifecycle operations are the real
// phases, bound to p.
func Backend(p Params) *nspawn.Backend {
	o := &ops{p: p, goalStates: map[string]*goalstates.MachineGoalState{}}
	if o.p.Log == nil {
		o.p.Log = slog.Default()
	}

	return &nspawn.Backend{
		Log: o.p.Log,
		Ops: nspawn.Ops{
			Prepare: o.prepare,
			Start:   o.start,
			Stop:    o.stop,
			Restart: o.restart,
			Destroy: o.destroy,
		},
	}
}

type ops struct {
	p Params

	mu         sync.Mutex
	goalStates map[string]*goalstates.MachineGoalState
}

// goalState resolves the goal state for slot once and reuses it, so a Prepare
// followed by a Start sees the same device and library discovery.
func (o *ops) goalState(slot string) (*goalstates.MachineGoalState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if gs, ok := o.goalStates[slot]; ok {
		return gs, nil
	}

	resolve := o.p.Resolve
	if resolve == nil {
		resolve = func(slot string) (*goalstates.MachineGoalState, error) {
			if o.p.Config == nil {
				return nil, fmt.Errorf("no agent configuration to resolve machine %s", slot)
			}

			return goalstates.ResolveMachine(o.p.Log, o.p.Config, slot, o.p.Downloads)
		}
	}

	gs, err := resolve(slot)
	if err != nil {
		return nil, fmt.Errorf("resolve machine goal state: %w", err)
	}

	o.goalStates[slot] = gs

	return gs, nil
}

// prepare provisions the rootfs for the slot. Grants are derived from the host
// when the goal state is resolved, so n.Grants is not consulted: the backend
// refuses a NodeSpec that carries any, so none reaches here.
func (o *ops) prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	gs, err := o.goalState(n.Slot)
	if err != nil {
		return nil, err
	}

	if err := rootfs.Provision(o.p.Log, gs.RootFS).Do(ctx); err != nil {
		return nil, err
	}

	return gs.RootFS.Root(), nil
}

func (o *ops) start(ctx context.Context, r isolation.NodeRoot) error {
	gs, err := o.goalState(r.Slot())
	if err != nil {
		return err
	}

	return nodestart.StartNode(o.p.Log, gs.NodeStart).Do(ctx)
}

func (o *ops) stop(ctx context.Context, r isolation.NodeRoot) error {
	return nodestop.StopNode(o.p.Log, r.Slot()).Do(ctx)
}

// restart restarts the machine unit, which re-runs the config refresh and
// post-start hooks, then waits for LocalDNS and the kubelet.
func (o *ops) restart(ctx context.Context, r isolation.NodeRoot) error {
	gs, err := o.goalState(r.Slot())
	if err != nil {
		return err
	}

	return phases.Serial(
		o.p.Log,
		nodestart.ReconcileNSpawnLifecycle(o.p.Log, r.Slot()),
		nodestart.WaitForLocalDNS(o.p.Log, gs.NodeStart),
		nodestart.WaitForKubelet(o.p.Log, r.Slot()),
	).Do(ctx)
}

func (o *ops) destroy(ctx context.Context, r isolation.NodeRoot) error {
	return reset.CleanupMachine(o.p.Log, r.Slot()).Do(ctx)
}
