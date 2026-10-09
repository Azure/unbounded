// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fsutil_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/fsutil"
)

func TestInstallFileStreamsAndReplacesAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "bin", "target")
	require.NoError(t, os.WriteFile(source, []byte("candidate"), 0o600))
	require.NoError(t, fsutil.InstallFile(source, target, 0o755))

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	// A failed install must leave the previously installed file intact.
	require.Error(t, fsutil.InstallFile(filepath.Join(dir, "missing"), target, 0o755))
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))
}

func TestWriteFileDurableCreatesAndPersistsParents(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "a", "b", "state.json")
	require.NoError(t, fsutil.WriteFileDurable(path, []byte("{}\n"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "{}\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSyncOpenFilesystemsDeduplicatesByDevice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := os.Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, first.Close()) })

	second, err := os.Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, second.Close()) })

	calls := 0

	require.NoError(t, fsutil.SyncOpenFilesystems([]*os.File{first, second}, func(int) error {
		calls++
		return nil
	}))
	require.Equal(t, 1, calls, "one barrier per filesystem, not per path")

	require.Error(t, fsutil.SyncOpenFilesystems([]*os.File{first}, func(int) error { return os.ErrPermission }))
}

func TestSyncFilesystemsReportsMissingPath(t *testing.T) {
	t.Parallel()
	require.Error(t, fsutil.SyncFilesystems(filepath.Join(t.TempDir(), "absent")))
}

func TestFileSHA256(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o600))

	digest, err := fsutil.FileSHA256(path)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256([]byte("content")), digest)

	_, err = fsutil.FileSHA256(filepath.Join(t.TempDir(), "absent"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestMountedNoexec checks the answer against the kernel's mount table, for a
// mount that runs programs and for /proc, which systemd and container
// runtimes mount noexec.
func TestMountedNoexec(t *testing.T) {
	t.Parallel()

	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("no mount table to check against: %v", err)
	}

	// The fifth field is the mount point, the sixth its options; the last
	// line for a mount point is the one on top.
	options := map[string][]string{}

	for line := range strings.SplitSeq(string(mountinfo), "\n") {
		if fields := strings.Fields(line); len(fields) > 5 {
			options[fields[4]] = strings.Split(fields[5], ",")
		}
	}

	for _, mountPoint := range []string{"/", "/proc"} {
		opts, ok := options[mountPoint]
		if !ok {
			continue
		}

		got, err := fsutil.MountedNoexec(mountPoint)
		require.NoError(t, err)
		require.Equal(t, slices.Contains(opts, "noexec"), got, "%s is mounted %s", mountPoint, strings.Join(opts, ","))
	}

	_, err = fsutil.MountedNoexec(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "inspect the filesystem of")
}
