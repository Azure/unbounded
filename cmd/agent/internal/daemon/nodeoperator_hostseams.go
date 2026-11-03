// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Seams for the host-mutating steps the none and container operators and the
// in-place isolation migration share. Tests replace them.
var (
	// resolveNodeDownloads resolves the artifact download overrides and the
	// offline container image archives for a configuration.
	resolveNodeDownloads = provision.ResolveDownloadOverridesWithOfflineArtifacts

	// stageNodeArchives stages the offline container image archives.
	stageNodeArchives = func(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error {
		return rootfs.DownloadContainerImageArchives(log, staging).Do(ctx)
	}

	// persistNodeConfig records cfg as the applied config of slot.
	persistNodeConfig = func(ctx context.Context, log *slog.Logger, slot string, cfg *provision.AgentConfig) error {
		return PersistAppliedConfig(log, slot, cfg).Do(ctx)
	}

	// removeNodeConfig removes the applied config recorded for slot.
	removeNodeConfig = func(ctx context.Context, log *slog.Logger, slot string) error {
		return RemoveAppliedConfig(log, slot).Do(ctx)
	}
)

// restartActiveNode runs restart for the active node's soft reboot, with the
// log lines and error wording every mode shares. It never restarts the host.
func restartActiveNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, restart func(ctx context.Context) error) error {
	log.Info("restarting active node", "slot", active.Name, "isolation", active.Mode)

	if err := restart(ctx); err != nil {
		return fmt.Errorf("restart node: %w", err)
	}

	log.Info("node restarted", "slot", active.Name)

	return nil
}

// hostNodeTeardown is the reset path of a node that does not run in an nspawn
// machine: the tasks that slot into the positions of the machine tasks they
// replace.
type hostNodeTeardown interface {
	Stop() phases.Task
	RemoveConfig() phases.Task
	Destroy() phases.Task
}
