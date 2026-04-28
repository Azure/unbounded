// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/google/renameio/v2"
)

// writeFile writes content to filename atomically, creating parent directories
// as needed.
func writeFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm)
}

// runCmd creates a command from the given factory, appends args, streams stdout
// at Debug and stderr at Error, and waits for it to finish.
func runCmd(ctx context.Context, logger *slog.Logger, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	cmd := newCmd(ctx)
	cmd.Args = append(cmd.Args, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stdout, slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, slog.LevelError)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return nil
}

// systemctl returns a command factory for systemctl.
func systemctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "systemctl")
	}
}

// streamLogs reads lines from reader and logs each one at the given level.
func streamLogs(ctx context.Context, logger *slog.Logger, reader io.Reader, level slog.Level) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
			logger.Log(ctx, level, scanner.Text())
		}
	}
}
