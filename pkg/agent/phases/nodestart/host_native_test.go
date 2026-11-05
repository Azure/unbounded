// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// hostNativeGoalState returns a goal state whose host root is a temporary
// directory, the way the none backend runs the node stack on the host.
func hostNativeGoalState(t *testing.T) (*goalstates.NodeStart, string) {
	t.Helper()

	dir := t.TempDir()

	return &goalstates.NodeStart{
		MachineName:  "kube1",
		NodeName:     "worker-1",
		RootOverride: isolation.DirRoot{SlotName: "v1", Dir: dir},
		Containerd:   goalstates.Containerd{SandboxImage: "pause", RuncBinaryPath: "/opt/unbounded/node/current/bin/runc", CNIBinDir: "/opt/cni/bin", CNIConfDir: "/etc/cni/net.d", MetricsAddress: "0.0.0.0:10257"},
		Gantry:       goalstates.Gantry{Disabled: true},
		Kubelet: goalstates.Kubelet{
			APIServer:       "https://api.example.com",
			CACertData:      []byte("ca"),
			ClusterDNS:      "10.0.0.10",
			ResolvConf:      goalstates.LocalDNSResolvConfPath,
			KubeletAuthInfo: config.KubeletAuthInfo{BootstrapToken: "token"},
		},
		LocalDNS: goalstates.LocalDNS{
			Enabled:                true,
			NodeListenerIP:         netip.MustParseAddr("169.254.20.10"),
			ClusterListenerIP:      netip.MustParseAddr("169.254.20.11"),
			NodeUpstreamIPs:        []netip.Addr{netip.MustParseAddr("10.0.0.2")},
			Corefile:               []byte(".:53 {}\n"),
			OriginalHostResolvConf: []byte("search example.com\nnameserver 10.0.0.2\n"),
			CPULimitInMilliCores:   500,
			MemoryLimitInMB:        128,
		},
	}, dir
}

func TestConfigureKubeletOnHostUsesAgentOwnedDropIns(t *testing.T) {
	t.Parallel()

	gs, dir := hostNativeGoalState(t)
	task := ConfigureKubeletOnHost(gs, "unbounded-kubelet.service")
	require.NoError(t, task.Do(context.Background()))
	require.True(t, task.Changed())

	dropIns := filepath.Join(dir, "etc/systemd/system/unbounded-kubelet.service.d")
	for _, name := range []string{"10-kubeconfig.conf", "20-node-config.conf", "50-env-file.conf"} {
		require.FileExists(t, filepath.Join(dropIns, name))
	}

	// The distro unit names are never written, and the backend owns the unit.
	require.NoFileExists(t, filepath.Join(dir, "etc/systemd/system/kubelet.service"))
	require.NoDirExists(t, filepath.Join(dir, "etc/systemd/system/kubelet.service.d"))
	require.NoFileExists(t, filepath.Join(dir, "etc/systemd/system/unbounded-kubelet.service"))

	// Same-path files land at their host paths.
	require.FileExists(t, filepath.Join(dir, "var/lib/kubelet/config.yaml"))
	require.FileExists(t, filepath.Join(dir, "etc/kubernetes/pki/apiserver-client-ca.crt"))

	// A second apply of identical content reports no change.
	again := ConfigureKubeletOnHost(gs, "unbounded-kubelet.service")
	require.NoError(t, again.Do(context.Background()))
	require.False(t, again.Changed())
}

func TestConfigureContainerdOnHostSkipsUnit(t *testing.T) {
	t.Parallel()

	gs, dir := hostNativeGoalState(t)
	task := ConfigureContainerdOnHost(gs)
	require.NoError(t, task.Do(context.Background()))
	require.True(t, task.Changed())

	require.FileExists(t, filepath.Join(dir, "etc/containerd/config.toml"))
	require.NoFileExists(t, filepath.Join(dir, "etc/systemd/system/containerd.service"))

	again := ConfigureContainerdOnHost(gs)
	require.NoError(t, again.Do(context.Background()))
	require.False(t, again.Changed())
}

