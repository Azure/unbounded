// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

var testGrants = []isolation.Grant{
	{Kind: isolation.GrantMount, Source: "/data/models", Target: "/models", ReadOnly: true},
	{Kind: isolation.GrantMount, Source: "/srv/scratch"},
	{Kind: isolation.GrantDevice, Source: "/dev/nvidia0"},
	{Kind: isolation.GrantLibraryPath, Source: "/opt/nvidia/lib64"},
}

var testDevices = map[string]DeviceInfo{"/dev/nvidia0": {Type: "c", Major: 195, Minor: 0, Mode: 0o666}}

func TestConfigGolden(t *testing.T) {
	for name, in := range map[string]BundleInput{
		"config-enforcing.json":  {Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1", SELinux: SELinuxEnforcing},
		"config-permissive.json": {Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1", SELinux: SELinuxPermissive},
		"config-grants.json": {
			Slot: "kube2", RootfsDir: "/var/lib/unbounded/containers/kube2", SELinux: SELinuxEnforcing,
			Grants: testGrants, Devices: testDevices,
		},
	} {
		got, err := RenderConfig(in)
		require.NoError(t, err)
		isolationtest.Golden(t, name, got)
		require.True(t, json.Valid(got))
	}
}

func TestUnitGolden(t *testing.T) {
	spec := UnitSpec{
		Slot: "kube1", RuntimePath: "/opt/unbounded/runtime/runc",
		BundleDir:  "/var/lib/unbounded/containers/bundles/kube1",
		HelperPath: "/usr/local/bin/unbounded-agent", After: []string{"network-online.target"},
	}
	isolationtest.Golden(t, "unbounded-node-kube1.service", RenderUnit(spec))

	spec.HelperPath = ""
	isolationtest.Golden(t, "unbounded-node-kube1-nohooks.service", RenderUnit(spec))
}

func TestConfigSELinuxLabel(t *testing.T) {
	label := func(m SELinuxMode) string {
		b, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", SELinux: m})
		require.NoError(t, err)

		var s spec
		require.NoError(t, json.Unmarshal(b, &s))

		return s.Process.SELinuxLabel
	}

	require.Equal(t, SELinuxLabel, label(SELinuxEnforcing))
	require.Contains(t, SELinuxLabel, "spc_t")
	require.Empty(t, label(SELinuxPermissive))
	require.Empty(t, label(SELinuxDisabled))
}

func TestConfigSamePathContract(t *testing.T) {
	b, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r"})
	require.NoError(t, err)

	var s spec
	require.NoError(t, json.Unmarshal(b, &s))

	byDest := map[string]mount{}
	for _, m := range s.Mounts {
		byDest[m.Destination] = m
	}

	for _, p := range []string{"/run/containerd", "/etc/containerd/certs.d", "/var/lib/kubelet", "/opt/cni"} {
		m, ok := byDest[p]
		require.True(t, ok, p)
		require.Equal(t, p, m.Source, "same path on both sides")
		require.Contains(t, m.Options, "rbind")
	}

	// Pod mounts made in the container must reach the host.
	require.Contains(t, byDest["/var/lib/kubelet"].Options, "rshared")
	require.NotContains(t, byDest["/run/containerd"].Options, "rshared")
	require.Equal(t, "rshared", s.Linux.RootfsPropagation)

	// Each slot has its own bpffs, as each nspawn machine does.
	require.Equal(t, "/run/bpffs/kube1", byDest["/sys/fs/bpf"].Source)

	// The container runs inside its unit's cgroup.
	require.Equal(t, "/system.slice/unbounded-node-kube1.service/payload", s.Linux.CgroupsPath)

	for _, ns := range s.Linux.Namespaces {
		require.NotEqual(t, "network", ns.Type, "host network namespace")
	}
}

// The host's /sys and /proc/sys are read-only in the container except
// /proc/sys/net, which the shared network namespace's CNI needs, and the
// default kernel interfaces are masked.
func TestConfigProtectsKernelInterfaces(t *testing.T) {
	b, err := RenderConfig(BundleInput{Slot: "kube2", RootfsDir: "/r"})
	require.NoError(t, err)

	var s spec
	require.NoError(t, json.Unmarshal(b, &s))

	for _, m := range s.Mounts {
		switch m.Destination {
		case "/sys":
			require.Contains(t, m.Options, "ro")
			require.NotContains(t, m.Options, "rw")
		case "/sys/fs/cgroup":
			require.Contains(t, m.Options, "rw", "systemd and the kubelet manage cgroups")
		}
	}

	for _, p := range []string{"/proc/sys/kernel", "/proc/sys/vm", "/proc/sys/fs", "/proc/sysrq-trigger"} {
		require.Contains(t, s.Linux.ReadonlyPaths, p)
	}

	for _, p := range s.Linux.ReadonlyPaths {
		require.NotEqual(t, "/proc/sys", p, "that would make /proc/sys/net read-only too")
		require.False(t, strings.HasPrefix(p, "/proc/sys/net"), p)
	}

	for _, p := range []string{"/proc/kcore", "/proc/keys", "/sys/firmware"} {
		require.Contains(t, s.Linux.MaskedPaths, p)
	}

	require.Equal(t, "/run/bpffs/kube2", func() string {
		for _, m := range s.Mounts {
			if m.Destination == "/sys/fs/bpf" {
				return m.Source
			}
		}

		return ""
	}())
}

func TestConfigRefusesAnInvalidSlot(t *testing.T) {
	_, err := RenderConfig(BundleInput{RootfsDir: "/r"})
	require.ErrorContains(t, err, "invalid container slot")
}

func TestConfigGrantsBecomeMountsDevicesRules(t *testing.T) {
	b, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: testGrants, Devices: testDevices})
	require.NoError(t, err)

	var s spec
	require.NoError(t, json.Unmarshal(b, &s))

	byDest := map[string]mount{}
	for _, m := range s.Mounts {
		byDest[m.Destination] = m
	}

	require.Equal(t, "/data/models", byDest["/models"].Source)
	require.Contains(t, byDest["/models"].Options, "ro")
	require.Equal(t, "/srv/scratch", byDest["/srv/scratch"].Source)
	require.Contains(t, byDest["/srv/scratch"].Options, "rw")
	require.Contains(t, byDest["/opt/nvidia/lib64"].Options, "ro")
	require.Contains(t, s.Process.Env, "LD_LIBRARY_PATH=/opt/nvidia/lib64")

	// The baseline nodes come first, then the granted ones.
	require.Len(t, s.Linux.Devices, len(baselineDevices)+1)
	require.Equal(t, device{Path: "/dev/nvidia0", Type: "c", Major: 195, Minor: 0, FileMode: 0o666}, s.Linux.Devices[len(baselineDevices)])

	var found bool

	for _, r := range s.Linux.Resources.Devices {
		if r.Major != nil && *r.Major == 195 && r.Minor != nil && *r.Minor == 0 && r.Allow {
			found = true
		}
	}

	require.True(t, found, "cgroup device rule for the granted device")
}

