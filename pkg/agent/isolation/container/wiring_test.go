// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
	"github.com/Azure/unbounded/pkg/agent/nodelifecycle"
)

func nodeSpec(slot string) isolation.NodeSpec {
	return isolation.NodeSpec{Slot: slot, Version: "v1.34.0"}
}

func TestHostPathConstantsMatchGoalStates(t *testing.T) {
	require.Equal(t, goalstates.ContainerImageArchiveDir, ImageArchiveDir)
	require.Equal(t, goalstates.ContainerImageArchiveHostDir, ImageArchiveHostDir)
}

func TestSlotNamesMatchGoalStates(t *testing.T) {
	require.Equal(t, goalstates.BPFFSMountPath("kube2"), BPFFSPath("kube2"), "the same per-slot bpffs as nspawn, so reset removes either")
	require.Equal(t, goalstates.NSpawnMachineKube1, slotKube1)
	require.Equal(t, goalstates.NSpawnMachineKube2, slotKube2)
}

func TestRootMapsSamePathMountsToTheHost(t *testing.T) {
	r := Root{SlotName: "kube1", Dir: "/var/lib/unbounded/containers/kube1", HostRoot: "/h"}

	require.Equal(t, "kube1", r.Slot())
	require.Equal(t, "/var/lib/unbounded/containers/kube1", r.Path("/"))
	require.Equal(t, "/var/lib/unbounded/containers/kube1/etc/containerd/config.toml", r.Path("/etc/containerd/config.toml"))

	// Bind-mounted over the rootfs, so files must be written on the host side.
	require.Equal(t, "/h/var/lib/kubelet/config.yaml", r.Path("/var/lib/kubelet/config.yaml"))
	require.Equal(t, "/h/var/lib/kubelet", r.Path("/var/lib/kubelet"))
	require.Equal(t, "/h/opt/cni/bin", r.Path("opt/cni/bin"))
	require.Equal(t, "/h/etc/containerd/certs.d/_default/hosts.toml", r.Path("/etc/containerd/certs.d/_default/hosts.toml"))

	// A sibling with a shared prefix is not below the mount.
	require.Equal(t, "/var/lib/unbounded/containers/kube1/var/lib/kubelet-extra/x", r.Path("/var/lib/kubelet-extra/x"))
	require.Equal(t, "/var/lib/unbounded/containers/kube1/etc/containerd/certs.dx", r.Path("/etc/containerd/certs.dx"))

	// Traversal cannot escape the rootfs.
	require.Equal(t, "/var/lib/unbounded/containers/kube1/etc/passwd", r.Path("/../../etc/passwd"))

	prod := Root{SlotName: "kube2", Dir: "/d"}
	require.Equal(t, "/var/lib/kubelet/pki", prod.Path("/var/lib/kubelet/pki"), "an empty HostRoot is the real host")
}

func TestPrepareReturnsRootThatMapsSamePaths(t *testing.T) {
	b, _, dir := newBackend(t)

	r, err := b.Prepare(t.Context(), nodeSpec("kube1"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "host", "var/lib/kubelet/config.yaml"), r.Path("/var/lib/kubelet/config.yaml"))
	require.Equal(t, filepath.Join(dir, "containers", "kube1", "etc/x"), r.Path("/etc/x"))

	got, err := b.NodeRoot("kube1")
	require.NoError(t, err)
	require.Equal(t, r, got)

	_, err = b.NodeRoot("kube3")
	require.Error(t, err)
}

func TestExtraBindsAreRenderedOnlyWhenTheSourceExists(t *testing.T) {
	b, _, dir := newBackend(t)
	host := filepath.Join(dir, "host")

	_, err := b.Prepare(t.Context(), nodeSpec("kube1"))
	require.NoError(t, err)

	cfg, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.NotContains(t, string(cfg), ImageArchiveDir, "no staged archives, no bind")
	require.NotContains(t, string(cfg), KernelModulesDir)

	require.NoError(t, os.MkdirAll(filepath.Join(host, ImageArchiveHostDir), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(host, KernelModulesDir), 0o755))

	// The pre-start hook's path picks up sources that appeared after Prepare.
	require.NoError(t, b.Regenerate(t.Context(), "kube1"))

	cfg, err = os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), `"destination": "`+ImageArchiveDir+`"`)
	require.Contains(t, string(cfg), `"source": "`+ImageArchiveHostDir+`"`)
	require.Contains(t, string(cfg), `"destination": "`+KernelModulesDir+`"`)
}

