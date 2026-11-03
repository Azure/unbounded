// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

// uninstallHarness runs the uninstall script against a temporary root with
// recording stubs for the host commands it calls.
type uninstallHarness struct {
	t    *testing.T
	root string
	bin  string
	log  string
}

func newUninstallHarness(t *testing.T) *uninstallHarness {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	dir := t.TempDir()
	h := &uninstallHarness{
		t:    t,
		root: filepath.Join(dir, "root"),
		bin:  filepath.Join(dir, "bin"),
		log:  filepath.Join(dir, "calls.log"),
	}

	require.NoError(t, os.MkdirAll(h.bin, 0o755))
	require.NoError(t, os.MkdirAll(h.root, 0o755))

	// id reports root. ip fails so the policy-rule loops terminate. machinectl
	// show fails, meaning no machine is running.
	h.stub("id", "echo 0")
	h.stub("ip", `echo "ip $*" >> "$STUB_LOG"; exit 1`)
	h.stub("machinectl", `echo "machinectl $*" >> "$STUB_LOG"; [ "$1" = show ] && exit 1; exit 0`)

	for _, name := range []string{"systemctl", "sysctl", "swapon"} {
		h.stub(name, `echo "`+name+` $*" >> "$STUB_LOG"; exit 0`)
	}

	h.stub("sleep", "exit 0")

	return h
}

func (h *uninstallHarness) stub(name, body string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755))
}

func (h *uninstallHarness) path(rel string) string {
	return filepath.Join(h.root, rel)
}

func (h *uninstallHarness) touch(rel string) {
	h.t.Helper()
	require.NoError(h.t, os.MkdirAll(filepath.Dir(h.path(rel)), 0o755))
	require.NoError(h.t, os.WriteFile(h.path(rel), []byte("x"), 0o644))
}

func (h *uninstallHarness) writeApplied(slot, json string) {
	h.t.Helper()
	h.touch("etc/unbounded/agent/" + slot + "-applied-config.json")
	require.NoError(h.t, os.WriteFile(h.path("etc/unbounded/agent/"+slot+"-applied-config.json"), []byte(json), 0o644))
}

func (h *uninstallHarness) run() (string, string) {
	h.t.Helper()

	script := filepath.Join(h.t.TempDir(), "uninstall.sh")
	require.NoError(h.t, os.WriteFile(script, []byte(UnboundedAgentUninstallScript("kube1")), 0o755))

	cmd := exec.Command("bash", script)
	cmd.Env = []string{
		"PATH=" + h.bin + ":" + os.Getenv("PATH"),
		"UNBOUNDED_UNINSTALL_ROOT=" + h.root,
		"STUB_LOG=" + h.log,
	}

	out, err := cmd.CombinedOutput()
	require.NoError(h.t, err, string(out))

	calls, _ := os.ReadFile(h.log) //nolint:errcheck // absent when nothing was called

	return string(out), string(calls)
}

func (h *uninstallHarness) requireGone(rel ...string) {
	h.t.Helper()

	for _, r := range rel {
		_, err := os.Lstat(h.path(r))
		require.True(h.t, os.IsNotExist(err), "%s should be removed", r)
	}
}

func (h *uninstallHarness) requireExists(rel ...string) {
	h.t.Helper()

	for _, r := range rel {
		_, err := os.Lstat(h.path(r))
		require.NoError(h.t, err, "%s should exist", r)
	}
}

func TestUninstallScriptNSpawnNoRecordedMode(t *testing.T) {
	t.Parallel()

	// Legacy host: applied config exists but has no Isolation key.
	h := newUninstallHarness(t)
	h.writeApplied("kube1", "{\n  \"MachineName\": \"kube1\"\n}\n")
	h.touch("etc/systemd/nspawn/kube1.nspawn")
	h.touch("etc/systemd/system/systemd-nspawn@kube1.service.d/override.conf")
	h.touch("var/lib/machines/kube1/etc/os-release")
	h.touch("opt/unbounded/node/current/keep")
	h.touch("etc/systemd/system/unbounded-kubelet.service")

	out, calls := h.run()

	require.Contains(t, out, "Isolation mode: nspawn")
	require.Contains(t, calls, "machinectl show kube1")
	h.requireGone(
		"etc/systemd/nspawn/kube1.nspawn",
		"etc/systemd/system/systemd-nspawn@kube1.service.d",
		"var/lib/machines/kube1",
		"etc/unbounded/agent",
	)
	// The nspawn path must not touch host-native or container locations.
	h.requireExists("opt/unbounded/node/current/keep", "etc/systemd/system/unbounded-kubelet.service")
	require.NotContains(t, calls, "unbounded-kubelet.service")
}

func TestUninstallScriptNoAppliedConfigFallsBackToNSpawn(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.touch("etc/systemd/nspawn/kube1.nspawn")
	h.touch("var/lib/machines/kube1/etc/os-release")

	out, _ := h.run()

	require.Contains(t, out, "Isolation mode: nspawn")
	h.requireGone("etc/systemd/nspawn/kube1.nspawn", "var/lib/machines/kube1")
}