// A device directory is bound whole and writable, so device nodes the host
// creates in it later appear in the container, and the allow-all baseline makes
// them usable. A node it allows that exists at render time also gets an explicit
// rule, as a device grant does; a device class gets none, as a class grant
// does not.
func TestConfigDeviceDirectoryIsBoundWhole(t *testing.T) {
	t.Parallel()

	rdmaCM := DeviceInfo{Type: "c", Major: 10, Minor: 58, Mode: 0o666}
	b, err := RenderConfig(BundleInput{
		Slot:      "kube1",
		RootfsDir: "/r",
		Grants: []isolation.Grant{{
			Kind:        isolation.GrantMount,
			Purpose:     isolation.PurposeHostMount,
			Source:      "/dev/infiniband",
			Target:      "/dev/infiniband",
			DeviceAllow: []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs9"},
		}},
		Devices: map[string]DeviceInfo{"/dev/infiniband/rdma_cm": rdmaCM},
	})
	require.NoError(t, err)

	var s spec
	require.NoError(t, json.Unmarshal(b, &s))

	var bound *mount

	for i := range s.Mounts {
		if s.Mounts[i].Destination == "/dev/infiniband" {
			bound = &s.Mounts[i]
		}
	}

	require.NotNil(t, bound, "the device directory is bound")
	require.Equal(t, "/dev/infiniband", bound.Source)
	require.Equal(t, []string{"rbind", "rw"}, bound.Options)

	// No device node is created for the directory or its contents: the bind
	// exposes them.
	require.Len(t, s.Linux.Devices, len(baselineDevices))

	require.Equal(t, deviceRule{Allow: true, Access: "rwm"}, s.Linux.Resources.Devices[0], "every device is allowed")

	explicit := s.Linux.Resources.Devices[1+len(baselineDevices):]
	require.Len(t, explicit, 1, "only the node that exists gets its own rule")
	require.Equal(t, "c", explicit[0].Type)
	require.EqualValues(t, 10, *explicit[0].Major)
	require.EqualValues(t, 58, *explicit[0].Minor)
}

