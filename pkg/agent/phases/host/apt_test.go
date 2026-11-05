// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestHardenAPT_WritesBothDropIns verifies the task writes the apt and
// needrestart drop-ins with the expected content and permissions.
func TestHardenAPT_WritesBothDropIns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	aptPath := filepath.Join(dir, "apt", "99-unbounded-no-restart-systemd")
	nrPath := filepath.Join(dir, "needrestart", "99-unbounded.conf")

	task := &hardenAPT{
		log:                   discardLogger(),
		aptDropInPath:         aptPath,
		needrestartDropInPath: nrPath,
		lookupPath:            existingPathLookup("apt-get"),
	}

	require.NoError(t, task.Do(context.Background()))

	aptBytes, err := os.ReadFile(aptPath)
	require.NoError(t, err)

	apt := string(aptBytes)
	require.Contains(t, apt, "Unattended-Upgrade::Package-Blacklist")

	for _, pkg := range []string{
		`"systemd";`,
		`"systemd-container";`,
		`"libcap2";`,
		`"libcap2-bin";`,
		`"libpam-cap";`,
	} {
		require.Contains(t, apt, pkg, "apt drop-in missing %s", pkg)
	}

	nrBytes, err := os.ReadFile(nrPath)
	require.NoError(t, err)
	require.Contains(t, string(nrBytes), `$nrconf{restart} = 'l';`)
}

// TestHardenAPT_Idempotent ensures running the task twice does not error
// and leaves the same content (no append/duplication).
func TestHardenAPT_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	aptPath := filepath.Join(dir, "apt.conf")
	nrPath := filepath.Join(dir, "nr.conf")

	task := &hardenAPT{
		log:                   discardLogger(),
		aptDropInPath:         aptPath,
		needrestartDropInPath: nrPath,
		lookupPath:            existingPathLookup("apt-get"),
	}

	require.NoError(t, task.Do(context.Background()))

	first, err := os.ReadFile(aptPath)
	require.NoError(t, err)

	require.NoError(t, task.Do(context.Background()))

	second, err := os.ReadFile(aptPath)
	require.NoError(t, err)

	require.Equal(t, first, second, "second run must not alter file contents")
}

func TestHardenAPT_SkipsWhenAPTUnavailable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	aptPath := filepath.Join(dir, "apt.conf")
	nrPath := filepath.Join(dir, "nr.conf")

	task := &hardenAPT{
		log:                   discardLogger(),
		aptDropInPath:         aptPath,
		needrestartDropInPath: nrPath,
		lookupPath:            missingPathLookup,
	}

	require.NoError(t, task.Do(context.Background()))
	require.NoFileExists(t, aptPath)
	require.NoFileExists(t, nrPath)
}

func TestRemoveAPTHardening_RemovesBothDropIns(t *testing.T) {
	dir := t.TempDir()
	apt := filepath.Join(dir, "apt.conf.d", "99-unbounded-no-restart-systemd")
	nr := filepath.Join(dir, "needrestart", "conf.d", "99-unbounded.conf")

	harden := &hardenAPT{
		log: slog.New(slog.DiscardHandler), aptDropInPath: apt, needrestartDropInPath: nr,
		lookupPath: func(string) (string, error) { return "/usr/bin/apt-get", nil },
	}
	require.NoError(t, harden.Do(t.Context()))

	remove := &removeAPTHardening{log: slog.New(slog.DiscardHandler), aptDropInPath: apt, needrestartDropInPath: nr}
	require.Equal(t, "remove-apt-hardening", remove.Name())
	require.NoError(t, remove.Do(t.Context()))
	require.NoFileExists(t, apt)
	require.NoFileExists(t, nr)

	// Nothing to remove is not an error.
	require.NoError(t, remove.Do(t.Context()))
}

func TestRemoveAPTHardening_ReportsAFailedRemoval(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the path cannot be removed with os.Remove.
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.MkdirAll(filepath.Join(blocked, "child"), 0o755))

	remove := &removeAPTHardening{log: slog.New(slog.DiscardHandler), aptDropInPath: blocked, needrestartDropInPath: filepath.Join(dir, "missing")}
	require.ErrorContains(t, remove.Do(t.Context()), "blocked")
}
