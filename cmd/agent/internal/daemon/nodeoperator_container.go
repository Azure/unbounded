// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// ContainerNode is the part of a containerops.Node the container operator
// drives. Upgrade takes the node it replaces.
type ContainerNode interface {
	Restart(ctx context.Context) error
	RewriteUnit(ctx context.Context) error
	Upgrade(ctx context.Context, old ContainerNode) error
	Destroy(ctx context.Context) error
}

// realContainerNode adapts *containerops.Node to ContainerNode.
type realContainerNode struct{ *containerops.Node }

func (n realContainerNode) Upgrade(ctx context.Context, old ContainerNode) error {
	o, ok := old.(realContainerNode)
	if !ok {
		return fmt.Errorf("cannot upgrade from %T", old)
	}

	return n.Node.Upgrade(ctx, o.Node)
}

// ContainerOperator performs the node operations of a node that runs in an
// OCI container. The host-level operations (discovery, reset, daemon stop,
// agent upgrade) are the host operator's, as for every mode.
//
// A container node is blue/green like nspawn: it runs in slot kube1 or kube2
// and records its applied config where goalstates.AppliedConfigName puts it
// for the container mode, which agents that predate the mode never read.
//
// Its fields are the host-touching steps; the zero value of each is the real
// host, and tests replace them.
//
// PROVISIONAL: the container mode is experimental; see the containerops package
// comment for what is unverified.
type ContainerOperator struct {
	// NewNode builds the node of a slot. Nil uses containerops.New.
	NewNode func(containerops.Params) ContainerNode
	// ResolveDownloads resolves the artifact download overrides and the
	// offline container image archives of a configuration. Nil uses
	// provision.ResolveDownloadOverridesWithOfflineArtifacts.
	ResolveDownloads func(ctx context.Context, cfg *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error)
	// StageArchives stages the offline container image archives. Nil downloads
	// them for real.
	StageArchives func(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error
	// Persist records cfg as the applied config of slot. Nil writes it where
	// goalstates.AppliedConfigName puts a container node's config.
	Persist func(ctx context.Context, log *slog.Logger, slot string, cfg *provision.AgentConfig) error
	// Remove removes the applied config record of slot. Nil removes the file.
	Remove func(ctx context.Context, log *slog.Logger, slot string) error
}

var _ ModeOperator = ContainerOperator{}

func (op ContainerOperator) node(p containerops.Params) ContainerNode {
	if op.NewNode != nil {
		return op.NewNode(p)
	}

	return realContainerNode{containerops.New(p)}
}

// EnsureLifecycleMigration re-renders the supervising unit so its hooks name
// the current lifecycle helper path (the host root may have moved it), and
// reloads systemd without restarting the container. The nspawn lifecycle files
// do not exist for a container.
func (op ContainerOperator) EnsureLifecycleMigration(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	if err := op.node(containerops.Params{Log: log, Config: &cfg, Slot: active.Name}).RewriteUnit(ctx); err != nil {
		return fmt.Errorf("reconcile container unit: %w", err)
	}

	log.Info("container unit reconciled", "slot", active.Name)

	return nil
}

// RestartNode restarts the container unit (the soft reboot), which re-runs the
// lifecycle hooks, and waits for systemd, LocalDNS and kubelet. It never
// restarts the host.
func (op ContainerOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	log.Info("restarting active node", "slot", active.Name, "isolation", isolation.ModeContainer)

	if err := op.node(containerops.Params{Log: log, Config: &cfg, Slot: active.Name}).Restart(ctx); err != nil {
		return fmt.Errorf("restart node: %w", err)
	}

	log.Info("node restarted", "slot", active.Name)

	return nil
}

// RepaveNode moves a container node to the alternate slot, as for nspawn. The
// caller has cordoned, drained and deleted the Node. The new slot is built
// beside the running one, then the old container is stopped and disabled, host
// networking cleaned as repave does, and the new container started and waited
// on. If the new slot does not come up it is removed and, once it is confirmed
// gone, the old container is started again, and the error says so. The
// applied config is persisted only after the new slot is healthy, then the old
// slot's record and files are removed.
//
// The node keeps its mode: a declared mode change is handled by the repave
// reconciler before it gets here, so the record is written as container
// whatever the desired config says.
func (op ContainerOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	oldSlot := active.Name
	newSlot := goalstates.AlternateMachine(oldSlot)

	log.Info("starting node repave",
		"old_slot", oldSlot,
		"new_slot", newSlot,
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	resolve := op.ResolveDownloads
	if resolve == nil {
		resolve = provision.ResolveDownloadOverridesWithOfflineArtifacts
	}

	downloads, archives, err := resolve(ctx, newCfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	applied := *newCfg
	applied.Isolation = string(isolation.ModeContainer)

	if err := op.stageArchives(ctx, log, archives); err != nil {
		return err
	}

	oldCfg := *active.Config
	oldNode := op.node(containerops.Params{Log: log, Config: &oldCfg, Slot: oldSlot})
	newNode := op.node(containerops.Params{Log: log, Config: &applied.AgentConfig, Downloads: downloads, Slot: newSlot})

	if err := newNode.Upgrade(ctx, oldNode); err != nil {
		return err
	}

	if err := op.persist(ctx, log, newSlot, &applied.AgentConfig); err != nil {
		return err
	}

	// The record goes before the files: if removing the files fails the node
	// is still correctly described, and the leftover slot is rebuilt from
	// scratch by the next repave.
	if err := op.remove(ctx, log, oldSlot); err != nil {
		return err
	}

	if err := oldNode.Destroy(ctx); err != nil {
		// The node is healthy on the new slot, and the old one cannot come
		// back: Upgrade stopped and disabled its unit, the units conflict, and
		// its pre-start hook refuses while the other slot runs. What is left
		// is disk, which the next repave removes before it reuses the slot.
		log.Warn("could not remove the superseded slot; the next repave removes it", "slot", oldSlot, "error", err)
	}

	log.Info("node repave completed", "active_slot", newSlot, "version", newCfg.Cluster.Version)

	return nil
}

func (op ContainerOperator) stageArchives(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error {
	if op.StageArchives != nil {
		return op.StageArchives(ctx, log, staging)
	}

	return rootfs.DownloadContainerImageArchives(log, staging).Do(ctx)
}

func (op ContainerOperator) persist(ctx context.Context, log *slog.Logger, slot string, cfg *provision.AgentConfig) error {
	if op.Persist != nil {
		return op.Persist(ctx, log, slot, cfg)
	}

	return PersistAppliedConfigFor(log, isolation.ModeContainer, slot, cfg).Do(ctx)
}

func (op ContainerOperator) remove(ctx context.Context, log *slog.Logger, slot string) error {
	if op.Remove != nil {
		return op.Remove(ctx, log, slot)
	}

	return RemoveAppliedConfig(log, goalstates.AppliedConfigName(isolation.ModeContainer, slot)).Do(ctx)
}