func TestConfigExtraBindsGoldenAndValidation(t *testing.T) {
	got, err := RenderConfig(BundleInput{
		Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1", SELinux: SELinuxEnforcing,
		ExtraBinds: []ExtraBind{
			{Source: ImageArchiveHostDir, Target: ImageArchiveDir, ReadOnly: true},
			{Source: KernelModulesDir, Target: KernelModulesDir, ReadOnly: true},
		},
	})
	require.NoError(t, err)
	isolationtest.Golden(t, "config-extra-binds.json", got)

	_, err = RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", ExtraBinds: []ExtraBind{{Source: "rel", Target: "/x"}}})
	require.ErrorContains(t, err, "absolute")

	_, err = RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", ExtraBinds: []ExtraBind{{Source: "/a", Target: "/var/lib/kubelet"}}})
	require.ErrorContains(t, err, "conflicts")

	_, err = RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", ExtraBinds: []ExtraBind{{Source: "/a", Target: "/x"}, {Source: "/b", Target: "/x"}}})
	require.ErrorContains(t, err, "conflicts")
}

func TestUnitRequiresGolden(t *testing.T) {
	spec := UnitSpec{
		Slot: "kube2", RuntimePath: "/opt/unbounded/runtime/runc",
		BundleDir:  "/var/lib/unbounded/containers/bundles/kube2",
		HelperPath: "/opt/unbounded/agent/bin/unbounded-agent-lifecycle",
		After:      []string{"network-online.target"},
		Requires:   []string{"unbounded-localdns-network.service"},
	}
	got := RenderUnit(spec)
	isolationtest.Golden(t, "unbounded-node-kube2-localdns.service", got)

	require.Contains(t, string(got), "Requires=unbounded-localdns-network.service\n")

	var after string

	for _, line := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(line, "After=") {
			after = line
		}
	}

	require.Equal(t, 1, strings.Count(after, "unbounded-localdns-network.service"), "ordered after exactly once")
}

func TestPrepareWritesRequiresIntoTheUnit(t *testing.T) {
	b, _, _ := newBackend(t)
	b.UnitRequires = []string{"unbounded-localdns-network.service"}

	_, err := b.Prepare(t.Context(), nodeSpec("kube1"))
	require.NoError(t, err)

	unit, err := os.ReadFile(filepath.Join(b.SystemdDir, UnitName("kube1")))
	require.NoError(t, err)
	require.Contains(t, string(unit), "Requires=unbounded-localdns-network.service")
}

func TestRewriteUnitReloadsWithoutRestarting(t *testing.T) {
	b, st, _ := newBackend(t)
	_, err := b.Prepare(t.Context(), nodeSpec("kube1"))
	require.NoError(t, err)

	b.HelperPath = "/moved/unbounded-agent"

	st.reset()

	require.NoError(t, b.RewriteUnit(t.Context(), "kube1"))

	unit, err := os.ReadFile(filepath.Join(b.SystemdDir, UnitName("kube1")))
	require.NoError(t, err)
	require.Contains(t, string(unit), "/moved/unbounded-agent node-lifecycle pre-start kube1")
	require.Equal(t, []string{"systemctl daemon-reload"}, st.calls, "no restart or stop")

	require.Error(t, b.RewriteUnit(t.Context(), "kube9"))
}

func TestPreflightToleratesAMissingRuntimeOnlyWhenProvisioned(t *testing.T) {
	b, _, dir := newBackend(t)
	b.WritableCheck = func(...string) error { return nil }
	require.NoError(t, os.WriteFile(b.Host.CgroupControllers, []byte("cpu\n"), 0o644))
	require.NoError(t, os.WriteFile(b.Host.SELinuxContext, []byte("x"), 0o644))

	require.ErrorContains(t, b.Preflight(), "container runtime")

	b.RuntimeProvisioned = true
	require.NoError(t, b.Preflight(), "the agent installs the runtime after preflight")

	// A present but non-executable runtime is still wrong.
	b.RuntimePath = filepath.Join(dir, "runc")
	require.NoError(t, os.WriteFile(b.RuntimePath, []byte("x"), 0o644))
	require.ErrorContains(t, b.Preflight(), "not executable")

	// A missing cgroup v2 is still wrong.
	b.RuntimePath = rt
	require.NoError(t, os.Remove(b.Host.CgroupControllers))
	require.ErrorContains(t, b.Preflight(), "cgroup v2")
}

