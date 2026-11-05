// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

var quiet = hostnodetest.Quiet

// host records host commands and answers them. active tracks unit state the
// way systemd would.
type host struct {
	mu     sync.Mutex
	calls  []string
	active map[string]bool
	fail   map[string]error
}

func (h *host) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	h.calls = append(h.calls, call)

	if err, ok := h.fail[call]; ok {
		return "", err
	}

	if name == "systemctl" && len(args) > 1 {
		unit := args[len(args)-1]

		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "stop":
			delete(h.active, unit)
		case "is-active":
			if !h.active[unit] {
				return "inactive", errors.New("inactive")
			}

			return "active", nil
		}
	}

	return "", nil
}

func (h *host) reset() { h.calls = nil }

type fixture struct {
	t    *testing.T
	h    *host
	node *Node
	root string
	sys  string
	gs   *goalstates.MachineGoalState
	hook struct {
		setup, cleanup, dns, kubelet, download int
		kubeletErr                             error
	}
}

func newFixture(t *testing.T, dns bool, mutate func(*goalstates.MachineGoalState)) *fixture {
	t.Helper()

	f := &fixture{t: t, h: &host{active: map[string]bool{}, fail: map[string]error{}}, root: t.TempDir(), sys: t.TempDir()}

	f.gs = hostnodetest.GoalState("kube1", dns)
	if mutate != nil {
		mutate(f.gs)
	}

	f.node = New(Params{
		Log:       quiet(),
		GoalState: f.gs,
		Overrides: Overrides{Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}, SystemdDir: f.sys, Run: f.h.run, Proc: filepath.Join(t.TempDir(), "noproc"), Download: func(ctx context.Context) error {
			f.hook.download++
			return nil
		}, WaitKubelet: func(context.Context) error { f.hook.kubelet++; return f.hook.kubeletErr }},
		Hooks: hostnode.Hooks{HostRoot: f.root, SetupNetwork: func(context.Context) error { f.hook.setup++; return nil }, CleanupNetwork: func(context.Context) error { f.hook.cleanup++; return nil }, WaitLocalDNS: func(context.Context) error { f.hook.dns++; return nil }},
	})

	return f
}

func (f *fixture) hostPath(p string) string { return filepath.Join(f.root, p) }

func TestVersionForTracksEveryInstalledArtifactInput(t *testing.T) {
	t.Parallel()

	base := func() *goalstates.RootFS {
		return &goalstates.RootFS{HostArch: "amd64", KubernetesVersion: "v1.34.1", ContainerdVersion: "2.1.8", RunCVersion: "1.5.0", CNIPluginVersion: "1.5.1"}
	}

	v := VersionFor(base())
	require.True(t, strings.HasPrefix(v, "1.34.1-"), v)
	require.Equal(t, v, VersionFor(base()), "stable for an unchanged configuration")

	for name, mutate := range map[string]func(*goalstates.RootFS){
		"kubernetes": func(r *goalstates.RootFS) { r.KubernetesVersion = "v1.34.2" },
		"containerd": func(r *goalstates.RootFS) { r.ContainerdVersion = "2.1.9" },
		"runc":       func(r *goalstates.RootFS) { r.RunCVersion = "1.5.1" },
		"cni":        func(r *goalstates.RootFS) { r.CNIPluginVersion = "1.5.2" },
		"arch":       func(r *goalstates.RootFS) { r.HostArch = "arm64" },
		"coredns":    func(r *goalstates.RootFS) { r.LocalDNS = goalstates.LocalDNS{Enabled: true, CoreDNSVersion: "1.12.3"} },
		"download":   func(r *goalstates.RootFS) { r.Downloads = &goalstates.DownloadOverrides{} },
	} {
		r := base()
		mutate(r)
		require.NotEqual(t, v, VersionFor(r), name)
	}

	r := base()
	r.KubernetesVersion = "1.34.1"
	require.Equal(t, v, VersionFor(r), "a leading v does not matter")
}

