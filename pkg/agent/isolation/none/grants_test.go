// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// gpuHost builds a directory tree standing in for a GPU host and the grants
// goalstates derives from it. Every path the grants name exists under HostRoot.
func gpuHost(t *testing.T) (hostRoot string, grants []isolation.Grant) {
	t.Helper()

	hostRoot = t.TempDir()

	touch := func(p string) {
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, p), nil, 0o755))
	}

	for _, p := range []string{"/dev/nvidia0", "/dev/nvidiactl", "/usr/bin/nvidia-smi", "/usr/bin/nvidia-ctk", "/usr/bin/nvidia-container-runtime"} {
		touch(p)
	}

	for _, d := range []string{"/usr/lib/x86_64-linux-gnu", "/usr/lib/i386-linux-gnu", "/dev/nvidia-caps"} {
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, d), 0o755))
	}

	host := goalstates.NvidiaHost{
		GPUDevicePaths:   []string{"/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-caps"},
		LibDirMounts:     []goalstates.NvidiaLibDirMount{{Index: 0, HostDir: "/usr/lib/x86_64-linux-gnu", ContainerDir: "/run/host-nvidia/0"}},
		I386LibDirMounts: []goalstates.NvidiaLibDirMount{{Index: 0, HostDir: "/usr/lib/i386-linux-gnu", ContainerDir: "/run/host-nvidia-i386/0"}},
		NvidiaSMIPath:    "/usr/bin/nvidia-smi",
	}

	return hostRoot, host.Grants()
}

func gpuBackend(t *testing.T) (*Backend, []isolation.Grant) {
	t.Helper()

	b, _ := newBackend(t)
	root, grants := gpuHost(t)
	b.HostRoot = root
	b.GPUHostPaths = []string{"/usr/bin/nvidia-ctk", "/usr/bin/nvidia-container-runtime"}

	return b, grants
}

func TestPrepareHonorsTheNvidiaGrantsNatively(t *testing.T) {
	b, grants := gpuBackend(t)

	// All four purposes are present in what the goal state produces.
	purposes := map[isolation.GrantPurpose]bool{}
	for _, g := range grants {
		purposes[g.Purpose] = true
	}

	require.Len(t, purposes, 4, "device, lib dir, i386 lib dir and helper bin")

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: grants})
	require.NoError(t, err)
}

func TestPrepareWithGrantsWritesTheSameUnitsAsWithout(t *testing.T) {
	read := func(b *Backend) map[string]string {
		out := map[string]string{}

		require.NoError(t, filepath.WalkDir(b.SystemdDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			rel, err := filepath.Rel(b.SystemdDir, p)
			out[rel] = string(data)

			return err
		}))

		return out
	}

	plain, _ := newBackend(t)
	_, err := plain.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	gpu, grants := gpuBackend(t)
	gpu.Layout = plain.Layout
	_, err = gpu.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: grants})
	require.NoError(t, err)

	require.NotEmpty(t, read(plain))
	require.Equal(t, read(plain), read(gpu), "the stack sees the host, so grants add nothing to the units")
}

func TestPrepareWithoutGrantsNeedsNothingFromTheHost(t *testing.T) {
	b, _ := newBackend(t)
	b.GPUHostPaths = []string{"/usr/bin/nvidia-ctk"}
	b.HostRoot = t.TempDir() // empty: no toolkit

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err, "a GPU-less node never looks for the toolkit")

	_, err = b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{}})
	require.NoError(t, err)
}

func TestPrepareRefusesGrantsItCannotHonor(t *testing.T) {
	for name, tc := range map[string]struct {
		grant isolation.Grant
		want  string
	}{
		"mount at another path": {
			grant: isolation.Grant{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data", Target: "/models"},
			want:  "cannot be honored in isolation mode none",
		},
		"library path": {
			grant: isolation.Grant{Kind: isolation.GrantLibraryPath, Source: "/opt/lib"},
			want:  "library-path grant",
		},
		"device at another path": {
			grant: isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/null", Target: "/dev/gpu0"},
			want:  "cannot be honored in isolation mode none",
		},
		"missing device": {
			grant: isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/absent"},
			want:  "/dev/absent",
		},
		"missing mount source": {
			grant: isolation.Grant{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/absent"},
			want:  "/absent",
		},
		"device with nothing to name": {
			grant: isolation.Grant{Kind: isolation.GrantDevice},
			want:  "neither a device node nor a device class",
		},
		"mount without a source": {
			grant: isolation.Grant{Kind: isolation.GrantMount},
			want:  "no source",
		},
		"relative source": {
			grant: isolation.Grant{Kind: isolation.GrantMount, Source: "data"},
			want:  "absolute",
		},
		"unknown kind": {
			grant: isolation.Grant{Kind: "bogus", Source: "/x"},
			want:  "unsupported grant kind",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := newBackend(t)
			b.HostRoot = t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(b.HostRoot, "data"), 0o755))

			_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{tc.grant}})
			require.ErrorContains(t, err, tc.want)
			require.NoDirExists(t, b.Layout.VersionDir("v1"), "a refused Prepare leaves nothing behind")
			require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitContainerd))
		})
	}
}

