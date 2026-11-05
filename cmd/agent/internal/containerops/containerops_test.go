// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

var quiet = hostnodetest.Quiet

const testRuntime = "/opt/unbounded/runtime/runc"

// host stands in for systemd and the OCI runtime. It records every command and
// tracks which supervising units are active.
type host struct {
	mu      sync.Mutex
	calls   []string
	active  map[string]bool   // unit name
	failOne map[string]error  // substring of the call line, consumed on first match
	failAll map[string]error  // substring of the call line, never consumed
	out     map[string]string // prefix of the call line to stdout
	state   string            // SystemState reported by systemd inside the container
	onCall  func(call string) // runs for every call, before any failure is injected
}

func newHost() *host {
	return &host{active: map[string]bool{}, failOne: map[string]error{}, failAll: map[string]error{}, out: map[string]string{}, state: "running"}
}

func slotOf(id string) string { return strings.TrimPrefix(id, "unbounded-") }

func (h *host) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	h.calls = append(h.calls, call)

	if h.onCall != nil {
		h.onCall(call)
	}

	for sub, err := range h.failOne {
		if strings.Contains(call, sub) {
			delete(h.failOne, sub)
			return "", err
		}
	}

	for sub, err := range h.failAll {
		if strings.Contains(call, sub) {
			return "", err
		}
	}

	switch {
	case name == "systemctl":
		unit := args[len(args)-1]

		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "stop", "disable":
			delete(h.active, unit)
		case "is-active":
			if !h.active[unit] {
				return "", errors.New("inactive")
			}
		}
	case strings.HasSuffix(name, "/runc") && args[0] == "state":
		if !h.active[container.UnitName(slotOf(args[1]))] {
			return "", errors.New("container does not exist")
		}

		return `{"status":"running"}`, nil
	case strings.HasSuffix(name, "/runc") && args[0] == "exec":
		if !h.active[container.UnitName(slotOf(args[1]))] {
			return "", errors.New("container not running")
		}

		rest := strings.Join(args[2:], " ")
		if strings.HasPrefix(rest, "systemctl show") {
			return h.state, nil
		}

		if strings.HasPrefix(rest, "systemctl is-active") {
			return "active", nil
		}
	}

	for prefix, out := range h.out {
		if strings.HasPrefix(call, prefix) {
			return out, nil
		}
	}

	return "", nil
}

func (h *host) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.calls...)
}

func (h *host) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = nil
}

// index returns the position of the first call containing sub at or after from,
// or -1.
func index(calls []string, sub string, from int) int {
	for i := from; i < len(calls); i++ {
		if strings.Contains(calls[i], sub) {
			return i
		}
	}

	return -1
}

func requireOrder(t *testing.T, calls []string, subs ...string) {
	t.Helper()

	at := 0

	for _, sub := range subs {
		i := index(calls, sub, at)
		require.GreaterOrEqual(t, i, 0, "missing %q after position %d in:\n%s", sub, at, strings.Join(calls, "\n"))

		at = i + 1
	}
}

type fixture struct {
	t     *testing.T
	h     *host
	dir   string
	hooks *counters
}

type counters struct {
	mu                                                      sync.Mutex
	runtime, helper, provision, setup, cleanup, dns, unpack int
	provisionErr, unpackErr, setupErr, cleanupErr           error
}

// inc counts a call and returns the injected error once.
func (c *counters) inc(n *int, errp *error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	*n++

	err := *errp
	*errp = nil

	return err
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	return &fixture{t: t, h: newHost(), dir: t.TempDir(), hooks: &counters{}}
}

func (f *fixture) goalState(slot string, dns bool) *goalstates.MachineGoalState {
	return hostnodetest.GoalState(slot, dns)
}

func (f *fixture) mountInfo() string {
	p := filepath.Join(f.dir, "mountinfo")
	if _, err := os.Stat(p); err != nil {
		require.NoError(f.t, os.WriteFile(p, nil, 0o644))
	}

	return p
}