func TestResolveFailures(t *testing.T) {
	t.Parallel()

	n := New(Params{Log: quiet()})
	_, err := n.Version()
	require.ErrorContains(t, err, "no agent configuration")

	boom := errors.New("gpu not ready")
	n = New(Params{
		Log:   quiet(),
		Hooks: hostnode.Hooks{Resolve: func(string) (*goalstates.MachineGoalState, error) { return nil, boom }},
	})
	_, err = n.GoalState()
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "resolve machine goal state")

	_, err = n.Backend()
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, n.Restart(t.Context()), boom)
	require.ErrorIs(t, n.Upgrade(t.Context()), boom)
}

func TestResolveUsesTheFixedSlot(t *testing.T) {
	t.Parallel()

	var slot string

	n := New(Params{
		Log: quiet(),
		Hooks: hostnode.Hooks{Resolve: func(s string) (*goalstates.MachineGoalState, error) {
			slot = s
			return &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{KubernetesVersion: "1.34.1"}, NodeStart: &goalstates.NodeStart{}}, nil
		}},
	})
	_, err := n.GoalState()
	require.NoError(t, err)
	require.Equal(t, "kube1", slot, "applied config is recorded under kube1 so discovery needs no change")
	require.Equal(t, Slot, slot)
}

func TestGoalStateIsPointedAtTheHost(t *testing.T) {
	f := newFixture(t, false, nil)

	gs, err := f.node.GoalState()
	require.NoError(t, err)

	v, err := f.node.Version()
	require.NoError(t, err)

	bin := f.node.p.Layout.BinDir()
	require.Equal(t, filepath.Join(bin, "containerd"), gs.NodeStart.Containerd.ContainerdBinPath)
	require.Equal(t, filepath.Join(bin, "runc"), gs.NodeStart.Containerd.RuncBinaryPath, "containerd reaches runc through the current link so a flip swaps it")
	require.True(t, gs.NodeStart.HostNative())

	// Installers write into the version; configuration stays at the same path.
	require.Equal(t, filepath.Join(f.node.p.Layout.VersionDir(v), "bin"), gs.RootFS.Root().Path(goalstates.BinDir))
	require.Equal(t, f.hostPath("/opt/cni/bin"), gs.RootFS.Root().Path("opt/cni/bin"))
	require.Equal(t, f.hostPath("/etc/containerd/config.toml"), gs.NodeStart.Root().Path(goalstates.ContainerdConfigPath))

	// Commands run on the host through the backend.
	f.h.reset()

	_, err = gs.NodeStart.Exec(t.Context(), quiet(), "systemctl", "is-active", "x")
	require.ErrorContains(t, err, "inactive")
	require.Equal(t, []string{"systemctl is-active x"}, f.h.calls)
}

func TestPrepareInstallsVersionWritesUnitsAndDoesNotActivate(t *testing.T) {
	f := newFixture(t, true, nil)

	r, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	v, _ := f.node.Version()
	require.Equal(t, v, r.Slot())
	require.Equal(t, 1, f.hook.download)

	for _, u := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS} {
		require.FileExists(t, filepath.Join(f.sys, u))
	}

	cur, err := f.node.p.Layout.Current()
	require.NoError(t, err)
	require.Empty(t, cur)

	// The supervisor lives in the version and runs the version's CoreDNS.
	sup := filepath.Join(f.node.p.Layout.VersionDir(v), "libexec", "unbounded-localdns-supervisor")
	info, err := os.Stat(sup)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0o111)

	data, err := os.ReadFile(sup)
	require.NoError(t, err)
	require.Contains(t, string(data), filepath.Join(f.node.p.Layout.BinDir(), "coredns")+" -conf")

	unit, err := os.ReadFile(filepath.Join(f.sys, none.UnitLocalDNS))
	require.NoError(t, err)
	require.Contains(t, string(unit), "ExecStart="+filepath.Join(f.node.p.Layout.CurrentLink(), "libexec", "unbounded-localdns-supervisor"))
}

func TestPrepareWithoutLocalDNSWritesNoSupervisor(t *testing.T) {
	f := newFixture(t, false, nil)

	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	v, _ := f.node.Version()
	require.NoDirExists(t, filepath.Join(f.node.p.Layout.VersionDir(v), "libexec"))
	require.NoFileExists(t, filepath.Join(f.sys, none.UnitLocalDNS))
}

