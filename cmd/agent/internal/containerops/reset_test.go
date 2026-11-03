// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

// teardownFixture has both slots prepared and kube1 running, plus the shared
// host state a node leaves behind.
func teardownFixture(t *testing.T) (*fixture, *Teardown, string) {
	t.Helper()

	f := newFixture(t)
	runtimeDir := filepath.Join(f.dir, "runtime")

	for _, slot := range []string{"kube1", "kube2"} {
		n := f.node(slot, false)
		_, err := n.Prepare(t.Context())
		require.NoError(t, err)
	}

	require.NoError(t, f.node("kube1", false).Start(t.Context(), false, false, false))

	runtime := filepath.Join(runtimeDir, "runc")
	require.NoError(t, os.MkdirAll(runtimeDir, 0o755))
	require.NoError(t, os.WriteFile(runtime, []byte("x"), 0o755))

	for _, p := range []string{"/var/lib/kubelet/config.yaml", "/opt/cni/bin/bridge", "/etc/cni/net.d/10.conf", "/var/lib/unbounded/container-images/a.tar", noneops.ModulesLoadPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(p)), 0o755))
		require.NoError(t, os.WriteFile(f.hostPath(p), []byte("x"), 0o644))
	}

	td := &Teardown{
		Layout:      container.Layout{Base: filepath.Join(f.dir, "containers")},
		SystemdDir:  filepath.Join(f.dir, "systemd"),
		RuntimePath: runtime,
		Run:         f.h.run,
		Root:        filepath.Join(f.dir, "host"),
		MountInfo:   f.mountInfo(),
	}

	f.h.reset()

	return f, td, runtime
}

func TestTeardownPresent(t *testing.T) {
	t.Parallel()

	td := &Teardown{Layout: container.Layout{Base: filepath.Join(t.TempDir(), "containers")}, SystemdDir: t.TempDir()}
	require.False(t, td.Present())

	require.NoError(t, os.MkdirAll(td.Layout.RootfsDir(""), 0o755))
	require.True(t, td.Present(), "the layout directory is a trace")

	td = &Teardown{Layout: container.Layout{Base: filepath.Join(t.TempDir(), "containers")}, SystemdDir: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(td.SystemdDir, container.UnitName("kube2")), nil, 0o644))
	require.True(t, td.Present(), "a unit is a trace")
}

func TestTeardownStopStopsBothSlotsAndSkipsAbsentOnes(t *testing.T) {
	f, td, _ := teardownFixture(t)

	require.NoError(t, td.Stop().Do(t.Context()))

	calls := f.h.snapshot()
	requireOrder(t, calls, "unbounded-kube1 systemctl stop kubelet.service", "systemctl stop "+container.UnitName("kube1"))
	require.NotEqual(t, -1, index(calls, "systemctl stop "+container.UnitName("kube2"), 0))
	require.Empty(t, f.h.active)

	// With no slots at all there is nothing to do and nothing fails.
	empty := &Teardown{Layout: container.Layout{Base: filepath.Join(t.TempDir(), "c")}, SystemdDir: t.TempDir(), Run: f.h.run}
	f.h.reset()
	require.NoError(t, empty.Stop().Do(t.Context()))
	require.Empty(t, f.h.snapshot())
}

func TestTeardownStopReportsAFailedSlot(t *testing.T) {
	f, td, _ := teardownFixture(t)
	f.h.failAll["systemctl stop "+container.UnitName("kube2")] = os.ErrPermission

	err := td.Stop().Do(t.Context())
	require.ErrorContains(t, err, "stop kube2")
	require.False(t, f.h.active[container.UnitName("kube1")], "the other slot is still stopped")
}

