// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
)

func Kubectl(env []string, kubeconfig string) func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		envMap := envSliceToMap(env)
		envMap["KUBECONFIG"] = kubeconfig

		c := exec.CommandContext(ctx, "kubectl")
		c.Env = envMapToSlice(envMap)

		return c
	}
}

// envSliceToMap converts KEY=VALUE entries into a map. An entry without "="
// maps to an empty value.
func envSliceToMap(env []string) map[string]string {
	m := map[string]string{}

	for _, ev := range env {
		if parts := strings.SplitN(ev, "=", 2); len(parts) == 2 {
			m[parts[0]] = parts[1]
		} else {
			m[parts[0]] = ""
		}
	}

	return m
}

// envMapToSlice converts a map into sorted KEY=VALUE entries.
func envMapToSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))

	for k, v := range env {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

func KubectlCmd(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, args ...string) error {
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

	go streamLogs(ctx, logger, stdout, slog.LevelInfo)
	go streamLogs(ctx, logger, stderr, slog.LevelError)

	if err := k.Wait(); err != nil {
		return fmt.Errorf("kubectl command failed: %w", err)
	}

	return nil
}

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
