// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// NoneNode is the part of noneops.Node the none operator drives.
type NoneNode interface {
	RecoverUpgrade(ctx context.Context) error
	Restart(ctx context.Context) error
	Upgrade(ctx context.Context) error
	Commit() error
	PruneVersions() error
}

// NoneOperator performs the node operations of a host-native (none) node. The
// host-level operations (discovery, reset, daemon stop, agent upgrade) are the
// host operator's, as for every mode.
//
// Its fields are the host-touching steps; the zero value of each is the real
// host, and tests replace them.
type NoneOperator struct {
	// NewNode builds the node for a configuration. Nil uses noneops.New.
	NewNode func(noneops.Params) NoneNode
	// ResolveDownloads resolves the artifact download overrides and the offline
	// container image archives of a configuration. Nil uses
	// provision.ResolveDownloadOverridesWithOfflineArtifacts.
	ResolveDownloads func(ctx context.Context, cfg *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error)
	// StageArchives stages the offline container image archives. Nil downloads
	// them for real.
	StageArchives func(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error
	// Persist records cfg as the applied config of the none node. Nil writes it
	// where goalstates.AppliedConfigName puts a none node's config.
	Persist func(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) error
}

var _ ModeOperator = NoneOperator{}

func (op NoneOperator) node(p noneops.Params) NoneNode {
	if op.NewNode != nil {
		return op.NewNode(p)
	}

	return noneops.New(p)
}

// EnsureLifecycleMigration runs at every daemon start. A host-native node has
// no nspawn lifecycle hooks to reconcile; what it may have is an upgrade the
// agent did not finish, which is finished here, before the daemon operates the
// node: rolled back, or committed when it had already succeeded.
//
// A recovery that fails is logged and does not stop the daemon. Refusing to
// start would leave the node with no in-band way out: no repave, which retries
// the recovery (see RepaveNode), and no reset.
func (op NoneOperator) EnsureLifecycleMigration(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	if err := op.recoverUpgrade(ctx, log, active); err != nil {
		log.Error("the node may be down; the next repave or agent start tries again", "error", err)
	}

	return nil
}

// recoverUpgrade finishes an interrupted upgrade against the applied config.
// The recovery decides from the configuration, not the version name, so it
// needs no download overrides; the repave's are not persisted with the applied
// config in any case.
func (op NoneOperator) recoverUpgrade(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	if err := op.node(noneops.Params{Log: log, Config: &cfg}).RecoverUpgrade(ctx); err != nil {
		return fmt.Errorf("recover an interrupted node upgrade: %w", err)
	}

	return nil
}

// RestartNode restarts LocalDNS, containerd and kubelet in order (the soft
// reboot) and waits for LocalDNS and kubelet. It never restarts the host, and
// the pods keep running.
func (op NoneOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	log.Info("restarting active node", "slot", active.Name, "isolation", isolation.ModeNone)

	if err := op.node(noneops.Params{Log: log, Config: &cfg}).Restart(ctx); err != nil {
		return fmt.Errorf("restart node: %w", err)
	}

	log.Info("node restarted", "slot", active.Name)

	return nil
}

// RepaveNode upgrades a host-native node in place. The caller has cordoned,
// drained and deleted the Node, as for nspawn. The new version is installed
// beside the running one, then the stack is stopped, its pods ended, host
// networking cleaned as repave does, the current link flipped, the stack
// started and kubelet waited on. If the new version does not come up the
// previous one is started again with the configuration it ran, and the error
// says so. The applied config is persisted only after the new version is
// healthy; the upgrade is committed after that, and the superseded version
// directories are removed last.
//
// The node keeps its mode: a declared mode change is refused by the repave
// reconciler before it gets here.
func (op NoneOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	log.Info("starting node upgrade",
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	// An upgrade that was interrupted and could not be recovered at the daemon
	// start is retried first; Upgrade refuses to start over its journal.
	if err := op.recoverUpgrade(ctx, log, active); err != nil {
		return err
	}

	resolve := op.ResolveDownloads
	if resolve == nil {
		resolve = provision.ResolveDownloadOverridesWithOfflineArtifacts
	}

	downloads, archives, err := resolve(ctx, newCfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	applied := *newCfg
	applied.Isolation = string(isolation.ModeNone)

	if err := op.stageArchives(ctx, log, archives); err != nil {
		return err
	}

	previous := *active.Config

	node := op.node(noneops.Params{Log: log, Config: &applied.AgentConfig, Downloads: downloads, Previous: &previous})
	if err := node.Upgrade(ctx); err != nil {
		return err
	}

	if err := op.persist(ctx, log, &applied.AgentConfig); err != nil {
		// The new version runs, but the agent would read the old applied config
		// at its next start. The journal stays, so that start rolls the node
		// back to the version the applied config describes.
		return err
	}

	if err := node.Commit(); err != nil {
		log.Warn("could not finish the node upgrade; the next agent start finishes it", "error", err)
	}

	if err := node.PruneVersions(); err != nil {
		// The node is healthy on the new version; leftover directories are
		// wasted disk, not a failed upgrade.
		log.Warn("could not remove superseded node versions", "error", err)
	}

	log.Info("node upgrade completed", "version", newCfg.Cluster.Version)

	return nil
}

func (op NoneOperator) stageArchives(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error {
	if op.StageArchives != nil {
		return op.StageArchives(ctx, log, staging)
	}

	return rootfs.DownloadContainerImageArchives(log, staging).Do(ctx)
}

func (op NoneOperator) persist(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) error {
	if op.Persist != nil {
		return op.Persist(ctx, log, cfg)
	}

	return PersistAppliedConfigFor(log, isolation.ModeNone, noneops.Slot, cfg).Do(ctx)
}
