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
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

type agentStages struct {
	log              *slog.Logger
	cfg              *provision.UnboundedAgentConfig
	gs               *goalstates.MachineGoalState
	archives         *goalstates.ContainerImageArchiveStaging
	reporter         *daemon.BootstrapStatusReporter
	credentialsReady bool

	// stagePlan is the plan of the configured isolation mode; see
	// stageplan.go. Nil is resolved from the mode composition table on first
	// use. Tests set it to drive the stages with a plan of their own.
	stagePlan stagePlan
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
	identity := struct {
		KubernetesVersion string
		OCIImage          string
		APIServer         string
	}{strings.TrimPrefix(cfg.Cluster.Version, "v"), canonicalImageIdentity(cfg.OCIImage), cfg.Kubelet.ApiServer}

	// The isolation mode is part of the identity only when it is not the
	// default, so every nspawn installation, with the mode unset or set to
	// nspawn, keeps its recorded fingerprint. A host installed in one mode then
	// refuses a start in another until it is reset, instead of building a
	// second node stack beside the first.
	var (
		data []byte
		err  error
	)

	if mode, merr := cfg.IsolationMode(); merr == nil && mode != isolation.DefaultMode {
		data, err = json.Marshal(struct {
			KubernetesVersion string
			OCIImage          string
			APIServer         string
			Isolation         string
		}{identity.KubernetesVersion, identity.OCIImage, identity.APIServer, string(mode)})
	} else {
		data, err = json.Marshal(identity)
	}

	if err != nil {
		return bootstrap.Identity{}, err
	}

	return bootstrap.Identity{MachineName: cfg.MachineName, ConfigFingerprint: installstate.Fingerprint(data)}, nil
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

	return fsutil.SyncFilesystems(hostroot.Resolve(), goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
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