func (f *fixture) node(slot string, dns bool) *Node {
	c := f.hooks

	return New(Params{
		Log:       quiet(),
		Slot:      slot,
		GoalState: f.goalState(slot, dns),
		Overrides: Overrides{Layout: container.Layout{Base: filepath.Join(f.dir, "containers")}, SystemdDir: filepath.Join(f.dir, "systemd"), RuntimePath: testRuntime, HelperPath: "/usr/local/bin/unbounded-agent", Run: f.h.run, MountInfoPath: f.mountInfo(), Host: &container.HostFS{SELinuxEnforce: filepath.Join(f.dir, "absent")}, PollInterval: time.Millisecond, ReadyTimeout: 50 * time.Millisecond, Unpack: func(_ context.Context, dir string) error {
			if err := c.inc(&c.unpack, &c.unpackErr); err != nil {
				return err
			}

			return os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
		}, InstallRuntime: func(context.Context) error { return c.inc(&c.runtime, new(error)) }, InstallHelper: func(context.Context) error { return c.inc(&c.helper, new(error)) }, ProvisionRootfs: func(context.Context) error { return c.inc(&c.provision, &c.provisionErr) }},
		Hooks: hostnode.Hooks{HostRoot: filepath.Join(f.dir, "host"), SetupNetwork: func(context.Context) error { return c.inc(&c.setup, &c.setupErr) }, CleanupNetwork: func(context.Context) error { return c.inc(&c.cleanup, &c.cleanupErr) }, WaitLocalDNS: func(context.Context, *goalstates.NodeStart) error { return c.inc(&c.dns, new(error)) }},
	})
}

func (f *fixture) hostPath(p string) string  { return filepath.Join(f.dir, "host", p) }
func (f *fixture) rootfs(slot string) string { return filepath.Join(f.dir, "containers", slot) }

func TestGoalStateIsPointedAtTheContainer(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", true)

	gs, err := n.GoalState()
	require.NoError(t, err)

	require.True(t, gs.NodeStart.InContainer)
	require.False(t, gs.NodeStart.HostNative())
	require.False(t, gs.NodeStart.NSpawnMachine())

	// Same-path files go to the host side, everything else into the rootfs.
	require.Equal(t, f.hostPath("var/lib/kubelet/config.yaml"), gs.NodeStart.Root().Path("/var/lib/kubelet/config.yaml"))
	require.Equal(t, f.hostPath("opt/cni/bin"), gs.RootFS.Root().Path("/opt/cni/bin"))
	require.Equal(t, filepath.Join(f.rootfs("kube1"), "etc/containerd/config.toml"), gs.NodeStart.Root().Path(goalstates.ContainerdConfigPath))
	require.Equal(t, "kube1", gs.NodeStart.Root().Slot())

	// Commands run through the runtime's exec.
	f.h.out[testRuntime+" exec unbounded-kube1 uname"] = "Linux"
	f.h.active[container.UnitName("kube1")] = true

	out, err := gs.NodeStart.Exec(t.Context(), quiet(), "uname", "-r")
	require.NoError(t, err)
	require.Equal(t, "Linux", out)
	require.Contains(t, f.h.snapshot(), testRuntime+" exec unbounded-kube1 uname -r")

	// The unit requires the LocalDNS network unit only when LocalDNS is on.
	require.Equal(t, []string{goalstates.LocalDNSNetworkUnit}, n.Backend().UnitRequires)
	require.Empty(t, f.node("kube1", false).mustBackendRequires(t))
}

func (n *Node) mustBackendRequires(t *testing.T) []string {
	t.Helper()

	_, err := n.GoalState()
	require.NoError(t, err)

	return n.Backend().UnitRequires
}

func TestResolveFailuresAndSlotValidation(t *testing.T) {
	n := New(Params{Log: quiet()})
	_, err := n.GoalState()
	require.ErrorContains(t, err, "no agent configuration")

	boom := errors.New("gpu not ready")
	n = New(Params{
		Log:   quiet(),
		Hooks: hostnode.Hooks{Resolve: func(string) (*goalstates.MachineGoalState, error) { return nil, boom }},
	})
	_, err = n.GoalState()
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "resolve machine goal state")
	require.ErrorIs(t, n.Restart(t.Context()), boom)
	require.ErrorIs(t, n.Resume(t.Context()), boom)

	_, _, err = n.Configure(t.Context())
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, n.Start(t.Context(), false, false, false), boom)

	_, err = n.Prepare(t.Context())
	require.ErrorIs(t, err, boom)

	bad := New(Params{Log: quiet(), Slot: "kube3"})
	_, err = bad.GoalState()
	require.ErrorContains(t, err, "invalid container slot")
	require.Error(t, bad.Stop(t.Context()))
	require.Error(t, bad.Destroy(t.Context()))

	_, err = bad.Running(t.Context())
	require.Error(t, err)

	var slot string

	New(Params{
		Log: quiet(),
		Hooks: hostnode.Hooks{Resolve: func(s string) (*goalstates.MachineGoalState, error) {
			slot = s
			return &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{}, NodeStart: &goalstates.NodeStart{}}, nil
		}},
	}).GoalState() //nolint:errcheck // only the slot matters
	require.Equal(t, "kube1", slot, "the default slot is the first")
}

