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
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func gpuHostState() goalstates.NvidiaHost {
	return goalstates.NvidiaHost{
		GPUDevicePaths:  []string{"/dev/nvidia0", "/dev/nvidiactl"},
		ContainerLibDir: "/usr/lib/x86_64-linux-gnu",
		LibMappings: []goalstates.NvidiaLibMapping{{
			HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1", ContainerPath: "/run/host-nvidia/0/libcuda.so.580.126.09",
			LinkPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		}},
		LibDirMounts:  []goalstates.NvidiaLibDirMount{{Index: 0, HostDir: "/usr/lib/x86_64-linux-gnu", ContainerDir: "/run/host-nvidia/0"}},
		DriverVersion: "580.126.09",
	}
}

func runningContainer(t *testing.T, f *fixture) *container.Backend {
	t.Helper()

	b := f.node("kube1", false).Backend()
	f.h.active[container.UnitName("kube1")] = true

	return b
}

func TestReconcileNVIDIAWithoutGPUsDoesNothing(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) { return goalstates.NvidiaHost{}, nil },
	})
	require.NoError(t, err)
	require.Empty(t, f.h.snapshot(), "a GPU-less host never reaches into the container")
}

func TestReconcileNVIDIARebuildsGPUStateInsideTheContainer(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)

	// The archives the start imported are imported again once containerd is
	// back, from the directory the hooks name.
	require.NoError(t, os.MkdirAll(f.images(), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.images(), "a.tar"), nil, 0o644))

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve:         func(string) (goalstates.NvidiaHost, error) { return gpuHostState(), nil },
		ImageArchiveDir: f.images(),
	})
	require.NoError(t, err)

	calls := f.h.snapshot()
	for _, c := range calls {
		require.Contains(t, c, testRuntime+" exec unbounded-kube1 ", "every command runs in the container, never on the host: %s", c)
	}

	requireOrder(t, calls,
		"systemctl show --property=SystemState",
		"systemctl stop "+goalstates.SystemdUnitKubelet,
		"systemctl stop "+goalstates.SystemdUnitContainerd,
		"ldconfig",
		goalstates.NvidiaCTKPath+" cdi generate --driver-root "+goalstates.NvidiaDriverDir,
		goalstates.NvidiaCTKPath+" config --set nvidia-container-runtime.mode=cdi --in-place",
		"systemctl enable --now "+goalstates.SystemdUnitContainerd,
		"ctr --namespace k8s.io images import "+filepath.Join(goalstates.ContainerImageArchiveDir, "a.tar"),
		"systemctl enable --now "+goalstates.SystemdUnitKubelet,
	)
}

func TestReconcileNVIDIARefusesIncompleteDriverState(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)

	state := gpuHostState()
	state.DriverVersion = ""

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) { return state, nil },
	})
	require.ErrorIs(t, err, goalstates.ErrNVIDIAStateUnavailable)
	require.Empty(t, f.h.snapshot(), "nothing is stopped for state that cannot be rebuilt")
}

func TestReconcileNVIDIAWaitsForSystemdBeforeStoppingAnything(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)
	f.h.state = "starting"

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve:      func(string) (goalstates.NvidiaHost, error) { return gpuHostState(), nil },
		PollInterval: 1, ReadyTimeout: 20_000_000,
	})
	require.ErrorContains(t, err, "not ready")

	for _, c := range f.h.snapshot() {
		require.NotContains(t, c, "systemctl stop")
	}
}

func TestReconcileNVIDIAReportsResolveFailureAndBadSlot(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) {
			return goalstates.NvidiaHost{}, errors.New("nvidia-smi hung")
		},
	})
	require.ErrorContains(t, err, "nvidia-smi hung")

	require.Error(t, ReconcileNVIDIA(t.Context(), quiet(), b, "../x", NVIDIAHooks{}))
}

func TestReconcileNVIDIAReportsTheFailingStep(t *testing.T) {
	f := newFixture(t)
	b := runningContainer(t, f)
	f.h.failAll["cdi generate"] = errors.New("no spec")

	err := ReconcileNVIDIA(t.Context(), quiet(), b, "kube1", NVIDIAHooks{
		Resolve:         func(string) (goalstates.NvidiaHost, error) { return gpuHostState(), nil },
		ImageArchiveDir: f.images(),
	})
	require.ErrorContains(t, err, "no spec")
	require.ErrorContains(t, err, "kube1")
}

func nvidiaRecorded(host goalstates.NvidiaHost) []isolation.Grant {
	return append(
		[]isolation.Grant{{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data"}},
		append(host.Grants(), isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeAMDDevice, Source: "/dev/kfd"})...,
	)
}

func TestRefreshNVIDIAGrantsFollowsTheHostAndKeepsTheRest(t *testing.T) {
	recorded := nvidiaRecorded(gpuHostState())

	// A reboot added a second GPU.
	next := gpuHostState()
	next.GPUDevicePaths = append(next.GPUDevicePaths, "/dev/nvidia1")

	got, err := RefreshNVIDIAGrants(NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) { return next, nil },
	})(t.Context(), recorded)
	require.NoError(t, err)

	require.Equal(t, recorded[0], got[0], "the grant before the NVIDIA set stays first")
	require.Equal(t, recorded[len(recorded)-1], got[len(got)-1], "the grant after it stays last")
	require.Equal(t, append(append([]isolation.Grant{recorded[0]}, next.Grants()...), recorded[len(recorded)-1]), got)
}

func TestRefreshNVIDIAGrantsUnchangedHostIsUnchanged(t *testing.T) {
	recorded := nvidiaRecorded(gpuHostState())

	got, err := RefreshNVIDIAGrants(NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) { return gpuHostState(), nil },
	})(t.Context(), recorded)
	require.NoError(t, err)
	require.Equal(t, recorded, got)
}

func TestRefreshNVIDIAGrantsLeavesACPUNodeAlone(t *testing.T) {
	recorded := []isolation.Grant{{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data"}}

	got, err := RefreshNVIDIAGrants(NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) {
			t.Fatal("a node without NVIDIA grants must not probe the host")

			return goalstates.NvidiaHost{}, nil
		},
	})(t.Context(), recorded)
	require.NoError(t, err)
	require.Equal(t, recorded, got)

	got, err = RefreshNVIDIAGrants(NVIDIAHooks{})(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestRefreshNVIDIAGrantsFailsWhenTheDriverIsGone(t *testing.T) {
	recorded := nvidiaRecorded(gpuHostState())

	for name, resolve := range map[string]func(string) (goalstates.NvidiaHost, error){
		"incomplete": func(string) (goalstates.NvidiaHost, error) { return goalstates.NvidiaHost{}, nil },
		"error": func(string) (goalstates.NvidiaHost, error) {
			return goalstates.NvidiaHost{}, errors.New("probe failed")
		},
	} {
		_, err := RefreshNVIDIAGrants(NVIDIAHooks{Resolve: resolve})(context.Background(), recorded)
		require.Error(t, err, name)
	}
}