// RenderBundle describes the device paths a device directory allows that exist
// on the host, and skips the ones that do not yet.
func TestRenderBundleDescribesDeviceDirectoryNodes(t *testing.T) {
	b, _, _ := newBackend(t)
	b.StatDevice = func(p string) (DeviceInfo, error) {
		if p == "/dev/infiniband/rdma_cm" {
			return DeviceInfo{Type: "c", Major: 10, Minor: 58, Mode: 0o666}, nil
		}

		return DeviceInfo{}, os.ErrNotExist
	}

	require.NoError(t, b.RenderBundle("kube1", []isolation.Grant{{
		Kind:        isolation.GrantMount,
		Purpose:     isolation.PurposeHostMount,
		Source:      "/dev/infiniband",
		Target:      "/dev/infiniband",
		DeviceAllow: []string{"char-infiniband_verbs", "/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs0"},
	}}))

	data, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)

	var s spec
	require.NoError(t, json.Unmarshal(data, &s))

	var minors []int64

	for _, r := range s.Linux.Resources.Devices {
		if r.Major != nil && *r.Major == 10 {
			minors = append(minors, *r.Minor)
		}
	}

	require.Equal(t, []int64{58}, minors)
}

func TestConfigRejectsBadGrants(t *testing.T) {
	for name, tt := range map[string]struct {
		in   BundleInput
		want string
	}{
		"relative rootfs":        {BundleInput{Slot: "kube1", RootfsDir: "r"}, "absolute"},
		"relative source":        {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantMount, Source: "x"}}}, "absolute"},
		"empty source":           {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantMount}}}, "absolute"},
		"relative target":        {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantMount, Source: "/x", Target: "y"}}}, "absolute"},
		"conflict with baseline": {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantMount, Source: "/x", Target: "/var/lib/kubelet"}}}, "conflicts"},
		"unknown kind":           {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: "bogus", Source: "/x"}}}, "unsupported"},
		"device without info":    {BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: []isolation.Grant{{Kind: isolation.GrantDevice, Source: "/dev/x"}}}, "no device information"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RenderConfig(tt.in)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestConfigDeterministic(t *testing.T) {
	in := BundleInput{Slot: "kube1", RootfsDir: "/r", Grants: testGrants, Devices: testDevices, SELinux: SELinuxEnforcing}
	a, err := RenderConfig(in)
	require.NoError(t, err)

	for range 5 {
		b, err := RenderConfig(in)
		require.NoError(t, err)
		require.Equal(t, string(a), string(b))
	}
}

// ---- backend with a stub runtime ----

type stub struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error // key: "name arg0 arg1" prefix
	out   map[string]string
}

func (s *stub) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))

	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, line)

	for prefix, err := range s.fail {
		if strings.HasPrefix(line, prefix) {
			return "", err
		}
	}

	for prefix, out := range s.out {
		if strings.HasPrefix(line, prefix) {
			return out, nil
		}
	}

	// A runtime with no answer configured knows no container.
	if strings.HasPrefix(line, rt+" state") {
		return "", errors.New("container does not exist")
	}

	return "", nil
}

func (s *stub) reset() { s.calls = nil }

const rt = "/opt/unbounded/runtime/runc"

func newBackend(t *testing.T) (*Backend, *stub, string) {
	t.Helper()

	dir := t.TempDir()
	st := &stub{fail: map[string]error{}, out: map[string]string{
		"systemctl --version": "systemd 255 (255.4-1ubuntu8)\n+PAM +AUDIT +SELINUX\n",
	}}
	b := &Backend{
		Layout:        Layout{Base: filepath.Join(dir, "containers")},
		SystemdDir:    filepath.Join(dir, "systemd"),
		RuntimePath:   rt,
		HelperPath:    "/usr/local/bin/unbounded-agent",
		Run:           st.run,
		SourceRoot:    filepath.Join(dir, "host"),
		MountInfoPath: filepath.Join(dir, "mountinfo"),
		GoneTimeout:   20 * time.Millisecond,
		PollInterval:  time.Millisecond,
		Host: &HostFS{
			CgroupControllers: filepath.Join(dir, "cgroup.controllers"),
			SELinuxEnforce:    filepath.Join(dir, "enforce"),
			SELinuxContext:    filepath.Join(dir, "context"),
		},
		StatDevice: func(p string) (DeviceInfo, error) {
			if d, ok := testDevices[p]; ok {
				return d, nil
			}

			return DeviceInfo{}, os.ErrNotExist
		},
	}
	require.NoError(t, os.WriteFile(b.Host.SELinuxEnforce, []byte("1\n"), 0o644))
	require.NoError(t, os.WriteFile(b.MountInfoPath, nil, 0o644))

	// The unpacker stands in for the OCI unpack step.
	b.Unpack = func(_ context.Context, _ isolation.NodeSpec, d string) error {
		return os.MkdirAll(filepath.Join(d, "etc"), 0o755)
	}

	return b, st, dir
}

