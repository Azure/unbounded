// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNvidiaHostGrants(t *testing.T) {
	t.Parallel()

	require.Empty(t, NvidiaHost{}.Grants())

	got := NvidiaHost{
		GPUDevicePaths:   []string{"/dev/nvidia0"},
		LibDirMounts:     []NvidiaLibDirMount{{HostDir: "/h/a", ContainerDir: "/run/host-nvidia/0"}},
		I386LibDirMounts: []NvidiaLibDirMount{{HostDir: "/h/i", ContainerDir: "/run/host-nvidia-i386/0"}},
		NvidiaSMIPath:    "/usr/bin/nvidia-smi",
	}.Grants()

	require.Equal(t, []isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidia0"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaLibDir, Source: "/h/a", Target: "/run/host-nvidia/0", ReadOnly: true},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaI386LibDir, Source: "/h/i", Target: "/run/host-nvidia-i386/0", ReadOnly: true},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaHelperBin, Source: "/usr/bin", Target: NvidiaHostBinDir, ReadOnly: true},
	}, got)
}

func TestNvidiaHostBinDirPrefersFirstDiscovered(t *testing.T) {
	t.Parallel()

	require.Empty(t, NvidiaHost{}.HostBinDir())
	require.Equal(t, "/b", NvidiaHost{NvidiaIMEXPath: "/b/nvidia-imex", NvidiaIMEXCtlPath: "/c/x"}.HostBinDir())
}

// Restores TestNvidiaHostBinDirFallsBackToIMEX, which tested the nspawn render
// wrapper nvidiaHostBinDir. That wrapper is gone; HostBinDir replaces it.
func TestNvidiaHostBinDirFallsBackToIMEX(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/usr/bin", NvidiaHost{
		NvidiaIMEXPath:    "/usr/bin/nvidia-imex",
		NvidiaIMEXCtlPath: "/usr/bin/nvidia-imex-ctl",
	}.HostBinDir())
}

func TestAMDHostGrantsSkipExcludedDevices(t *testing.T) {
	t.Parallel()

	got := AMDHost{GPUDevicePaths: []string{"/dev/kfd", "/dev/dri/card0"}, SysFSPaths: []string{"/sys/class/kfd"}}.
		Grants([]string{"/dev/dri/card0"})

	require.Equal(t, []isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeAMDDevice, Source: "/dev/kfd"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeAMDSysFS, Source: "/sys/class/kfd", ReadOnly: true},
	}, got)
}

func TestHostDevicesGrantsSplitPathsAndClasses(t *testing.T) {
	t.Parallel()

	got := HostDevices{KVM: []string{"/dev/kvm"}, Additional: []string{"char-input", "/dev/fuse"}}.Grants()

	require.Equal(t, []isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/fuse"},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/kvm"},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: "char-input"},
	}, got)
	require.Empty(t, HostDevices{}.Grants())
}

func TestHostDevicesGrantsIncludeDeviceDirectories(t *testing.T) {
	t.Parallel()

	allow := []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm"}
	d := HostDevices{
		KVM:               []string{"/dev/kvm"},
		DeviceDirectories: []config.AdditionalHostMount{{Source: "/dev/infiniband", Target: "/dev/infiniband", DeviceAllow: allow}},
	}

	got := d.Grants()
	require.Equal(t, []isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/kvm"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/dev/infiniband", Target: "/dev/infiniband", DeviceAllow: allow},
	}, got)

	// The grant owns its DeviceAllow slice.
	got[1].DeviceAllow[0] = "mutated"

	require.Equal(t, "char-infiniband_verbs", allow[0])
}

func TestRootFSGrantsOrderAndDedup(t *testing.T) {
	t.Parallel()

	r := &RootFS{
		HostDevices:          HostDevices{KVM: []string{"/dev/kvm"}},
		AdditionalHostMounts: []config.AdditionalHostMount{{Source: "/s", Target: "/t", ReadOnly: true}},
		Nvidia:               NvidiaHost{GPUDevicePaths: []string{"/dev/dri/card0"}},
		AMD:                  AMDHost{GPUDevicePaths: []string{"/dev/dri/card0", "/dev/kfd"}},
	}

	var purposes []isolation.GrantPurpose
	for _, g := range r.Grants() {
		purposes = append(purposes, g.Purpose)
	}

	require.Equal(t, []isolation.GrantPurpose{
		isolation.PurposeHostDevice, isolation.PurposeHostMount, isolation.PurposeNvidiaDevice, isolation.PurposeAMDDevice,
	}, purposes)
	require.Empty(t, (&RootFS{}).Grants())
}

func TestPathsExcluding(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"a"}, PathsExcluding([]string{"a"}, nil))
	require.Equal(t, []string{"a", "c"}, PathsExcluding([]string{"a", "b", "c"}, []string{"b"}))
	require.Empty(t, PathsExcluding(nil, []string{"x"}))
}