func TestPrepareBuildsTheSlotInOrder(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube2", true)

	r, err := n.Prepare(t.Context())
	require.NoError(t, err)
	require.Equal(t, "kube2", r.Slot())

	h := f.hooks
	require.Equal(t, [5]int{1, 1, 1, 1, 0}, [5]int{h.runtime, h.helper, h.unpack, h.provision, h.setup})

	require.DirExists(t, filepath.Join(f.rootfs("kube2"), "etc"))
	require.FileExists(t, filepath.Join(f.dir, "containers", "bundles", "kube2", "config.json"))

	unit, err := os.ReadFile(filepath.Join(f.dir, "systemd", container.UnitName("kube2")))
	require.NoError(t, err)
	require.Contains(t, string(unit), "Requires="+goalstates.LocalDNSNetworkUnit)
	require.Contains(t, string(unit), "/usr/local/bin/unbounded-agent node-lifecycle pre-start kube2")

	// Same-path source directories exist on the host for the bind mounts.
	require.DirExists(t, f.hostPath("var/lib/kubelet"))
	require.DirExists(t, f.hostPath("opt/cni"))

	// Nothing was started.
	require.Empty(t, f.h.active)
}

func TestPrepareStopsAtTheFirstFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(*counters)
		want string
		ran  func(*counters) bool // whether the work after the failure ran
	}{
		"unpack": {func(c *counters) { c.unpackErr = errors.New("registry down") }, "unpack rootfs", func(c *counters) bool { return c.provision > 0 }},
		"rootfs": {func(c *counters) { c.provisionErr = errors.New("download failed") }, "download failed", func(c *counters) bool { return false }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.set(f.hooks)

			_, err := f.node("kube1", false).Prepare(t.Context())
			require.ErrorContains(t, err, tc.want)
			require.False(t, tc.ran(f.hooks))
			require.Empty(t, f.h.active)
		})
	}
}

func TestPrepareRuntimeAndHelperFailures(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)
	n.p.InstallRuntime = func(context.Context) error { return errors.New("no network") }

	_, err := n.Prepare(t.Context())
	require.ErrorContains(t, err, "no network")
	require.Zero(t, f.hooks.unpack, "nothing is unpacked without a runtime")

	n.p.InstallRuntime = nil
	n.p.InstallHelper = func(context.Context) error { return errors.New("disk full") }
	n.p.InstallRuntime = func(context.Context) error { return nil }

	_, err = n.Prepare(t.Context())
	require.ErrorContains(t, err, "disk full")
	require.Zero(t, f.hooks.unpack)
}

func TestConfigureWritesIntoTheRootAndTheHost(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", true)

	containerd, kubelet, err := n.Configure(t.Context())
	require.NoError(t, err)
	require.True(t, containerd)
	require.True(t, kubelet)
	require.Equal(t, 1, f.hooks.setup)

	// Distro unit names inside the container's own rootfs.
	require.FileExists(t, filepath.Join(f.rootfs("kube1"), "etc/systemd/system/containerd.service"))
	require.FileExists(t, filepath.Join(f.rootfs("kube1"), "etc/systemd/system/kubelet.service"))
	require.FileExists(t, filepath.Join(f.rootfs("kube1"), "etc/containerd/config.toml"))

	// The kubelet configuration is under the same-path bind mount, so it is
	// written on the host side where the container reads it.
	require.FileExists(t, f.hostPath("var/lib/kubelet/config.yaml"))
	require.NoFileExists(t, filepath.Join(f.rootfs("kube1"), "var/lib/kubelet/config.yaml"))

	// The container's own resolver file carries the LocalDNS listener.
	resolv, err := os.ReadFile(filepath.Join(f.rootfs("kube1"), "etc/resolv.conf"))
	require.NoError(t, err)
	require.Contains(t, string(resolv), "169.254.20.10")

	// A reapply of identical content changes nothing; a changed input does.
	containerd, kubelet, err = n.Configure(t.Context())
	require.NoError(t, err)
	require.False(t, containerd)
	require.False(t, kubelet)

	gs, err := n.GoalState()
	require.NoError(t, err)

	gs.NodeStart.Kubelet.ClusterDNS = "10.0.0.11"
	containerd, kubelet, err = n.Configure(t.Context())
	require.NoError(t, err)
	require.False(t, containerd)
	require.True(t, kubelet)

	f.hooks.setupErr = errors.New("nft failed")
	_, _, err = n.Configure(t.Context())
	require.ErrorContains(t, err, "nft failed")
}