func TestPrepareDownloadFailureLeavesNoUnits(t *testing.T) {
	f := newFixture(t, false, nil)
	f.node.p.Download = func(context.Context) error { return errors.New("mirror down") }

	_, err := f.node.Prepare(t.Context())
	require.ErrorContains(t, err, "mirror down")

	entries, rerr := os.ReadDir(f.sys)
	require.NoError(t, rerr)
	require.Empty(t, entries)
}

func TestConfigureWritesSharedPathsAndNeverDistroUnits(t *testing.T) {
	f := newFixture(t, true, nil)

	cd, kl, err := f.node.Configure(t.Context())
	require.NoError(t, err)
	require.True(t, cd)
	require.True(t, kl)
	require.Equal(t, 1, f.hook.setup)

	for _, p := range []string{
		"/etc/containerd/config.toml", "/var/lib/kubelet/config.yaml", "/etc/kubernetes/pki/apiserver-client-ca.crt",
		"/var/lib/kubelet/bootstrap-kubeconfig", "/etc/unbounded/localdns/Corefile", "/etc/systemd/system/localdns.slice",
		"/etc/systemd/system/unbounded-kubelet.service.d/20-node-config.conf",
	} {
		require.FileExists(t, f.hostPath(p), p)
	}

	for _, p := range []string{"/etc/systemd/system/kubelet.service", "/etc/systemd/system/containerd.service", "/etc/resolv.conf"} {
		require.NoFileExists(t, f.hostPath(p), p)
	}

	cd, kl, err = f.node.Configure(t.Context())
	require.NoError(t, err)
	require.False(t, cd)
	require.False(t, kl, "a reapply with nothing changed asks for no restarts")
}

func TestConfigureFailureStopsBeforeNetwork(t *testing.T) {
	f := newFixture(t, true, func(gs *goalstates.MachineGoalState) { gs.NodeStart.Kubelet.BootstrapToken = "" })

	_, _, err := f.node.Configure(t.Context())
	require.ErrorContains(t, err, "no kubelet auth method")
	require.Zero(t, f.hook.setup)
}

func TestStartOrdersUnitsAndGates(t *testing.T) {
	f := newFixture(t, true, nil)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(goalstates.KubeletKubeconfigPath)), 0o755))
	require.NoError(t, os.WriteFile(f.hostPath(goalstates.KubeletKubeconfigPath), []byte("kc"), 0o600))

	f.h.reset()
	// Bootstrap completion is observed through the host's test command, which
	// the recording host answers successfully.
	require.NoError(t, f.node.Start(t.Context(), false, false, false))

	require.Equal(t, []string{
		"systemctl enable --now " + none.UnitLocalDNS,
		"systemctl enable --now " + none.UnitContainerd,
		"systemctl enable --now " + none.UnitKubelet,
		"test -s " + goalstates.KubeletKubeconfigPath,
	}, f.h.calls)
	require.Equal(t, 1, f.hook.dns, "LocalDNS is waited on before containerd starts")
}

func TestStartRestartsOnlyChangedServicesOfARunningNode(t *testing.T) {
	f := newFixture(t, false, nil)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	f.h.reset()
	require.NoError(t, f.node.Start(t.Context(), true, false, true))
	require.Contains(t, f.h.calls, "systemctl restart "+none.UnitKubelet)
	require.NotContains(t, f.h.calls, "systemctl restart "+none.UnitContainerd)

	f.h.reset()
	require.NoError(t, f.node.Start(t.Context(), true, true, true))

	var restarts []string

	for _, c := range f.h.calls {
		if strings.HasPrefix(c, "systemctl restart") {
			restarts = append(restarts, c)
		}
	}

	require.Equal(t, []string{"systemctl restart " + none.UnitContainerd, "systemctl restart " + none.UnitKubelet}, restarts, "containerd first")

	f.h.reset()
	require.NoError(t, f.node.Start(t.Context(), false, true, true))
	require.NotContains(t, strings.Join(f.h.calls, "\n"), "restart", "a stack this call started has read the new configuration")
}