func TestBackendModeAndRequirements(t *testing.T) {
	b, _, _ := newBackend(t)
	require.Equal(t, isolation.ModeContainer, b.Mode())

	req := b.Requirements()
	require.Contains(t, req.HostCommands, rt)
	require.Subset(t, req.WritablePaths, []string{"/etc", "/opt", "/var"})
}

func TestPrepareWritesBundleAndUnit(t *testing.T) {
	b, st, _ := newBackend(t)

	r, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)
	require.Equal(t, "kube1", r.Slot())
	require.Equal(t, filepath.Join(b.Layout.RootfsDir("kube1"), "etc/x"), r.Path("/etc/x"))

	cfg, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), SELinuxLabel, "enforcing host")
	require.Contains(t, string(cfg), `"/dev/nvidia0"`)

	unit, err := os.ReadFile(filepath.Join(b.SystemdDir, UnitName("kube1")))
	require.NoError(t, err)
	require.Contains(t, string(unit), "ExecStart="+rt+" run --bundle "+b.Layout.BundleDir("kube1")+" unbounded-kube1")
	require.Contains(t, st.calls, "systemctl daemon-reload")

	for _, s := range SamePathMounts {
		require.DirExists(t, filepath.Join(b.SourceRoot, s.Path))
	}
}

func TestPrepareIdempotentNoTempFiles(t *testing.T) {
	b, _, _ := newBackend(t)

	var first []byte

	for i := range 3 {
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube2", Grants: testGrants})
		require.NoError(t, err)

		cfg, err := os.ReadFile(b.Layout.ConfigPath("kube2"))
		require.NoError(t, err)

		if i == 0 {
			first = cfg
		}

		require.Equal(t, string(first), string(cfg))
	}

	for _, d := range []string{b.Layout.BundleDir("kube2"), b.SystemdDir} {
		entries, err := os.ReadDir(d)
		require.NoError(t, err)

		for _, e := range entries {
			require.False(t, strings.HasSuffix(e.Name(), ".tmp"), e.Name())
		}
	}
}

func TestPrepareRejectsBadSlot(t *testing.T) {
	b, st, _ := newBackend(t)

	for _, slot := range []string{"", "kube3", "../kube1", "bundles", "kube1/x"} {
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: slot})
		require.ErrorContains(t, err, "invalid container slot", slot)
	}

	require.Empty(t, st.calls)

	r := isolation.SlotName("bogus")
	require.Error(t, b.Start(context.Background(), r))
	require.Error(t, b.Stop(context.Background(), r))
	require.Error(t, b.Restart(context.Background(), r))
	require.Error(t, b.Destroy(context.Background(), r))
	_, err := b.Exec(context.Background(), r, isolation.ExecSpec{Args: []string{"x"}})
	require.Error(t, err)
	_, err = b.Logs(context.Background(), r, isolation.LogOptions{})
	require.Error(t, err)
	_, err = b.Status(context.Background(), r)
	require.Error(t, err)
	require.Empty(t, st.calls)
}

func TestPrepareWithoutUnpackerNeedsRootfs(t *testing.T) {
	b, _, _ := newBackend(t)
	b.Unpack = nil

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
	require.ErrorContains(t, err, "no unpacker")

	require.NoError(t, os.MkdirAll(b.Layout.RootfsDir("kube1"), 0o755))
	_, err = b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)
}

func TestPrepareFailures(t *testing.T) {
	t.Run("unpack", func(t *testing.T) {
		b, _, _ := newBackend(t)
		b.Unpack = func(context.Context, isolation.NodeSpec, string) error { return errors.New("boom") }
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
		require.ErrorContains(t, err, "unpack rootfs")
	})
	t.Run("missing device", func(t *testing.T) {
		b, _, _ := newBackend(t)
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1", Grants: []isolation.Grant{{Kind: isolation.GrantDevice, Source: "/dev/none"}}})
		require.ErrorContains(t, err, "/dev/none")
	})
	t.Run("daemon-reload", func(t *testing.T) {
		b, st, _ := newBackend(t)
		st.fail["systemctl daemon-reload"] = errors.New("busy")
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
		require.ErrorContains(t, err, "daemon-reload")
	})
}

func TestStartStopRestartCommands(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()
	r := isolation.SlotName("kube1")

	require.NoError(t, b.Start(ctx, r))
	require.NoError(t, b.Start(ctx, r), "idempotent")
	require.NoError(t, b.Restart(ctx, r))
	require.NoError(t, b.Stop(ctx, r))
	require.NoError(t, b.Stop(ctx, r), "idempotent")

	u := UnitName("kube1")
	require.Equal(t, []string{
		"systemctl reset-failed " + u,
		"systemctl enable --now " + u,
		"systemctl reset-failed " + u,
		"systemctl enable --now " + u,
		"systemctl restart " + u,
		"systemctl stop " + u,
		rt + " state unbounded-kube1",
		rt + " delete --force unbounded-kube1",
		"systemctl stop " + u,
		rt + " state unbounded-kube1",
		rt + " delete --force unbounded-kube1",
	}, st.calls)
}

