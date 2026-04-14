// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/template"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
)

const daemonUnit = "unbounded-agent-daemon.service"

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceTmpl string

var daemonServiceTemplate = template.Must(
	template.New(daemonUnit).Parse(daemonServiceTmpl),
)

type enableDaemon struct {
	log      *slog.Logger
	endpoint string
}

// EnableDaemon returns a task that installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit runs
// "unbounded-agent daemon --endpoint=<endpoint>" which connects to the
// given task server gRPC endpoint.
func EnableDaemon(log *slog.Logger, endpoint string) phases.Task {
	return &enableDaemon{log: log, endpoint: endpoint}
}

func (d *enableDaemon) Name() string { return "enable-daemon" }

func (d *enableDaemon) Do(ctx context.Context) error {
	var buf bytes.Buffer
	if err := daemonServiceTemplate.Execute(&buf, map[string]string{
		"Endpoint": d.endpoint,
	}); err != nil {
		return fmt.Errorf("rendering %s template: %w", daemonUnit, err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, daemonUnit)

	if err := utilio.WriteFile(unitPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	systemctl := utilexec.Systemctl()

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "enable", daemonUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", daemonUnit, err)
	}

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "start", daemonUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", daemonUnit, err)
	}

	d.log.Info("daemon unit started", "unit", daemonUnit, "endpoint", d.endpoint)

	return nil
}