func TestStartFailsWhenAUnitDoesNotStart(t *testing.T) {
	f := newFixture(t, false, nil)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	f.h.fail["systemctl enable --now "+none.UnitContainerd] = errors.New("exec format error")
	require.ErrorContains(t, f.node.Start(t.Context(), false, false, false), "exec format error")
	require.NotContains(t, f.h.calls, "systemctl enable --now "+none.UnitKubelet)
}

func TestRestartRestartsStackThenWaitsForKubelet(t *testing.T) {
	f := newFixture(t, true, nil)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	f.h.reset()
	require.NoError(t, f.node.Restart(t.Context()))
	require.Equal(t, []string{
		"systemctl restart " + none.UnitLocalDNS,
		"systemctl restart " + none.UnitContainerd,
		"systemctl restart " + none.UnitKubelet,
	}, f.h.calls)
	require.Equal(t, 1, f.hook.dns)
	require.Equal(t, 1, f.hook.kubelet)
}

func TestRestartReportsKubeletNeverComingBack(t *testing.T) {
	f := newFixture(t, false, nil)
	f.hook.kubeletErr = errors.New("kubelet not active")

	require.ErrorContains(t, f.node.Restart(t.Context()), "kubelet not active")
}

func TestRestartStopsAtFirstFailingUnit(t *testing.T) {
	f := newFixture(t, false, nil)
	f.h.fail["systemctl restart "+none.UnitContainerd] = errors.New("failed")

	require.ErrorContains(t, f.node.Restart(t.Context()), "failed")
	require.Zero(t, f.hook.kubelet)
}

// installPrevious sets up a running node on an earlier version.
func installPrevious(t *testing.T, f *fixture, prev string) {
	t.Helper()

	_, err := f.node.p.Layout.Ensure(prev)
	require.NoError(t, err)
	_, err = f.node.p.Layout.Flip(prev)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(goalstates.KubeletKubeconfigPath)), 0o755))
	require.NoError(t, os.WriteFile(f.hostPath(goalstates.KubeletKubeconfigPath), []byte("old"), 0o600))
	require.NoError(t, os.MkdirAll(f.hostPath("/var/lib/kubelet/pki"), 0o755))
}

func TestUpgradeFlipsCleansNetworkAndResetsIdentity(t *testing.T) {
	f := newFixture(t, true, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	// The new version is bootstrapped, so its kubeconfig appears once kubelet
	// starts; stand in for that when the host is asked to start it.
	f.h.reset()
	require.NoError(t, f.node.Upgrade(t.Context()))

	v, _ := f.node.Version()
	cur, err := f.node.p.Layout.Current()
	require.NoError(t, err)
	require.Equal(t, v, cur)

	require.Equal(t, 1, f.hook.cleanup, "host networking is cleaned between stop and start, as repave does")
	require.Equal(t, 2, f.hook.setup, "once for configure and once re-establishing LocalDNS after the cleanup removed it")
	require.NoFileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath), "stale credentials are dropped so kubelet bootstraps afresh")
	require.NoDirExists(t, f.hostPath("/var/lib/kubelet/pki"))

	// Stop precedes start, and the units start in order.
	joined := strings.Join(f.h.calls, "\n")
	require.Less(t, strings.Index(joined, "stop "+none.UnitKubelet), strings.Index(joined, "enable --now "+none.UnitLocalDNS))
}

func TestUpgradeRollsBackWhenKubeletNeverComesUp(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	f.hook.kubeletErr = errors.New("kubelet crashlooping")

	err := f.node.Upgrade(t.Context())
	require.ErrorContains(t, err, "kubelet crashlooping")
	require.ErrorContains(t, err, "rolled back to 1.33.0-aaaaaaaa")

	cur, cerr := f.node.p.Layout.Current()
	require.NoError(t, cerr)
	require.Equal(t, "1.33.0-aaaaaaaa", cur)
	require.Equal(t, 2, f.hook.cleanup, "the previous version also starts on a cleaned host")
	require.True(t, f.h.active[none.UnitKubelet], "the previous version was started again")
}

