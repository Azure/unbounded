// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInContainerIsNotHostNative(t *testing.T) {
	t.Parallel()

	gs, _ := hostNativeGoalState(t)
	require.True(t, gs.HostNative())
	require.False(t, gs.NSpawnMachine())

	gs.InContainer = true
	require.False(t, gs.HostNative(), "a container has its own root")
	require.False(t, gs.NSpawnMachine(), "a container is not an nspawn machine")
}

func TestConfigureReportsChangesAndWritesUnitsIntoTheRoot(t *testing.T) {
	t.Parallel()

	gs, dir := hostNativeGoalState(t)
	gs.InContainer = true

	containerd := ConfigureContainerdTracked(gs)
	kubelet := ConfigureKubeletTracked(gs)

	require.NoError(t, containerd.Do(context.Background()))
	require.NoError(t, kubelet.Do(context.Background()))
	require.True(t, containerd.Changed())
	require.True(t, kubelet.Changed())

	// Like the nspawn machine, the node's own rootfs carries the distro unit
	// names, so the units are written (the none backend leaves them out).
	require.FileExists(t, filepath.Join(dir, "etc/systemd/system/containerd.service"))
	require.FileExists(t, filepath.Join(dir, "etc/systemd/system/kubelet.service"))

	again := ConfigureContainerdTracked(gs)
	require.NoError(t, again.Do(context.Background()))
	require.False(t, again.Changed(), "identical content is not a change")

	againKubelet := ConfigureKubeletTracked(gs)
	require.NoError(t, againKubelet.Do(context.Background()))
	require.False(t, againKubelet.Changed())
}

func TestConfigureLocalDNSInContainerWritesRootfsResolver(t *testing.T) {
	t.Parallel()

	gs, dir := hostNativeGoalState(t)
	gs.InContainer = true

	require.NoError(t, ConfigureLocalDNS(gs).Do(context.Background()))

	// The container's /etc/resolv.conf is its own file, unlike a host-native
	// node where it is the host's.
	data, err := os.ReadFile(filepath.Join(dir, "etc/resolv.conf"))
	require.NoError(t, err)
	require.Contains(t, string(data), "nameserver 169.254.20.10")

	hostNative, hostDir := hostNativeGoalState(t)
	require.NoError(t, ConfigureLocalDNS(hostNative).Do(context.Background()))
	require.NoFileExists(t, filepath.Join(hostDir, "etc/resolv.conf"))
}