func TestPrepareAcceptsIdentityMountsAndDeviceClasses(t *testing.T) {
	b, _ := newBackend(t)
	b.HostRoot = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(b.HostRoot, "data"), 0o755))

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data", Target: "/data", ReadOnly: true},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: "char-input"},
	}})
	require.NoError(t, err)
}

// A device directory such as /dev/infiniband is used at its own path, and the
// units run without a device cgroup policy, so its DeviceAllow entries need
// nothing: the grant is accepted when the directory exists and refused when it
// does not.
func TestPrepareAcceptsADeviceDirectoryAtItsOwnPath(t *testing.T) {
	grant := isolation.Grant{
		Kind:        isolation.GrantMount,
		Purpose:     isolation.PurposeHostMount,
		Source:      "/dev/infiniband",
		Target:      "/dev/infiniband",
		DeviceAllow: []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm"},
	}

	b, _ := newBackend(t)
	b.HostRoot = t.TempDir()

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{grant}})
	require.ErrorContains(t, err, "/dev/infiniband")

	require.NoError(t, os.MkdirAll(filepath.Join(b.HostRoot, "dev", "infiniband"), 0o755))

	_, err = b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{grant}})
	require.NoError(t, err)
}

func TestPrepareNamesAMissingToolkitOnAGPUHost(t *testing.T) {
	b, grants := gpuBackend(t)
	require.NoError(t, os.Remove(filepath.Join(b.HostRoot, "usr/bin/nvidia-container-runtime")))

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: grants})
	require.ErrorContains(t, err, "/usr/bin/nvidia-container-runtime")
	require.ErrorContains(t, err, "NVIDIA Container Toolkit")
	require.NoDirExists(t, b.Layout.VersionDir("v1"))
}

func TestPrepareReportsEveryRefusedGrant(t *testing.T) {
	b, _ := newBackend(t)
	b.HostRoot = t.TempDir()

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{
		{Kind: isolation.GrantDevice, Source: "/dev/a"},
		{Kind: isolation.GrantDevice, Source: "/dev/b"},
	}})
	require.ErrorContains(t, err, "/dev/a")
	require.ErrorContains(t, err, "/dev/b")
}

func TestBeforeStartRunsBeforeAnyUnitOnStartAndRestart(t *testing.T) {
	b, rec := newBackend(t)

	var seen []string

	b.BeforeStart = func(context.Context) error {
		seen = append(seen, "before:"+string(rune('0'+len(rec.calls))))

		return nil
	}

	r := root("v1")
	require.NoError(t, b.Start(context.Background(), r))
	require.NoError(t, b.Restart(context.Background(), r))

	// Start touched no unit before its hook; Restart's hook came after the two
	// enables and before its first restart.
	require.Equal(t, []string{"before:0", "before:2"}, seen)
	require.Equal(t, "systemctl restart "+UnitContainerd, rec.calls[2])
}

func TestBeforeStartFailureStopsTheStartBeforeAnyUnit(t *testing.T) {
	b, rec := newBackend(t)
	b.BeforeStart = func(context.Context) error { return errors.New("no toolkit") }

	for name, op := range map[string]func(context.Context, isolation.SlotRef) error{"start": b.Start, "restart": b.Restart} {
		err := op(context.Background(), root("v1"))
		require.ErrorContains(t, err, "no toolkit", name)
		require.Empty(t, rec.calls, name)
	}
}

func TestBeforeStartRunsOnUpgradeAndItsRollback(t *testing.T) {
	b, _ := newBackend(t)
	installVersions(t, b, "v1", "v2")

	starts := 0
	b.BeforeStart = func(context.Context) error { starts++; return nil }

	require.NoError(t, b.Upgrade(context.Background(), root("v2"), nil))
	require.Equal(t, 1, starts, "the upgraded stack was prepared once")

	starts = 0
	err := b.Upgrade(context.Background(), root("v1"), func(context.Context) error { return errors.New("not ready") })
	require.ErrorContains(t, err, "rolled back")
	require.Equal(t, 2, starts, "the new version and the version restored by rollback each prepared the host")
}

// The stack runs on the host and nothing mounts the path, so ReadOnly cannot be
// enforced. The grant is accepted (refusing would reject configs that follow
// the documented advice to set ReadOnly) but the limit is logged, not silent.
func TestPrepareLogsThatReadOnlyIsNotEnforced(t *testing.T) {
	var buf bytes.Buffer

	b, _ := newBackend(t)
	b.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b.HostRoot = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(b.HostRoot, "data"), 0o755))

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: []isolation.Grant{
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/data", ReadOnly: true},
	}})
	require.NoError(t, err)
	require.Contains(t, buf.String(), "read-only is not enforced")
	require.Contains(t, buf.String(), "/data")
}

// The NVIDIA library and helper directories are read-only on every mode and the
// host's own are never writable by the stack's workloads, so there is nothing
// to warn about.
func TestPrepareDoesNotWarnForTheNativeNvidiaDirectories(t *testing.T) {
	var buf bytes.Buffer

	b, grants := gpuBackend(t)
	b.Log = slog.New(slog.NewTextHandler(&buf, nil))

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1", Grants: grants})
	require.NoError(t, err)
	require.NotContains(t, buf.String(), "read-only is not enforced")
}
