// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package confsnap

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, root, p, content string, mode os.FileMode) {
	t.Helper()

	full := filepath.Join(root, p)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), mode))
	require.NoError(t, os.Chmod(full, mode))
}

func read(t *testing.T, root, p string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, p))
	require.NoError(t, err)

	return string(data)
}

func newTree(t *testing.T) (root, snap string) {
	t.Helper()

	root = t.TempDir()
	snap = filepath.Join(t.TempDir(), "snap")

	write(t, root, "etc/containerd/config.toml", "version = 2\n", 0o644)
	write(t, root, "var/lib/kubelet/kubeconfig", "client: cert\n", 0o600)
	write(t, root, "var/lib/kubelet/pki/kubelet-client-current.pem", "KEY-OLD", 0o600)
	write(t, root, "var/lib/kubelet/pki/ca.crt", "CA", 0o644)
	require.NoError(t, os.Symlink("kubelet-client-current.pem", filepath.Join(root, "var/lib/kubelet/pki/current")))

	return root, snap
}

var treePaths = []string{
	"/etc/containerd/config.toml",
	"/etc/containerd/conf.d",
	"/var/lib/kubelet/kubeconfig",
	"/var/lib/kubelet/pki",
}

func TestRestoreUndoesEveryKindOfChange(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))

	// A target stack rewrites, deletes, adds and replaces.
	write(t, root, "etc/containerd/config.toml", "version = 3\n", 0o600)
	require.NoError(t, os.Remove(filepath.Join(root, "var/lib/kubelet/kubeconfig")))
	require.NoError(t, os.RemoveAll(filepath.Join(root, "var/lib/kubelet/pki")))
	write(t, root, "var/lib/kubelet/pki/kubelet-client-current.pem", "KEY-NEW", 0o600)
	write(t, root, "var/lib/kubelet/pki/extra.pem", "new", 0o600)
	write(t, root, "etc/containerd/conf.d/10-new.toml", "new", 0o644)

	require.NoError(t, Restore(snap, root))

	assert.Equal(t, "version = 2\n", read(t, root, "etc/containerd/config.toml"))
	assert.Equal(t, "client: cert\n", read(t, root, "var/lib/kubelet/kubeconfig"))
	assert.Equal(t, "KEY-OLD", read(t, root, "var/lib/kubelet/pki/kubelet-client-current.pem"))
	assert.Equal(t, "CA", read(t, root, "var/lib/kubelet/pki/ca.crt"))
	assert.NoFileExists(t, filepath.Join(root, "var/lib/kubelet/pki/extra.pem"), "files the target added are removed")
	assert.NoDirExists(t, filepath.Join(root, "etc/containerd/conf.d"), "a path that did not exist is removed again")

	link, err := os.Readlink(filepath.Join(root, "var/lib/kubelet/pki/current"))
	require.NoError(t, err)
	assert.Equal(t, "kubelet-client-current.pem", link)

	for p, want := range map[string]os.FileMode{
		"etc/containerd/config.toml":                     0o644,
		"var/lib/kubelet/kubeconfig":                     0o600,
		"var/lib/kubelet/pki/kubelet-client-current.pem": 0o600,
	} {
		info, err := os.Stat(filepath.Join(root, p))
		require.NoError(t, err)
		assert.Equal(t, want, info.Mode().Perm(), p)
	}
}

func TestRestoreReplacesKindMismatches(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))

	// A file became a directory, and a directory became a file.
	require.NoError(t, os.Remove(filepath.Join(root, "etc/containerd/config.toml")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/containerd/config.toml/sub"), 0o755))
	require.NoError(t, os.RemoveAll(filepath.Join(root, "var/lib/kubelet/pki")))
	write(t, root, "var/lib/kubelet/pki", "not a dir", 0o644)

	require.NoError(t, Restore(snap, root))

	assert.Equal(t, "version = 2\n", read(t, root, "etc/containerd/config.toml"))
	assert.Equal(t, "KEY-OLD", read(t, root, "var/lib/kubelet/pki/kubelet-client-current.pem"))
}

func TestRestoreIsIdempotentAndLeavesMatchingFilesAlone(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))

	path := filepath.Join(root, "etc/containerd/config.toml")
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, Restore(snap, root))
	require.NoError(t, Restore(snap, root))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "an unchanged file is not rewritten")
	assert.True(t, before.ModTime().Equal(after.ModTime()))
}

