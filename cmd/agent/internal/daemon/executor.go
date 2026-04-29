// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

func (e *defaultExecutor) machineRun(ctx context.Context, log *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return outputCmd(ctx, log, "systemd-run", runArgs...)
}

func (e *defaultExecutor) systemctlRestart(ctx context.Context, log *slog.Logger, unit string) error {
	return runCmd(ctx, log, systemctl(), "restart", unit)
}

// outputCmd runs the command specified by name and args and returns stdout.
func outputCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	var buf bytes.Buffer

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, io.TeeReader(stdout, &buf), slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, slog.LevelError)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return strings.TrimRight(buf.String(), "\n"), nil
}
