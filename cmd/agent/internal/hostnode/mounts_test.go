// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeMountInfo(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mountinfo")

	var data string
	for _, l := range lines {
		data += l + "\n"
	}

	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	return path
}

// mountLine is a mountinfo line with the given mount point.
func mountLine(mountPoint string) string {
	return "36 35 98:0 / " + mountPoint + " rw,noatime shared:1 - ext3 /dev/root rw"
}

func TestUnmountBelowUnmountsDeepestFirst(t *testing.T) {
	t.Parallel()

	info := writeMountInfo(t,
		mountLine("/var/lib/kubelet"),
		mountLine("/var/lib/kubelet/pods/a/volumes"),
		mountLine("/var/lib/kubelet/pods/a"),
		mountLine("/var/lib/kubelet-other"),
		mountLine("/var/lib/containerd/x"),
		mountLine("/run/other"),
	)

	var got []string

	run := func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		got = append(got, name+" "+args[0]+" "+args[1])
		return "", nil
	}

	require.NoError(t, UnmountBelow(t.Context(), nil, run, info, KubeletDir))
	assert.Equal(t, []string{
		"umount -l /var/lib/kubelet/pods/a/volumes",
		"umount -l /var/lib/kubelet/pods/a",
		"umount -l /var/lib/kubelet",
	}, got, "only mounts at or below the prefix, deepest first; a sibling with the same prefix text is left alone")
}

func TestUnmountBelowDecodesEscapedMountPoints(t *testing.T) {
	t.Parallel()

	info := writeMountInfo(t, mountLine(`/var/lib/kubelet/with\040space`))

	var got string

	run := func(_ context.Context, _ *slog.Logger, _ string, args ...string) (string, error) {
		got = args[1]
		return "", nil
	}

	require.NoError(t, UnmountBelow(t.Context(), nil, run, info, KubeletDir))
	assert.Equal(t, "/var/lib/kubelet/with space", got)
}

func TestUnmountBelowMissingMountTableIsNothingToDo(t *testing.T) {
	t.Parallel()

	run := func(context.Context, *slog.Logger, string, ...string) (string, error) {
		t.Fatal("nothing to unmount")
		return "", nil
	}

	require.NoError(t, UnmountBelow(t.Context(), nil, run, filepath.Join(t.TempDir(), "absent"), KubeletDir))
}

func TestUnmountBelowJoinsEveryFailure(t *testing.T) {
	t.Parallel()

	info := writeMountInfo(t, mountLine("/var/lib/kubelet/a"), mountLine("/var/lib/kubelet/b"))
	boom := errors.New("busy")

	var calls int

	run := func(context.Context, *slog.Logger, string, ...string) (string, error) {
		calls++
		return "", boom
	}

	err := UnmountBelow(t.Context(), nil, run, info, KubeletDir)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 2, calls, "one failed unmount does not stop the others")
	require.ErrorContains(t, err, "unmount /var/lib/kubelet/a")
	require.ErrorContains(t, err, "unmount /var/lib/kubelet/b")
}

func TestUnmountBelowByPrefixAttributesFailuresToTheirPrefixInOnePass(t *testing.T) {
	t.Parallel()

	info := writeMountInfo(t,
		mountLine("/var/lib/kubelet/pods/a"),
		mountLine("/var/lib/containerd/io/rootfs"),
		mountLine("/var/lib/kubelet/pods/a/volumes/v"),
	)

	var got []string

	run := func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		got = append(got, args[1])

		if args[1] == "/var/lib/kubelet/pods/a" {
			return "", errors.New("busy")
		}

		return "", nil
	}

	failed, err := UnmountBelowByPrefix(t.Context(), nil, run, info, KubeletDir, "/var/lib/containerd")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"/var/lib/kubelet/pods/a/volumes/v",
		"/var/lib/containerd/io/rootfs",
		"/var/lib/kubelet/pods/a",
	}, got, "one deepest-first pass across every prefix")
	require.Contains(t, failed, KubeletDir)
	assert.ErrorContains(t, failed[KubeletDir], "unmount /var/lib/kubelet/pods/a")
	assert.NotContains(t, failed, "/var/lib/containerd", "a prefix whose mounts all came off is not reported")
}

func TestUnmountBelowByPrefixReportsAnUnreadableMountTable(t *testing.T) {
	t.Parallel()

	_, err := UnmountBelowByPrefix(t.Context(), nil, nil, t.TempDir(), KubeletDir)
	require.Error(t, err)
}
