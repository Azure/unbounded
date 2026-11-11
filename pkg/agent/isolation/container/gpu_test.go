// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

// nvidiaGrants is what the machine's NVIDIA host state asks for, with the same
// purposes, sources and targets goalstates.NvidiaHost.Grants produces: device
// nodes, a directory of capability devices, a library directory and an i386
// one (bound at /run/host-nvidia*), and the helper binary directory.
var nvidiaGrants = []isolation.Grant{
	{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidia0"},
	{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidiactl"},
	{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidia-uvm"},
	{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/nvidia-caps"},
	{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaLibDir, Source: "/usr/lib/x86_64-linux-gnu", Target: "/run/host-nvidia/0", ReadOnly: true},
	{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaI386LibDir, Source: "/usr/lib/i386-linux-gnu", Target: "/run/host-nvidia-i386/0", ReadOnly: true},
	{Kind: isolation.GrantMount, Purpose: isolation.PurposeNvidiaHelperBin, Source: "/usr/bin", Target: "/run/host-nvidia-bin", ReadOnly: true},
}

var nvidiaDevices = map[string]DeviceInfo{
	"/dev/nvidia0":    {Type: "c", Major: 195, Minor: 0, Mode: 0o666},
	"/dev/nvidiactl":  {Type: "c", Major: 195, Minor: 255, Mode: 0o666},
	"/dev/nvidia-uvm": {Type: "c", Major: 510, Minor: 0, Mode: 0o666},
}

func TestConfigNvidiaGolden(t *testing.T) {
	got, err := RenderConfig(BundleInput{
		Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1", SELinux: SELinuxEnforcing,
		Grants: nvidiaGrants, Devices: nvidiaDevices,
	})
	require.NoError(t, err)
	isolationtest.Golden(t, "config-nvidia.json", got)
}

func TestConfigNvidiaGrantsBecomeDevicesRulesAndReadOnlyMounts(t *testing.T) {
	b, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: nvidiaGrants, Devices: nvidiaDevices})
	require.NoError(t, err)

	var cfg struct {
		Mounts []struct {
			Destination string
			Source      string
			Options     []string
		}
		Linux struct {
			Devices []struct {
				Path  string
				Major int64
				Minor int64
			}
			Resources struct {
				Devices []struct {
					Allow bool
					Major *int64
					Minor *int64
				}
			}
		}
	}

	require.NoError(t, json.Unmarshal(b, &cfg))

	paths := map[string]bool{}
	for _, d := range cfg.Linux.Devices {
		paths[d.Path] = true
	}

	require.Equal(t, map[string]bool{"/dev/kmsg": true, "/dev/nvidia0": true, "/dev/nvidiactl": true, "/dev/nvidia-uvm": true}, paths)

	rules := 0

	for _, r := range cfg.Linux.Resources.Devices {
		if r.Allow && r.Major != nil && *r.Major == 195 {
			rules++
		}
	}

	require.Equal(t, 2, rules, "an explicit cgroup rule for each nvidia0 and nvidiactl")

	mounts := map[string][]string{}
	for _, m := range cfg.Mounts {
		mounts[m.Destination] = m.Options
	}

	for dest, src := range map[string]string{
		"/run/host-nvidia/0":      "/usr/lib/x86_64-linux-gnu",
		"/run/host-nvidia-i386/0": "/usr/lib/i386-linux-gnu",
		"/run/host-nvidia-bin":    "/usr/bin",
	} {
		require.Contains(t, mounts[dest], "ro", dest+" from "+src+" is read-only")
	}

	require.Contains(t, mounts, "/dev/nvidia-caps", "a directory of device nodes is bound")
}

func TestRenderBundleOfNvidiaGrantsBindsTheCapsDirectory(t *testing.T) {
	b, _, _ := newBackend(t)
	b.StatDevice = func(p string) (DeviceInfo, error) {
		if d, ok := nvidiaDevices[p]; ok {
			return d, nil
		}

		return DeviceInfo{}, os.ErrNotExist
	}

	// The host tree has /dev/nvidia-caps as a directory.
	grants := []isolation.Grant{}

	for _, g := range nvidiaGrants {
		if g.Source == "/dev/nvidia-caps" {
			g.Kind = isolation.GrantDevice
		}

		grants = append(grants, g)
	}

	require.NoError(t, os.MkdirAll(b.SourceRoot+"/dev/nvidia-caps", 0o755))
	require.NoError(t, b.RenderBundle("kube1", grants))

	data, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Contains(t, string(data), "/dev/nvidia-caps")
	require.Contains(t, string(data), "/run/host-nvidia/0")
}

func TestPrepareWithNoGrantsHasNoGPUWiring(t *testing.T) {
	b, _, _ := newBackend(t)

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)

	data, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)

	for _, s := range []string{"nvidia", "LD_LIBRARY_PATH"} {
		require.NotContains(t, string(data), s)
	}

	// Identical to the bundle rendered for no grants at all.
	want, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: b.Layout.RootfsDir("kube1"), SELinux: SELinuxEnforcing, ExtraBinds: nil, Devices: map[string]DeviceInfo{}})
	require.NoError(t, err)
	require.Equal(t, string(want), string(data))
}