func TestRestoreWithoutASnapshot(t *testing.T) {
	require.ErrorIs(t, Restore(filepath.Join(t.TempDir(), "none"), t.TempDir()), ErrNoSnapshot)
	assert.False(t, Exists(filepath.Join(t.TempDir(), "none")))
}

func TestTakeThatCrashedLeavesNothingToRestore(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))
	require.True(t, Exists(snap))

	// The manifest is written last: without it the blobs are not a snapshot.
	require.NoError(t, os.Remove(filepath.Join(snap, manifestName)))
	assert.False(t, Exists(snap))
	require.ErrorIs(t, Restore(snap, root), ErrNoSnapshot)
}

func TestTakeReplacesAnEarlierSnapshot(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))

	write(t, root, "etc/containerd/config.toml", "version = 9\n", 0o644)
	require.NoError(t, Take(snap, root, treePaths))
	write(t, root, "etc/containerd/config.toml", "garbage", 0o644)

	require.NoError(t, Restore(snap, root))
	assert.Equal(t, "version = 9\n", read(t, root, "etc/containerd/config.toml"))
}

func TestSnapshotIsPrivate(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))

	for _, p := range []string{snap, filepath.Join(snap, blobDir), filepath.Join(snap, manifestName)} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o077, "%s must not be readable by others", p)
	}
}

func TestTakeRefusesWhatCannotBePutBack(t *testing.T) {
	root, snap := newTree(t)

	// Unix socket paths are short; use a directory under the temp root.
	sock := filepath.Join(root, "var/lib/kubelet/pki/s")
	l, err := net.Listen("unix", sock)
	require.NoError(t, err)

	defer l.Close() //nolint:errcheck // test cleanup

	require.ErrorContains(t, Take(snap, root, treePaths), "unsupported file type")
}

// A Take that fails after it copied some files must not leave their copies
// (kubelet keys among them) behind: nothing records them, so nothing would
// ever remove them.
func TestTakeThatFailsRemovesWhatItCopied(t *testing.T) {
	root, snap := newTree(t)

	l, err := net.Listen("unix", filepath.Join(root, "var/lib/kubelet/pki/s"))
	require.NoError(t, err)

	defer l.Close() //nolint:errcheck // test cleanup

	require.Error(t, Take(snap, root, treePaths))
	assert.NoDirExists(t, snap)
}

func TestDiscard(t *testing.T) {
	root, snap := newTree(t)
	require.NoError(t, Take(snap, root, treePaths))
	require.NoError(t, Discard(snap))
	assert.False(t, Exists(snap))
	require.NoError(t, Discard(snap), "discarding twice is fine")
}

func TestRestoreWorksOnTheRealRootPrefix(t *testing.T) {
	// An empty root means the filesystem root: paths are used as given.
	dir := t.TempDir()
	file := filepath.Join(dir, "conf")
	require.NoError(t, os.WriteFile(file, []byte("old"), 0o600))

	snap := filepath.Join(t.TempDir(), "snap")
	require.NoError(t, Take(snap, "", []string{dir}))

	require.NoError(t, os.WriteFile(file, []byte("new"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o600))

	require.NoError(t, Restore(snap, ""))

	data, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "old", string(data))
	assert.NoFileExists(t, filepath.Join(dir, "extra"))
}

func TestSharedPathsAreAbsoluteCleanAndDistinct(t *testing.T) {
	seen := map[string]bool{}

	for _, p := range SharedPaths() {
		assert.True(t, filepath.IsAbs(p), p)
		assert.Equal(t, filepath.Clean(p), p)
		assert.False(t, seen[p], "%s is listed twice", p)

		seen[p] = true
	}

	// The none mode writes the kubelet's drop-ins (node IP, labels, taints,
	// kubeconfig arguments) to its own unit's directory, not kubelet.service.d.
	// A rollback that leaves them restarts the old version with the new ones.
	for _, want := range []string{
		"/etc/containerd/config.toml", "/var/lib/kubelet/config.yaml", "/var/lib/kubelet/pki", "/etc/cni/net.d",
		"/etc/systemd/system/kubelet.service.d", "/etc/systemd/system/unbounded-kubelet.service.d",
	} {
		assert.True(t, seen[want], "%s must be covered", want)
	}
}