func TestStartRunsTheStackInOrder(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", true)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)
	f.h.reset()

	require.NoError(t, n.Start(t.Context(), false, true, true))

	calls := f.h.snapshot()
	requireOrder(t, calls,
		"systemctl enable --now "+container.UnitName("kube1"),
		testRuntime+" exec unbounded-kube1 systemctl show --property=SystemState --value",
		testRuntime+" exec unbounded-kube1 systemctl enable --now containerd.service",
		testRuntime+" exec unbounded-kube1 systemctl enable --now kubelet.service",
		testRuntime+" exec unbounded-kube1 test -s "+goalstates.KubeletKubeconfigPath,
	)
	require.Equal(t, 1, f.hooks.dns)

	// The container booted with this start, so nothing is restarted.
	require.Equal(t, -1, index(calls, "systemctl restart", 0))

	running, err := n.Running(t.Context())
	require.NoError(t, err)
	require.True(t, running)
}

func TestStartRestartsOnlyChangedServicesOfARunningNode(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		containerd, kubelet bool
		want                []string
	}{
		"none":       {false, false, nil},
		"kubelet":    {false, true, []string{"systemctl restart kubelet.service"}},
		"containerd": {true, false, []string{"systemctl restart containerd.service"}},
		"both":       {true, true, []string{"systemctl restart containerd.service", "systemctl restart kubelet.service"}},
	} {
		t.Run(name, func(t *testing.T) {
			f.h.reset()
			require.NoError(t, n.Start(t.Context(), true, tc.containerd, tc.kubelet))

			var restarts []string

			for _, c := range f.h.snapshot() {
				if _, rest, ok := strings.Cut(c, "unbounded-kube1 "); ok && strings.HasPrefix(rest, "systemctl restart") {
					restarts = append(restarts, rest)
				}
			}

			require.Equal(t, tc.want, restarts, "containerd first")
		})
	}
}

func TestStartFailures(t *testing.T) {
	t.Run("container does not start", func(t *testing.T) {
		f := newFixture(t)
		n := f.node("kube1", false)
		f.h.failOne["enable --now "+container.UnitName("kube1")] = errors.New("unit failed")

		err := n.Start(t.Context(), false, false, false)
		require.ErrorContains(t, err, "start-container")
		require.ErrorContains(t, err, "unit failed")
		require.Equal(t, -1, index(f.h.snapshot(), "exec", 0), "no service is started in a container that did not start")
	})

	t.Run("systemd never becomes ready", func(t *testing.T) {
		f := newFixture(t)
		n := f.node("kube1", false)
		f.h.state = "starting"

		err := n.Start(t.Context(), false, false, false)
		require.ErrorContains(t, err, "not ready")
		require.ErrorContains(t, err, "system state starting")
		require.Equal(t, -1, index(f.h.snapshot(), "enable --now containerd", 0))
	})

	t.Run("degraded systemd is accepted", func(t *testing.T) {
		f := newFixture(t)
		n := f.node("kube1", false)
		f.h.state = "degraded"

		require.NoError(t, n.Start(t.Context(), false, false, false))
	})

	t.Run("kubelet does not start", func(t *testing.T) {
		f := newFixture(t)
		n := f.node("kube1", false)
		f.h.failOne["enable --now kubelet.service"] = errors.New("kubelet failed")

		err := n.Start(t.Context(), false, false, false)
		require.ErrorContains(t, err, "start-kubelet")
	})

	t.Run("restart of a changed service fails", func(t *testing.T) {
		f := newFixture(t)
		n := f.node("kube1", false)
		f.h.failOne["systemctl restart kubelet.service"] = errors.New("busy")

		err := n.Start(t.Context(), true, false, true)
		require.ErrorContains(t, err, "systemctl restart kubelet.service")
	})
}

