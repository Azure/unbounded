// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// The plans of the modes that run the node stack outside an nspawn machine
// (none) differ in how a node is built and started, not in the bootstrap
// sequence around it. The sequence lives here; each plan owns its node and
// states what differs through a hostMode.

// hostNode is the part of a host-native node the shared sequence drives.
type hostNode interface {
	Running(ctx context.Context) (bool, error)
	Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error)
	Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error
}

// hostMode is what differs between the host-native plans.
type hostMode struct {
	mode isolation.Mode
	// slot is the slot the node is built in and its applied config recorded under.
	slot string
	// base is the layout directory synced after the node is built.
	base string
	// prepareTask names the task that builds the node root.
	prepareTask string
	// configureTask and startTask name the tasks of the start stage.
	configureTask, startTask string
	// runningMsg and runningAttrs are logged when the node root is not rebuilt
	// because the node is already up.
	runningMsg   string
	runningAttrs []any
}

// hostSeams are the host-touching steps of a host-native plan; tests replace them.
type hostSeams struct {
	sync     func(paths ...string) error
	archives func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task
	persist  func(log *slog.Logger, mode isolation.Mode, slot string, cfg *config.AgentConfig) phases.Task
}

func (h hostSeams) syncFS(paths ...string) error {
	if h.sync != nil {
		return h.sync(paths...)
	}

	return fsutil.SyncFilesystems(paths...)
}

func (h hostSeams) downloadArchives(s *agentStages) phases.Task {
	if h.archives != nil {
		return h.archives(s.log, s.archives)
	}

	return rootfs.DownloadContainerImageArchives(s.log, s.archives)
}

// persistApplied records cfg as the applied config of slot in mode, under the
// name goalstates.AppliedConfigName gives it.
func (h hostSeams) persistApplied(log *slog.Logger, mode isolation.Mode, slot string, cfg *config.AgentConfig) phases.Task {
	if h.persist != nil {
		return h.persist(log, mode, slot, cfg)
	}

	return daemon.PersistAppliedConfigFor(log, mode, slot, cfg)
}

// goalStater is a node that exposes its resolved goal state.
type goalStater interface {
	GoalState() (*goalstates.MachineGoalState, error)
}

// resolveInputs resolves the download overrides, the offline archives and the
// goal state for the mode's slot into s, then builds the node from the
// overrides. Resolving the node's goal state now applies its overrides to s.gs,
// which the mode independent stages (attestation) share.
func (m hostMode) resolveInputs(ctx context.Context, s *agentStages, newNode func(downloads *goalstates.DownloadOverrides) goalStater) error {
	downloads, archives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, s.cfg)
	if err != nil {
		return err
	}

	s.archives = archives

	s.gs, err = goalstates.ResolveMachine(s.log, &s.cfg.AgentConfig, m.slot, downloads)
	if err != nil {
		return err
	}

	_, err = newNode(downloads).GoalState()

	return err
}

// prepareHostSteps is the host preparation every plan runs: the host root, the
// bootstrap binary, then steps in order, then the sync that makes it durable.
func prepareHostSteps(ctx context.Context, s *agentStages, steps ...phases.Task) error {
	if err := hostroot.Prepare(ctx, s.log, "bin", "libexec"); err != nil {
		return err
	}

	if err := daemon.InstallBootstrapBinary(); err != nil {
		return err
	}

	if err := phases.Serial(s.log, steps...).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/etc", hostroot.Resolve(), installstate.DefaultDirectory)
}

// prepareNodeRoot builds the node root unless the node is already running: a
// running node owns its files, so as for nspawn it is left alone. build does
// the mode's work once the archives are downloaded.
func (m hostMode) prepareNodeRoot(ctx context.Context, s *agentStages, node hostNode, seams hostSeams, build func(context.Context) error) error {
	running, err := node.Running(ctx)
	if err != nil {
		return err
	}

	if running {
		s.log.Info(m.runningMsg, m.runningAttrs...)

		return nil
	}

	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	if err := phases.Serial(s.log,
		seams.downloadArchives(s),
		hostnode.Func(m.prepareTask, build),
	).Do(ctx); err != nil {
		return err
	}

	return seams.syncFS(m.base, hostroot.Resolve(), goalstates.SystemdSystemDir)
}

// ensureNodeStarted configures and starts the node and, when this run built it,
// records the applied config.
func (m hostMode) ensureNodeStarted(ctx context.Context, s *agentStages, node hostNode, seams hostSeams) error {
	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	// Asked before the stage runs, as for nspawn, because afterwards the answer
	// is always yes. A node found running was not built by this attempt, so the
	// applied config is left alone; see nodeStartTask.
	wasRunning, err := node.Running(ctx)
	if err != nil {
		return err
	}

	var containerdChanged, kubeletChanged bool

	tasks := []phases.Task{
		hostnode.Func(m.configureTask, func(ctx context.Context) (err error) {
			containerdChanged, kubeletChanged, err = node.Configure(ctx)
			return err
		}),
		hostnode.Func(m.startTask, func(ctx context.Context) error {
			return node.Start(ctx, wasRunning, containerdChanged, kubeletChanged)
		}),
	}

	if !wasRunning {
		tasks = append(tasks, seams.persistApplied(s.log, m.mode, m.slot, &s.cfg.AgentConfig))
	}

	if err := phases.Serial(s.log, tasks...).Do(ctx); err != nil {
		return err
	}

	return seams.syncFS(m.base, goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