// Only one slot comes back at boot: starting a slot disables the other one,
// and stopping a slot disables it.
func TestStartDisablesTheOtherSlotAndStopDisablesItself(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()

	for _, slot := range []string{"kube1", "kube2"} {
		_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: slot})
		require.NoError(t, err)
	}

	st.reset()
	require.NoError(t, b.Start(ctx, isolation.SlotName("kube2")))
	require.Equal(t, []string{
		"systemctl disable " + UnitName("kube1"),
		"systemctl reset-failed " + UnitName("kube2"),
		"systemctl enable --now " + UnitName("kube2"),
	}, st.calls)

	st.reset()
	require.NoError(t, b.Stop(ctx, isolation.SlotName("kube2")))
	require.Contains(t, st.calls, "systemctl disable "+UnitName("kube2"))

	// The other slot's unit cannot be disabled: this one is not started.
	st.reset()
	st.fail["systemctl disable"] = errors.New("read-only /etc")

	require.ErrorContains(t, b.Start(ctx, isolation.SlotName("kube1")), "disable "+UnitName("kube2"))
	require.NotContains(t, st.calls, "systemctl enable --now "+UnitName("kube1"))
}

// Stop returns only once the runtime reports the container gone. One that
// outlives the unit is killed; one that survives that is an error.
func TestStopVerifiesTheContainerIsGone(t *testing.T) {
	ctx := context.Background()
	r := isolation.SlotName("kube1")

	b, st, _ := newBackend(t)
	st.out[rt+" state"] = `{"status":"running"}`

	err := b.Stop(ctx, r)
	require.ErrorContains(t, err, "still alive after stopping, killing and deleting it")
	require.Contains(t, st.calls, rt+" kill --all unbounded-kube1 KILL")

	// A container the kill ends is gone, and the stop succeeds.
	b, st, _ = newBackend(t)
	killed := false
	b.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if name == rt && args[0] == "kill" {
			killed = true
		}

		if name == rt && args[0] == "state" && !killed {
			return `{"status":"running"}`, nil
		}

		return st.run(ctx, log, name, args...)
	}

	require.NoError(t, b.Stop(ctx, r))
	require.True(t, killed)

	// A stopped container is gone too.
	b, st, _ = newBackend(t)
	st.out[rt+" state"] = `{"status":"stopped"}`

	require.NoError(t, b.Stop(ctx, r))
	require.NotContains(t, st.calls, rt+" kill --all unbounded-kube1 KILL")
}

// Destroy never trusts a stop that reported success: while the runtime has a
// live container nothing is removed.
func TestDestroyKeepsEverythingWhileTheContainerLives(t *testing.T) {
	ctx := context.Background()
	b, st, _ := newBackend(t)

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)

	st.out[rt+" state"] = `{"status":"running"}`

	err = b.Destroy(ctx, isolation.SlotName("kube1"))
	require.ErrorContains(t, err, "still alive")
	require.DirExists(t, b.Layout.RootfsDir("kube1"))
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitName("kube1")))
	require.DirExists(t, b.Layout.BundleDir("kube1"))
}

// The slot's private bpffs is unmounted and removed with the slot.
func TestDestroyRemovesTheSlotsBPFFS(t *testing.T) {
	ctx := context.Background()
	b, st, _ := newBackend(t)

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube2"})
	require.NoError(t, err)

	bpffs := filepath.Join(b.SourceRoot, BPFFSPath("kube2"))
	require.NoError(t, os.MkdirAll(bpffs, 0o755))
	require.NoError(t, os.WriteFile(b.MountInfoPath, []byte("40 30 0:40 / "+bpffs+" rw - bpf bpf rw\n"), 0o644))

	require.NoError(t, b.Destroy(ctx, isolation.SlotName("kube2")))
	require.Contains(t, st.calls, "umount "+bpffs)
	require.NoDirExists(t, bpffs)

	// An unmount failure is reported and the mount point kept.
	b, st, _ = newBackend(t)
	bpffs = filepath.Join(b.SourceRoot, BPFFSPath("kube1"))
	require.NoError(t, os.MkdirAll(bpffs, 0o755))
	require.NoError(t, os.WriteFile(b.MountInfoPath, []byte("40 30 0:40 / "+bpffs+" rw - bpf bpf rw\n"), 0o644))

	st.fail["umount"] = errors.New("busy")

	require.ErrorContains(t, b.Destroy(ctx, isolation.SlotName("kube1")), "unmount bpffs")
	require.DirExists(t, bpffs)
}

