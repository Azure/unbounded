// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func init() {
	registerStagePlan(isolation.ModeNone, func() stagePlan { return &nonePlan{} })
}

// noneMode is what differs in the none plan from the shared host-native sequence.
var noneMode = hostMode{
	mode:          isolation.ModeNone,
	slot:          noneops.Slot,
	base:          none.DefaultBase,
	prepareTask:   "install-none-node",
	configureTask: "configure-none-node",
	startTask:     "start-none-node",
	runningMsg:    "node units are already running; leaving the installed version in place",
}

// nonePlan is the bootstrap plan for the none mode: the node stack runs as
// agent-owned units directly on the host. The host-global phases (nftables,
// sysctls, swap, docker, WireGuard keys later in reset) are the same ones the
// nspawn plan runs. What differs is that nothing is unpacked and no machine is
// started: artifacts are installed into a versioned directory, the node
// configuration is written at the shared host paths, and the units are started.
type nonePlan struct {
	node *noneops.Node

	hostSeams
}

func (nonePlan) EnsureHostClean(ctx context.Context, s *agentStages) error {
	return host.EnsureNoExistingDeploymentFor(ctx, s.log, isolation.ModeNone)
}

func (p *nonePlan) ResolveInputs(ctx context.Context, s *agentStages) error {
	return noneMode.resolveInputs(ctx, s, func(downloads *goalstates.DownloadOverrides) goalStater {
		p.node = noneops.New(noneops.Params{Log: s.log, Config: &s.cfg.AgentConfig, Downloads: downloads, GoalState: s.gs})

		return p.node
	})
}

// PrepareHost is the nspawn plan's sequence without the steps that exist only
// for the machine: no systemd-container, no apt hardening against machined
// restarts, and no DisableContainerd/DisableKubelet, since those stop and
// disable the distro units and preflight has already refused a host that has
// any. The kernel modules the machine would have loaded at boot are loaded on
// the host first, because the sysctls below need br_netfilter.
func (p *nonePlan) PrepareHost(ctx context.Context, s *agentStages) error {
	return prepareHostSteps(ctx, s,
		host.InstallPackagesFor(s.log, isolation.ModeNone),
		noneops.LoadKernelModules(s.log, nil),
		phases.Parallel(s.log,
			host.ConfigureOS(s.log), host.ConfigureNFTablesFor(s.log, isolation.ModeNone),
			phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
			host.DisableSwap(s.log)),
	)
}

// PrepareNodeRoot installs the version and writes the units. A running node
// owns its binaries, so as for nspawn a node that is already up is left alone.
func (p *nonePlan) PrepareNodeRoot(ctx context.Context, s *agentStages) error {
	return noneMode.prepareNodeRoot(ctx, s, p.node, p.hostSeams, p.node.Provision)
}

func (p *nonePlan) EnsureNodeStarted(ctx context.Context, s *agentStages) error {
	return noneMode.ensureNodeStarted(ctx, s, p.node, p.hostSeams)
}
