// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
)

const daemonServiceUnit = "unbounded-agent-daemon.service"

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceContent []byte

type enableDaemon struct {
	log *slog.Logger
}

// EnableDaemon returns a task that installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit runs the
// agent daemon process that watches for Machine CR changes.
func EnableDaemon(log *slog.Logger) phases.Task {
	return &enableDaemon{log: log}
}

func (e *enableDaemon) Name() string { return "enable-daemon" }

func (e *enableDaemon) Do(ctx context.Context) error {
	unitPath := filepath.Join(goalstates.SystemdSystemDir, daemonServiceUnit)

	if err := utilio.WriteFile(unitPath, daemonServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	systemctl := utilexec.Systemctl()

	if err := utilexec.RunCmd(ctx, e.log, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := utilexec.RunCmd(ctx, e.log, systemctl, "enable", daemonServiceUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", daemonServiceUnit, err)
	}

	if err := utilexec.RunCmd(ctx, e.log, systemctl, "restart", daemonServiceUnit); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", daemonServiceUnit, err)
	}

	return nil
}
