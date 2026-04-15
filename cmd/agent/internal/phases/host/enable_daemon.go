// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
	"github.com/Azure/unbounded-kube/internal/provision"
)

const (
	daemonServiceUnit = "unbounded-agent-daemon.service"

	// agentConfigPath is the well-known location for the agent config
	// file on the host, consistent with the cloud-init and vendor-data
	// provisioning paths.
	agentConfigPath = "/etc/unbounded-agent/config.json"
)

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceContent []byte

type enableDaemon struct {
	log *slog.Logger
	cfg *provision.AgentConfig
}

// EnableDaemon returns a task that persists the agent configuration to a
// well-known path on disk, then installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit reads the
// config file via the UNBOUNDED_AGENT_CONFIG_FILE environment variable
// and runs the agent daemon process that registers the Machine CR.
func EnableDaemon(log *slog.Logger, cfg *provision.AgentConfig) phases.Task {
	return &enableDaemon{log: log, cfg: cfg}
}

func (e *enableDaemon) Name() string { return "enable-daemon" }

func (e *enableDaemon) Do(ctx context.Context) error {
	// Persist the agent config so the daemon can read it on (re)start.
	cfgData, err := json.MarshalIndent(e.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent config: %w", err)
	}

	if err := utilio.WriteFile(agentConfigPath, cfgData, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", agentConfigPath, err)
	}

	e.log.Info("agent config persisted", "path", agentConfigPath)

	// Install and start the systemd unit.
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