func TestStopPreStopsServicesWhenRunning(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)
	require.NoError(t, n.Start(t.Context(), false, false, false))
	f.h.reset()

	require.NoError(t, n.Stop(t.Context()))
	requireOrder(t, f.h.snapshot(),
		testRuntime+" exec unbounded-kube1 systemctl stop kubelet.service",
		testRuntime+" exec unbounded-kube1 systemctl stop containerd.service",
		"systemctl stop "+container.UnitName("kube1"),
		testRuntime+" delete --force unbounded-kube1",
	)

	// Stopping a stopped node does no exec and succeeds.
	f.h.reset()
	require.NoError(t, n.Stop(t.Context()))
	require.Equal(t, -1, index(f.h.snapshot(), testRuntime+" exec", 0))
}

func TestStopToleratesAFailedPreStopButNotAFailedContainerStop(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)
	require.NoError(t, n.Start(t.Context(), false, false, false))

	f.h.failOne["systemctl stop kubelet.service"] = errors.New("hung")

	require.NoError(t, n.Stop(t.Context()), "the container stop still ends the services")

	require.NoError(t, n.Start(t.Context(), false, false, false))

	f.h.failOne["systemctl stop "+container.UnitName("kube1")] = errors.New("cannot stop")

	require.ErrorContains(t, n.Stop(t.Context()), "cannot stop")
}

func TestRestartKeepsTheHostUp(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", true)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)
	require.NoError(t, n.Start(t.Context(), false, false, false))
	f.h.reset()

	require.NoError(t, n.Restart(t.Context()))

	calls := f.h.snapshot()
	requireOrder(t, calls,
		"systemctl restart "+container.UnitName("kube1"),
		"systemctl show --property=SystemState --value",
		"systemctl is-active kubelet.service",
	)
	require.Equal(t, -1, index(calls, "reboot", 0))
	require.Equal(t, -1, index(calls, "systemctl stop", 0), "the unit restarts, it is not stopped")
	require.Equal(t, 2, f.hooks.dns, "LocalDNS is waited for after the restart")

	f.h.failOne["systemctl restart "+container.UnitName("kube1")] = errors.New("restart failed")

	require.ErrorContains(t, n.Restart(t.Context()), "restart failed")
}

func TestDestroyRemovesTheSlot(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)
	require.NoError(t, n.Start(t.Context(), false, false, false))

	require.NoError(t, n.Destroy(t.Context()))
	require.NoDirExists(t, f.rootfs("kube1"))
	require.NoFileExists(t, filepath.Join(f.dir, "systemd", container.UnitName("kube1")))
	require.Empty(t, f.h.active)
}

func TestRewriteUnitPicksUpTheHelperPathWithoutARestart(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube1", false)

	_, err := n.Prepare(t.Context())
	require.NoError(t, err)

	n.Backend().HelperPath = "/opt/unbounded/agent/bin/unbounded-agent-lifecycle"

	f.h.reset()

	require.NoError(t, n.RewriteUnit(t.Context()))
	require.Equal(t, 2, f.hooks.helper, "the helper is reinstalled for the new path")

	unit, err := os.ReadFile(filepath.Join(f.dir, "systemd", container.UnitName("kube1")))
	require.NoError(t, err)
	require.Contains(t, string(unit), "/opt/unbounded/agent/bin/unbounded-agent-lifecycle node-lifecycle pre-start kube1")
	require.Equal(t, []string{"systemctl daemon-reload"}, f.h.snapshot())
}

// ---- upgrade ----

// upgradeFixture has a running kube1 and a node for kube2.
func upgradeFixture(t *testing.T) (f *fixture, oldNode, newNode *Node) {
	t.Helper()

	f = newFixture(t)
	oldNode = f.node("kube1", true)

	_, err := oldNode.Prepare(t.Context())
	require.NoError(t, err)
	require.NoError(t, oldNode.Start(t.Context(), false, false, false))

	// The old node's identity, which a repave must discard.
	for _, p := range []string{goalstates.KubeletKubeconfigPath, hostnode.KubeletClientPKIDir + "/kubelet-client-current.pem"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(p)), 0o755))
		require.NoError(t, os.WriteFile(f.hostPath(p), []byte("old"), 0o600))
	}

	f.h.reset()
	*f.hooks = counters{}

	return f, oldNode, f.node("kube2", true)
}

