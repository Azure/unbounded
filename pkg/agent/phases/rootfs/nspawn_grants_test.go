// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

// TestWriteNSpawnConfigsFromGrantsMatchesLegacyData pins that routing devices,
// mounts and libraries through backend-neutral grants renders the same files as
// filling the template data directly from the goal state fields, which is how
// the nspawn configuration was produced before grants existed.
func TestWriteNSpawnConfigsFromGrantsMatchesLegacyData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	machineDir := filepath.Join(dir, "kube1")

	gs := &goalstates.RootFS{
		MachineDir:             machineDir,
		NSpawnConfigFile:       filepath.Join(dir, "kube1.nspawn"),
		ServiceOverrideFile:    filepath.Join(dir, "override.conf"),
		ConfigRegenerationFile: filepath.Join(dir, "regen.service"),
		HostDevices: goalstates.HostDevices{
			KVM:        []string{"/dev/kvm"},
			Network:    []string{"/dev/net/tun"},
			Additional: []string{"char-input", "/dev/fuse"},
			DeviceDirectories: []config.AdditionalHostMount{{
				Source:      "/dev/infiniband",
				Target:      "/dev/infiniband",
				DeviceAllow: []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm"},
			}},
		},
		AdditionalHostMounts: []config.AdditionalHostMount{
			{Source: "/opt/ro", Target: "/opt/ro", ReadOnly: true},
			{Source: "/var/data", Target: "/data"},
		},
		Nvidia: goalstates.NvidiaHost{
			GPUDevicePaths:    []string{"/dev/nvidia0", "/dev/nvidia-caps", "/dev/nvidia-caps-imex-channels"},
			LibDirMounts:      []goalstates.NvidiaLibDirMount{{Index: 0, HostDir: "/usr/lib/a", ContainerDir: "/run/host-nvidia/0"}, {Index: 1, HostDir: "/usr/lib/b", ContainerDir: "/run/host-nvidia/1"}},
			I386LibDirMounts:  []goalstates.NvidiaLibDirMount{{Index: 0, HostDir: "/usr/lib/i386", ContainerDir: "/run/host-nvidia-i386/0"}},
			NvidiaIMEXCtlPath: "/usr/bin/nvidia-imex-ctl",
		},
		AMD: goalstates.AMDHost{
			GPUDevicePaths: []string{"/dev/kfd", "/dev/dri/card0", "/dev/nvidia0"},
			SysFSPaths:     []string{"/sys/class/kfd"},
		},
	}

	require.NoError(t, writeNSpawnConfigs(slog.New(slog.DiscardHandler), gs))

	legacy := nspawnTemplateData{
		MachineName:                  "kube1",
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		ConfigRegenerationUnit:       goalstates.ConfigRegenerationUnit("kube1"),
		HostDevicePaths:              gs.HostDevices.Paths(),
		HostDeviceGroupSpecifiers:    gs.HostDevices.DeviceGroupSpecifiers(),
		MountDeviceAllow:             []string{"/dev/infiniband/rdma_cm", "char-infiniband_verbs"},
		AdditionalHostMounts:         append(slices.Clone(gs.HostDevices.DeviceDirectories), gs.AdditionalHostMounts...),
		NvidiaDeviceTargets:          nspawn.NvidiaDeviceTargets(gs.Nvidia.GPUDevicePaths),
		NvidiaLibDirMounts:           gs.Nvidia.LibDirMounts,
		NvidiaI386LibDirMounts:       gs.Nvidia.I386LibDirMounts,
		NvidiaBinDir:                 gs.Nvidia.HostBinDir(),
		AMDGPUDevicePaths:            goalstates.PathsExcluding(gs.AMD.GPUDevicePaths, gs.Nvidia.GPUDevicePaths),
		AMDSysFSPaths:                gs.AMD.SysFSPaths,
	}
	legacy.PreStartCommand, legacy.PostStartCommand = nspawnLifecycleCommands(gs, "kube1")
	// The goal state runs the lifecycle helper, whose refused exit is not restarted.
	legacy.PreStartRefusedExitCode = goalstates.NSpawnPreStartRefusedExitCode

	for file, tmpl := range map[string]string{
		gs.NSpawnConfigFile:       "nspawn.conf",
		gs.ServiceOverrideFile:    "service-override.conf",
		gs.ConfigRegenerationFile: "config-regeneration.service",
	} {
		var want bytes.Buffer
		require.NoError(t, nspawn.Templates.ExecuteTemplate(&want, tmpl, legacy))

		got, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Equal(t, want.String(), string(got), tmpl)
	}

	// A device NVIDIA already owns is bound once, not again for AMD.
	rendered, err := os.ReadFile(gs.NSpawnConfigFile)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(rendered), "Bind=/dev/nvidia0\n"))
	require.Contains(t, string(rendered), "Bind=/dev/kfd\n")
	// The device directory is bound whole, with its devices allowed by group.
	require.Contains(t, string(rendered), "Bind=/dev/infiniband:/dev/infiniband\n")

	override, err := os.ReadFile(gs.ServiceOverrideFile)
	require.NoError(t, err)
	require.Contains(t, string(override), "DeviceAllow=char-infiniband_verbs rwm\n")
}
