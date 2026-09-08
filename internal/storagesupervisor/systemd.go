// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package storagesupervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// systemdUnitDir is the host-absolute directory systemd reads unit files from.
const systemdUnitDir = "/etc/systemd/system"

// CommandRunner executes an external command. It is injected so systemctl
// invocations can be stubbed in tests and wrapped (e.g. with nsenter) in
// production.
type CommandRunner interface {
	Run(ctx context.Context, argv []string) error
}

// execRunner runs commands with os/exec, forwarding stdout/stderr.
type execRunner struct{}

// Run executes argv, streaming the child's output to the parent's stdio.
func (execRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %v: %w", argv, err)
	}

	return nil
}

// writeUnit renders the systemd unit and writes it under HostRoot, returning the
// path it was written to.
func writeUnit(cfg Config) (string, error) {
	unitPath := filepath.Join(cfg.HostRoot, systemdUnitDir, cfg.ServiceName+".service")

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", filepath.Dir(unitPath), err)
	}

	if err := os.WriteFile(unitPath, []byte(renderUnit(cfg)), 0o644); err != nil {
		return "", fmt.Errorf("write unit %q: %w", unitPath, err)
	}

	return unitPath, nil
}

// reloadAndStart reloads systemd and, unless NoEnable is set, enables and
// restarts the service. All systemctl calls go through runner using the
// configured argv prefix (which may be an nsenter wrapper).
func reloadAndStart(ctx context.Context, cfg Config, runner CommandRunner) error {
	slog.Info("reloading systemd")

	if err := runner.Run(ctx, systemctlArgs(cfg, "daemon-reload")); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if cfg.NoEnable {
		slog.Info("NO_ENABLE set; unit installed but not enabled/started", "service", cfg.ServiceName)

		return nil
	}

	slog.Info("enabling and starting service", "service", cfg.ServiceName)

	if err := runner.Run(ctx, systemctlArgs(cfg, "enable", cfg.ServiceName)); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}

	if err := runner.Run(ctx, systemctlArgs(cfg, "restart", cfg.ServiceName)); err != nil {
		return fmt.Errorf("systemctl restart: %w", err)
	}

	return nil
}

// systemctlArgs builds the full argv for a systemctl subcommand, prepending the
// configured systemctl invocation (e.g. an nsenter wrapper).
func systemctlArgs(cfg Config, args ...string) []string {
	argv := make([]string, 0, len(cfg.Systemctl)+len(args))
	argv = append(argv, cfg.Systemctl...)
	argv = append(argv, args...)

	return argv
}

// renderUnit produces the systemd unit file contents. All embedded paths are
// host-absolute (they are resolved by host systemd, not within HostRoot).
func renderUnit(cfg Config) string {
	execStart := fmt.Sprintf("%s/current/bin/unbounded-storage --config %s", cfg.Prefix, cfg.ConfigPath)
	if cfg.StorageArgs != "" {
		execStart += " " + cfg.StorageArgs
	}

	hugepagePreStart := fmt.Sprintf("ExecStartPre=+/bin/sh -c '%s'\n", hugepageReserveCmd(cfg))
	if cfg.NoHugepages {
		hugepagePreStart = ""
	}

	return fmt.Sprintf(`[Unit]
Description=unbounded-storage daemon
Documentation=https://github.com/%[1]s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
Environment=LD_LIBRARY_PATH=%[2]s/current/lib
%[3]sExecStartPre=+/bin/sh -c '%[4]s'
ExecStart=%[5]s
Restart=always
RestartSec=2s

LimitNOFILE=infinity
LimitNPROC=infinity
LimitMEMLOCK=infinity
LimitCORE=infinity
LimitFSIZE=infinity
LimitAS=infinity
LimitDATA=infinity
LimitSTACK=infinity
LimitCPU=infinity
TasksMax=infinity

[Install]
WantedBy=multi-user.target
`, cfg.Repo, cfg.Prefix, hugepagePreStart, configEnsureCmd(cfg), execStart)
}

// hugepageReserveCmd builds the shell one-liner used as an ExecStartPre to
// reserve 2 MiB hugepages before the daemon starts. It mirrors the reserve
// logic in hack/scripts/install-unbounded-storage.sh: derive a target count
// from PoolBytes (plus headroom) unless Hugepages overrides it, attempt the
// reservation, compact memory once if short, and fail loudly if it cannot be
// satisfied.
func hugepageReserveCmd(cfg Config) string {
	return fmt.Sprintf(`hp=/sys/kernel/mm/hugepages/hugepages-2048kB; `+
		`[ -d "$hp" ] || { echo "unbounded-storage: kernel exposes no 2MiB hugepage pool; hugepages are required" >&2; exit 1; }; `+
		`want=%[1]d; `+
		`if [ "$want" -le 0 ]; then pool=$(( (%[2]d + 2097151) / 2097152 )); n=$(nproc 2>/dev/null || echo 1); [ "$n" -gt 8 ] && n=8; need=$(( pool + 8 * n )); want=$(( need + need / 2 )); else need=$want; fi; `+
		`cur=$(cat "$hp/nr_hugepages" 2>/dev/null || echo 0); `+
		`[ "$cur" -lt "$want" ] && echo "$want" > "$hp/nr_hugepages" 2>/dev/null || true; `+
		`free=$(cat "$hp/free_hugepages" 2>/dev/null || echo 0); `+
		`if [ "$free" -lt "$need" ] && [ -w /proc/sys/vm/compact_memory ]; then echo 1 > /proc/sys/vm/compact_memory 2>/dev/null || true; [ "$cur" -lt "$want" ] && echo "$want" > "$hp/nr_hugepages" 2>/dev/null || true; free=$(cat "$hp/free_hugepages" 2>/dev/null || echo 0); fi; `+
		`nr=$(cat "$hp/nr_hugepages" 2>/dev/null || echo 0); `+
		`echo "unbounded-storage: 2MiB hugepages nr=$nr free=$free (need $need target $want)" >&2; `+
		`[ "$free" -ge "$need" ] || { echo "unbounded-storage: could not reserve $need free 2MiB hugepages (have $free); free host memory or reserve hugepages at boot" >&2; exit 1; }`,
		cfg.Hugepages, cfg.PoolBytes)
}

// configEnsureCmd builds the shell one-liner used as an ExecStartPre to ensure
// the config directory, default file-backed disk directory, and an (at least
// empty) config file exist on the host.
func configEnsureCmd(cfg Config) string {
	return fmt.Sprintf(`d=$(dirname "%[1]s"); mkdir -p "$d" "%[2]s"; [ -f "%[1]s" ] || : > "%[1]s"`, cfg.ConfigPath, defaultStorageFileDiskDir)
}