func TestTeardownDestroyRemovesEverythingTheNodeLeft(t *testing.T) {
	f, td, runtime := teardownFixture(t)

	require.NoError(t, os.WriteFile(f.mountInfo(), []byte("100 1 0:50 / /var/lib/kubelet/pods/x/volumes/v rw - tmpfs tmpfs rw\n"), 0o644))
	require.NoError(t, td.Stop().Do(t.Context()))
	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.NoError(t, td.Destroy().Do(t.Context()))

	calls := f.h.snapshot()
	requireOrder(t, calls, "umount -l /var/lib/kubelet/pods/x/volumes/v")

	for _, p := range []string{
		td.Layout.RootfsDir(""), filepath.Join(td.SystemdDir, container.UnitName("kube1")), filepath.Join(td.SystemdDir, container.UnitName("kube2")),
		runtime, filepath.Dir(runtime),
		f.hostPath("/var/lib/kubelet"), f.hostPath("/opt/cni/bin"), f.hostPath("/etc/cni/net.d"), f.hostPath("/var/lib/unbounded/container-images"),
		f.hostPath(noneops.ModulesLoadPath),
	} {
		_, err := os.Lstat(p)
		require.ErrorIs(t, err, os.ErrNotExist, p)
	}

	require.False(t, td.Present())

	// Repeating a finished teardown succeeds.
	require.NoError(t, td.Stop().Do(t.Context()))
	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.NoError(t, td.Destroy().Do(t.Context()))
}

func TestTeardownDestroyKeepsTheLayoutWhenASlotCannotBeRemoved(t *testing.T) {
	f, td, runtime := teardownFixture(t)

	require.NoError(t, td.Stop().Do(t.Context()))
	// Something is still mounted below kube1's rootfs: removing it would delete
	// through the mount, so the backend refuses.
	require.NoError(t, os.WriteFile(f.mountInfo(), []byte("100 1 0:50 / "+td.Layout.RootfsDir("kube1")+"/var/lib/containerd rw - xfs /dev/sda rw\n"), 0o644))

	err := td.Destroy().Do(t.Context())
	require.ErrorContains(t, err, "destroy kube1")
	require.ErrorContains(t, err, "still mounted")

	require.DirExists(t, td.Layout.RootfsDir("kube1"), "the data below the mount is untouched")
	require.FileExists(t, runtime, "the layout and runtime stay so the next reset can finish")
	require.NoDirExists(t, td.Layout.RootfsDir("kube2"), "the other slot is still removed")
}

func TestTeardownRemoveConfigLeavesOtherFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	other := filepath.Join(root, "etc/modules-load.d/other.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o755))
	require.NoError(t, os.WriteFile(other, nil, 0o644))

	td := &Teardown{Root: root}
	require.NoError(t, td.RemoveConfig().Do(t.Context()), "an absent file is fine")
	require.FileExists(t, other)
	require.Equal(t, "/etc/modules-load.d/unbounded-kubernetes.conf", noneops.ModulesLoadPath)
}

// A pod volume that stays mounted below /var/lib/kubelet would be followed by
// RemoveAll, which deletes the volume's data. The shared data is kept, the
// failure is reported, and the slots (which the mounts do not reach) are still
// removed.
func TestTeardownDestroyKeepsSharedDataWhenAPodMountCannotBeUnmounted(t *testing.T) {
	f, td, _ := teardownFixture(t)

	require.NoError(t, os.WriteFile(f.mountInfo(), []byte("100 1 0:50 / /var/lib/kubelet/pods/x/volumes/v rw - nfs srv:/x rw\n"), 0o644))
	f.h.failAll["umount -l /var/lib/kubelet/pods/x/volumes/v"] = errors.New("busy")

	require.NoError(t, td.Stop().Do(t.Context()))

	err := td.Destroy().Do(t.Context())
	require.ErrorContains(t, err, "unmount /var/lib/kubelet/pods/x/volumes/v")

	require.FileExists(t, f.hostPath("/var/lib/kubelet/config.yaml"), "kubelet state may sit on the mounted volume")
	require.NoFileExists(t, f.hostPath("/opt/cni/bin/bridge"), "nothing is mounted below the CNI directories")
	require.NoFileExists(t, f.hostPath("/etc/cni/net.d/10.conf"))
	require.NoDirExists(t, td.Layout.RootfsDir("kube2"), "the slots are unaffected by the mount")
}