func TestFailureRecovery(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()
	r := isolation.SlotName("kube1")

	st.fail["systemctl enable"] = errors.New("start failed")

	require.ErrorContains(t, b.Start(ctx, r), "start failed")

	delete(st.fail, "systemctl enable")
	require.NoError(t, b.Start(ctx, r), "a retry after a failed start succeeds")

	// A runtime that has no such container must not fail Stop.
	st.fail[rt+" delete"] = errors.New("container does not exist")

	require.NoError(t, b.Stop(ctx, r))

	st.fail["systemctl stop"] = errors.New("stuck")

	require.ErrorContains(t, b.Stop(ctx, r), "stuck")
	require.NoError(t, b.Restart(ctx, r), "restart is unaffected by stop failures")

	st.fail["systemctl restart"] = errors.New("nope")

	require.ErrorContains(t, b.Restart(ctx, r), "nope")
}

func TestStatus(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()
	r := isolation.SlotName("kube1")

	st.fail["systemctl is-active"] = errors.New("inactive")
	s, err := b.Status(ctx, r)
	require.NoError(t, err)
	require.False(t, s.Running)
	require.Contains(t, s.Detail, "not active")

	delete(st.fail, "systemctl is-active")
	st.fail[rt+" state"] = errors.New("no container")
	s, err = b.Status(ctx, r)
	require.NoError(t, err)
	require.False(t, s.Running)

	delete(st.fail, rt+" state")
	st.out[rt+" state"] = `{"status":"stopped"}`
	s, _ = b.Status(ctx, r)
	require.False(t, s.Running)
	require.Contains(t, s.Detail, "stopped")

	st.out[rt+" state"] = `garbage`
	s, _ = b.Status(ctx, r)
	require.False(t, s.Running)

	st.out[rt+" state"] = `{"status":"running"}`
	s, _ = b.Status(ctx, r)
	require.True(t, s.Running)
}

func TestExec(t *testing.T) {
	b, st, _ := newBackend(t)
	r := isolation.SlotName("kube2")
	st.out[rt+" exec"] = "ok"

	res, err := b.Exec(context.Background(), r, isolation.ExecSpec{Args: []string{"kubectl", "get", "nodes"}})
	require.NoError(t, err)
	require.Equal(t, "ok", res.Stdout)
	require.Equal(t, []string{rt + " exec unbounded-kube2 kubectl get nodes"}, st.calls)

	_, err = b.Exec(context.Background(), r, isolation.ExecSpec{})
	require.ErrorContains(t, err, "no command")

	st.fail[rt+" exec"] = errors.New("exit 1")
	_, err = b.Exec(context.Background(), r, isolation.ExecSpec{Args: []string{"false"}})
	require.Error(t, err)
}

func TestLogs(t *testing.T) {
	b, _, _ := newBackend(t)

	var got []string

	b.Stream = func(_ context.Context, name string, args ...string) (io.ReadCloser, error) {
		got = append([]string{name}, args...)
		return io.NopCloser(strings.NewReader("log line\n")), nil
	}

	rc, err := b.Logs(context.Background(), isolation.SlotName("kube1"), isolation.LogOptions{Unit: "kubelet.service", Follow: true, Lines: 20})
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "log line\n", string(data))
	require.Equal(t, []string{rt, "exec", "unbounded-kube1", "journalctl", "--no-pager", "--unit=kubelet.service", "--follow", "--lines=20"}, got)

	b.Stream = func(context.Context, string, ...string) (io.ReadCloser, error) { return nil, errors.New("no runtime") }
	_, err = b.Logs(context.Background(), isolation.SlotName("kube1"), isolation.LogOptions{})
	require.Error(t, err)
}

func TestDestroyRemovesEverything(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)
	st.reset()

	require.NoError(t, b.Destroy(ctx, r))
	require.NoDirExists(t, b.Layout.RootfsDir("kube1"))
	require.NoDirExists(t, b.Layout.BundleDir("kube1"))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitName("kube1")))
	require.Contains(t, st.calls, "systemctl disable "+UnitName("kube1"))
	require.Contains(t, st.calls, "systemctl daemon-reload")

	require.NoError(t, b.Destroy(ctx, r), "destroying an absent slot succeeds")
}

