// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// restartReconfigured restarts containerd and kubelet when this invocation
// actually changed their configuration.
//
// Writing a configuration file does not affect a service that has already read
// it. That is harmless when this sequence boots the machine, because the
// services start afterwards and read the new files. It is not harmless when the
// sequence runs against a machine that is already up: without this, the files
// on disk and the running services would disagree, with nothing to reconcile
// them, which is a worse outcome than not reapplying at all.
//
// Only an actual change restarts anything. A reapply that writes identical
// content leaves the node alone, so the ordinary case of rerunning bootstrap
// after a failure costs nothing.
//
// It covers containerd and kubelet, and deliberately not everything the node
// stage writes. LocalDNS and the NVIDIA drop-in write through utilio directly
// and are not tracked, so a reapply that changes one updates the file without
// restarting its reader. Those inputs are all carried in the applied config,
// which a retry against a running node does not rewrite, so the daemon still
// sees drift and repaves. Extending tracking to them would make the reapply
// converge without a repave; until then the repave is what closes the gap.
type restartReconfigured struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart

	startMachine *startNSpawnMachine
	containerd   *configureContainerd
	kubelet      *configureKubelet
}

func (r *restartReconfigured) Name() string { return "restart-reconfigured-services" }

func (r *restartReconfigured) Do(ctx context.Context) error {
	if !r.startMachine.wasRunning {
		// This sequence started the machine, so its services have already read
		// the configuration written above.
		return nil
	}

	// containerd first: kubelet talks to it, so restarting kubelet into a
	// restarting runtime would only make it retry.
	for _, unit := range []struct {
		name    string
		changed bool
	}{
		{goalstates.SystemdUnitContainerd, r.containerd.changed},
		{goalstates.SystemdUnitKubelet, r.kubelet.changed},
	} {
		if !unit.changed {
			continue
		}

		r.log.Info("configuration changed on a running node, restarting service",
			"unit", unit.name,
			"machine", r.goalState.MachineName,
		)

		if _, err := executil.MachineRun(ctx, r.log, r.goalState.MachineName,
			"systemctl", "restart", unit.name,
		); err != nil {
			return fmt.Errorf("systemctl restart %s in %s: %w", unit.name, r.goalState.MachineName, err)
		}
	}

	return nil
}

var _ phases.Task = (*restartReconfigured)(nil)