func TestConfigureLocalDNSLeavesHostResolver(t *testing.T) {
	t.Parallel()

	gs, dir := hostNativeGoalState(t)
	require.NoError(t, ConfigureLocalDNS(gs).Do(context.Background()))

	require.FileExists(t, filepath.Join(dir, "etc/unbounded/localdns/Corefile"))
	require.FileExists(t, filepath.Join(dir, "etc/unbounded/localdns/resolv.conf"))
	require.FileExists(t, filepath.Join(dir, "etc/systemd/system/localdns.slice"))
	require.NoFileExists(t, filepath.Join(dir, "etc/resolv.conf"), "the host resolver file must not be rewritten")
}

func TestWaitForKubeletOnHostUsesExecutorAndUnit(t *testing.T) {
	t.Parallel()

	gs, _ := hostNativeGoalState(t)

	var got []string

	gs.ExecOverride = func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
		got = args

		return "active\n", nil
	}

	require.NoError(t, WaitForKubeletOnHost(slog.New(slog.DiscardHandler), gs, "unbounded-kubelet.service").Do(context.Background()))
	require.Equal(t, []string{"systemctl", "is-active", "unbounded-kubelet.service"}, got)
}

func TestWaitForKubeletBootstrapOnHostChecksKubeconfig(t *testing.T) {
	t.Parallel()

	gs, _ := hostNativeGoalState(t)

	calls := 0
	gs.ExecOverride = func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
		calls++

		require.Equal(t, []string{"test", "-s", goalstates.KubeletKubeconfigPath}, args)

		if calls < 2 {
			return "", errors.New("missing")
		}

		return "", nil
	}

	require.NoError(t, WaitForKubeletBootstrapOnHost(slog.New(slog.DiscardHandler), gs).Do(context.Background()))
	require.Equal(t, 2, calls)
}

func TestImportContainerImagesOnHostUsesHostPath(t *testing.T) {
	t.Parallel()

	gs, _ := hostNativeGoalState(t)
	task := &importContainerImages{goalState: gs}
	archive := stagedContainerImageArchive{hostPath: "/host/a.tar", machinePath: "/machine/a.tar"}
	require.Equal(t, "/host/a.tar", task.archivePath(archive))

	gs.RootOverride = nil

	require.Equal(t, "/machine/a.tar", task.archivePath(archive))
}

func TestPreflightNoneUsesTheHostRootAndCurrentLink(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: "/var/lib/machines/kube1"},
		NodeStart: &goalstates.NodeStart{Containerd: goalstates.Containerd{MetricsAddress: "0.0.0.0:10257"}},
	}

	nspawn := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{}, gs)
	host := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{Isolation: "none"}, gs)

	require.Len(t, host, len(nspawn), "the same checks run in both modes")
}

func TestOwnerIdentityPerMode(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: "/var/lib/machines/kube1"},
		NodeStart: &goalstates.NodeStart{},
	}

	root, _, _ := ownerIdentity(config.AgentConfig{}, gs)
	require.Equal(t, "/var/lib/machines/kube1", root)

	root, kubelet, containerd := ownerIdentity(config.AgentConfig{Isolation: "none"}, gs)
	require.Equal(t, "/", root)
	require.Contains(t, kubelet, "kubelet")
	require.Contains(t, containerd, "containerd")
}

func TestOwnerIdentityTreatsAnUnsetOrInvalidModeAsNSpawn(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: "/var/lib/machines/kube1"},
		NodeStart: &goalstates.NodeStart{},
	}

	for _, mode := range []string{"", "nspawn", "microvm"} {
		root, kubelet, containerd := ownerIdentity(config.AgentConfig{Isolation: mode}, gs)
		require.Equal(t, "/var/lib/machines/kube1", root, mode)
		require.Equal(t, kubeletExecutablePath, kubelet, mode)
		require.Equal(t, containerdExecutablePath, containerd, mode)
	}
}
