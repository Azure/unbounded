// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func newTeardown(t *testing.T) (*Teardown, *host, string) {
	t.Helper()

	h := &host{active: map[string]bool{}, fail: map[string]error{}}
	root := t.TempDir()

	return &Teardown{
		Log:        quiet(),
		Layout:     none.Layout{Base: filepath.Join(root, "opt/unbounded/node")},
		SystemdDir: filepath.Join(root, "etc/systemd/system"),
		Run:        h.run,
		Root:       root,
		MountInfo:  filepath.Join(root, "mountinfo"),
		Proc:       filepath.Join(root, "proc"),
	}, h, root
}

func put(t *testing.T, root, p, content string) {
	t.Helper()

	full := filepath.Join(root, p)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func TestTeardownPresent(t *testing.T) {
	td, _, root := newTeardown(t)
	require.False(t, td.Present())

	put(t, root, "etc/systemd/system/"+none.UnitKubelet, "")
	require.True(t, td.Present(), "an agent-owned unit is enough")

	td2, _, _ := newTeardown(t)
	require.NoError(t, os.MkdirAll(td2.Layout.VersionDir("v1"), 0o755))
	require.True(t, td2.Present(), "so is the layout")
}

func TestTeardownStopStopsOnlyInstalledUnitsInReverseOrder(t *testing.T) {
	td, h, root := newTeardown(t)

	require.NoError(t, td.Stop().Do(t.Context()), "nothing installed is not an error")
	require.Empty(t, h.calls)

	for _, u := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS} {
		put(t, root, "etc/systemd/system/"+u, "")
	}

	require.NoError(t, td.Stop().Do(t.Context()))
	require.Equal(t, []string{
		"systemctl stop " + none.UnitKubelet,
		"systemctl stop " + none.UnitContainerd,
		"systemctl stop " + none.UnitLocalDNS,
	}, h.calls)
}

func TestTeardownStopTriesEveryUnitAndReports(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, "etc/systemd/system/"+none.UnitContainerd, "")
	put(t, root, "etc/systemd/system/"+none.UnitKubelet, "")

	h.fail["systemctl stop "+none.UnitKubelet] = errors.New("stuck")

	err := td.Stop().Do(t.Context())
	require.ErrorContains(t, err, "stuck")
	require.Contains(t, h.calls, "systemctl stop "+none.UnitContainerd)
}

func TestTeardownRemoveConfig(t *testing.T) {
	td, _, root := newTeardown(t)

	for _, p := range []string{
		none.ContainerdConfigPath, goalstates.NvidiaRuntimeDropInPath, goalstates.KubeletAPIServerCACertPath,
		"/etc/systemd/system/localdns.slice", "/etc/unbounded/localdns/Corefile", "/etc/unbounded/localdns/environment",
	} {
		put(t, root, p, "x")
	}

	put(t, root, "/etc/modules-load.d/unbounded-kubernetes.conf", "overlay\n")
	put(t, root, goalstates.ContainerdDefaultHostsPath, gantryHostsMarker+"\n[host]\n")

	// Not ours: a foreign containerd drop-in and CA file must survive, and so
	// must the directories they keep alive.
	put(t, root, "/etc/containerd/conf.d/50-site.toml", "site")
	put(t, root, "/etc/kubernetes/pki/other.crt", "other")
	// The distro's containerd configuration is not the agent's: a none node
	// never writes it, so reset never deletes it.
	put(t, root, goalstates.ContainerdConfigPath, "distro")

	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, goalstates.ContainerdConfigPath))
	require.NoDirExists(t, filepath.Join(root, filepath.Dir(none.ContainerdConfigPath)))

	for _, p := range []string{none.ContainerdConfigPath, goalstates.KubeletAPIServerCACertPath, "/etc/unbounded/localdns", goalstates.ContainerdDefaultHostsPath, "/etc/systemd/system/localdns.slice"} {
		require.NoFileExists(t, filepath.Join(root, p), p)
	}

	require.NoFileExists(t, filepath.Join(root, ModulesLoadPath))
	require.FileExists(t, filepath.Join(root, "/etc/containerd/conf.d/50-site.toml"))
	require.FileExists(t, filepath.Join(root, "/etc/kubernetes/pki/other.crt"))

	// Idempotent.
	require.NoError(t, td.RemoveConfig().Do(t.Context()))
}

func TestTeardownRemoveConfigKeepsForeignGantryHostsFile(t *testing.T) {
	td, _, root := newTeardown(t)
	put(t, root, goalstates.ContainerdDefaultHostsPath, "[host.\"https://mirror\"]\n")

	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, goalstates.ContainerdDefaultHostsPath))
}

