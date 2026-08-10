// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestServiceOverride_RenderedSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "service-override-kube1.conf.golden", "service-override.conf", defaultNSpawnTemplateData("kube1"))
}

func TestServiceOverride_MachineNameSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "service-override-kube2.conf.golden", "service-override.conf", defaultNSpawnTemplateData("kube2"))
}

func TestNSpawnConfig_RenderedSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "nspawn.conf.golden", "nspawn.conf", defaultNSpawnTemplateData("kube1"))
}

func TestNSpawnRenderedScenarios(t *testing.T) {
	t.Parallel()

	cases := map[string]nspawnTemplateData{
		"cpu-only": nspawnRenderScenarioData(),
		"nvidia-gb300-rack-full": func() nspawnTemplateData {
			data := nspawnRenderScenarioData()
			data.NvidiaDeviceTargets = nvidiaNSpawnDeviceTargets([]string{
				"/dev/dri/card0",
				"/dev/dri/card1",
				"/dev/dri/card2",
				"/dev/dri/card3",
				"/dev/dri/card4",
				"/dev/dri/renderD128",
				"/dev/dri/renderD129",
				"/dev/dri/renderD130",
				"/dev/dri/renderD131",
				"/dev/nvidia-modeset",
				"/dev/nvidia-uvm",
				"/dev/nvidia-uvm-tools",
				"/dev/nvidia0",
				"/dev/nvidia1",
				"/dev/nvidia2",
				"/dev/nvidia3",
				"/dev/nvidiactl",
				"/dev/nvidia-caps",
				"/dev/nvidia-caps-imex-channels",
			})
			data.NvidiaLibDirMounts = []goalstates.NvidiaLibDirMount{
				{
					HostDir:      "/usr/lib/aarch64-linux-gnu",
					ContainerDir: "/run/host-nvidia/0",
				},
				{
					HostDir:      "/usr/lib/aarch64-linux-gnu/vdpau",
					ContainerDir: "/run/host-nvidia/1",
				},
			}
			data.NvidiaBinDir = "/usr/bin"

			return data
		}(),
		"nvidia-all-helpers": func() nspawnTemplateData {
			data := nspawnRenderScenarioData()
			data.NvidiaDeviceTargets = nvidiaNSpawnDeviceTargets([]string{
				"/dev/nvidia0",
				"/dev/nvidiactl",
				"/dev/nvidia-uvm",
				"/dev/dri/renderD128",
			})
			data.NvidiaBinDir = nvidiaHostBinDir(goalstates.NvidiaHost{
				NvidiaSMIPath:     "/usr/bin/nvidia-smi",
				NvidiaIMEXPath:    "/usr/bin/nvidia-imex",
				NvidiaIMEXCtlPath: "/usr/bin/nvidia-imex-ctl",
			})

			return data
		}(),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireRenderedGolden(t, name, data)
		})
	}
}

func TestServiceOverride_HostDevicesDeviceAllow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	data := defaultNSpawnTemplateData("kube1")
	data.HostDevicePaths = []string{"/dev/kvm"}
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data))

	out := buf.String()

	// Binding a device in the .nspawn [Files] section is not enough: the
	// cgroup device controller blocks it unless the service drop-in also
	// grants access. Assert the DeviceAllow line is emitted for host devices.
	require.Contains(t, out, "DeviceAllow=/dev/kvm rwm")

	// DeviceAllow must live in the [Service] section.
	require.Less(t, strings.Index(out, "[Service]"), strings.Index(out, "DeviceAllow=/dev/kvm rwm"))
}

func TestServiceOverride_HostDeviceGroupSpecifiers(t *testing.T) {
	t.Parallel()

	var nspawnBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", nspawnTemplateData{
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		HostDeviceGroupSpecifiers:    []string{"char-input", "char-pts"},
	}))

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", nspawnTemplateData{
		MachineName:               "kube1",
		BPFFSMountPath:            goalstates.BPFFSMountPath("kube1"),
		HostDeviceGroupSpecifiers: []string{"char-input", "char-pts"},
	}))

	require.NotContains(t, nspawnBuf.String(), "Bind=char-")
	require.Contains(t, overrideBuf.String(), "DeviceAllow=char-input rwm")
	require.Contains(t, overrideBuf.String(), "DeviceAllow=char-pts rwm")
}

