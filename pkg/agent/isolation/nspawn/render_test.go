// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestApplyGrantsPlacesEveryPurpose(t *testing.T) {
	t.Parallel()

	var d TemplateData

	d.ApplyGrants([]isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/kvm"},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: "char-input"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/s", Target: "/t", ReadOnly: true},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidia-caps"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaLibDir, Source: "/h/0", Target: "/run/host-nvidia/0", ReadOnly: true},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaLibDir, Source: "/h/1", Target: "/run/host-nvidia/1", ReadOnly: true},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaI386LibDir, Source: "/h/i", Target: "/run/host-nvidia-i386/0"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaHelperBin, Source: "/usr/bin", Target: "/run/host-nvidia-bin"},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeAMDDevice, Source: "/dev/kfd"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeAMDSysFS, Source: "/sys/class/kfd"},
		{Kind: isolation.GrantLibraryPath, Purpose: "unknown", Source: "/ignored"},
	})

	require.Equal(t, []string{"/dev/kvm"}, d.HostDevicePaths)
	require.Equal(t, []string{"char-input"}, d.HostDeviceGroupSpecifiers)
	require.Equal(t, []config.AdditionalHostMount{{Source: "/s", Target: "/t", ReadOnly: true}}, d.AdditionalHostMounts)
	require.Equal(t, "char-nvidia-caps", d.NvidiaDeviceTargets[0].Allow.Specifier)
	require.Equal(t, 1, d.NvidiaLibDirMounts[1].Index)
	require.Equal(t, "/h/i", d.NvidiaI386LibDirMounts[0].HostDir)
	require.Equal(t, "/usr/bin", d.NvidiaBinDir)
	require.Equal(t, []string{"/dev/kfd"}, d.AMDGPUDevicePaths)
	require.Equal(t, []string{"/sys/class/kfd"}, d.AMDSysFSPaths)
}

func TestRenderEmitsBindsAndDeviceAllow(t *testing.T) {
	t.Parallel()

	d := TemplateData{MachineName: "kube1", BPFFSMountPath: "/run/b", ConfigRegenerationUnit: "regen.service"}
	d.ApplyGrants([]isolation.Grant{
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/kvm"},
		{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: "char-input"},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/s", Target: "/t", ReadOnly: true},
	})

	r, err := Render(d)
	require.NoError(t, err)
	require.Contains(t, string(r.Settings), "Bind=/dev/kvm\n")
	require.Contains(t, string(r.Settings), "BindReadOnly=/s:/t\n")
	require.Contains(t, string(r.ServiceOverride), "DeviceAllow=/dev/kvm rwm\n")
	require.Contains(t, string(r.ServiceOverride), "DeviceAllow=char-input rwm\n")
	require.NotEmpty(t, r.ConfigRegenerationUnit)
}

func TestRenderWithNoGrantsHasNoDeviceSections(t *testing.T) {
	t.Parallel()

	r, err := Render(TemplateData{MachineName: "kube1"})
	require.NoError(t, err)
	require.NotContains(t, string(r.Settings), "GPU support")
	require.NotContains(t, string(r.ServiceOverride), "GPU support")
}

func TestApplyGrantsCarriesMountDeviceAllow(t *testing.T) {
	t.Parallel()

	var d TemplateData

	d.ApplyGrants([]isolation.Grant{
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/dev/infiniband", Target: "/dev/infiniband", DeviceAllow: []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm"}},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/dev/dri", Target: "/dev/dri", DeviceAllow: []string{"char-drm", "char-infiniband_verbs"}},
		{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/s", Target: "/t"},
	})

	require.Equal(t, []string{"/dev/infiniband/rdma_cm", "char-drm", "char-infiniband_verbs"}, d.MountDeviceAllow)
	require.Equal(t, []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm"}, d.AdditionalHostMounts[0].DeviceAllow)
	require.Nil(t, d.AdditionalHostMounts[2].DeviceAllow)

	r, err := Render(d)
	require.NoError(t, err)
	require.Contains(t, string(r.Settings), "Bind=/dev/infiniband:/dev/infiniband\n")
	require.NotContains(t, string(r.Settings), "Bind=/dev/infiniband/")

	for _, allow := range d.MountDeviceAllow {
		require.Contains(t, string(r.ServiceOverride), "DeviceAllow="+allow+" rwm\n")
	}
}

func TestMountDeviceAllowEntries(t *testing.T) {
	t.Parallel()

	require.Nil(t, MountDeviceAllowEntries([]config.AdditionalHostMount{{Source: "/a"}}))
	require.Equal(t, []string{"char-a", "char-b"}, MountDeviceAllowEntries([]config.AdditionalHostMount{
		{DeviceAllow: []string{"char-b", "char-a"}},
		{DeviceAllow: []string{"char-a"}},
	}))
}