func TestUpgradeKeepsCredentialsForDirectKubeconfigAuth(t *testing.T) {
	f := newFixture(t, false, func(gs *goalstates.MachineGoalState) {
		gs.NodeStart.Kubelet.BootstrapToken = ""
		gs.NodeStart.Kubelet.KubeconfigData = []byte("apiVersion: v1\nkind: Config\n")
	})
	installPrevious(t, f, "1.33.0-aaaaaaaa")

	require.NoError(t, f.node.Upgrade(t.Context()))
	require.FileExists(t, f.hostPath(goalstates.KubeletKubeconfigPath), "the raw kubeconfig is rewritten by configure, not removed")
}

func TestUpgradeInstallFailureLeavesRunningVersionAlone(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	f.node.p.Download = func(context.Context) error { return errors.New("no network") }

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "no network")

	cur, err := f.node.p.Layout.Current()
	require.NoError(t, err)
	require.Equal(t, "1.33.0-aaaaaaaa", cur)
	require.Empty(t, f.h.calls, "nothing was stopped")
	require.NoFileExists(t, f.hostPath("/etc/containerd/config.toml"), "configuration is not touched before the install succeeds")
}

func TestPruneVersionsKeepsActive(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "old")

	for _, v := range []string{"older", "new"} {
		_, err := f.node.p.Layout.Ensure(v)
		require.NoError(t, err)
	}

	_, err := f.node.p.Layout.Flip("new")
	require.NoError(t, err)

	require.NoError(t, f.node.PruneVersions())

	versions, err := f.node.p.Layout.Versions()
	require.NoError(t, err)
	require.Equal(t, []string{"new"}, versions)
}

func TestRunningReflectsUnitState(t *testing.T) {
	f := newFixture(t, false, nil)

	ok, err := f.node.Running(t.Context())
	require.NoError(t, err)
	require.False(t, ok)

	f.h.active[none.UnitContainerd] = true
	f.h.active[none.UnitKubelet] = true

	ok, err = f.node.Running(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestBackendConformsToInterface(t *testing.T) {
	f := newFixture(t, false, nil)
	b, err := f.node.Backend()
	require.NoError(t, err)

	var _ isolation.Backend = b

	require.Equal(t, isolation.ModeNone, b.Mode())
}

// seedSharedConfig writes the files the running version uses.
func seedSharedConfig(t *testing.T, f *fixture) {
	t.Helper()

	for p, content := range map[string]string{
		"/etc/containerd/config.toml":          "version = 2 # old\n",
		goalstates.KubeletConfigurationPath:    "old: kubelet config\n",
		"/var/lib/kubelet/pki/kubelet-key.pem": "OLD-KEY",
		"/etc/cni/net.d/10-old.conflist":       "old cni",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(p)), 0o755))
		require.NoError(t, os.WriteFile(f.hostPath(p), []byte(content), 0o600))
	}
}

func readHost(t *testing.T, f *fixture, p string) string {
	t.Helper()

	data, err := os.ReadFile(f.hostPath(p))
	require.NoError(t, err)

	return string(data)
}

func TestUpgradeRollbackPutsTheSharedConfigurationBack(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	seedSharedConfig(t, f)

	f.hook.kubeletErr = errors.New("kubelet crashlooping")

	err := f.node.Upgrade(t.Context())
	require.ErrorContains(t, err, "rolled back to 1.33.0-aaaaaaaa")
	require.NotContains(t, err.Error(), "restore shared configuration")

	// Configure rewrote these and the identity reset removed the credentials.
	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/containerd/config.toml"))
	assert.Equal(t, "old: kubelet config\n", readHost(t, f, goalstates.KubeletConfigurationPath))
	assert.Equal(t, "OLD-KEY", readHost(t, f, "/var/lib/kubelet/pki/kubelet-key.pem"), "the credentials the previous version ran with are back")
	assert.Equal(t, "old", readHost(t, f, goalstates.KubeletKubeconfigPath))
	assert.Equal(t, "old cni", readHost(t, f, "/etc/cni/net.d/10-old.conflist"))
	assert.NoDirExists(t, f.node.configSnapshotDir(), "the snapshot is dropped once restored")
}