func TestServiceOverride_MultipleHostDevices(t *testing.T) {
	t.Parallel()

	devices := []string{"/dev/kvm", "/dev/net/tun", "/dev/vhost-net", "/dev/sda", "/dev/infiniband/uverbs0"}

	var nspawnBuf bytes.Buffer

	data := defaultNSpawnTemplateData("kube1")
	data.HostDevicePaths = devices
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", data))

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", data))

	// Every host device must get both a bind mount in the .nspawn config and a
	// matching cgroup DeviceAllow in the service drop-in; otherwise the node is
	// either invisible or blocked inside the container.
	for _, dev := range devices {
		require.Contains(t, nspawnBuf.String(), "Bind="+dev)
		require.Contains(t, overrideBuf.String(), "DeviceAllow="+dev+" rwm")
	}
}

func TestServiceOverride_AMDGPUDevices(t *testing.T) {
	t.Parallel()

	devices := []string{"/dev/dri/card0", "/dev/dri/renderD128", "/dev/kfd"}

	var nspawnBuf bytes.Buffer

	data := defaultNSpawnTemplateData("kube1")
	data.AMDGPUDevicePaths = devices
	data.AMDSysFSPaths = []string{"/sys/module/amdgpu", "/sys/class/kfd"}
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", data))

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", data))

	for _, dev := range devices {
		require.Contains(t, nspawnBuf.String(), "Bind="+dev)
		require.Contains(t, overrideBuf.String(), "DeviceAllow="+dev+" rwm")
	}

	require.Contains(t, nspawnBuf.String(), "AMD GPU support")
	require.Contains(t, overrideBuf.String(), "AMD GPU support")
	require.Contains(t, nspawnBuf.String(), "BindReadOnly=/sys/module/amdgpu")
	require.Contains(t, nspawnBuf.String(), "BindReadOnly=/sys/class/kfd")
}

func TestNSpawnConfig_NVIDIADriverRootMounts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", nspawnTemplateData{
		NvidiaDeviceTargets: nvidiaNSpawnDeviceTargets([]string{"/dev/nvidia0"}),
		NvidiaLibDirMounts: []goalstates.NvidiaLibDirMount{{
			HostDir:      "/usr/lib/x86_64-linux-gnu",
			ContainerDir: "/run/host-nvidia/0",
		}},
		NvidiaI386LibDirMounts: []goalstates.NvidiaLibDirMount{{
			HostDir:      "/usr/lib/i386-linux-gnu",
			ContainerDir: "/run/host-nvidia-i386/0",
		}},
		NvidiaBinDir: "/usr/bin",
	}))

	require.Contains(t, buf.String(), "BindReadOnly=/usr/lib/x86_64-linux-gnu:/run/host-nvidia/0")
	require.Contains(t, buf.String(), "BindReadOnly=/usr/lib/i386-linux-gnu:/run/host-nvidia-i386/0")
	require.Contains(t, buf.String(), "BindReadOnly=/usr/bin:/run/host-nvidia-bin")
	require.NotContains(t, buf.String(), ":/run/nvidia/driver")
}

func TestNSpawnConfig_AdditionalHostMounts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", nspawnTemplateData{
		AdditionalHostMounts: []config.AdditionalHostMount{
			{Source: "/opt/config", Target: "/opt/config", ReadOnly: true},
			{Source: "/var/lib/data", Target: "/data"},
		},
	}))

	require.Contains(t, buf.String(), "BindReadOnly=/opt/config:/opt/config")
	require.Contains(t, buf.String(), "Bind=/var/lib/data:/data")
}

func TestNSpawnConfig_NvidiaIMEXDevice(t *testing.T) {
	t.Parallel()

	const channels = "/dev/nvidia-caps-imex-channels"

	data := nspawnTemplateData{
		MachineName:                  "kube1",
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		NvidiaDeviceTargets:          nvidiaNSpawnDeviceTargets([]string{"/dev/nvidia-caps", channels}),
	}

	var nspawnBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", data))
	require.Contains(t, nspawnBuf.String(), "Bind=/dev/nvidia-caps")
	require.Contains(t, nspawnBuf.String(), "Bind="+channels)

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", data))
	require.Contains(t, overrideBuf.String(), "DeviceAllow=char-nvidia-caps rwm")
	require.Contains(t, overrideBuf.String(), "DeviceAllow=char-nvidia-caps-imex-channels rwm")
	require.NotContains(t, overrideBuf.String(), "DeviceAllow=/dev/nvidia-caps rwm")
	require.NotContains(t, overrideBuf.String(), "DeviceAllow="+channels+" rwm")
	require.NotContains(t, overrideBuf.String(), "DevicePolicy=")
}

