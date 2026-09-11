// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestNewCmdReset_IsRegistered(t *testing.T) {
	t.Parallel()

	cmdCtx := &CommandContext{LogFormat: "text"}
	cmd := newCmdReset(cmdCtx)

	assert.Equal(t, "reset", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)

	flag := cmd.Flags().Lookup("machine-name")
	require.Nil(t, flag)
}

func TestCLIResetDoesNotStopDaemonWhenInstallLockHeld(t *testing.T) {
	original := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")
	t.Cleanup(func() { installstate.LockPathForTest = original })

	lock, err := installstate.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	dir := t.TempDir()
	called := filepath.Join(dir, "systemctl-called")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\ntouch '"+called+"'\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	err = resetAgent(slog.New(slog.DiscardHandler)).Do(context.Background())
	require.ErrorIs(t, err, installstate.ErrLockHeld)
	_, err = os.Stat(called)
	require.ErrorIs(t, err, os.ErrNotExist, "CLI must acquire the lock before even stopping the daemon")
}
