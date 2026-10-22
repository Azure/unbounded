// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
)

const rebootHostTimeout = time.Minute

// RebootHost asks systemd to reboot the host. It runs independently of ctx
// cancellation because systemd stops the caller as soon as the reboot starts.
// If systemctl reports an error while systemd is already stopping, the reboot
// is treated as under way.
func RebootHost(ctx context.Context, log *slog.Logger) error {
	cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebootHostTimeout)
	defer cancel()

	rebootErr := executil.RunCmd(cmdCtx, log, executil.Systemctl(), "reboot")
	if rebootErr == nil {
		return nil
	}

	state, err := executil.OutputCmd(cmdCtx, log, "systemctl", "show", "--property=SystemState", "--value")
	if err == nil && strings.TrimSpace(state) == "stopping" {
		log.Warn("systemctl reboot reported an error, but systemd is already stopping", "error", rebootErr)
		return nil
	}

	return fmt.Errorf("systemctl reboot: %w", rebootErr)
}
