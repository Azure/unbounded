// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/attest"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/bootstrap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// agentStages adapts the agent's existing phase tasks to the coordinator's
// stages.
//
// The coordinator decides what runs after a failure; this decides what each
// stage actually does. Keeping them apart is what lets the recovery behavior
// be tested without a host, and what keeps the task implementations unaware of
// checkpoints.
type agentStages struct {
	log      *slog.Logger
	cfg      *provision.UnboundedAgentConfig
	rootFS   *goalstates.RootFS
	nodeStar *goalstates.NodeStart

	// containerImageArchives are staged during host preparation, before status
	// reporting begins.
	containerImageArchives *goalstates.ContainerImageArchiveStaging
}

func (s *agentStages) EnsureHostClean(ctx context.Context) error {
	return host.EnsureNoExistingDeployment(ctx, s.log, s.cfg.HostPrefix)
}

func (s *agentStages) PrepareHost(ctx context.Context) error {
	// Host preparation is naturally repeatable: every task here converges on a
	// desired state rather than accumulating. It only ever runs before a node
	// exists, which is what makes the firewall flush safe to repeat.
	if err := phases.Serial(s.log,
		host.InstallPackages(s.log),
		phases.Parallel(s.log,
			host.ConfigureOS(s.log),
			host.ConfigureNFTables(s.log),
			phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
			host.DisableContainerd(s.log),
			host.DisableKubelet(s.log),
			host.DisableSwap(s.log),
			host.HardenAPT(s.log),
		),

		rootfs.DownloadContainerImageArchives(s.log, s.containerImageArchives),
	).Do(ctx); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems("/etc", "/var/lib/unbounded")
}

// ResolveInputs performs TPM attestation and folds its result into the config.
//
// This is deliberately outside the checkpointed stages. Attestation yields a
// bootstrap token and cluster CA that live only in memory, so a resumed process
// has to obtain them again however far the previous attempt got; running it
// inside a stage meant a resume past that stage proceeded with no token, which
// on an attested host means no way to join.
//
// It is a no-op when attestation is not configured.
func (s *agentStages) ResolveInputs(ctx context.Context) error {
	if err := phases.ExecuteTask(ctx, s.log,
		attest.ApplyAttestation(s.log, s.cfg.Attest, s.cfg.MachineName, s.nodeStar)); err != nil {
		return err
	}

	syncAttestedKubeletConfig(&s.cfg.AgentConfig, s.nodeStar)

	return nil
}

func (s *agentStages) PrepareRootFS(ctx context.Context, rebuildOwned bool) error {
	rebuild := rootfs.RebuildNever
	if rebuildOwned {
		rebuild = rootfs.RebuildOwned
	}

	if err := phases.ExecuteTask(ctx, s.log, rootfs.Provision(s.log, s.rootFS, rebuild)); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(s.rootFS.MachineDir, s.rootFS.HostPaths.Prefix, "/etc/systemd/system", "/etc/systemd/nspawn")
}

func (s *agentStages) EnsureNodeStarted(ctx context.Context) error {
	// StartNode configures and starts; on a repeat it reconciles an already
	// running machine rather than recreating it, which is what makes this
	// checkpoint safe to re-enter.
	if err := phases.ExecuteTask(ctx, s.log, nodestart.StartNode(s.log, s.nodeStar)); err != nil {
		return err
	}

	if err := phases.ExecuteTask(ctx, s.log,
		nodestart.WaitForKubeletBootstrap(s.log, s.nodeStar.MachineName)); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(s.rootFS.MachineDir, "/etc/systemd/system")
}

func (s *agentStages) EnsureDaemonInstalled(ctx context.Context) error {
	if err := phases.Serial(s.log,
		daemon.PersistAppliedConfig(s.log, s.nodeStar.MachineName, &s.cfg.AgentConfig),
		daemon.EnableDaemon(s.log, s.cfg.HostPrefix),
	).Do(ctx); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(goalstates.HostPrefixOrDefault(s.cfg.HostPrefix), goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}

func (s *agentStages) VerifyInstalled(ctx context.Context) error {
	return daemon.VerifyDaemonInstalled(ctx, s.log)
}

func (s *agentStages) RepairDaemon(ctx context.Context) error {
	return daemon.RepairDaemon(ctx, s.log)
}

// bootstrapReporter adapts the Machine status reporter to the coordinator.
type bootstrapReporter struct {
	reporter   *daemon.BootstrapStatusReporter
	initialize func(context.Context) *daemon.BootstrapStatusReporter
	started    bool
}

func (r *bootstrapReporter) StageStarted(ctx context.Context, checkpoint installstate.Checkpoint) {
	if checkpoint == installstate.CheckpointRepairingDaemon {
		return
	}
	// Report running once, at the first stage that does host work, so a resume
	// does not look like a fresh start.
	if !r.started {
		r.reporter = r.initialize(ctx)
		r.reporter.Running(ctx)
		r.started = true
	}
}

func (r *bootstrapReporter) succeeded(ctx context.Context) {
	if r.reporter != nil {
		r.reporter.Succeeded(ctx)
	}
}

func (r *bootstrapReporter) StageFailed(ctx context.Context, checkpoint installstate.Checkpoint, err error) {
	if r.reporter != nil {
		r.reporter.Failed(ctx, checkpointFailureReason(checkpoint, err), err)
	}
}

// checkpointFailureReason maps a failed stage to the Machine condition reason,
// preserving the reasons callers already match on.
func checkpointFailureReason(checkpoint installstate.Checkpoint, err error) string {
	switch checkpoint {
	case installstate.CheckpointPreparingRootFS:
		return "RootFSFailed"
	case installstate.CheckpointStartingNode:
		return classifyNodeStartFailure(err)
	default:
		return "Failed"
	}
}

// bootstrapIdentity describes what this bootstrap attempt is for.
//
// The fingerprint covers the whole UnboundedAgentConfig rather than only the
// shared AgentConfig, because download overrides and attestation settings are
// part of the intent: a retry that changes them is a different install, not a
// continuation.
func bootstrapIdentity(cfg *provision.UnboundedAgentConfig) (bootstrap.Identity, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return bootstrap.Identity{}, fmt.Errorf("fingerprint agent config: %w", err)
	}

	return bootstrap.Identity{
		MachineName:       cfg.MachineName,
		HostPrefix:        goalstates.HostPrefixOrDefault(cfg.HostPrefix),
		ConfigFingerprint: installstate.Fingerprint(data),
	}, nil
}
