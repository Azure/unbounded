// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKubectlCmdLogsLineLongerThanScannerDefault is the regression test for a
// single output line longer than bufio.Scanner's 64 KiB default token. The
// scanner used to stop silently at that line, dropping everything after it.
func TestKubectlCmdLogsLineLongerThanScannerDefault(t *testing.T) {
	const (
		lineLen = 96 * 1024
		marker  = "after the long line"
	)

	installFakeKubectl(t, fmt.Sprintf("printf '%%*s\\n' %d ''\necho %s", lineLen, marker))

	var logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))

	err := KubectlCmd(context.Background(), logger, Kubectl(nil, ""))
	require.NoError(t, err)
	require.Contains(t, logs.String(), marker)
	require.Greater(t, logs.Len(), lineLen)
}

// TestKubectlCmdReturnsCommandFailure confirms a nonzero exit is still
// reported after the reader goroutines are joined.
func TestKubectlCmdReturnsCommandFailure(t *testing.T) {
	installFakeKubectl(t, "echo boom >&2\nexit 3")

	var logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))

	err := KubectlCmd(context.Background(), logger, Kubectl(nil, ""))
	require.ErrorContains(t, err, "kubectl command failed")

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 3, exitErr.ExitCode())
	require.Contains(t, logs.String(), "boom")
}

func installFakeKubectl(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir)
}
