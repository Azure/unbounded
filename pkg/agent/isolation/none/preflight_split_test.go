// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// runningNode is a host that already runs a kubelet and containerd, as the node
// being migrated does.
func runningNode(t *testing.T) (*Backend, HostFS) {
	t.Helper()

	h, root := fakeHost(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], "kubelet.service"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], "containerd.service"), nil, 0o644))

	cgroup := filepath.Join(root, "cgroup")
	require.NoError(t, os.MkdirAll(cgroup, 0o755))

	h.Cgroup = cgroup

	b := &Backend{
		Layout:     Layout{Base: filepath.Join(root, "node")},
		Host:       &h,
		CheckPaths: []string{root},
		LookPath:   func(file string) (string, error) { return "/usr/bin/" + file, nil },
	}

	return b, h
}

func TestPreflightStaticAcceptsAHostThatRunsANode(t *testing.T) {
	b, _ := runningNode(t)

	require.NoError(t, b.PreflightStatic(), "the node being migrated must not fail the static half")
	require.ErrorContains(t, b.PreflightOwnership(), "not managed by the agent")
	require.ErrorContains(t, b.Preflight(), "not managed by the agent", "the combined preflight keeps refusing a foreign stack")
}

func TestPreflightOwnershipAcceptsACleanHost(t *testing.T) {
	h, root := fakeHost(t)
	b := &Backend{Layout: Layout{Base: filepath.Join(root, "node")}, Host: &h, CheckPaths: []string{root}}

	require.NoError(t, b.PreflightOwnership())
	require.NoError(t, b.PreflightStatic())
	require.NoError(t, b.Preflight())
}

func TestPreflightStaticFailures(t *testing.T) {
	t.Run("missing cgroup filesystem", func(t *testing.T) {
		b, h := runningNode(t)
		h.Cgroup = filepath.Join(t.TempDir(), "missing")
		b.Host = &h

		require.ErrorContains(t, b.PreflightStatic(), "cgroup filesystem is required")
	})

	t.Run("missing command", func(t *testing.T) {
		b, _ := runningNode(t)
		b.LookPath = func(string) (string, error) { return "", errors.New("not in PATH") }

		err := b.PreflightStatic()
		require.ErrorContains(t, err, `required command "systemctl" not found`)
	})

	t.Run("unset LookPath skips the command check", func(t *testing.T) {
		b, _ := runningNode(t)
		b.LookPath = nil

		require.NoError(t, b.PreflightStatic())
	})

	t.Run("path that is not a directory", func(t *testing.T) {
		b, _ := runningNode(t)
		file := filepath.Join(t.TempDir(), "f")
		require.NoError(t, os.WriteFile(file, nil, 0o644))
		b.CheckPaths = []string{file}

		require.ErrorContains(t, b.PreflightStatic(), "not a directory")
	})

	t.Run("reports every failure", func(t *testing.T) {
		b, h := runningNode(t)
		h.Cgroup = filepath.Join(t.TempDir(), "missing")
		b.Host = &h
		b.LookPath = func(string) (string, error) { return "", errors.New("not in PATH") }

		err := b.PreflightStatic()
		require.ErrorContains(t, err, "cgroup filesystem")
		require.ErrorContains(t, err, "systemctl")
	})
}