func TestUpgradeMovesToTheAlternateSlot(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	// A pod volume mounted under the shared kubelet directory by the old node.
	require.NoError(t, os.WriteFile(f.mountInfo(), []byte("100 1 0:50 / /var/lib/kubelet/pods/x/volumes/v rw - tmpfs tmpfs rw\n"), 0o644))

	require.NoError(t, newNode.Upgrade(t.Context(), oldNode))

	calls := f.h.snapshot()
	requireOrder(t, calls,
		// The old slot is stopped gracefully only after the new one is built.
		"systemctl stop "+container.UnitName("kube1"),
		"umount -l /var/lib/kubelet/pods/x/volumes/v",
		"systemctl enable --now "+container.UnitName("kube2"),
		"unbounded-kube2 systemctl enable --now kubelet.service",
		"unbounded-kube2 test -s "+goalstates.KubeletKubeconfigPath,
		"unbounded-kube2 systemctl is-active kubelet.service",
	)
	require.Equal(t, 1, f.hooks.unpack)
	require.Equal(t, 1, f.hooks.cleanup, "host networking is cleaned as repave does")

	// The new slot is up, the old one is stopped but left for the caller to retire.
	require.True(t, f.h.active[container.UnitName("kube2")])
	require.False(t, f.h.active[container.UnitName("kube1")])
	require.DirExists(t, f.rootfs("kube1"))

	// The previous identity is gone so the new slot bootstraps fresh credentials.
	require.NoFileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath))
	require.NoDirExists(t, f.hostPath(hostnode.KubeletClientPKIDir))

	// Retiring removes the old slot.
	require.NoError(t, oldNode.Destroy(t.Context()))
	require.NoDirExists(t, f.rootfs("kube1"))
	require.DirExists(t, f.rootfs("kube2"))
}

func TestUpgradeKeepsDirectCredentials(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	gs, err := newNode.GoalState()
	require.NoError(t, err)

	gs.NodeStart.Kubelet.BootstrapToken = ""
	gs.NodeStart.Kubelet.KubeconfigData = []byte("kubeconfig")

	require.NoError(t, newNode.Upgrade(t.Context(), oldNode))
	require.FileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath), "Configure writes the direct kubeconfig, nothing removes it")
}

func TestUpgradeRejectsTheSameSlot(t *testing.T) {
	f, oldNode, _ := upgradeFixture(t)

	require.ErrorContains(t, f.node("kube1", true).Upgrade(t.Context(), oldNode), "different slot")
	require.True(t, f.h.active[container.UnitName("kube1")])
}

func TestUpgradeRollsBack(t *testing.T) {
	type stage struct {
		name string
		set  func(f *fixture)
		// oldStopped says whether the old slot had been stopped by the time of
		// the failure, so a restore is needed.
		oldStopped bool
		want       string
	}

	for _, tc := range []stage{
		{"unpack", func(f *fixture) { f.hooks.unpackErr = errors.New("registry down") }, false, "registry down"},
		{"provision", func(f *fixture) { f.hooks.provisionErr = errors.New("download failed") }, false, "download failed"},
		{"stop old", func(f *fixture) {
			f.h.failOne["systemctl stop "+container.UnitName("kube1")] = errors.New("cannot stop")
		}, true, "cannot stop"},
		{"cleanup network", func(f *fixture) { f.hooks.cleanupErr = errors.New("routes busy") }, true, "routes busy"},
		{"new container does not start", func(f *fixture) {
			f.h.failOne["enable --now "+container.UnitName("kube2")] = errors.New("unit failed")
		}, true, "unit failed"},
		{"new kubelet does not start", func(f *fixture) {
			f.h.failOne["unbounded-kube2 systemctl enable --now kubelet.service"] = errors.New("kubelet failed")
		}, true, "start-kubelet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, oldNode, newNode := upgradeFixture(t)
			tc.set(f)

			err := newNode.Upgrade(t.Context(), oldNode)
			require.ErrorContains(t, err, tc.want)

			// The new slot is removed in every case.
			require.NoDirExists(t, f.rootfs("kube2"))
			require.NoFileExists(t, filepath.Join(f.dir, "systemd", container.UnitName("kube2")))
			require.False(t, f.h.active[container.UnitName("kube2")])

			// The previous slot is running with its rootfs intact.
			require.True(t, f.h.active[container.UnitName("kube1")], "previous slot must run")
			require.DirExists(t, f.rootfs("kube1"))

			if tc.oldStopped {
				require.ErrorContains(t, err, "previous slot kube1 restored")
			} else {
				require.NotContains(t, err.Error(), "restored", "nothing needed restoring")
				require.Equal(t, -1, index(f.h.snapshot(), "systemctl stop "+container.UnitName("kube1"), 0), "the old slot is never touched before the new one is built")
			}
		})
	}
}