func TestUninstallScriptExplicitNSpawn(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.writeApplied("kube2", `{"Isolation": "nspawn"}`)
	h.touch("var/lib/machines/kube1/etc/os-release")

	out, _ := h.run()

	require.Contains(t, out, "Isolation mode: nspawn")
	h.requireGone("var/lib/machines/kube1")
}

func TestUninstallScriptNone(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.writeApplied("kube1", `{"MachineName":"kube1","Isolation":"none"}`)
	h.touch("etc/systemd/system/unbounded-containerd.service")
	h.touch("etc/systemd/system/unbounded-kubelet.service")
	h.touch("etc/systemd/system/unbounded-localdns.service")
	h.touch("etc/systemd/system/unbounded-localdns-network.service")
	h.touch("etc/systemd/system/unbounded-kubelet.service.d/00-path.conf")
	h.touch("etc/systemd/system/unbounded-kubelet.service.d/05-localdns.conf")
	h.touch("etc/systemd/system/unbounded-containerd.service.d/00-path.conf")
	h.touch("etc/systemd/system/unbounded-localdns.service.d/00-path.conf")
	h.touch("etc/cdi/nvidia.yaml")
	h.touch("etc/cdi/other-vendor.yaml")
	h.touch("etc/systemd/system/unrelated.service")
	h.touch("etc/systemd/system/unrelated.service.d/x.conf")
	h.touch("opt/unbounded/node/v1.34.0/bin/kubelet")
	h.touch("var/lib/machines/kube1/etc/os-release")

	out, calls := h.run()

	require.Contains(t, out, "Isolation mode: none")
	require.Contains(t, calls, "systemctl disable --now unbounded-kubelet.service")
	require.Contains(t, calls, "systemctl disable --now unbounded-containerd.service")
	require.Contains(t, calls, "systemctl disable --now unbounded-localdns.service")
	require.Contains(t, calls, "systemctl disable --now unbounded-localdns-network.service")
	require.NotContains(t, calls, "machinectl")
	require.Less(t, strings.Index(calls, "stop unbounded-kubelet.service"), strings.Index(calls, "stop unbounded-containerd.service"))
	h.requireGone(
		"etc/systemd/system/unbounded-containerd.service",
		"etc/systemd/system/unbounded-kubelet.service",
		"etc/systemd/system/unbounded-localdns.service",
		"etc/systemd/system/unbounded-localdns-network.service",
		"etc/systemd/system/unbounded-kubelet.service.d",
		"etc/systemd/system/unbounded-containerd.service.d",
		"etc/systemd/system/unbounded-localdns.service.d",
		"etc/cdi/nvidia.yaml",
		"opt/unbounded/node",
		"etc/unbounded/agent",
	)
	h.requireExists(
		"etc/systemd/system/unrelated.service",
		"etc/systemd/system/unrelated.service.d/x.conf",
		"etc/cdi/other-vendor.yaml",
		"var/lib/machines/kube1/etc/os-release",
	)
}

func TestUninstallScriptContainer(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.writeApplied("kube2", `{"Isolation":"container"}`)
	h.touch("etc/systemd/system/" + container.UnitName("kube2"))
	h.touch("var/lib/unbounded/containers/kube2/rootfs/x")
	h.touch("var/lib/unbounded/containers/kube1/rootfs/x")
	h.touch("etc/systemd/system/unrelated.service")

	out, calls := h.run()

	require.Contains(t, out, "Isolation mode: container")
	require.Contains(t, calls, "systemctl disable --now "+container.UnitName("kube2"))
	require.NotContains(t, calls, "machinectl")
	h.requireGone(
		"etc/systemd/system/"+container.UnitName("kube2"),
		"var/lib/unbounded/containers/kube2",
		"etc/unbounded/agent",
	)
	h.requireExists("etc/systemd/system/unrelated.service", "var/lib/unbounded/containers/kube1/rootfs/x")
}

func TestUninstallScriptUnknownModeFails(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.writeApplied("kube1", `{"Isolation":"microvm"}`)
	h.touch("var/lib/machines/kube1/etc/os-release")

	script := filepath.Join(t.TempDir(), "uninstall.sh")
	require.NoError(t, os.WriteFile(script, []byte(UnboundedAgentUninstallScript("kube1")), 0o755))

	cmd := exec.Command("bash", script)
	cmd.Env = []string{"PATH=" + h.bin + ":" + os.Getenv("PATH"), "UNBOUNDED_UNINSTALL_ROOT=" + h.root, "STUB_LOG=" + h.log}

	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), `unsupported isolation mode "microvm"`)
	// Nothing is deleted when the mode is not understood.
	h.requireExists("var/lib/machines/kube1/etc/os-release", "etc/unbounded/agent/kube1-applied-config.json")
}

func TestUninstallScriptIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newUninstallHarness(t)
	h.writeApplied("kube1", `{"Isolation":"none"}`)

	h.run()
	h.run()
}
