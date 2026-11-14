// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"

	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

// containerMode is what differs in the container plan from the shared
// host-native sequence.
var containerMode = hostMode{
	mode:          isolation.ModeContainer,
	slot:          containerops.Slot,
	base:          container.DefaultBase,
	prepareTask:   "prepare-container-node",
	configureTask: "configure-container-node",
	startTask:     "start-container-node",
	runningMsg:    "node container is already running; leaving its rootfs in place",
	runningAttrs:  []any{"slot", containerops.Slot},
}

// containerPlan is the bootstrap plan for the container mode: the node stack
// runs in a privileged OCI container (systemd as PID 1) supervised by an
// agent-owned unit. The host-global phases are the nspawn plan's, minus the
// systemd-container package and the machined hardening. What differs is that
// the OS image is unpacked into a container slot, an OCI bundle and unit are
// generated, and the stack inside is started through "<runtime> exec".
//
// PROVISIONAL: the container mode is experimental and unverified on a real
// host; see the containerops package comment. It runs only on a host whose
// agent configuration opts in (Experimental.ContainerIsolation); configuredMode
// refuses it otherwise.
type containerPlan struct {
	node *containerops.Node

	hostSeams
}

var _ stagePlan = (*containerPlan)(nil)

func (containerPlan) EnsureHostClean(ctx context.Context, s *agentStages) error {
	return host.EnsureNoExistingDeploymentFor(ctx, s.log, isolation.ModeContainer)
}

func (p *containerPlan) ResolveInputs(ctx context.Context, s *agentStages) error {
	return containerMode.resolveInputs(ctx, s, func(downloads *goalstates.DownloadOverrides) goalStater {
		p.node = containerops.New(containerops.Params{Log: s.log, Config: &s.cfg.AgentConfig, Downloads: downloads, Slot: containerops.Slot, GoalState: s.gs})

		return p.node
	})
}

// PrepareHost is the nspawn plan's sequence without the steps that exist only
// for the machine: no systemd-container and no apt hardening against machined
// restarts (the guards first boot writes for every mode are removed). The distro containerd and kubelet units are still disabled because
// the container shares /run/containerd with the host (design invariant 3), so a
// host containerd would own the socket the container's containerd needs. The
// kernel modules the node needs are loaded on the host, because the sysctls
// below need br_netfilter and a container does not load modules at boot.
func (p *containerPlan) PrepareHost(ctx context.Context, s *agentStages) error {
	return prepareHostSteps(ctx, s,
		host.InstallPackagesFor(s.log, isolation.ModeContainer),
		noneops.LoadKernelModules(s.log, nil),
		phases.Parallel(s.log,
			host.ConfigureOS(s.log), host.ConfigureNFTablesFor(s.log, isolation.ModeContainer),
			phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
			host.DisableContainerd(s.log), host.DisableKubelet(s.log),
			host.DisableSwap(s.log), host.RemoveAPTHardening(s.log)),
	)
}

// PrepareNodeRoot installs the runtime, unpacks the image and builds the
// slot. A running container owns its rootfs, so as for nspawn a node that is
// already up is left alone; the unpack would otherwise rebuild it in place.
func (p *containerPlan) PrepareNodeRoot(ctx context.Context, s *agentStages) error {
	return containerMode.prepareNodeRoot(ctx, s, p.node, p.hostSeams, func(ctx context.Context) error {
		_, err := p.node.Prepare(ctx)
		return err
	})
}

func (p *containerPlan) EnsureNodeStarted(ctx context.Context, s *agentStages) error {
	return containerMode.ensureNodeStarted(ctx, s, p.node, p.hostSeams)
}
