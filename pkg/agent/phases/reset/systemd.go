// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type reloadSystemd struct {
	log *slog.Logger
}

// ReloadSystemd returns a task that runs systemctl daemon-reload to pick up
// unit file changes made by other reset tasks.
func ReloadSystemd(log *slog.Logger) phases.Task {
	return &reloadSystemd{log: log}
}

func (t *reloadSystemd) Name() string { return "reload-systemd" }

func (t *reloadSystemd) Do(ctx context.Context) error {
	t.log.Info("reloading systemd daemon")

	return executil.RunCmd(ctx, t.log, executil.Systemctl(), "daemon-reload")
}