func TestTeardownDestroyRemovesNodeAndData(t *testing.T) {
	td, h, root := newTeardown(t)

	for _, u := range []string{none.UnitContainerd, none.UnitKubelet} {
		put(t, root, "etc/systemd/system/"+u, "")
	}

	_, err := td.Layout.Ensure("v1")
	require.NoError(t, err)
	put(t, root, "/var/lib/kubelet/pods/p1/volumes/x", "x")
	put(t, root, "/var/lib/containerd/io.containerd/x", "x")
	put(t, root, "/etc/cni/net.d/10-net.conflist", "x")
	put(t, root, "/opt/cni/bin/bridge", "x")

	// Pod volumes are mounted below /var/lib/kubelet; the deepest must be
	// unmounted first, and mounts elsewhere are left alone.
	info := strings.Join([]string{
		"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
		"2 1 0:30 / " + "/var/lib/kubelet/pods/p1/volumes/kubernetes.io~secret/s rw - tmpfs tmpfs rw",
		"3 1 0:31 / " + "/var/lib/kubelet/pods rw - ext4 /dev/sdb rw",
		"4 1 0:32 / /var/lib/kubelet-other rw - tmpfs tmpfs rw",
		"5 1 0:33 / /var/lib/containerd/io.containerd.runtime.v2.task/k8s.io/c/rootfs rw - overlay overlay rw",
		"6 1 0:34 / /mnt/with\\040space rw - tmpfs tmpfs rw",
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(td.MountInfo, []byte(info), 0o644))

	require.NoError(t, td.Destroy().Do(t.Context()))

	var umounts []string

	for _, c := range h.calls {
		if strings.HasPrefix(c, "umount") {
			umounts = append(umounts, c)
		}
	}

	require.Equal(t, []string{
		"umount -l /var/lib/containerd/io.containerd.runtime.v2.task/k8s.io/c/rootfs",
		"umount -l /var/lib/kubelet/pods/p1/volumes/kubernetes.io~secret/s",
		"umount -l /var/lib/kubelet/pods",
	}, umounts)

	for _, p := range []string{"/var/lib/kubelet", "/var/lib/containerd", "/etc/cni/net.d", "/opt/cni/bin", "/opt/unbounded/node", "/etc/systemd/system/" + none.UnitKubelet} {
		require.NoFileExists(t, filepath.Join(root, p), p)
	}

	// Unmount comes after the units stop and before the removal reports.
	stopIdx, umountIdx := -1, -1

	for i, c := range h.calls {
		if c == "systemctl stop "+none.UnitKubelet {
			stopIdx = i
		}

		if strings.HasPrefix(c, "umount") && umountIdx < 0 {
			umountIdx = i
		}
	}

	require.Less(t, stopIdx, umountIdx)
}

func TestTeardownDestroyOnCleanHostIsANoOp(t *testing.T) {
	td, h, _ := newTeardown(t)

	require.NoError(t, td.Destroy().Do(t.Context()))
	require.Equal(t, []string{"systemctl daemon-reload"}, h.calls)
	require.NoError(t, td.Destroy().Do(t.Context()), "idempotent")
}

func TestTeardownDestroyReportsUnmountFailureButRemovesTheRest(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, "/var/lib/kubelet/x", "x")
	require.NoError(t, os.WriteFile(td.MountInfo, []byte("2 1 0:30 / /var/lib/kubelet/m rw - tmpfs tmpfs rw\n"), 0o644))

	h.fail["umount -l /var/lib/kubelet/m"] = errors.New("busy")

	err := td.Destroy().Do(t.Context())
	require.ErrorContains(t, err, "unmount /var/lib/kubelet/m")
	require.NoDirExists(t, filepath.Join(root, "/var/lib/containerd"))
}

func TestTeardownTaskNamesAreStable(t *testing.T) {
	td, _, _ := newTeardown(t)
	require.Equal(t, "stop-none-node", td.Stop().Name())
	require.Equal(t, "remove-none-config", td.RemoveConfig().Name())
	require.Equal(t, "remove-none-node", td.Destroy().Name())
}

