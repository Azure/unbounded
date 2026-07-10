// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestServiceOverride_RenderedSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "service-override-kube1.conf.golden", "service-override.conf", nspawnTemplateData{
		MachineName:    "kube1",
		BPFFSMountPath: goalstates.BPFFSMountPath("kube1"),
	})
}

func TestServiceOverride_MachineNameSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "service-override-kube2.conf.golden", "service-override.conf", nspawnTemplateData{
		MachineName:    "kube2",
		BPFFSMountPath: goalstates.BPFFSMountPath("kube2"),
	})
}

func TestNSpawnConfig_RenderedSnapshot(t *testing.T) {
	t.Parallel()

	requireRenderedSnapshot(t, "nspawn.conf.golden", "nspawn.conf", nspawnTemplateData{
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
	})
}

func TestServiceOverride_HostDevicesDeviceAllow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", nspawnTemplateData{
		MachineName:     "kube1",
		BPFFSMountPath:  goalstates.BPFFSMountPath("kube1"),
		HostDevicePaths: []string{"/dev/kvm"},
	}))

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
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", nspawnTemplateData{
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		HostDevicePaths:              devices,
	}))

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", nspawnTemplateData{
		MachineName:     "kube1",
		BPFFSMountPath:  goalstates.BPFFSMountPath("kube1"),
		HostDevicePaths: devices,
	}))

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
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&nspawnBuf, "nspawn.conf", nspawnTemplateData{
		BPFFSMountPath:               goalstates.BPFFSMountPath("kube1"),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		AMDGPUDevicePaths:            devices,
		AMDSysFSPaths:                []string{"/sys/module/amdgpu", "/sys/class/kfd"},
	}))

	var overrideBuf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&overrideBuf, "service-override.conf", nspawnTemplateData{
		MachineName:       "kube1",
		BPFFSMountPath:    goalstates.BPFFSMountPath("kube1"),
		AMDGPUDevicePaths: devices,
	}))

	for _, dev := range devices {
		require.Contains(t, nspawnBuf.String(), "Bind="+dev)
		require.Contains(t, overrideBuf.String(), "DeviceAllow="+dev+" rwm")
	}

	require.Contains(t, nspawnBuf.String(), "AMD GPU support")
	require.Contains(t, overrideBuf.String(), "AMD GPU support")
	require.Contains(t, nspawnBuf.String(), "BindReadOnly=/sys/module/amdgpu")
	require.Contains(t, nspawnBuf.String(), "BindReadOnly=/sys/class/kfd")
}

func TestPathsExcluding(t *testing.T) {
	t.Parallel()

	got := pathsExcluding(
		[]string{"/dev/dri/card0", "/dev/dri/renderD128", "/dev/kfd"},
		[]string{"/dev/dri/card0", "/dev/dri/renderD128"},
	)

	require.Equal(t, []string{"/dev/kfd"}, got)
}

func TestServiceOverride_NoHostDevicesNoDeviceAllow(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", nspawnTemplateData{
		MachineName:    "kube1",
		BPFFSMountPath: goalstates.BPFFSMountPath("kube1"),
		// No HostDevicePaths and no GPU devices.
	}))

	// With no devices the drop-in must not contain any DeviceAllow lines,
	// which is what keeps the existing golden snapshots unchanged.
	require.NotContains(t, buf.String(), "DeviceAllow=")
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
