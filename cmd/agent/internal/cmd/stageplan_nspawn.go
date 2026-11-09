// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// nspawnPlan is the bootstrap plan for the default mode: it unpacks an OS image
// into the kube1 machine and starts the node stack inside it. The bodies are
// the agentStages methods they replace, moved here unchanged except that the
// machine directory is reached through the node root (Root().Path("/"), which
// is the machine directory for nspawn).
type nspawnPlan struct{}

var _ stagePlan = nspawnPlan{}

func (nspawnPlan) EnsureHostClean(ctx context.Context, s *agentStages) error {
	return host.EnsureNoExistingDeployment(ctx, s.log)
}

func (nspawnPlan) ResolveInputs(ctx context.Context, s *agentStages) error {
	downloads, archives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, s.cfg)
	if err != nil {
		return err
	}

	s.archives = archives

	s.gs, err = goalstates.ResolveMachine(s.log, &s.cfg.AgentConfig, goalstates.NSpawnMachineKube1, downloads)
	if err != nil {
		return err
	}

	return nil
}

// nspawnHostSteps are the host preparation tasks of the nspawn mode, in the
// order they run.
func nspawnHostSteps(s *agentStages) phases.Task {
	return phases.Serial(s.log, host.InstallPackages(s.log), phases.Parallel(s.log,
		host.ConfigureOS(s.log), host.ConfigureNFTables(s.log), phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
		host.DisableContainerd(s.log), host.DisableKubelet(s.log), host.DisableSwap(s.log), host.HardenAPT(s.log)))
}

func (nspawnPlan) PrepareHost(ctx context.Context, s *agentStages) error {
	if err := hostroot.Prepare(ctx, s.log, "bin", "libexec"); err != nil {
		return err
	}

	if err := daemon.InstallBootstrapBinary(); err != nil {
		return err
	}

	if err := nspawnHostSteps(s).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/etc", hostroot.Resolve(), installstate.DefaultDirectory)
}

func (nspawnPlan) PrepareNodeRoot(ctx context.Context, s *agentStages) error {
	// ProvisionOwned rebuilds the rootfs in place and must never be pointed at
	// a slot that has started a node, which would pull the filesystem out from
	// under a running one. A registered machine means the rootfs this stage
	// would build is already built and in use, so the requirement is met and
	// there is nothing to do.
	//
	// This is a property of the host, not of how far a previous attempt got. It
	// holds whether the machine was started by an earlier attempt of this
	// installation or independently afterwards.
	//
	// Asked of the slot this bootstrap manages rather than of either slot.
	// Bootstrap only ever builds gs.NodeStart.MachineName, so a machine in the
	// other slot says nothing about whether this one needs a rootfs.
	registered, err := reset.RegisteredMachine(ctx, s.log, s.gs.NodeStart.MachineName)
	if err != nil {
		return err
	}

	if registered {
		s.log.Info("nspawn machine is registered; leaving its rootfs in place", "machine", s.gs.NodeStart.MachineName)

		return nil
	}

	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	if err := phases.Serial(s.log, rootfs.DownloadContainerImageArchives(s.log, s.archives), rootfs.ProvisionOwned(s.log, s.gs.RootFS)).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems(s.gs.RootFS.Root().Path("/"), hostroot.Resolve(), goalstates.SystemdSystemDir, goalstates.SystemdNSpawnDir)
}

func (nspawnPlan) EnsureNodeStarted(ctx context.Context, s *agentStages) error {
	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	// Asked before the stage runs, because afterwards every answer is yes, and
	// asked of the slot this bootstrap manages: a machine in the other slot was
	// not built by this attempt either, but it is not the node being started.
	registered, err := reset.RegisteredMachine(ctx, s.log, s.gs.NodeStart.MachineName)
	if err != nil {
		return err
	}

	if err := s.nodeStartTask(registered).Do(ctx); err != nil {
		return err
	}

	// AgentConfigDir holds the applied config written above, so it must reach
	// disk before this stage reports success.
	return fsutil.SyncFilesystems(s.gs.RootFS.Root().Path("/"), goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