func TestNvidiaHostBinDirFallsBackToIMEX(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/usr/bin", nvidiaHostBinDir(goalstates.NvidiaHost{
		NvidiaIMEXPath:    "/usr/bin/nvidia-imex",
		NvidiaIMEXCtlPath: "/usr/bin/nvidia-imex-ctl",
	}))
}

func TestPathsExcluding(t *testing.T) {
	t.Parallel()

	got := pathsExcluding(
		[]string{"/dev/dri/card0", "/dev/dri/renderD128", "/dev/kfd"},
		[]string{"/dev/dri/card0", "/dev/dri/renderD128"},
	)

	require.Equal(t, []string{"/dev/kfd"}, got)
}

func TestServiceOverride_BaseDeviceAllow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", defaultNSpawnTemplateData("kube1")))

	out := buf.String()

	// With no detected host devices, the override must not contain direct
	// device paths. This keeps the golden image unchanged while still allowing
	// generic device classes that do not depend on host discovery.
	require.NotContains(t, out, "DeviceAllow=/")
	require.Contains(t, out, "DeviceAllow=char-ipvtap rwm")
	require.Contains(t, out, "DeviceAllow=char-macvtap rwm")
	require.Equal(t, 2, strings.Count(out, "DeviceAllow="))
}

func TestServiceOverride_ConfigRegenerationDependency(t *testing.T) {
	t.Parallel()

	out := requireRenderedSnapshot(t, "service-override-kube1.conf.golden", "service-override.conf", defaultNSpawnTemplateData("kube1"))

	require.Contains(t, out, "Requires=unbounded-agent-regenerate-config@kube1.service")
	require.Contains(t, out, "After=unbounded-agent-regenerate-config@kube1.service")
	require.Less(t, strings.Index(out, "[Unit]"), strings.Index(out, "Requires=unbounded-agent-regenerate-config@kube1.service"))
	require.Less(t, strings.Index(out, "After=unbounded-agent-regenerate-config@kube1.service"), strings.Index(out, "[Service]"))
}

func TestConfigRegenerationUnit(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "config-regeneration.service", defaultNSpawnTemplateData("kube1")))

	out := buf.String()
	require.Contains(t, out, "Description=Regenerate configuration for kube1")
	require.Contains(t, out, "Wants=systemd-udev-settle.service")
	require.Contains(t, out, "After=systemd-udev-settle.service")
	require.Contains(t, out, "Type=oneshot")
	require.Contains(t, out, "ExecStart=/usr/local/lib/unbounded-agent/nspawn-lifecycle-helper nspawn-lifecycle pre-start kube1")
	require.NotContains(t, out, "ExecStart=-")
	require.NotContains(t, out, "if [ ! -x")
	require.Contains(t, out, "Restart=on-failure")
	require.Contains(t, out, "RestartMode=direct")
}

func TestServiceOverride_NVIDIAReconcilesOnEveryStart(t *testing.T) {
	t.Parallel()

	data := defaultNSpawnTemplateData("kube1")

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data))
	require.Contains(t, buf.String(), "ExecStartPost=/usr/local/lib/unbounded-agent/nspawn-lifecycle-helper nspawn-lifecycle post-start kube1")
	require.NotContains(t, buf.String(), "ExecStartPost=-")
	require.NotContains(t, buf.String(), "if [ ! -x")
}

func TestServiceOverride_CPUNodesRunCommonPostStartHook(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", defaultNSpawnTemplateData("kube1")))
	require.Contains(t, buf.String(), "nspawn-lifecycle post-start")
}

func defaultNSpawnTemplateData(machineName string) nspawnTemplateData {
	return nspawnTemplateData{
		MachineName:                  machineName,
		BPFFSMountPath:               goalstates.BPFFSMountPath(machineName),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		ConfigRegenerationUnit:       goalstates.ConfigRegenerationUnit(machineName),
		AgentBinaryPath:              goalstates.NSpawnLifecycleBinaryPath,
	}
}

func nspawnRenderScenarioData() nspawnTemplateData {
	return defaultNSpawnTemplateData("kube1")
}

func requireRenderedGolden(t *testing.T, name string, data nspawnTemplateData) {
	t.Helper()

	for _, templateName := range []string{"nspawn.conf", "service-override.conf"} {
		var buf bytes.Buffer
		require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, templateName, data))
		requireGolden(t, filepath.Join("testdata", "render", name+"."+templateName+".golden"), buf.String())
	}
}

func requireGolden(t *testing.T, path, got string) {
	t.Helper()

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))

		return
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}

