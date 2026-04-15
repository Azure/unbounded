// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
)

const daemonUnit = "unbounded-agent-daemon.service"

type stopDaemon struct {
	log *slog.Logger
}

// StopDaemon returns a task that stops, disables, and removes the
// unbounded-agent-daemon systemd unit. Errors from stop and disable are
// logged but do not fail the task since the unit may not be present.
func StopDaemon(log *slog.Logger) phases.Task {
	return &stopDaemon{log: log}
}

func (t *stopDaemon) Name() string { return "stop-daemon" }

func (t *stopDaemon) Do(ctx context.Context) error {
	systemctl := utilexec.Systemctl()

	if err := utilexec.RunCmd(ctx, t.log, systemctl, "stop", daemonUnit); err != nil {
		t.log.Warn("failed to stop daemon (may not be running)", "error", err)
	}

	if err := utilexec.RunCmd(ctx, t.log, systemctl, "disable", daemonUnit); err != nil {
		t.log.Warn("failed to disable daemon (may not be enabled)", "error", err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, daemonUnit)
	removeFileIfExists(t.log, unitPath)

	return nil
}