func TestUpgradeReportsAFailedRestore(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	f.h.failOne["enable --now "+container.UnitName("kube2")] = errors.New("new unit failed")
	// The restart of the old slot also fails; both errors are reported.
	f.h.failAll["enable --now "+container.UnitName("kube1")] = errors.New("old unit down")

	err := newNode.Upgrade(t.Context(), oldNode)
	require.ErrorContains(t, err, "new unit failed")
	require.ErrorContains(t, err, "restart previous slot kube1")
	require.ErrorContains(t, err, "old unit down")
	require.NotContains(t, err.Error(), "restored")
}

func TestUpgradeRollbackSurvivesACanceledContext(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	f.hooks.provisionErr = nil
	newNode.p.SetupNetwork = func(context.Context) error { cancel(); return errors.New("canceled mid way") }

	err := newNode.Upgrade(ctx, oldNode)
	require.ErrorContains(t, err, "canceled mid way")
	require.True(t, f.h.active[container.UnitName("kube1")], "the restore does not depend on the canceled context")
}

func TestUpgradeRollsBackWhenTheNewNodeNeverBootstraps(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	f.h.failAll["unbounded-kube2 test -s"] = errors.New("no kubeconfig")

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	err := newNode.Upgrade(ctx, oldNode)
	require.ErrorContains(t, err, "wait-for-kubelet-bootstrap")
	require.ErrorContains(t, err, "previous slot kube1 restored")
	require.True(t, f.h.active[container.UnitName("kube1")])
	require.False(t, f.h.active[container.UnitName("kube2")])
	require.NoDirExists(t, f.rootfs("kube2"))
}

func TestUpgradeRemovesAStaleTargetSlotFirst(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	// A leftover rootfs of an earlier upgrade, from another image.
	stale := filepath.Join(f.rootfs("kube2"), "etc", "stale-marker")
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o755))
	require.NoError(t, os.WriteFile(stale, nil, 0o644))

	require.NoError(t, newNode.Upgrade(t.Context(), oldNode))
	require.NoFileExists(t, stale, "the slot was rebuilt, not reused")
	require.Equal(t, 1, f.hooks.unpack)

	// A target slot that cannot be cleaned stops the upgrade before the running
	// node is touched.
	f, oldNode, newNode = upgradeFixture(t)
	require.NoError(t, os.MkdirAll(f.rootfs("kube2"), 0o755))
	require.NoError(t, os.WriteFile(f.mountInfo(), []byte("100 1 0:50 / "+f.rootfs("kube2")+"/var/lib/containerd rw - xfs /dev/sda rw\n"), 0o644))

	err := newNode.Upgrade(t.Context(), oldNode)
	require.ErrorContains(t, err, "remove stale slot kube2")
	require.True(t, f.h.active[container.UnitName("kube1")])
	require.Equal(t, -1, index(f.h.snapshot(), "systemctl stop "+container.UnitName("kube1"), 0))
}

func TestUpgradeRollbackPutsTheSharedConfigurationBack(t *testing.T) {
	for name, set := range map[string]func(f *fixture){
		"cleanup network": func(f *fixture) { f.hooks.cleanupErr = errors.New("routes busy") },
		"new kubelet does not start": func(f *fixture) {
			f.h.failOne["unbounded-kube2 systemctl enable --now kubelet.service"] = errors.New("kubelet failed")
		},
		"new container does not start": func(f *fixture) { f.h.failOne["enable --now "+container.UnitName("kube2")] = errors.New("unit failed") },
	} {
		t.Run(name, func(t *testing.T) {
			f, oldNode, newNode := upgradeFixture(t)
			set(f)

			// Configuration the old slot runs with and the new slot rewrites.
			cfg := f.hostPath(goalstates.KubeletConfigurationPath)
			require.NoError(t, os.WriteFile(cfg, []byte("old: config\n"), 0o600))

			err := newNode.Upgrade(t.Context(), oldNode)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "restore shared configuration")

			// resetSharedKubeletState removed these for the new slot; the old slot
			// is started on the identity it had.
			require.FileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath))
			require.FileExists(t, f.hostPath(hostnode.KubeletClientPKIDir+"/kubelet-client-current.pem"))
			require.NoDirExists(t, newNode.configSnapshotDir(), "the snapshot is dropped once restored")
		})
	}
}

