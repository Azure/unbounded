// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func TestUpgradeWithStartupPublishesBeforeContainerStart(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	ctx := t.Context()
	published := false
	calls := 0
	f.h.onCall = func(call string) {
		if call == "systemctl enable --now "+container.UnitName("kube2") {
			require.True(t, published, "systemd inside the container may start kubelet immediately")
		}
	}

	require.NoError(t, newNode.UpgradeWithStartup(ctx, oldNode, func(got context.Context) error {
		calls++

		require.Equal(t, ctx, got)
		require.False(t, f.h.active[container.UnitName("kube1")])
		require.False(t, f.h.active[container.UnitName("kube2")])
		require.Equal(t, 1, f.hooks.cleanup)
		require.Equal(t, 1, f.hooks.setup)
		require.NoFileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath))
		require.NoDirExists(t, f.hostPath(hostnode.KubeletClientPKIDir))
		require.FileExists(t, f.hostPath(goalstates.KubeletConfigurationPath))
		require.FileExists(t, filepath.Join(f.rootfs("kube2"), "etc/containerd/config.toml"))
		require.True(t, confsnap.Exists(newNode.configSnapshotDir()))

		published = true

		return nil
	}))
	require.Equal(t, 1, calls)
	require.True(t, f.h.active[container.UnitName("kube2")])
	require.False(t, f.h.active[container.UnitName("kube1")])
	require.DirExists(t, f.rootfs("kube1"))
	require.DirExists(t, f.rootfs("kube2"))
	require.True(t, confsnap.Exists(newNode.configSnapshotDir()))
	require.NotNil(t, newNode.snapshot)

	require.NoError(t, newNode.CommitUpgrade())
	require.NoDirExists(t, newNode.configSnapshotDir())
	require.Nil(t, newNode.snapshot)
	require.NoError(t, newNode.CommitUpgrade(), "commit is idempotent")
	require.DirExists(t, f.rootfs("kube1"))
	require.DirExists(t, f.rootfs("kube2"))
	require.True(t, f.h.active[container.UnitName("kube2")])
}

func TestUpgradeWithStartupRequiresPublicationCallback(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	require.ErrorContains(t, newNode.UpgradeWithStartup(t.Context(), oldNode, nil), "callback is required")
	require.Empty(t, f.h.snapshot())
	require.True(t, f.h.active[container.UnitName("kube1")])
	require.NoDirExists(t, f.rootfs("kube2"))
	require.NoDirExists(t, newNode.configSnapshotDir())
}

func TestUpgradeWithStartupRollsBackBeforePublication(t *testing.T) {
	boom := errors.New("before publication")

	for _, step := range []string{"prepare", "configure"} {
		t.Run(step, func(t *testing.T) {
			f, oldNode, newNode := upgradeFixture(t)
			if step == "prepare" {
				f.hooks.provisionErr = boom
			} else {
				newNode.p.SetupNetwork = func(context.Context) error { return boom }
			}

			published := false
			err := newNode.UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error {
				published = true
				return nil
			})
			require.ErrorIs(t, err, boom)
			require.False(t, published)
			require.True(t, f.h.active[container.UnitName("kube1")])
			require.False(t, f.h.active[container.UnitName("kube2")])
			require.NoDirExists(t, f.rootfs("kube2"))
			require.NoDirExists(t, newNode.configSnapshotDir())
			require.FileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath))
		})
	}
}

