// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	"github.com/Azure/unbounded/cmd/agent/internal/attest"
	"github.com/Azure/unbounded/cmd/agent/internal/bootstrap"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type agentStages struct {
	log              *slog.Logger
	cfg              *provision.UnboundedAgentConfig
	gs               *goalstates.MachineGoalState
	archives         *goalstates.ContainerImageArchiveStaging
	reporter         *daemon.BootstrapStatusReporter
	credentialsReady bool
}

// canonicalImageIdentity reduces an OCI image reference to the part that
// determines which image gets installed. HTTPS archive references carry
// expiring signed query parameters, so a refreshed signature points at the same
// artifact and must not read as a different installation. Registry and
// oci-layout references carry no such credentials and are used as-is.
//
// Trailing path slashes are trimmed to match how parseHTTPSArchiveReference
// normalizes the reference before fetching it.
func canonicalImageIdentity(image string) string {
	if !strings.HasPrefix(image, "https://") {
		return image
	}

	parsed, err := url.Parse(image)
	if err != nil {
		// Unparseable references fail later at acquire time with a better
		// message. Hash the original so identity stays deterministic.
		return image
	}

	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")

	return parsed.String()
}

func bootstrapIdentity(cfg *provision.UnboundedAgentConfig) (bootstrap.Identity, error) {
	// Keep identity tied to the cluster and installed rootfs, while allowing
	// credentials and artifact locations to be refreshed for a retry.
	//
	// HostPrefix enters the hash only when it resolves somewhere other than the
	// default, and carries omitempty so that at the default it contributes
	// nothing at all. Every host already in the field was fingerprinted without
	// this input; if the default hashed as a value, each of them would read as a
	// different installation and demand an explicit reset on upgrade, for a
	// field they never set. TestBootstrapV1CompatibilityFixtures catches that.
	//
	// It is the resolved prefix that matters, not how it was written. Leaving it
	// unset and naming /usr/local explicitly put the files in the same place, so
	// they are the same installation and must hash alike.
	//
	// A prefix that resolves elsewhere does belong in the identity. The agent's
	// own files live under it, so starting with a different one is not a retry:
	// it would leave the first installation behind and build a second one
	// beside it.
	resolvedPrefix := goalstates.HostPrefixOrDefault(cfg.HostPrefix)

	fingerprintedPrefix := resolvedPrefix
	if fingerprintedPrefix == goalstates.DefaultHostPrefix {
		fingerprintedPrefix = ""
	}

	data, err := json.Marshal(struct {
		KubernetesVersion string
		OCIImage          string
		APIServer         string
		HostPrefix        string `json:",omitempty"`
	}{
		strings.TrimPrefix(cfg.Cluster.Version, "v"),
		canonicalImageIdentity(cfg.OCIImage),
		cfg.Kubelet.ApiServer,
		fingerprintedPrefix,
	})
	if err != nil {
		return bootstrap.Identity{}, err
	}

	return bootstrap.Identity{
		MachineName:       cfg.MachineName,
		ConfigFingerprint: installstate.Fingerprint(data),
		// Resolved rather than configured, so the record names a real directory
		// instead of an empty string meaning "wherever the default was at the
		// time", which is what teardown would have to guess from.
		HostPrefix: resolvedPrefix,
	}, nil
}

func (s *agentStages) EnsureHostClean(ctx context.Context) error {
	return host.EnsureNoExistingDeployment(ctx, s.log)
}

