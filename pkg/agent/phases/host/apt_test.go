// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	}

	require.NoError(t, task.Do(context.Background()))

	first, err := os.ReadFile(aptPath)
	require.NoError(t, err)

	require.NoError(t, task.Do(context.Background()))

	second, err := os.ReadFile(aptPath)
	require.NoError(t, err)

	require.Equal(t, first, second, "second run must not alter file contents")
}
