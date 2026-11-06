// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withUmask sets the process umask for the rest of the test. The umask is
// process-wide, so a test that uses it must not be parallel.
func withUmask(t *testing.T, mask int) {
	t.Helper()

	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, want, info.Mode().Perm(), path)
}

// A hardened host's umask 027 must not leave the layout untraversable for the
// LocalDNS dynamic user.
func TestEnsureMakesTheLayoutTraversableUnderARestrictiveUmask(t *testing.T) {
	withUmask(t, 0o027)

	parent := filepath.Join(t.TempDir(), "opt")
	l := Layout{Base: filepath.Join(parent, "unbounded", "node")}

	dir, err := l.Ensure("v1")
	require.NoError(t, err)

	for _, d := range []string{filepath.Join(parent, "unbounded"), l.Base, dir, filepath.Join(dir, "bin")} {
		requireMode(t, d, 0o755)
	}
}

// A parent the agent did not create keeps its mode, and an existing layout
// directory left 0750 by an earlier install is repaired.
func TestMkdirTraversableChangesOnlyWhatItOwns(t *testing.T) {
	withUmask(t, 0o027)

	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))

	dir := filepath.Join(parent, "node")
	require.NoError(t, os.Mkdir(dir, 0o750))

	require.NoError(t, MkdirTraversable(filepath.Join(dir, "v1", "libexec")))

	requireMode(t, parent, 0o700)
	requireMode(t, dir, 0o750) // existed and was not the directory asked for
	requireMode(t, filepath.Join(dir, "v1"), 0o755)
	requireMode(t, filepath.Join(dir, "v1", "libexec"), 0o755)

	require.NoError(t, MkdirTraversable(dir))
	requireMode(t, dir, 0o755)
}