func TestLoadKernelModulesWritesListAndLoadsEach(t *testing.T) {
	h := &host{active: map[string]bool{}, fail: map[string]error{}}
	path := filepath.Join(t.TempDir(), "modules-load.d", "unbounded-kubernetes.conf")

	k := LoadKernelModules(quiet(), h.run)
	k.path = path

	require.Equal(t, "load-kernel-modules", k.Name())
	require.NoError(t, k.Do(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "overlay\nbr_netfilter\n", string(data))
	require.Equal(t, []string{"modprobe overlay", "modprobe br_netfilter"}, h.calls)

	h.fail["modprobe br_netfilter"] = errors.New("not found")

	require.ErrorContains(t, k.Do(context.Background()), "modprobe br_netfilter")
}

func TestTeardownRemoveConfigRemovesTheCDISpecItGenerated(t *testing.T) {
	td, _, root := newTeardown(t)
	put(t, root, goalstates.CDISpecFile, "cdiVersion: 0.6.0\n")

	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.NoFileExists(t, filepath.Join(root, goalstates.CDISpecFile))
	require.NoDirExists(t, filepath.Join(root, goalstates.CDISpecDir), "the directory the agent created goes with it")
}

func TestTeardownRemoveConfigKeepsAForeignCDISpec(t *testing.T) {
	td, _, root := newTeardown(t)
	put(t, root, goalstates.CDISpecDir+"/vendor.yaml", "site")

	require.NoError(t, td.RemoveConfig().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, goalstates.CDISpecDir, "vendor.yaml"))
}

// A pod volume that could not be unmounted still holds live data. RemoveAll
// would follow the mount and delete it, so the directory is kept and the
// failure is reported. Directories with no failed unmount are still removed.
func TestTeardownDestroyKeepsADirectoryWhoseMountsCouldNotBeUnmounted(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, "/var/lib/kubelet/pods/p1/volumes/data/precious", "x")
	put(t, root, "/var/lib/containerd/x", "x")
	put(t, root, "/etc/cni/net.d/10-net.conflist", "x")
	require.NoError(t, os.WriteFile(td.MountInfo, []byte("2 1 0:30 / /var/lib/kubelet/pods/p1/volumes/data rw - nfs srv:/x rw\n"), 0o644))

	h.fail["umount -l /var/lib/kubelet/pods/p1/volumes/data"] = errors.New("busy")

	err := td.Destroy().Do(t.Context())
	require.ErrorContains(t, err, "unmount /var/lib/kubelet/pods/p1/volumes/data")
	require.FileExists(t, filepath.Join(root, "/var/lib/kubelet/pods/p1/volumes/data/precious"))
	require.NoDirExists(t, filepath.Join(root, "/var/lib/containerd"))
	require.NoDirExists(t, filepath.Join(root, "/etc/cni/net.d"))
}

// A failed unmount below containerd's directory keeps that directory only.
func TestTeardownDestroyKeepsContainerdDataWhenItsMountsStay(t *testing.T) {
	td, h, root := newTeardown(t)
	put(t, root, "/var/lib/kubelet/x", "x")
	put(t, root, "/var/lib/containerd/io/keep", "x")
	require.NoError(t, os.WriteFile(td.MountInfo, []byte("2 1 0:30 / /var/lib/containerd/io rw - overlay overlay rw\n"), 0o644))

	h.fail["umount -l /var/lib/containerd/io"] = errors.New("busy")

	require.Error(t, td.Destroy().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, "/var/lib/containerd/io/keep"))
	require.NoDirExists(t, filepath.Join(root, "/var/lib/kubelet"))
}

// With no readable mount table nothing proves a data directory has no mounts
// below it, so both are kept.
func TestTeardownDestroyKeepsMountableDirectoriesWhenTheMountTableIsUnreadable(t *testing.T) {
	td, _, root := newTeardown(t)
	put(t, root, "/var/lib/kubelet/x", "x")
	put(t, root, "/var/lib/containerd/x", "x")
	put(t, root, "/etc/cni/net.d/10-net.conflist", "x")

	// A directory opens but cannot be read as a mount table.
	require.NoError(t, os.MkdirAll(td.MountInfo, 0o755))

	require.Error(t, td.Destroy().Do(t.Context()))
	require.FileExists(t, filepath.Join(root, "/var/lib/kubelet/x"))
	require.FileExists(t, filepath.Join(root, "/var/lib/containerd/x"))
	require.NoDirExists(t, filepath.Join(root, "/etc/cni/net.d"))
}

// The teardown removes only the CNI plugin links the node made; plugins other
// components installed in /opt/cni/bin stay.
func TestTeardownDestroyKeepsOtherCNIPlugins(t *testing.T) {
	td, _, root := newTeardown(t)

	version := filepath.Join(td.Layout.VersionDir("v1"), "cni", "bin")
	put(t, "/", filepath.Join(version, "bridge"), "ours")
	_, err := td.Layout.Flip("v1")
	require.NoError(t, err)

	cni := filepath.Join(root, "/opt/cni/bin")
	require.NoError(t, os.MkdirAll(cni, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(td.Layout.CNIBinDir(), "bridge"), filepath.Join(cni, "bridge")))
	put(t, root, "/opt/cni/bin/unbounded-ipam", "theirs")

	require.NoError(t, td.Destroy().Do(t.Context()))

	_, err = os.Lstat(filepath.Join(cni, "bridge"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.FileExists(t, filepath.Join(cni, "unbounded-ipam"))
}