func TestUpgradeSuccessKeepsTheNewConfigurationAndDropsTheSnapshot(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	seedSharedConfig(t, f)

	require.NoError(t, f.node.Upgrade(t.Context()))

	assert.NotEqual(t, "version = 2 # old\n", readHost(t, f, "/etc/containerd/config.toml"), "configure wrote the new containerd configuration")
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

func TestUpgradeFailureBeforeTheSwitchRestoresTheSharedConfiguration(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	seedSharedConfig(t, f)

	// Stopping the running version fails, so the backend never flips.
	f.h.fail["systemctl stop "+none.UnitKubelet] = errors.New("stuck")

	err := f.node.Upgrade(t.Context())
	require.ErrorContains(t, err, "stuck")

	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/containerd/config.toml"))
	assert.Equal(t, "OLD-KEY", readHost(t, f, "/var/lib/kubelet/pki/kubelet-key.pem"))
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

func TestUpgradeInstallFailureLeavesTheSharedConfigurationAndNoSnapshot(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	seedSharedConfig(t, f)
	f.node.p.Download = func(context.Context) error { return errors.New("no network") }

	require.ErrorContains(t, f.node.Upgrade(t.Context()), "no network")

	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/containerd/config.toml"))
	assert.NoDirExists(t, f.node.configSnapshotDir())
}

func TestUpgradeRefusesToStartWhenTheSharedConfigurationCannotBeSaved(t *testing.T) {
	f := newFixture(t, false, nil)
	installPrevious(t, f, "1.33.0-aaaaaaaa")
	seedSharedConfig(t, f)

	// A socket in the PKI directory cannot be saved byte for byte.
	l, err := net.Listen("unix", f.hostPath("/var/lib/kubelet/pki/s"))
	require.NoError(t, err)

	defer l.Close() //nolint:errcheck // test cleanup

	f.h.reset()
	require.ErrorContains(t, f.node.Upgrade(t.Context()), "save shared configuration")
	assert.Empty(t, f.h.calls, "nothing was stopped or started")
	assert.Equal(t, 0, f.hook.download, "nothing was installed")
	assert.Equal(t, "version = 2 # old\n", readHost(t, f, "/etc/containerd/config.toml"))
}

func TestUpgradeSnapshotDirDefaultsBelowTheHostRootAndCanBeOverridden(t *testing.T) {
	f := newFixture(t, false, nil)
	assert.Equal(t, filepath.Join(f.root, goalstates.AgentConfigDir, "none-upgrade-config"), f.node.configSnapshotDir())

	f.node.p.ConfigSnapshotDir = "/somewhere/else"
	assert.Equal(t, "/somewhere/else", f.node.configSnapshotDir())
}

// gpuFixture is a fixture whose goal state has an NVIDIA GPU and whose host
// tree has the devices, libraries and (optionally) the toolkit it names.
func gpuFixture(t *testing.T, toolkit bool) *fixture {
	t.Helper()

	return gpuFixtureAt(t, goalstates.NvidiaToolkit{}, toolkit)
}

// gpuFixtureAt is gpuFixture with the toolkit installed at the paths tk names.
func gpuFixtureAt(t *testing.T, tk goalstates.NvidiaToolkit, toolkit bool) *fixture {
	t.Helper()

	f := newFixture(t, false, nil)

	for _, p := range []string{"/dev/nvidia0", "/dev/nvidiactl"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(p)), 0o755))
		require.NoError(t, os.WriteFile(f.hostPath(p), nil, 0o600))
	}

	require.NoError(t, os.MkdirAll(f.hostPath("/usr/lib/x86_64-linux-gnu"), 0o755))

	if toolkit {
		for _, p := range []string{tk.CTK(), tk.ContainerRuntime()} {
			require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(p)), 0o755))
			require.NoError(t, os.WriteFile(f.hostPath(p), nil, 0o755))
		}
	}

	host := goalstates.NvidiaHost{
		Required:       true,
		GPUDevicePaths: []string{"/dev/nvidia0", "/dev/nvidiactl"},
		LibDirMounts:   []goalstates.NvidiaLibDirMount{{HostDir: "/usr/lib/x86_64-linux-gnu", ContainerDir: "/run/host-nvidia/0"}},
		LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1"}},
		DriverVersion:  "580.126.09",
	}

	f.gs.RootFS.Nvidia = host
	f.gs.NodeStart.Nvidia = host
	f.gs.NodeStart.Containerd.NvidiaRuntime = goalstates.NvidiaRuntime{Enabled: true}
	f.gs.NodeStart.NvidiaToolkit = tk

	return f
}

