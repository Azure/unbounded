// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/stretchr/testify/require"
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
