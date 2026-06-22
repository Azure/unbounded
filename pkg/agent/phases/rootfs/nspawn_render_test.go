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
		BPFFSMountPath: goalstates.BPFFSMountPath("kube1"),
	})
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
		requireFullDeviceAccess(t, buf.String())
	} else if templateName == "nspawn.conf" {
		requireFullDevPassthrough(t, buf.String())
	}

	return buf.String()
}

func requireFullDeviceAccess(t *testing.T, out string) {
	t.Helper()

	require.Contains(t, out, "DeviceAllow=\n")
	require.Contains(t, out, "DevicePolicy=auto\n")
	require.Contains(t, out, "TasksMax=infinity\n")
}

func requireFullDevPassthrough(t *testing.T, out string) {
	t.Helper()

	require.Contains(t, out, "Bind=/dev:/dev:rbind\n")
	require.Contains(t, out, "BindReadOnly=/run/udev\n")
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