func TestPrepareGivesTheBackendTheMachinesGrants(t *testing.T) {
	f := gpuFixture(t, true)

	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	// A grant the host cannot honor reaches the backend and fails Prepare,
	// instead of being dropped on the way.
	f2 := gpuFixture(t, true)
	f2.gs.RootFS.AdditionalHostMounts = []config.AdditionalHostMount{{Source: "/data", Target: "/models"}}

	_, err = f2.node.Prepare(t.Context())
	require.ErrorContains(t, err, "cannot be honored in isolation mode none")
}

func TestPrepareOnAGPUHostWithoutTheToolkitFailsBeforeAnythingIsWritten(t *testing.T) {
	f := gpuFixture(t, false)

	_, err := f.node.Prepare(t.Context())
	require.ErrorContains(t, err, "NVIDIA Container Toolkit")
	require.NoFileExists(t, filepath.Join(f.sys, none.UnitContainerd))
	require.Empty(t, f.h.calls, "no systemctl call, not even daemon-reload")
}

func TestStartPreparesGPUSupportBeforeAnyUnit(t *testing.T) {
	f := gpuFixture(t, true)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(goalstates.KubeletKubeconfigPath)), 0o755))
	require.NoError(t, os.WriteFile(f.hostPath(goalstates.KubeletKubeconfigPath), []byte("kc"), 0o600))

	f.h.reset()
	require.NoError(t, f.node.Start(t.Context(), false, false, false))

	require.Equal(t, []string{
		goalstates.NvidiaCTKPath + " config --set nvidia-container-runtime.mode=cdi --in-place",
		goalstates.NvidiaCTKPath + " cdi generate --driver-root / --dev-root / --output " + goalstates.CDISpecFile +
			" --disable-hook all --enable-hook create-symlinks --enable-hook update-ldcache",
		"systemctl enable --now " + none.UnitContainerd,
		"systemctl enable --now " + none.UnitKubelet,
		"test -s " + goalstates.KubeletKubeconfigPath,
	}, f.h.calls)
	require.DirExists(t, f.hostPath(goalstates.CDISpecDir))
}

func TestRestartRebuildsTheCDISpec(t *testing.T) {
	f := gpuFixture(t, true)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	f.h.reset()
	require.NoError(t, f.node.Restart(t.Context()))

	require.Contains(t, f.h.calls[1], "cdi generate", "a soft reboot follows the devices present now")
	require.Equal(t, "systemctl restart "+none.UnitContainerd, f.h.calls[2])
}

func TestGPUSetupFailureStopsTheStartBeforeAnyUnit(t *testing.T) {
	f := newFixture(t, false, nil)
	f.node = New(Params{
		Log:       quiet(),
		GoalState: f.gs,
		Overrides: Overrides{Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}, SystemdDir: f.sys, Run: f.h.run, Proc: filepath.Join(t.TempDir(), "noproc"), SetupGPU: func(context.Context) error { return errors.New("no toolkit") }},
		Hooks:     hostnode.Hooks{HostRoot: f.root, SetupNetwork: func(context.Context) error { return nil }, WaitLocalDNS: func(context.Context) error { return nil }},
	})

	require.ErrorContains(t, f.node.Start(t.Context(), false, false, false), "no toolkit")
	require.Empty(t, f.h.calls)
}

func TestAGPUlessNodeRunsNoNvidiaCommands(t *testing.T) {
	f := newFixture(t, false, nil)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(goalstates.KubeletKubeconfigPath)), 0o755))
	require.NoError(t, os.WriteFile(f.hostPath(goalstates.KubeletKubeconfigPath), []byte("kc"), 0o600))
	require.NoError(t, f.node.Start(t.Context(), false, false, false))

	for _, c := range f.h.calls {
		require.NotContains(t, c, "nvidia")
	}

	require.NoDirExists(t, f.hostPath(goalstates.CDISpecDir))
}