func (s *agentStages) ResolveInputs(ctx context.Context) error {
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

func (s *agentStages) PrepareHost(ctx context.Context) error {
	if err := daemon.InstallBootstrapBinary(s.cfg.HostPrefix); err != nil {
		return err
	}

	if err := phases.Serial(s.log, host.InstallPackages(s.log), phases.Parallel(s.log,
		host.ConfigureOS(s.log), host.ConfigureNFTables(s.log), phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
		host.DisableContainerd(s.log), host.DisableKubelet(s.log), host.DisableSwap(s.log), host.HardenAPT(s.log))).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/etc", "/usr/local", installstate.DefaultDirectory)
}

// Credentials must be resolved on every unfinished attempt, but TPM prerequisites
// must first be installed on a fresh host. This stage always precedes node work.
func (s *agentStages) prepareCredentials(ctx context.Context) error {
	if s.credentialsReady {
		return nil
	}

	if err := attest.ApplyAttestation(s.log, s.cfg.Attest, s.cfg.MachineName, s.gs.NodeStart).Do(ctx); err != nil {
		return err
	}

	syncAttestedKubeletConfig(&s.cfg.AgentConfig, s.gs.NodeStart)

	// The reporter is built here rather than in the constructor because it
	// captures credentials at construction: an empty bootstrap token makes it a
	// permanent no-op, and it registers the Machine over the API. On an attested
	// host the token does not exist until ApplyAttestation has run just above,
	// so constructing it earlier would silently disable status reporting for the
	// whole bootstrap and issue the registration call before admission.
	if s.reporter == nil {
		s.reporter = daemon.NewBootstrapStatusReporter(ctx, s.log, &s.cfg.AgentConfig)
		s.reporter.Running(ctx)
	}

	s.credentialsReady = true

	return nil
}

func (s *agentStages) PrepareRootFS(ctx context.Context) error {
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

	return fsutil.SyncFilesystems(s.gs.RootFS.MachineDir, "/usr/local", goalstates.SystemdSystemDir, goalstates.SystemdNSpawnDir)
}

// nodeStartTask composes the work that brings the node up.
//
// The applied config records what the running node was built from, and the
// daemon compares it against the desired config to decide whether the node has
// drifted far enough to need a repave. Only an attempt that actually built the
// node may write it. nodeAlreadyBuilt says a machine was already registered
// when this stage began, so this attempt found the node standing rather than
// raising it.
//
// Writing it anyway would be a claim the host cannot support. Node labels reach
// a node through kubelet's --node-labels at registration; restarting kubelet
// under an already-registered node does not revise them. Recording a label the
// node never took would leave the applied config matching the desired config,
// which reads as no drift, which is precisely what suppresses the repave that
// would have delivered it. Leaving the record alone keeps the difference
// visible and lets the daemon resolve it.
func (s *agentStages) nodeStartTask(nodeAlreadyBuilt bool) phases.Task {
	tasks := []phases.Task{
		nodestart.StartNode(s.log, s.gs.NodeStart),
		nodestart.WaitForKubeletBootstrap(s.log, s.gs.NodeStart.MachineName),
	}

	if !nodeAlreadyBuilt {
		tasks = append(tasks, daemon.PersistAppliedConfig(s.log, s.gs.NodeStart.MachineName, &s.cfg.AgentConfig))
	}

	return phases.Serial(s.log, tasks...)
}

func (s *agentStages) EnsureNodeStarted(ctx context.Context) error {
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
	return fsutil.SyncFilesystems(s.gs.RootFS.MachineDir, goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}

func (s *agentStages) daemonInstallTask() phases.Task {
	return phases.Serial(s.log, daemon.EnableDaemon(s.log))
}

func (s *agentStages) EnsureDaemonInstalled(ctx context.Context) error {
	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	if err := s.daemonInstallTask().Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/usr/local", goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}

func (s *agentStages) VerifyInstalled(ctx context.Context) error {
	return daemon.VerifyDaemonInstalled(ctx, s.log)
}

func (s *agentStages) RepairDaemon(ctx context.Context) error { return daemon.RepairDaemon(ctx, s.log) }

func (s *agentStages) StageStarted(_ context.Context, stage bootstrap.Stage) {
	s.log.Info("bootstrap stage", "stage", stage)
}

func (s *agentStages) StageFailed(ctx context.Context, stage bootstrap.Stage, err error) {
	reason := "Failed"
	if stage == bootstrap.StagePrepareRootFS {
		reason = "RootFSFailed"
	}

	if stage == bootstrap.StageStartNode {
		reason = classifyNodeStartFailure(err)
	}

	// Safe before the reporter exists: it reports through a nil-receiver check.
	s.reporter.Failed(ctx, reason, err)
}