func TestDeviceClassGrantNeedsNoRule(t *testing.T) {
	got, err := RenderConfig(BundleInput{
		Slot: "kube1", RootfsDir: "/r",
		Grants: []isolation.Grant{{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: "char-input"}},
	})
	require.NoError(t, err)
	require.NotContains(t, string(got), "char-input")

	// A device grant with neither a node nor a class is still an error.
	_, err = RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantDevice}}})
	require.ErrorContains(t, err, "absolute path")
}

func TestDeviceGrantOfADirectoryBecomesAMount(t *testing.T) {
	b, _, dir := newBackend(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "host", "dev/dri"), 0o755))

	grants := []isolation.Grant{{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: "/dev/dri"}}
	require.NoError(t, b.RenderBundle("kube1", grants))
	require.Equal(t, isolation.GrantDevice, grants[0].Kind, "the caller's grants are not modified")

	cfg, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), `"destination": "/dev/dri"`)
	require.Contains(t, string(cfg), `"type": "bind"`)
}

func TestRuntimeStatusIgnoresTheUnitState(t *testing.T) {
	b, st, _ := newBackend(t)
	r := isolation.SlotName("kube1")

	// While ExecStartPost runs the unit is activating, so is-active fails; the
	// runtime still reports running, which is what the hook must wait for.
	st.fail["systemctl is-active"] = errors.New("activating")
	st.out[rt+" state"] = `{"status":"running"}`

	s, err := b.Status(t.Context(), r)
	require.NoError(t, err)
	require.False(t, s.Running)

	s, err = b.RuntimeStatus(t.Context(), r)
	require.NoError(t, err)
	require.True(t, s.Running)

	st.out[rt+" state"] = `{"status":"created"}`
	s, _ = b.RuntimeStatus(t.Context(), r)
	require.False(t, s.Running)
	require.Contains(t, s.Detail, "created")

	_, err = b.RuntimeStatus(t.Context(), isolation.SlotName("x"))
	require.Error(t, err)
}

func TestDestroyOfASlotWithoutAUnitSkipsSystemd(t *testing.T) {
	b, st, _ := newBackend(t)

	// An interrupted Prepare: the rootfs exists, the unit was never written.
	require.NoError(t, os.MkdirAll(filepath.Join(b.Layout.RootfsDir("kube2"), "etc"), 0o755))

	st.fail["systemctl"] = errors.New("Unit unbounded-node-kube2.service not loaded")

	require.NoError(t, b.Destroy(t.Context(), isolation.SlotName("kube2")))
	require.NoDirExists(t, b.Layout.RootfsDir("kube2"))

	for _, c := range st.calls {
		require.NotContains(t, c, "systemctl", "systemd is not asked about a unit that does not exist")
	}

	require.Contains(t, st.calls, rt+" delete --force unbounded-kube2", "stale runtime state is still cleared")
}

func TestRegenerateRefreshesGrantsAndRecordsThem(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	config := func() string {
		d, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
		require.NoError(t, err)

		return string(d)
	}

	require.Contains(t, config(), "/dev/nvidia0")

	// The refresh drops the device and keeps the rest.
	var seen []isolation.Grant

	b.RefreshGrants = func(_ context.Context, recorded []isolation.Grant) ([]isolation.Grant, error) {
		seen = recorded

		var out []isolation.Grant

		for _, g := range recorded {
			if g.Source != "/dev/nvidia0" {
				out = append(out, g)
			}
		}

		return out, nil
	}

	require.NoError(t, b.Regenerate(ctx, "kube1"))
	require.Equal(t, testGrants, seen, "the refresh sees what Prepare recorded")
	require.NotContains(t, config(), "/dev/nvidia0")
	require.Contains(t, config(), "/models", "the other grants stay")

	// The next start begins from the refreshed grants.
	b.RefreshGrants = func(_ context.Context, recorded []isolation.Grant) ([]isolation.Grant, error) {
		require.Len(t, recorded, len(testGrants)-1)

		return recorded, nil
	}

	require.NoError(t, b.Regenerate(ctx, "kube1"))
	require.NotContains(t, config(), "/dev/nvidia0")
}