func TestUpgradeWithStartupRetainsSlotsAndSnapshotAfterPublicationAttempt(t *testing.T) {
	boom := errors.New("startup failed")

	for _, step := range []string{"publish", "container start", "kubelet start", "bootstrap", "readiness"} {
		t.Run(step, func(t *testing.T) {
			f, oldNode, newNode := upgradeFixture(t)

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			var publishErr error

			switch step {
			case "publish":
				publishErr = boom
			case "container start":
				f.h.failOne["enable --now "+container.UnitName("kube2")] = boom
			case "kubelet start":
				f.h.failOne["unbounded-kube2 systemctl enable --now kubelet.service"] = boom
			case "bootstrap":
				f.h.failAll["unbounded-kube2 test -s"] = boom
			case "readiness":
				f.h.failAll["unbounded-kube2 systemctl is-active kubelet.service"] = boom
			}

			calls := 0
			err := newNode.UpgradeWithStartup(ctx, oldNode, func(context.Context) error {
				calls++
				return publishErr
			})
			require.Error(t, err)
			require.ErrorContains(t, err, "slots and shared snapshot retained")
			require.Equal(t, 1, calls)
			require.False(t, f.h.active[container.UnitName("kube1")])
			require.DirExists(t, f.rootfs("kube1"))
			require.DirExists(t, f.rootfs("kube2"))
			require.True(t, confsnap.Exists(newNode.configSnapshotDir()))
			require.NotNil(t, newNode.snapshot)
			require.NoFileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath), "shared state is not restored")

			for _, call := range f.h.snapshot() {
				require.NotContains(t, call, "enable --now "+container.UnitName("kube1"))
				require.NotContains(t, call, "systemctl stop "+container.UnitName("kube2"))
				require.NotContains(t, call, testRuntime+" delete --force unbounded-kube2")
			}

			if step == "publish" {
				require.ErrorIs(t, err, boom)
				require.Equal(t, -1, index(f.h.snapshot(), "enable --now "+container.UnitName("kube2"), 0))
			}

			// The saved source identity remains available for explicit recovery.
			saved := t.TempDir()
			require.NoError(t, confsnap.Restore(newNode.configSnapshotDir(), saved))
			data, err := os.ReadFile(filepath.Join(saved, goalstates.KubeletKubeconfigPath))
			require.NoError(t, err)
			require.Equal(t, "old", string(data))

			// A retry, including after process restart, must not delete the target.
			before := f.h.snapshot()
			err = New(newNode.p).UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error {
				t.Fatal("an unresolved snapshot must block publication")
				return nil
			})
			require.ErrorIs(t, err, confsnap.ErrSnapshotExists)
			require.Equal(t, before, f.h.snapshot())
			require.DirExists(t, f.rootfs("kube2"))
		})
	}
}

func TestCommitUpgradeAfterRecreatingNode(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	require.NoError(t, newNode.UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error { return nil }))

	recreated := New(newNode.p)
	pending, err := recreated.PendingUpgrade()
	require.NoError(t, err)
	require.True(t, pending)
	require.NoError(t, recreated.CommitUpgrade())

	pending, err = recreated.PendingUpgrade()
	require.NoError(t, err)
	require.False(t, pending)
	require.NoDirExists(t, newNode.configSnapshotDir())
	require.DirExists(t, f.rootfs("kube1"))
	require.DirExists(t, f.rootfs("kube2"))
	require.True(t, f.h.active[container.UnitName("kube2")])
}

func TestPendingUpgradeBlocksIncompleteRecoveryState(t *testing.T) {
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			f, oldNode, newNode := upgradeFixture(t)
			pending, err := newNode.PendingUpgrade()
			require.NoError(t, err)
			require.False(t, pending)

			dir := newNode.configSnapshotDir()
			require.NoError(t, os.MkdirAll(filepath.Dir(dir), 0o755))

			if kind == "directory" {
				require.NoError(t, os.Mkdir(dir, 0o700))
			} else {
				require.NoError(t, os.WriteFile(dir, []byte("interrupted"), 0o600))
			}

			pending, err = newNode.PendingUpgrade()
			require.NoError(t, err)
			require.True(t, pending)

			err = newNode.UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error {
				t.Fatal("existing recovery state must block publication")
				return nil
			})
			require.ErrorIs(t, err, confsnap.ErrSnapshotExists)
			require.Empty(t, f.h.snapshot())
			require.True(t, f.h.active[container.UnitName("kube1")])
			require.NoDirExists(t, f.rootfs("kube2"))
		})
	}
}

func TestPendingUpgradeReportsInspectionErrors(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	blocked := filepath.Join(f.dir, "blocked")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	newNode.p.ConfigSnapshotDir = filepath.Join(blocked, "snapshot")
	pending, err := newNode.PendingUpgrade()
	require.ErrorContains(t, err, "inspect container upgrade snapshot")
	require.False(t, pending)

	err = newNode.UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error {
		t.Fatal("inspection errors must block publication")
		return nil
	})
	require.ErrorContains(t, err, "inspect container upgrade snapshot")
	require.Empty(t, f.h.snapshot())
	require.True(t, f.h.active[container.UnitName("kube1")])
}

func TestCommitUpgradeReportsFilesystemErrors(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube2", false)
	n.p.ConfigSnapshotDir = filepath.Join(f.dir, "missing-parent", "snapshot")
	require.ErrorContains(t, n.CommitUpgrade(), "sync container upgrade snapshot parent")

	loop := filepath.Join(f.dir, "loop")
	require.NoError(t, os.Symlink("loop", loop))

	n.p.ConfigSnapshotDir = filepath.Join(loop, "snapshot")
	require.ErrorContains(t, n.CommitUpgrade(), "discard container upgrade snapshot")
}

func TestUpgradeWithStartupReturnsStartFailure(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	boom := errors.New("runtime refused target")
	f.h.failOne["enable --now "+container.UnitName("kube2")] = boom

	err := newNode.UpgradeWithStartup(t.Context(), oldNode, func(context.Context) error { return nil })
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "start slot kube2")
}
