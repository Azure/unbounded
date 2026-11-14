// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func TestPrepareInstallsCallerArtifactsBeforeStart(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube2", false)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var installed *goalstates.MachineGoalState

	n.p.Install = func(got context.Context, gs *goalstates.MachineGoalState) error {
		require.Equal(t, ctx, got)
		require.Equal(t, [4]int{1, 1, 1, 1}, [4]int{f.hooks.runtime, f.hooks.helper, f.hooks.unpack, f.hooks.provision})
		require.Empty(t, f.h.active)
		require.DirExists(t, filepath.Join(f.rootfs("kube2"), "etc"))
		require.FileExists(t, filepath.Join(f.dir, "containers", "bundles", "kube2", "config.json"))
		require.Equal(t, gs.RootFS.Root(), gs.NodeStart.Root())
		require.Equal(t, filepath.Join(f.rootfs("kube2"), "usr/bin/node-problem-detector"), gs.RootFS.Root().Path("/usr/bin/node-problem-detector"))
		require.Equal(t, f.hostPath("opt/cni/bin"), gs.RootFS.Root().Path("/opt/cni/bin"))

		installed = gs

		bin := gs.RootFS.Root().Path("/usr/bin/node-problem-detector")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}

		return os.WriteFile(bin, []byte("caller binary"), 0o755)
	}

	r, err := n.Prepare(ctx)
	require.NoError(t, err)
	require.Equal(t, "kube2", r.Slot())

	gs, err := n.GoalState()
	require.NoError(t, err)
	require.Same(t, gs, installed)

	f.h.onCall = func(call string) {
		if call == "systemctl enable --now "+container.UnitName("kube2") {
			require.FileExists(t, gs.RootFS.Root().Path("/usr/bin/node-problem-detector"))
		}
	}

	containerdChanged, kubeletChanged, err := n.Configure(ctx)
	require.NoError(t, err)
	require.NoError(t, n.Start(ctx, false, containerdChanged, kubeletChanged))
	require.True(t, f.h.active[container.UnitName("kube2")])
}

func TestPrepareCallerInstallFailureCanBeRetried(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)
	boom := errors.New("caller binary unavailable")
	calls := 0
	n.p.Install = func(context.Context, *goalstates.MachineGoalState) error {
		calls++
		if calls == 1 {
			return boom
		}

		return nil
	}

	r, err := n.Prepare(t.Context())
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "install caller artifacts")
	require.Nil(t, r)
	require.Empty(t, f.h.active)
	require.NoFileExists(t, filepath.Join(f.rootfs("kube1"), "etc/containerd/config.toml"))

	r, err = n.Prepare(t.Context())
	require.NoError(t, err)
	require.Equal(t, "kube1", r.Slot())
	require.Equal(t, 2, calls)
	require.Empty(t, f.h.active)
}

func TestPrepareReappliesCallerInstallIdempotently(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)
	calls := 0
	n.p.Install = func(_ context.Context, gs *goalstates.MachineGoalState) error {
		calls++

		return os.WriteFile(gs.RootFS.Root().Path("/etc/caller-config"), []byte("caller config"), 0o644)
	}

	for _, node := range []*Node{n, n, New(n.p)} {
		_, err := node.Prepare(t.Context())
		require.NoError(t, err)

		data, err := os.ReadFile(filepath.Join(f.rootfs("kube1"), "etc/caller-config"))
		require.NoError(t, err)
		require.Equal(t, "caller config", string(data))
		require.Empty(t, f.h.active)
	}

	require.Equal(t, 3, calls, "every Prepare installs, including a new Node using an existing root")
}

func TestPrepareSkipsCallerInstallWhenBasePreparationFails(t *testing.T) {
	boom := errors.New("base preparation failed")

	for _, step := range []string{"runtime", "helper", "unpack", "rootfs"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			n := f.node("kube1", false)
			called := false
			n.p.Install = func(context.Context, *goalstates.MachineGoalState) error {
				called = true
				return nil
			}

			switch step {
			case "runtime":
				n.p.InstallRuntime = func(context.Context) error { return boom }
			case "helper":
				n.p.InstallHelper = func(context.Context) error { return boom }
			case "unpack":
				f.hooks.unpackErr = boom
			case "rootfs":
				f.hooks.provisionErr = boom
			}

			r, err := n.Prepare(t.Context())
			require.ErrorIs(t, err, boom)
			require.Nil(t, r)
			require.False(t, called)
			require.Empty(t, f.h.active)
		})
	}
}