func TestDestroyAttemptsEverythingOnFailure(t *testing.T) {
	b, st, _ := newBackend(t)
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)

	st.fail["systemctl stop"] = errors.New("stuck")
	st.fail["systemctl disable"] = errors.New("cannot disable")
	// The runtime knows no container, so the failed stop leaves nothing alive.
	st.fail[rt+" state"] = errors.New("container does not exist")

	err = b.Destroy(ctx, r)
	require.ErrorContains(t, err, "stuck")
	require.ErrorContains(t, err, "cannot disable")
	require.NoDirExists(t, b.Layout.RootfsDir("kube1"), "files are still removed")
}

func TestDestroyRefusesWhileMounted(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1"})
	require.NoError(t, err)

	mp := filepath.Join(b.Layout.RootfsDir("kube1"), "var/lib/kubelet")
	require.NoError(t, os.WriteFile(b.MountInfoPath, []byte("40 30 0:30 / "+mp+" rw shared:1 - ext4 /dev/sda rw\n"), 0o644))

	err = b.Destroy(ctx, r)
	require.ErrorContains(t, err, "still mounted")
	require.DirExists(t, b.Layout.RootfsDir("kube1"), "rootfs must be left alone")

	require.NoError(t, os.WriteFile(b.MountInfoPath, nil, 0o644))
	require.NoError(t, b.Destroy(ctx, r))
}

func TestRegenerateTracksSELinux(t *testing.T) {
	b, _, _ := newBackend(t)
	ctx := context.Background()

	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "kube1", Grants: testGrants})
	require.NoError(t, err)

	read := func() string {
		d, err := os.ReadFile(b.Layout.ConfigPath("kube1"))
		require.NoError(t, err)

		return string(d)
	}

	require.Contains(t, read(), SELinuxLabel)

	require.NoError(t, os.WriteFile(b.Host.SELinuxEnforce, []byte("0\n"), 0o644))
	require.NoError(t, b.Regenerate(ctx, "kube1"))
	require.NotContains(t, read(), SELinuxLabel)
	require.Contains(t, read(), "/dev/nvidia0", "grants are preserved")

	require.Error(t, b.Regenerate(ctx, "kube2"), "never prepared")
	require.Error(t, b.Regenerate(ctx, "x"))
}

// ---- preflight ----

func TestPreflight(t *testing.T) {
	b, _, dir := newBackend(t)
	b.WritableCheck = func(...string) error { return nil }

	require.ErrorContains(t, b.Preflight(), "container runtime", "runtime missing")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "runc"), []byte("#!/bin/sh\n"), 0o644))
	b.RuntimePath = filepath.Join(dir, "runc")
	require.ErrorContains(t, b.Preflight(), "not executable")

	require.NoError(t, os.Chmod(b.RuntimePath, 0o755))
	require.ErrorContains(t, b.Preflight(), "cgroup v2")

	require.NoError(t, os.WriteFile(b.Host.CgroupControllers, []byte("cpu memory\n"), 0o644))

	// Enforcing with a context interface the kernel does not offer.
	require.ErrorContains(t, b.Preflight(), "selinux label check")

	// Permissive hosts need no label support.
	require.NoError(t, os.WriteFile(b.Host.SELinuxEnforce, []byte("0\n"), 0o644))
	require.NoError(t, b.Preflight())

	// Enforcing with a working context interface (a file echoes the write).
	require.NoError(t, os.WriteFile(b.Host.SELinuxEnforce, []byte("1\n"), 0o644))
	require.NoError(t, os.WriteFile(b.Host.SELinuxContext, []byte("x"), 0o644))
	require.NoError(t, b.Preflight())

	b.WritableCheck = func(...string) error { return errors.New("/usr is read only") }
	require.ErrorContains(t, b.Preflight(), "read only")
}

func TestPreflightRequiresSystemd254(t *testing.T) {
	ready := func(t *testing.T) (*Backend, *stub) {
		t.Helper()

		b, st, dir := newBackend(t)
		b.WritableCheck = func(...string) error { return nil }
		b.RuntimePath = filepath.Join(dir, "runc")
		require.NoError(t, os.WriteFile(b.RuntimePath, []byte("#!/bin/sh\n"), 0o755))
		require.NoError(t, os.WriteFile(b.Host.CgroupControllers, []byte("cpu memory\n"), 0o644))
		require.NoError(t, os.WriteFile(b.Host.SELinuxEnforce, []byte("0\n"), 0o644))

		return b, st
	}

	for _, tc := range []struct {
		name, out, wantErr string
	}{
		{name: "ubuntu 24.04", out: "systemd 255 (255.4-1ubuntu8)\n+PAM +AUDIT\n"},
		{name: "exactly 254", out: "systemd 254 (254.5-1)\n"},
		{name: "no distro suffix", out: "systemd 256\n"},
		{name: "ubuntu 22.04", out: "systemd 249 (249.11-0ubuntu3.12)\n", wantErr: "systemd 249 is too old: the container mode needs systemd 254 or newer"},
		{name: "rhel 9", out: "systemd 252 (252-46.el9_5.2)\n", wantErr: "systemd 252 is too old"},
		{name: "unparsable", out: "not systemd at all\n", wantErr: `unrecognized systemctl --version output "not systemd at all"`},
		{name: "empty", out: "", wantErr: "unrecognized systemctl --version output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, st := ready(t)
			st.out["systemctl --version"] = tc.out

			err := b.Preflight()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}

			require.Contains(t, st.calls, "systemctl --version")
		})
	}

	t.Run("systemctl fails", func(t *testing.T) {
		b, st := ready(t)
		st.fail["systemctl --version"] = errors.New("exec: systemctl: not found")
		require.ErrorContains(t, b.Preflight(), "reading the systemd version (systemctl --version): exec: systemctl: not found")
	})
}

