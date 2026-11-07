// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"reflect"
	"strings"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// ActiveMachine holds the currently active node slot, its applied agent
// configuration and the isolation mode it runs in.
type ActiveMachine struct {
	Name   string
	Config *provision.AgentConfig
	// Mode is the isolation mode the machine was applied with. The empty mode
	// is the default mode.
	Mode isolation.Mode
}

// nodeOperator performs host-local node operations for the daemon. Reconcile
// code depends on this interface so tests can substitute the host-mutating
// implementation with a fake.
//
// The daemon's implementation is dispatchOperator: the operations about the
// host go to the host operator, and the operations on the active node go to
// the ModeOperator of the mode the node runs in.
type nodeOperator interface {
	hostOperations
	// EnsureLifecycleMigration installs lifecycle hooks for the active machine
	// without restarting its currently running services.
	EnsureLifecycleMigration(context.Context, *slog.Logger, *ActiveMachine) error
	// RestartNode restarts the provided active node in place.
	RestartNode(context.Context, *slog.Logger, *ActiveMachine) error
	// RepaveNode replaces the active node with one built from the desired
	// configuration; see ModeOperator.
	RepaveNode(context.Context, *slog.Logger, *ActiveMachine, *provision.UnboundedAgentConfig) error
	// ReconcileHostRoot moves the agent's files to the host root on a host an
	// older agent installed, once that cannot strand a rollback, and finishes
	// an interrupted move. The caller holds installation ownership. It reports
	// whether the daemon has to be restarted from the moved files, which it
	// is by stopping rather than taking any work; see ErrRestartRequested.
	ReconcileHostRoot(context.Context, *slog.Logger, *ActiveMachine) (bool, error)
}

// hostOperations are the operations about the host rather than one node
// stack. They do not depend on the isolation mode the node runs in.
type hostOperations interface {
	// FindActiveMachine returns the currently active node slot and its
	// applied agent configuration.
	FindActiveMachine(*slog.Logger) (*ActiveMachine, error)
	// ResetAgentResources removes the unbounded-agent and associated resources
	// without stopping the currently running daemon process.
	ResetAgentResources(context.Context, *slog.Logger) error
	// StopDaemon stops, disables, and removes the unbounded-agent-daemon unit.
	StopDaemon(context.Context, *slog.Logger) error
	// BootID returns the kernel boot ID of the current host boot.
	BootID() (string, error)
	// RebootHost asks systemd to reboot the host operating system. It returns
	// once the reboot is queued; systemd then stops this daemon.
	RebootHost(context.Context, *slog.Logger) error
	// StageAgentUpgrade stages a new host-side agent binary.
	StageAgentUpgrade(context.Context, *slog.Logger, agentUpgradeRequest) error
}

// ModeOperator is the node operations one isolation mode implements. Every
// mode implements all of them itself: a mode's operator never embeds another
// mode's, so a method added here is a compile error in every mode that does
// not implement it rather than a silent fallback to another mode's behavior.
type ModeOperator interface {
	// EnsureLifecycleMigration installs lifecycle hooks for the active node
	// without restarting its currently running services.
	EnsureLifecycleMigration(context.Context, *slog.Logger, *ActiveMachine) error
	// RestartNode restarts the active node stack in place. It never restarts
	// the host.
	RestartNode(context.Context, *slog.Logger, *ActiveMachine) error
	// RepaveNode replaces the active node with one built from the desired
	// configuration. The caller triggers it only after the Kubernetes Node has
	// been cordoned, drained and deleted; it does not perform Kubernetes
	// eviction or CNI-specific dataplane cleanup itself.
	RepaveNode(context.Context, *slog.Logger, *ActiveMachine, *provision.UnboundedAgentConfig) error
}

// ModeOperators holds the operator of every isolation mode this agent can
// operate. The agent's mode composition table builds it.
type ModeOperators map[isolation.Mode]ModeOperator

// BackendFactory builds a mode's isolation backend bound to one agent
// configuration.
type BackendFactory func(log *slog.Logger, cfg *provision.AgentConfig) isolation.Backend

// hasDrift reports whether the desired AgentConfig differs from the applied
// config in any field that would require a machine update.
func hasDrift(applied, desired *provision.AgentConfig) bool {
	appliedVersion := strings.TrimPrefix(applied.Cluster.Version, "v")

	desiredVersion := strings.TrimPrefix(desired.Cluster.Version, "v")
	if appliedVersion != desiredVersion {
		return true
	}

	if applied.OCIImage != desired.OCIImage {
		return true
	}

	if applied.Kubelet.ApiServer != desired.Kubelet.ApiServer {
		return true
	}

	if applied.Cluster.CaCertBase64 != desired.Cluster.CaCertBase64 {
		return true
	}

	if applied.Cluster.ClusterDNS != desired.Cluster.ClusterDNS {
		return true
	}

	if applied.Kubelet.Auth.BootstrapToken != desired.Kubelet.Auth.BootstrapToken {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.Labels, desired.Kubelet.Labels) {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.RegisterWithTaints, desired.Kubelet.RegisterWithTaints) {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.Configuration, desired.Kubelet.Configuration) {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.ImageCredentialProvider, desired.Kubelet.ImageCredentialProvider) {
		return true
	}

	if gantryDisabled(applied) != gantryDisabled(desired) {
		return true
	}

	if !reflect.DeepEqual(applied.LocalDNS, desired.LocalDNS) {
		return true
	}

	return false
}

func gantryDisabled(cfg *provision.AgentConfig) bool {
	return cfg.Gantry != nil && cfg.Gantry.Disabled
}
