// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func stubKill(t *testing.T) *[]int {
	t.Helper()

	var killed []int

	old := killProcess
	killProcess = func(pid int) error { killed = append(killed, pid); return nil }

	t.Cleanup(func() { killProcess = old })

	return &killed
}

func writeCgroupFile(t *testing.T, root, rel, content string) string {
	t.Helper()

	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))

	return p
}

// SIGKILL on a shim reparents the pod's processes; they keep running. The pods'
// cgroup is what ends them.
func TestKillPodCgroupsWritesCgroupKill(t *testing.T) {
	killed := stubKill(t)
	root := t.TempDir()
	kill := writeCgroupFile(t, root, "kubepods.slice/cgroup.kill", "")
	writeCgroupFile(t, root, "kubepods.slice/cgroup.procs", "10\n")
	unrelated := writeCgroupFile(t, root, "system.slice/cgroup.kill", "")

	require.NoError(t, killPodCgroups(root))

	data, err := os.ReadFile(kill)
	require.NoError(t, err)
	require.Equal(t, "1", string(data))
	require.Empty(t, *killed, "cgroup.kill needs no per-process signals")

	data, err = os.ReadFile(unrelated)
	require.NoError(t, err)
	require.Empty(t, data, "only the pods' cgroup is killed")
}

func TestKillPodCgroupsFallsBackToSignalingEveryProcess(t *testing.T) {
	killed := stubKill(t)
	root := t.TempDir()
	writeCgroupFile(t, root, "kubepods.slice/cgroup.procs", "10\n11\n")
	writeCgroupFile(t, root, "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podx.slice/cri-containerd-c.scope/cgroup.procs", "12\n")
	writeCgroupFile(t, root, "system.slice/cgroup.procs", "99\n")

	require.NoError(t, killPodCgroups(root))

	sort.Ints(*killed)
	require.Equal(t, []int{10, 11, 12}, *killed)
}

func TestKillPodCgroupsHandlesTheCgroupfsDriverName(t *testing.T) {
	killed := stubKill(t)
	root := t.TempDir()
	kill := writeCgroupFile(t, root, "kubepods/cgroup.kill", "")

	require.NoError(t, killPodCgroups(root))

	data, err := os.ReadFile(kill)
	require.NoError(t, err)
	require.Equal(t, "1", string(data))
	require.Empty(t, *killed)
}

func TestKillPodCgroupsIsNothingWithoutPodCgroups(t *testing.T) {
	stubKill(t)

	require.NoError(t, killPodCgroups(t.TempDir()))
	require.NoError(t, killPodCgroups(filepath.Join(t.TempDir(), "missing")))
}

func TestKillPodCgroupsReportsAFailedSignal(t *testing.T) {
	old := killProcess
	killProcess = func(int) error { return errors.New("denied") }

	t.Cleanup(func() { killProcess = old })

	root := t.TempDir()
	writeCgroupFile(t, root, "kubepods.slice/cgroup.procs", "10\n")

	require.ErrorContains(t, killPodCgroups(root), "denied")
}

func TestDestroyKillsPodCgroupsOnlyWhenCgroupRootIsSet(t *testing.T) {
	stubKill(t)

	b, _ := newBackend(t)
	cg := t.TempDir()
	kill := writeCgroupFile(t, cg, "kubepods.slice/cgroup.kill", "")

	require.NoError(t, b.Destroy(context.Background(), root("v1")))

	data, err := os.ReadFile(kill)
	require.NoError(t, err)
	require.Empty(t, data, "an empty CgroupRoot never touches cgroups")

	b.CgroupRoot = cg

	require.NoError(t, b.Destroy(context.Background(), root("v1")))

	data, err = os.ReadFile(kill)
	require.NoError(t, err)
	require.Equal(t, "1", string(data))
}