func TestParseSystemdVersion(t *testing.T) {
	t.Parallel()

	for out, want := range map[string]int{
		"systemd 255 (255.4-1ubuntu8)\n+PAM": 255,
		"  systemd 239 (239-74.el8)\n":       239,
		"systemd 1000":                       1000,
	} {
		got, err := parseSystemdVersion(out)
		require.NoError(t, err, out)
		require.Equal(t, want, got, out)
	}

	for _, out := range []string{"", "systemd", "systemd x", "systemd255", "udev 255", "systemd 25a"} {
		_, err := parseSystemdVersion(out)
		require.Error(t, err, out)
	}
}

func TestSELinuxModeDetection(t *testing.T) {
	dir := t.TempDir()
	h := HostFS{SELinuxEnforce: filepath.Join(dir, "enforce")}
	require.Equal(t, SELinuxDisabled, h.SELinuxMode())

	require.NoError(t, os.WriteFile(h.SELinuxEnforce, []byte("0"), 0o644))
	require.Equal(t, SELinuxPermissive, h.SELinuxMode())

	require.NoError(t, os.WriteFile(h.SELinuxEnforce, []byte("1\n"), 0o644))
	require.Equal(t, SELinuxEnforcing, h.SELinuxMode())
}

// NewDefault hands back a working backend with production defaults, which is
// what a caller that only needs Status, Exec or Logs uses.
func TestNewDefault(t *testing.T) {
	b := NewDefault(slog.Default())
	require.Equal(t, isolation.ModeContainer, b.Mode())
	require.Equal(t, DefaultRuntimePath, b.runtimePath())
}

// The kubelet exits with "open /dev/kmsg: no such file or directory" when the
// node container has no /dev/kmsg, which e2e showed keeps the Node from ever
// registering. Every bundle must carry it, with a cgroup rule.
func TestConfigAlwaysCreatesDevKmsg(t *testing.T) {
	t.Parallel()

	b, err := RenderConfig(BundleInput{Slot: "kube1", RootfsDir: "/var/lib/unbounded/containers/kube1", SELinux: SELinuxPermissive})
	require.NoError(t, err)

	var cfg struct {
		Linux struct {
			Devices []struct {
				Path     string
				Type     string
				Major    int64
				Minor    int64
				FileMode uint32
			}
			Resources struct {
				Devices []struct {
					Allow bool
					Type  string
					Major *int64
					Minor *int64
				}
			}
		}
	}

	require.NoError(t, json.Unmarshal(b, &cfg))
	require.Len(t, cfg.Linux.Devices, 1)
	require.Equal(t, "/dev/kmsg", cfg.Linux.Devices[0].Path)
	require.Equal(t, "c", cfg.Linux.Devices[0].Type)
	require.EqualValues(t, 1, cfg.Linux.Devices[0].Major)
	require.EqualValues(t, 11, cfg.Linux.Devices[0].Minor)

	var ruled bool

	for _, r := range cfg.Linux.Resources.Devices {
		if r.Allow && r.Type == "c" && r.Major != nil && *r.Major == 1 && r.Minor != nil && *r.Minor == 11 {
			ruled = true
		}
	}

	require.True(t, ruled, "an explicit cgroup rule for /dev/kmsg")
}

// udev belongs to the host; the container's copy is masked in its rootfs, and
// preparing again keeps it masked.
func TestPrepareMasksUdevInTheRootfs(t *testing.T) {
	b, _, _ := newBackend(t)

	for range 2 {
		_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "kube1"})
		require.NoError(t, err)
	}

	for _, unit := range MaskedUnits {
		target, err := os.Readlink(filepath.Join(b.Layout.RootfsDir("kube1"), "etc/systemd/system", unit))
		require.NoError(t, err, unit)
		require.Equal(t, os.DevNull, target, unit)
	}

	require.Contains(t, MaskedUnits, "systemd-udevd.service")
}