func requireRenderedSnapshot(t *testing.T, goldenFile, templateName string, data nspawnTemplateData) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, templateName, data))

	expected, err := os.ReadFile("testdata/" + goldenFile)
	require.NoError(t, err)
	require.Equal(t, string(expected), buf.String())

	if templateName == "service-override.conf" {
		requireBPFFSExecStartPreOrder(t, buf.String(), data.BPFFSMountPath)
	}

	return buf.String()
}

func requireBPFFSExecStartPreOrder(t *testing.T, out, bpffsPath string) {
	t.Helper()

	mkdir := "ExecStartPre=/usr/bin/mkdir -p " + bpffsPath
	mount := "ExecStartPre=/bin/sh -c '/usr/bin/mountpoint -q " + bpffsPath + " || /usr/bin/mount -t bpf bpf " + bpffsPath + "'"

	// systemd runs ExecStartPre entries in order; the bpffs mount depends on
	// the target directory existing and the entries being in the [Service] section.
	require.Contains(t, out, mkdir)
	require.Contains(t, out, mount)
	require.Less(t, strings.Index(out, "[Service]"), strings.Index(out, mkdir))
	require.Less(t, strings.Index(out, mkdir), strings.Index(out, mount))
}

// TestAdditionalHostMounts_ConfigToNSpawn exercises the full AdditionalHostMounts
// pipeline from a JSON agent config through to the rendered nspawn.conf content.
// It validates that:
//   - The AdditionalHostMounts field survives JSON round-trip unmarshalling.
//   - ValidateAdditionalHostMounts accepts the entries.
//   - ResolveMachine defaults an omitted Target to the Source path.
//   - The resolved mounts render to the correct Bind / BindReadOnly directives.
func TestAdditionalHostMounts_ConfigToNSpawn(t *testing.T) {
	t.Parallel()

	// Step 1: parse an AgentConfig with AdditionalHostMounts from JSON - the
	// same format a real operator would supply.
	const cfgJSON = `{
		"MachineName": "machine-1",
		"NodeName": "node-1",
		"AdditionalHostMounts": [
			{"Source": "/opt/config", "ReadOnly": true},
			{"Source": "/var/lib/data", "Target": "/data"}
		],
		"Cluster": {"CaCertBase64": "Y2EtYnl0ZXM=", "ClusterDNS": "10.0.0.10"},
		"Kubelet": {"ApiServer": "https://api.example.com"}
	}`

	var cfg config.AgentConfig
	require.NoError(t, json.Unmarshal([]byte(cfgJSON), &cfg))

	// Step 2: validate - both entries must pass path validation.
	require.NoError(t, config.ValidateAdditionalHostMounts(cfg.AdditionalHostMounts))

	// Step 3: resolve the goal state. ResolveMachine defaults an omitted
	// Target to the Source value; verify that invariant holds.
	log := slog.New(slog.DiscardHandler)
	gs, err := goalstates.ResolveMachine(log, &cfg, "machine-1", nil)
	require.NoError(t, err)

	require.Len(t, gs.RootFS.AdditionalHostMounts, 2)
	// First entry: no Target in JSON - must default to Source.
	require.Equal(t, "/opt/config", gs.RootFS.AdditionalHostMounts[0].Source)
	require.Equal(t, "/opt/config", gs.RootFS.AdditionalHostMounts[0].Target)
	require.True(t, gs.RootFS.AdditionalHostMounts[0].ReadOnly)
	// Second entry: explicit Target must be preserved as-is.
	require.Equal(t, "/var/lib/data", gs.RootFS.AdditionalHostMounts[1].Source)
	require.Equal(t, "/data", gs.RootFS.AdditionalHostMounts[1].Target)
	require.False(t, gs.RootFS.AdditionalHostMounts[1].ReadOnly)
	// The original config slice must not be mutated by resolution.
	require.Empty(t, cfg.AdditionalHostMounts[0].Target,
		"ResolveMachine must not mutate the caller's config slice")

	// Step 4: render the nspawn.conf template with the resolved mounts and
	// verify that the correct Bind / BindReadOnly directives are emitted.
	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "nspawn.conf", nspawnTemplateData{
		BPFFSMountPath:               goalstates.BPFFSMountPath("machine-1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		AdditionalHostMounts:         gs.RootFS.AdditionalHostMounts,
	}))

	out := buf.String()
	// Read-only mount must use BindReadOnly with source:target notation.
	require.Contains(t, out, "BindReadOnly=/opt/config:/opt/config")
	// Writable mount must use Bind with source:target notation.
	require.Contains(t, out, "Bind=/var/lib/data:/data")
	// The writable mount must not appear as a BindReadOnly entry.
	require.NotContains(t, out, "BindReadOnly=/var/lib/data")
}