func TestUpgradeRestoresSharedConfigurationBeforeTheOldSlotStarts(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	f.h.failOne["unbounded-kube2 systemctl enable --now kubelet.service"] = errors.New("kubelet failed")

	var seen string

	f.h.onCall = func(call string) {
		if strings.Contains(call, "enable --now "+container.UnitName("kube1")) {
			data, err := os.ReadFile(f.hostPath(goalstates.KubeletKubeconfigPath))
			if err == nil {
				seen = string(data)
			}
		}
	}

	require.Error(t, newNode.Upgrade(t.Context(), oldNode))
	require.Equal(t, "old", seen, "the old slot starts with its kubeconfig back")
}

func TestUpgradeSuccessDropsTheSnapshotAndKeepsTheNewFiles(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	require.NoError(t, newNode.Upgrade(t.Context(), oldNode))
	require.NoDirExists(t, newNode.configSnapshotDir())
	require.NoFileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath), "the new slot bootstraps its own credentials")
}

func TestUpgradeRollbackKeepsTheSnapshotWhenTheRestoreFails(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)
	f.h.failOne["unbounded-kube2 systemctl enable --now kubelet.service"] = errors.New("kubelet failed")

	// A snapshot directory that was replaced by a file cannot be read back.
	newNode.p.ConfigSnapshotDir = filepath.Join(f.dir, "snap")

	f.h.onCall = func(call string) {
		if strings.Contains(call, "systemctl stop "+container.UnitName("kube1")) {
			require.NoError(t, os.RemoveAll(newNode.p.ConfigSnapshotDir))
		}
	}

	err := newNode.Upgrade(t.Context(), oldNode)
	require.ErrorContains(t, err, "kubelet failed")
	require.ErrorContains(t, err, "restore shared configuration")
}

func TestUpgradeRefusesToStartWhenTheSharedConfigurationCannotBeSaved(t *testing.T) {
	f, oldNode, newNode := upgradeFixture(t)

	// A named pipe cannot be saved byte for byte.
	require.NoError(t, syscall.Mkfifo(f.hostPath(hostnode.KubeletClientPKIDir+"/pipe"), 0o600))

	require.ErrorContains(t, newNode.Upgrade(t.Context(), oldNode), "save shared configuration")
	require.Equal(t, -1, index(f.h.snapshot(), "systemctl stop "+container.UnitName("kube1"), 0), "the old slot was never touched")
	require.Equal(t, 0, f.hooks.unpack, "nothing was built")
}

func TestUpgradeSnapshotDirDefaultsBelowTheHostRoot(t *testing.T) {
	f := newFixture(t)
	n := f.node("kube2", true)

	require.Equal(t, filepath.Join(f.dir, "host", goalstates.AgentConfigDir, "container-upgrade-config"), n.configSnapshotDir())
}

// With the default runtime path the node checks the runtime before a start, so
// a host without it gets a clear error; a caller that overrides the path is
// not checked.
func TestNodeChecksTheDefaultRuntimeBeforeStarting(t *testing.T) {
	root := t.TempDir()

	n := New(Params{Overrides: Overrides{}, Hooks: hostnode.Hooks{HostRoot: root}})
	check := n.Backend().CheckRuntime
	require.NotNil(t, check)
	require.ErrorIs(t, check(container.DefaultRuntimePath), os.ErrNotExist)

	full := filepath.Join(root, container.DefaultRuntimePath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("x"), 0o755))
	require.NoError(t, check(container.DefaultRuntimePath))

	custom := New(Params{Overrides: Overrides{RuntimePath: "/elsewhere/runc"}})
	require.Nil(t, custom.Backend().CheckRuntime)
}
