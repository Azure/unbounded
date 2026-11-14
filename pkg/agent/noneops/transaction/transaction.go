// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package transaction adds durable, forward-only upgrade recovery to a
// host-native noneops.Node. Callers authorize destructive recovery only after
// draining workloads and observing deletion of the previous Kubernetes Node.
package transaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

// Slot is the applied configuration slot of a host-native node.
const Slot = noneops.Slot

// Params bind a transactional node to its target and source configurations.
// StateDir is required and, like ExtraSnapshotPaths, is a host path prefixed by
// Hooks.HostRoot in filesystem tests. Completion is opaque caller-owned state.
// Node methods are serialized by the caller, including Commit and ClearStartup.
type Params struct {
	// Log is required. Lifecycle operations never substitute a global logger.
	Log *slog.Logger
	// Config is required for Upgrade so exposed startup can replay it.
	Config                  *config.AgentConfig
	Downloads               *goalstates.DownloadOverrides
	GoalState               *goalstates.MachineGoalState
	Previous                *config.AgentConfig
	PreviousGoalState       *goalstates.MachineGoalState
	Generation              string
	PreviousGeneration      string
	ArtifactVersion         string
	PreviousArtifactVersion string
	Completion              json.RawMessage
	PreviousCompletion      json.RawMessage
	StateDir                string
	ExtraSnapshotPaths      []string
	// Stage runs after the source has stopped and its pods have been reaped.
	Stage func(context.Context) error
	// Install adds application artifacts through the base noneops installer.
	Install func(context.Context, *goalstates.MachineGoalState) error
	// ReapPods overrides scoped cleanup in tests. Nil uses Backend.ReapPods.
	ReapPods func() error
	noneops.Overrides
	hostnode.Hooks
}

// Node reuses the standalone lifecycle for provisioning, configuration, GPU
// setup, artifacts, and ordinary starts/restarts. Upgrade uses a separate durable
// protocol; it never invokes the standalone Node.Upgrade or Backend.Upgrade.
type Node struct {
	base    *noneops.Node
	p       Params
	once    sync.Once
	err     error
	gs      *goalstates.MachineGoalState
	backend *none.Backend
	version string
}

// New defers goal resolution and validation of required Params until first use.
func New(p Params) *Node {
	return &Node{p: p}
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		if err := n.validate(); err != nil {
			n.err = err
			return
		}

		n.base = noneops.New(noneops.Params{
			Log: n.p.Log, Config: n.p.Config, Downloads: n.p.Downloads, GoalState: n.p.GoalState,
			Previous: n.p.Previous, PreviousGoalState: n.p.PreviousGoalState,
			ArtifactVersion: n.p.ArtifactVersion, Install: n.p.Install,
			Overrides: n.p.Overrides, Hooks: n.p.Hooks,
		})

		n.gs, n.err = n.base.GoalState()
		if n.err != nil {
			return
		}

		n.backend, n.err = n.base.Backend()
		if n.err != nil {
			return
		}

		n.version, n.err = n.base.Version()
	})

	return n.err
}

func (n *Node) validate() error {
	if n.p.Log == nil {
		return errors.New("transaction Log is required")
	}

	if n.p.StateDir == "" {
		return errors.New("transaction StateDir is required")
	}

	if !filepath.IsAbs(n.p.StateDir) {
		return errors.New("transaction StateDir must be an absolute host path")
	}

	return nil
}

// GoalState returns the host-native goal state resolved by the base node.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

// Backend returns the base node's configured public backend.
func (n *Node) Backend() (*none.Backend, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend, nil
}

// Version returns the version directory identity, including application artifacts.
func (n *Node) Version() (string, error) {
	if err := n.resolve(); err != nil {
		return "", err
	}

	return n.version, nil
}

// Prepare installs the boot guard before delegating installation to the base node.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	if err := n.EnsureBootGuard(ctx); err != nil {
		return nil, err
	}

	return n.base.Prepare(ctx)
}

// Provision prepares and activates the first installation.
func (n *Node) Provision(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.EnsureBootGuard(ctx); err != nil {
		return err
	}

	return n.base.Provision(ctx)
}

// Configure delegates shared host configuration and networking to the base node.
func (n *Node) Configure(ctx context.Context) (bool, bool, error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	return n.base.Configure(ctx)
}

// Running reports whether the entire stack is active.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := n.resolve(); err != nil {
		return false, err
	}

	return n.base.Running(ctx)
}

// Start uses the ordinary standalone start behavior, including changed-unit restarts.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return n.base.Start(ctx, wasRunning, containerdChanged, kubeletChanged)
}

// Restart delegates the ordinary soft restart, preserving workloads.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return n.base.Restart(ctx)
}

// Upgrade stops and reaps the source before preparation. Once startup intent is
// durable, failures and crashes can only complete that startup forward.
func (n *Node) Upgrade(ctx context.Context) error { return n.upgradePrepared(ctx) }

// Commit removes rollback data after the caller persists applied state. Startup
// remains until the caller observes registration and invokes ClearStartup.
func (n *Node) Commit() error {
	if err := n.validate(); err != nil {
		return err
	}

	if err := confsnap.Discard(n.configSnapshotDir()); err != nil {
		return fmt.Errorf("remove saved configuration: %w", err)
	}

	return n.removeJournal()
}

