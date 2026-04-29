// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
)

func (e *defaultExecutor) machineRun(ctx context.Context, log *slog.Logger, machine string, args ...string) (string, error) {
	return executil.MachineRun(ctx, log, machine, args...)
}

func (e *defaultExecutor) systemctlRestart(ctx context.Context, log *slog.Logger, unit string) error {
	return executil.RunCmd(ctx, log, executil.Systemctl(), "restart", unit)
}
