// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func init() {
	registerNodeOperator(isolation.ModeNone, func() nodeOperator { return noneNodeOperator{} })
}

// noneNode is the part of noneops.Node the operator uses, so tests can replace
// the host-mutating implementation.
type noneNode interface {
	Restart(ctx context.Context) error
	Upgrade(ctx context.Context) error
	PruneVersions() error
}

var newNoneNode = func(p noneops.Params) noneNode { return noneops.New(p) }

// noneNodeOperator performs node operations for a host-native node. It embeds
// the nspawn operator for the operations that are about the host and not the
// node stack: resetting the agent, stopping the daemon, staging an agent
// upgrade, and the isolation change (which finds the source mode in the
// active machine and so needs nothing mode-specific here).
//
// A none node records its applied config under the slot kube1, so the
// discovery the dispatcher delegates to the default operator finds it with no
// change; see the noneops package comment.
type noneNodeOperator struct {
	nspawnNodeOperator
}

// EnsureLifecycleMigration has nothing to do: the nspawn lifecycle hooks, the
// machine's service override and the config regeneration unit do not exist on
// a host-native node.
func (noneNodeOperator) EnsureLifecycleMigration(_ context.Context, log *slog.Logger, active *ActiveMachine) error {
	log.Debug("no lifecycle hooks to reconcile for a host-native node", "slot", active.Name)

	return nil
}

// RestartNode restarts LocalDNS, containerd and kubelet in order (the soft
// reboot) and waits for LocalDNS and kubelet. It never restarts the host.
func (noneNodeOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	cfg := *active.Config

	return restartActiveNode(ctx, log, active, newNoneNode(noneops.Params{Log: log, Config: &cfg}).Restart)
}

// RepaveNode upgrades a host-native node in place. The caller has cordoned,
// drained and deleted the Node, as for nspawn. The new version is installed
// beside the running one, then the stack is stopped, host networking cleaned
// as repave does, the current link flipped, the stack started and kubelet
// waited on. If the new version does not come up the previous one is started
// again and the error says so. The applied config is persisted only after the
// new version is healthy, and the superseded version directories are removed
// after that.
//
// The applied config records the mode the node runs in. The desired config is
// derived from the applied one, so it already carries that mode and the repave
// sets it again only so that a caller passing another config cannot change the
// mode here: a different mode is handled by the repave reconciler before it
// gets here (changeIsolation), never by this method.
func (noneNodeOperator) RepaveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.UnboundedAgentConfig) error {
	log.Info("starting node upgrade",
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	downloads, archives, err := resolveNodeDownloads(ctx, newCfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	applied := *newCfg
	applied.Isolation = string(isolation.ModeNone)

	if err := stageNodeArchives(ctx, log, archives); err != nil {
		return err
	}

	node := newNoneNode(noneops.Params{Log: log, Config: &applied.AgentConfig, Downloads: downloads})
	if err := node.Upgrade(ctx); err != nil {
		return err
	}

	if err := persistNodeConfig(ctx, log, noneops.Slot, &applied.AgentConfig); err != nil {
		return err
	}

	if err := node.PruneVersions(); err != nil {
		// The node is healthy on the new version; leftover directories are
		// wasted disk, not a failed upgrade.
		log.Warn("could not remove superseded node versions", "error", err)
	}

	log.Info("node upgrade completed", "version", newCfg.Cluster.Version)

	return nil
}

// ReconcileHostRoot is the nspawn operator's with this operator as the one
// whose EnsureLifecycleMigration runs, so no nspawn files are written.
func (op noneNodeOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error) {
	return reconcileHostRoot(ctx, log, op, active)
}

var _ nodeOperator = noneNodeOperator{}

// noneTeardown is the reset path of a host-native node.
var noneTeardown = func(log *slog.Logger) *noneops.Teardown {
	return &noneops.Teardown{Log: log, Layout: none.Layout{}}
}