func TestRegenerateKeepsTheBundleWhenTheRefreshFails(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	before, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)

	b.RefreshGrants = func(context.Context, []isolation.Grant) ([]isolation.Grant, error) {
		return nil, errors.New("driver state incomplete")
	}

	require.ErrorContains(t, b.Regenerate(ctx, "kube1"), "driver state incomplete")

	after, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestRegenerateWithoutARefreshIsUnchanged(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	before, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)

	require.NoError(t, b.Regenerate(ctx, "kube1"))

	after, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestUnescapeMountinfoDecodesEveryOctalEscape(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		`/plain/path`:        "/plain/path",
		`/with\040space`:     "/with space",
		`/tab\011here`:       "/tab\there",
		`/new\012line`:       "/new\nline",
		`/back\134slash`:     `/back\slash`,
		`/trailing\`:         `/trailing\`,
		`/short\04`:          `/short\04`,
		`/not\octal`:         `/not\octal`,
		`/two\040\040spaces`: "/two  spaces",
	} {
		require.Equal(t, want, unescapeMountinfo(in), in)
	}
}

func TestMountsBelowFindsMountsWithEscapedPaths(t *testing.T) {
	t.Parallel()

	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := strings.Join([]string{
		`36 35 98:0 / /var/lib/unbounded/containers/kube1/rootfs/a\040b rw - tmpfs tmpfs rw`,
		`37 35 98:0 / /var/lib/unbounded/containers/kube1/rootfs/c\134d rw - tmpfs tmpfs rw`,
		`38 35 98:0 / /var/lib/unbounded/containers/kube10/rootfs rw - tmpfs tmpfs rw`,
		`39 35 98:0 / /elsewhere rw - tmpfs tmpfs rw`,
	}, "\n")
	require.NoError(t, os.WriteFile(info, []byte(lines), 0o600))

	got, err := mountsBelow(info, "/var/lib/unbounded/containers/kube1")
	require.NoError(t, err)
	require.Equal(t, []string{
		"/var/lib/unbounded/containers/kube1/rootfs/a b",
		`/var/lib/unbounded/containers/kube1/rootfs/c\d`,
	}, got, "a sibling slot with a shared name prefix is not below the directory")
}

// A host reboot empties /run, and runc fails the start with "open
// /run/containerd: no such file or directory" when the bind source is missing,
// which e2e showed leaves the unit restarting forever.
func TestRegenerateRecreatesTheSamePathSources(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	for _, s := range SamePathMounts {
		require.NoError(t, os.RemoveAll(filepath.Join(b.SourceRoot, s.Path)))
	}

	require.NoError(t, b.Regenerate(ctx, "kube1"))

	for _, s := range SamePathMounts {
		require.DirExists(t, filepath.Join(b.SourceRoot, s.Path))
	}
}

// An empty /var/lib/unbounded/containers left by a destroyed slot makes the
// next bootstrap refuse the host as an existing deployment, which e2e showed
// leaves a reprovision to container failing until it gives up.
func TestDestroyOfTheLastSlotLeavesNoLayoutDirectories(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	r1, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	r2, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube2", Grants: testGrants})
	require.NoError(t, err)

	require.NoError(t, b.Destroy(ctx, r2))
	require.DirExists(t, b.Layout.RootfsDir("kube1"), "the other slot is untouched")
	require.DirExists(t, b.Layout.BundleDir("kube1"))
	require.NoDirExists(t, b.Layout.RootfsDir("kube2"))

	require.NoError(t, b.Destroy(ctx, r1))
	require.NoDirExists(t, b.Layout.BundleDir(""))
	require.NoDirExists(t, b.Layout.RootfsDir(""))
}

// A container that is still running after a failed stop owns its rootfs; the
// host mount table cannot show the binds inside its mount namespace, so the
// rootfs and bundle are kept and the stop error is returned.
func TestDestroyKeepsTheRootfsWhileAFailedStopLeavesTheContainerAlive(t *testing.T) {
	for _, state := range []string{`{"status":"running"}`, `{"status":"paused"}`, `{"status":"created"}`, "not json"} {
		b, st, _ := newBackend(t)
		ctx := context.Background()

		r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
		require.NoError(t, err)

		st.fail["systemctl stop"] = errors.New("stuck")
		st.out[rt+" state"] = state

		err = b.Destroy(ctx, r)
		require.ErrorContains(t, err, "stuck", state)
		require.ErrorContains(t, err, "still", state)
		require.DirExists(t, b.Layout.RootfsDir("kube1"), state)
		require.DirExists(t, b.Layout.BundleDir("kube1"), state)
	}
}

// A stopped container, or none at all, is no reason to keep the files even
// when the stop request itself failed.
func TestDestroyRemovesTheRootfsWhenTheContainerIsGoneAfterAFailedStop(t *testing.T) {
	for name, set := range map[string]func(*stub){
		"stopped":   func(st *stub) { st.out[rt+" state"] = `{"status":"stopped"}` },
		"not found": func(st *stub) { st.fail[rt+" state"] = errors.New("container does not exist") },
	} {
		b, st, _ := newBackend(t)
		ctx := context.Background()

		r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
		require.NoError(t, err)

		st.fail["systemctl stop"] = errors.New("stuck")
		set(st)

		require.ErrorContains(t, b.Destroy(ctx, r), "stuck", name)
		require.NoDirExists(t, b.Layout.RootfsDir("kube1"), name)
	}
}

// Restart=always keeps the node coming back, but a start that can never
// succeed (a GPU hook that keeps failing) must not loop forever: the unit has
// a start limit, in the [Unit] section, and its start timeout covers the
// post-start hook's waits. A missing runtime must also not make systemd skip
// the unit and report success.
func TestUnitStartLimitTimeoutAndNoRuntimeCondition(t *testing.T) {
	unit := string(RenderUnit(UnitSpec{Slot: "kube1", RuntimePath: rt, BundleDir: "/b"}))

	require.NotContains(t, unit, "ConditionPathExists")

	unitSection, serviceSection, found := strings.Cut(unit, "[Service]")
	require.True(t, found)
	require.Contains(t, unitSection, "StartLimitIntervalSec=3600\n", "it is a [Unit] setting")
	require.Contains(t, unitSection, "StartLimitBurst=10\n")
	require.NotContains(t, serviceSection, "StartLimitIntervalSec")

	require.Greater(t, StartTimeout, nodelifecycle.PostStartBudget, "the start timeout leaves room after the post-start waits")
	require.Contains(t, serviceSection, "TimeoutStartSec=360\n")
}

// The two slots' units conflict, and the container's cgroup is inside the
// unit's, with runc in its own subgroup.
func TestUnitConflictsWithTheOtherSlotAndDelegatesItsCgroup(t *testing.T) {
	for slot, other := range map[string]string{"kube1": "kube2", "kube2": "kube1"} {
		unit := string(RenderUnit(UnitSpec{Slot: slot, RuntimePath: rt, BundleDir: "/b"}))

		require.Contains(t, unit, "Conflicts="+UnitName(other)+"\n")
		require.Contains(t, unit, "Slice=system.slice\n")
		require.Contains(t, unit, "Delegate=yes\nDelegateSubgroup=supervisor\n")
		require.Contains(t, unit, "ExecStartPre=/usr/bin/mkdir -p /run/bpffs/"+slot+"\n")
		require.Contains(t, unit, "mount -t bpf bpf /run/bpffs/"+slot)
		require.Equal(t, "/system.slice/"+UnitName(slot)+"/payload", CgroupPath(slot))

		// The host's kubelet directory is a shared mount before runc starts,
		// and that runs before the container, at every start.
		share := strings.Index(unit, "ExecStartPre=/usr/bin/mount --make-rshared /var/lib/kubelet\n")
		require.Positive(t, share)
		require.Contains(t, unit, "mountpoint -q /var/lib/kubelet || /usr/bin/mount --bind /var/lib/kubelet /var/lib/kubelet")
		require.Less(t, share, strings.Index(unit, "ExecStart="))
	}
}

func TestStartFailsClearlyWhenTheRuntimeIsMissing(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)
	st.reset()

	var checked []string

	b.CheckRuntime = func(path string) error {
		checked = append(checked, path)

		return os.ErrNotExist
	}

	err = b.Start(ctx, r)
	require.ErrorContains(t, err, "container runtime "+rt+" is not installed")
	require.Equal(t, []string{rt}, checked)
	require.Empty(t, st.calls, "nothing is enabled or started without a runtime")

	b.CheckRuntime = func(string) error { return nil }

	require.NoError(t, b.Start(ctx, r))
	require.Contains(t, st.calls, "systemctl enable --now "+UnitName("kube1"))

	b.CheckRuntime = nil

	require.NoError(t, b.Start(ctx, r), "no check configured means no check")
}
