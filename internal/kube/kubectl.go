// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/Azure/unbounded/internal/helpers"
)

type KubectlFunc func(context.Context) *exec.Cmd

const (
	kubectlBinary = "kubectl"
)

func CheckKubectlAvailable() error {
	if _, err := exec.LookPath(kubectlBinary); err != nil {
		return fmt.Errorf("kubectl not found in PATH: %w", err)
	}

	return nil
}

func Kubectl(env []string, kubeconfig string) KubectlFunc {
	return func(ctx context.Context) *exec.Cmd {
		envMap := helpers.EnvSliceToMap(env)
		envMap["KUBECONFIG"] = kubeconfig

		c := exec.CommandContext(ctx, kubectlBinary)
		c.Env = helpers.EnvMapToSlice(envMap)

		return c
	}
}

func KubectlCmd(ctx context.Context, logger *slog.Logger, kubectl KubectlFunc, args ...string) error {
	k := kubectl(ctx)
	k.Args = append(k.Args, args...)

	stdout, err := k.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := k.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := k.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl: %w", err)
	}

	var (
		wg                   sync.WaitGroup
		stdoutErr, stderrErr error
	)

	wg.Add(2)

	go func() {
		defer wg.Done()

		stdoutErr = streamLogs(ctx, logger, stdout, slog.LevelInfo)
	}()

	go func() {
		defer wg.Done()

		stderrErr = streamLogs(ctx, logger, stderr, slog.LevelError)
	}()

	// Join the readers before returning so no line is logged after the call.
	wg.Wait()

	var waitErr error
	if err := k.Wait(); err != nil {
		waitErr = fmt.Errorf("kubectl command failed: %w", err)
	}

	return errors.Join(waitErr, stdoutErr, stderrErr)
}

// maxLogLine bounds how much of a single line streamLogs will buffer. The
// default bufio.Scanner limit is 64 KiB, which kubectl output can exceed when
// it prints a whole object on one line.
const maxLogLine = 4 << 20

// streamLogs reads lines from reader and logs each one at the given level.
//
// A line longer than maxLogLine ends the stream with an error. The rest of the
// reader is consumed first so the command can finish writing and exit instead
// of blocking on a pipe nobody reads.
func streamLogs(ctx context.Context, logger *slog.Logger, reader io.Reader, level slog.Level) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxLogLine)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
			logger.Log(ctx, level, scanner.Text())
		}
	}

	err := scanner.Err()
	if err == nil {
		return nil
	}

	_, _ = io.Copy(io.Discard, reader) //nolint:errcheck // the read error that ended the scan is the one worth reporting

	return fmt.Errorf("failed to read kubectl output: %w", err)
}