// RecoverUpgrade resumes exposed startup without rewriting files or ending pods.
// Before exposure it may restore the source; the caller must authorize that
// destructive action by first observing Kubernetes Node absence.
func (n *Node) RecoverUpgrade(ctx context.Context) error {
	startup, err := n.PendingStartup()
	if err != nil {
		return err
	}

	if startup != nil {
		return n.ResumeStartup(ctx)
	}

	j, err := n.readJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if err := n.resolve(); err != nil {
		return err
	}

	current, err := n.p.Layout.Current()
	if err != nil {
		return err
	}

	if j.TargetGeneration != "" && j.TargetGeneration == n.p.Generation &&
		j.TargetConfig != "" && j.TargetConfig == configDigest(n.p.Config) &&
		j.TargetArtifactVersion == n.p.ArtifactVersion && current == j.Target {
		n.bindVersion(current)

		if err := n.publishStartup(false); err != nil {
			return err
		}

		return n.ResumeStartup(ctx)
	}

	if j.Previous == "" {
		return fmt.Errorf("upgrade journal %s names no source version", n.journalPath())
	}

	source, err := n.previous()
	if err != nil {
		return err
	}

	return source.restoreSource(ctx, j.Previous)
}

type upgradeJournal struct {
	Previous              string `json:"previous"`
	Target                string `json:"target"`
	TargetConfig          string `json:"targetConfig,omitempty"`
	TargetGeneration      string `json:"targetGeneration,omitempty"`
	TargetArtifactVersion string `json:"targetArtifactVersion,omitempty"`
}

func configDigest(cfg *config.AgentConfig) string {
	if cfg == nil {
		return ""
	}

	data, _ := json.Marshal(cfg) //nolint:errcheck // plain data always marshals
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func (n *Node) stateDir() string    { return filepath.Join(n.p.HostRoot, n.p.StateDir) }
func (n *Node) journalPath() string { return filepath.Join(n.stateDir(), "none-upgrade-journal.json") }

func (n *Node) configSnapshotDir() string {
	if n.p.ConfigSnapshotDir != "" {
		return n.p.ConfigSnapshotDir
	}

	return filepath.Join(n.stateDir(), "none-upgrade-config")
}

func (n *Node) readJournal() (upgradeJournal, error) {
	var j upgradeJournal
	if err := n.validate(); err != nil {
		return j, err
	}

	data, err := os.ReadFile(n.journalPath())
	if err != nil {
		return j, err
	}

	if err := json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("decode upgrade journal %s: %w", n.journalPath(), err)
	}

	return j, nil
}

func (n *Node) writeJournal(j upgradeJournal) error {
	if err := n.validate(); err != nil {
		return err
	}

	data, err := json.Marshal(j)
	if err != nil {
		return err
	}

	return fsutil.WriteFileDurable(n.journalPath(), data, 0o600)
}

func (n *Node) removeJournal() error {
	if err := os.Remove(n.journalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return fsutil.SyncDir(n.stateDir())
}

func (n *Node) previous() (*Node, error) {
	if n.p.Previous == nil && n.p.PreviousGoalState == nil {
		return n, nil
	}

	p := n.p
	p.Config, p.GoalState, p.Previous, p.PreviousGoalState = n.p.Previous, n.p.PreviousGoalState, nil, nil
	p.ArtifactVersion, p.PreviousArtifactVersion = n.p.PreviousArtifactVersion, ""
	p.Generation, p.Completion = n.p.PreviousGeneration, n.p.PreviousCompletion
	p.PreviousGeneration, p.PreviousCompletion = "", nil

	prev := New(p)
	if err := prev.resolve(); err != nil {
		return nil, fmt.Errorf("resolve source configuration: %w", err)
	}

	return prev, nil
}

func (n *Node) root() none.Root {
	return none.Root{Layout: n.p.Layout, Version: n.version, HostRoot: n.p.HostRoot}
}

func (n *Node) nodeSpec() isolation.NodeSpec {
	return isolation.NodeSpec{Slot: n.version, Version: n.gs.RootFS.KubernetesVersion, Grants: n.gs.RootFS.Grants()}
}
func (n *Node) unitSpec() none.UnitSpec { return n.backend.Units }

// bindVersion honors persisted directory names if resolution defaults changed.
func (n *Node) bindVersion(version string) {
	n.version = version
	n.gs.RootFS.RootOverride, n.gs.NodeStart.RootOverride = n.root(), n.root()
}

func (n *Node) resetHostNetwork(ctx context.Context) error {
	if err := n.cleanupNetwork(ctx); err != nil {
		return err
	}

	return n.p.SetupLocalDNSNetwork(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) cleanupNetwork(ctx context.Context) error { return n.p.CleanupHostNetwork(ctx, n.p.Log) }

func (n *Node) setupGPU(ctx context.Context) error {
	if n.backend.BeforeStart != nil {
		return n.backend.BeforeStart(ctx)
	}

	return nil
}

func (n *Node) waitKubelet(ctx context.Context) error {
	if n.p.WaitKubelet != nil {
		return n.p.WaitKubelet(ctx)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, none.UnitKubelet).Do(ctx)
}

func (n *Node) waitBootstrap(ctx context.Context) error {
	return phases.Serial(n.p.Log, nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, n.gs.NodeStart)).Do(ctx)
}

func (n *Node) reapPods() error {
	if n.p.ReapPods != nil {
		return n.p.ReapPods()
	}

	return n.backend.ReapPods()
}

func (n *Node) configSnapshotPaths() []string {
	return append(append(confsnap.SharedPaths(),
		none.DropInDir(goalstates.SystemdSystemDir, none.UnitContainerd),
		none.DropInDir(goalstates.SystemdSystemDir, none.UnitLocalDNS),
		filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSSliceUnit)), n.p.ExtraSnapshotPaths...)
}

// PruneVersions delegates removal of inactive artifacts to the base node.
func (n *Node) PruneVersions() error {
	if err := n.resolve(); err != nil {
		return err
	}

	return n.base.PruneVersions()
}

func unitDir(dir string) string                  { return hostnode.OrDefault(dir, none.SystemdDir) }
func execSpec(args ...string) isolation.ExecSpec { return isolation.ExecSpec{Args: args} }