func TestAGPUHostWithTheToolkitElsewhereUsesTheConfiguredPaths(t *testing.T) {
	tk := goalstates.NvidiaToolkit{
		CTKPath:              "/usr/local/nvidia/toolkit/nvidia-ctk",
		ContainerRuntimePath: "/usr/local/nvidia/toolkit/nvidia-container-runtime",
	}

	f := gpuFixtureAt(t, tk, true)
	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err, "the default /usr/bin copies are not required when the paths are configured")

	require.NoError(t, os.MkdirAll(filepath.Dir(f.hostPath(goalstates.KubeletKubeconfigPath)), 0o755))
	require.NoError(t, os.WriteFile(f.hostPath(goalstates.KubeletKubeconfigPath), []byte("kc"), 0o600))

	f.h.reset()
	require.NoError(t, f.node.Start(t.Context(), false, false, false))

	require.Equal(t, []string{
		tk.CTKPath + " config --set nvidia-container-runtime.mode=cdi --in-place",
		tk.CTKPath + " cdi generate --driver-root / --dev-root / --output " + goalstates.CDISpecFile +
			" --disable-hook all --enable-hook create-symlinks --enable-hook update-ldcache",
		"systemctl enable --now " + none.UnitContainerd,
		"systemctl enable --now " + none.UnitKubelet,
		"test -s " + goalstates.KubeletKubeconfigPath,
	}, f.h.calls)
}

func TestPrepareNamesAMissingConfiguredToolkit(t *testing.T) {
	tk := goalstates.NvidiaToolkit{CTKPath: "/usr/local/nvidia/toolkit/nvidia-ctk"}

	// The toolkit is at the default paths, not where the configuration says.
	f := gpuFixtureAt(t, goalstates.NvidiaToolkit{}, true)
	f.gs.NodeStart.NvidiaToolkit = tk

	node := New(Params{
		Log:       quiet(),
		GoalState: f.gs,
		Overrides: Overrides{Layout: none.Layout{Base: filepath.Join(t.TempDir(), "node")}, SystemdDir: f.sys, Run: f.h.run, Proc: filepath.Join(t.TempDir(), "noproc")},
		Hooks:     hostnode.Hooks{HostRoot: f.root, SetupNetwork: func(context.Context) error { return nil }, WaitLocalDNS: func(context.Context) error { return nil }},
	})

	_, err := node.Prepare(t.Context())
	require.ErrorContains(t, err, tk.CTKPath)
	require.Empty(t, f.h.calls, "nothing is written or started")
}

// A config-only repave prepares the version that is already active, so the
// supervisor script may be running. Truncating it in place changes what the
// running shell reads; the replacement must be a new file.
func TestPrepareReplacesARunningSupervisorWithoutTruncatingIt(t *testing.T) {
	f := newFixture(t, true, nil)

	_, err := f.node.Prepare(t.Context())
	require.NoError(t, err)

	v, _ := f.node.Version()
	sup := filepath.Join(f.node.p.Layout.VersionDir(v), "libexec", "unbounded-localdns-supervisor")

	old, err := os.Open(sup)
	require.NoError(t, err)

	t.Cleanup(func() { _ = old.Close() })

	want, err := io.ReadAll(old)
	require.NoError(t, err)
	require.NotEmpty(t, want)

	// Different content, so that an in-place rewrite would show.
	require.NoError(t, f.node.writeSupervisorWith([]byte("#!/bin/sh\necho replaced\n")))

	_, err = old.Seek(0, io.SeekStart)
	require.NoError(t, err)

	still, err := io.ReadAll(old)
	require.NoError(t, err)
	require.Equal(t, string(want), string(still), "the open script is untouched")

	now, err := os.ReadFile(sup)
	require.NoError(t, err)
	require.Contains(t, string(now), "replaced")

	info, err := os.Stat(sup)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0o111, "still executable")
}
