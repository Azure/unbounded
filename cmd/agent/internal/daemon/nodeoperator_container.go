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
)

func init() {
	registerNodeOperator(isolation.ModeContainer, func() nodeOperator { return containerNodeOperator{} })
}

// containerNode is the part of a containerops.Node the operator uses, so tests
// can replace the host-mutating implementation. Upgrade takes the node it
// replaces.
type containerNode interface {
	Restart(ctx context.Context) error
	RewriteUnit(ctx context.Context) error
	Upgrade(ctx context.Context, old containerNode) error
	Destroy(ctx context.Context) error
}

// realContainerNode adapts *containerops.Node to containerNode.
type realContainerNode struct{ *containerops.Node }

func (n realContainerNode) Upgrade(ctx context.Context, old containerNode) error {
	o, ok := old.(realContainerNode)
	if !ok {
		return fmt.Errorf("cannot upgrade from %T", old)
	}

	return n.Node.Upgrade(ctx, o.Node)
}

var newContainerNode = func(p containerops.Params) containerNode { return realContainerNode{containerops.New(p)} }

// containerNodeOperator performs node operations for a node that runs in an OCI
// container. It embeds the nspawn operator for the operations that are about
// the host and not the node stack: resetting the agent, stopping the daemon,
// staging an agent upgrade and the isolation change, whose error already names
// the active mode.
//
// A container node records its applied config under its slot (kube1 or kube2,
// blue/green as for nspawn), so the discovery the dispatcher delegates to the
// default operator finds it with no change.
//
// PROVISIONAL: the container mode is experimental; see the containerops package
// comment for what is unverified.
type containerNodeOperator struct {
	nspawnNodeOperator
}

// EnsureLifecycleMigration re-renders the supervising unit so its hooks name the
// current lifecycle helper path (the host root may have moved it), and reloads
// systemd without restarting the container. The nspawn lifecycle files do not
// exist for a container.
func (containerNodeOperator) EnsureLifecycleMigration(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	node := newContainerNode(containerops.Params{Log: log, Config: &cfg, Slot: active.Name})
	if err := node.RewriteUnit(ctx); err != nil {
		return fmt.Errorf("reconcile container unit: %w", err)
	}

	log.Info("container unit reconciled", "slot", active.Name)

	return nil
}

// RestartNode restarts the container unit (the soft reboot), which re-runs the
// lifecycle hooks, and waits for systemd, LocalDNS and kubelet. It never
// restarts the host.
func (containerNodeOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	return restartActiveNode(ctx, log, active, newContainerNode(containerops.Params{Log: log, Config: &cfg, Slot: active.Name}).Restart)
}

// RepaveNode moves a container node to the alternate slot, as for nspawn. The
// caller has cordoned, drained and deleted the Node. The new slot is built
// beside the running one, then the old container is stopped, host networking
// cleaned as repave does, and the new container started and waited on. If the
// new slot does not come up it is removed and the old container is started
// again, and the error says so. The applied config is persisted only after the
// new slot is healthy, then the old slot's record and files are removed.
//
// The applied config records the mode the node runs in. The desired config is
// derived from the applied one, so it already carries that mode and the repave
// sets it again only so that a caller passing another config cannot change the
// mode here: a different mode is handled by the repave reconciler before it
// gets here (changeIsolation), never by this method.
func (containerNodeOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	oldSlot := active.Name
	newSlot := goalstates.AlternateMachine(oldSlot)

	log.Info("starting node repave",
		"old_slot", oldSlot,
		"new_slot", newSlot,
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	downloads, archives, err := resolveNodeDownloads(ctx, newCfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	applied := *newCfg
	applied.Isolation = string(isolation.ModeContainer)

	if err := stageNodeArchives(ctx, log, archives); err != nil {
		return err
	}

	oldCfg := *active.Config
	oldNode := newContainerNode(containerops.Params{Log: log, Config: &oldCfg, Slot: oldSlot})
	newNode := newContainerNode(containerops.Params{Log: log, Config: &applied.AgentConfig, Downloads: downloads, Slot: newSlot})

	if err := newNode.Upgrade(ctx, oldNode); err != nil {
		return err
	}

	if err := persistNodeConfig(ctx, log, newSlot, &applied.AgentConfig); err != nil {
		return err
	}

	// The record goes before the files: if removing the files fails the node
	// is still correctly described, and the leftover slot is rebuilt from
	// scratch by the next repave.
	if err := removeNodeConfig(ctx, log, oldSlot); err != nil {
		return err
	}

	if err := oldNode.Destroy(ctx); err != nil {
		// The node is healthy on the new slot; leftover files are wasted disk,
		// not a failed repave.
		log.Warn("could not remove the superseded slot", "slot", oldSlot, "error", err)
	}

	log.Info("node repave completed", "active_slot", newSlot, "version", newCfg.Cluster.Version)

	return nil
}

// ReconcileHostRoot is the nspawn operator's with this operator as the one
// whose EnsureLifecycleMigration runs, so no nspawn files are written.
func (op containerNodeOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error) {
	return reconcileHostRoot(ctx, log, op, active)
}

var _ nodeOperator = containerNodeOperator{}

// containerTeardown is the reset path of a container node.
var containerTeardown = func(log *slog.Logger) *containerops.Teardown {
	return &containerops.Teardown{Log: log}
}
