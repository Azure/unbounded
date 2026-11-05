// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

func TestRootMapsInstallDirsIntoVersionAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()

	r := Root{Layout: Layout{Base: "/opt/unbounded/node"}, Version: "v1"}
	require.Equal(t, "v1", r.Slot())
	require.Equal(t, "/opt/unbounded/node/v1/bin", r.Path("usr/local/bin"))
	require.Equal(t, "/opt/unbounded/node/v1/bin/kubelet", r.Path("/usr/local/bin/kubelet"))
	require.Equal(t, "/opt/unbounded/node/v1/libexec/unbounded-localdns-supervisor", r.Path("/usr/local/libexec/unbounded-localdns-supervisor"))
	require.Equal(t, "/opt/unbounded/node/v1/bin/coredns", r.Path("/usr/local/bin/../bin/coredns"))

	// Same-path contract: these resolve identically for every consumer.
	require.Equal(t, "/opt/unbounded/node/v1/cni/bin/bridge", r.Path("/opt/cni/bin/bridge"), "the CNI plugins are versioned")

	for _, p := range []string{"/etc/containerd/config.toml", "/var/lib/kubelet/config.yaml", "/etc/cni/net.d", "/run/containerd/containerd.sock", "/etc/containerd/certs.d/_default/hosts.toml"} {
		require.Equal(t, p, r.Path(p))
		require.Equal(t, p, r.Path(p[1:]))
	}

	// A sibling that merely shares a prefix is not the bin directory.
	require.Equal(t, "/usr/local/binary", r.Path("/usr/local/binary"))
}

func TestRenderedDropInsAndLocalDNSGolden(t *testing.T) {
	isolationtest.Golden(t, "path.conf", RenderPathDropIn("/opt/unbounded/node/current/bin"))
	isolationtest.Golden(t, "localdns-deps.conf", RenderLocalDNSDropIn())
	isolationtest.Golden(t, UnitLocalDNS, RenderLocalDNS("/opt/unbounded/node/current/libexec/unbounded-localdns-supervisor"))
}

func TestLocalDNSUnitIsAgentOwnedAndOrdersBeforeNodeUnits(t *testing.T) {
	t.Parallel()

	u := string(RenderLocalDNS("/s"))
	require.Contains(t, u, "Before="+UnitContainerd+" "+UnitKubelet)
	require.Contains(t, u, "After=network.target "+UnitLocalDNSNetwork)
	require.NotContains(t, u, "\nBefore=containerd.service")
	require.NotEqual(t, "localdns.service", UnitLocalDNS)
}

func TestWriteDropInsWithAndWithoutLocalDNS(t *testing.T) {
	dir := t.TempDir()
	spec := UnitSpec{BinDir: "/b/bin", LocalDNS: true}

	paths, err := WriteDropIns(dir, spec)
	require.NoError(t, err)
	require.Len(t, paths, 6)
	require.FileExists(t, filepath.Join(dir, UnitLocalDNS))
	require.FileExists(t, filepath.Join(dir, UnitKubelet+".d", "05-localdns.conf"))
	require.FileExists(t, filepath.Join(dir, UnitContainerd+".d", "00-path.conf"))

	sup, err := os.ReadFile(filepath.Join(dir, UnitLocalDNS))
	require.NoError(t, err)
	require.Contains(t, string(sup), "ExecStart=/b/libexec/unbounded-localdns-supervisor")

	// Turning LocalDNS off removes what it added and nothing else.
	spec.LocalDNS = false
	paths, err = WriteDropIns(dir, spec)
	require.NoError(t, err)
	require.Len(t, paths, 2)
	require.NoFileExists(t, filepath.Join(dir, UnitLocalDNS))
	require.NoFileExists(t, filepath.Join(dir, UnitKubelet+".d", "05-localdns.conf"))
	require.FileExists(t, filepath.Join(dir, UnitKubelet+".d", "00-path.conf"))

	// Idempotent when there is nothing to remove.
	_, err = WriteDropIns(dir, spec)
	require.NoError(t, err)
}

func TestPrepareRunsInstallBeforeUnitsAndDefaultsKubeletConfig(t *testing.T) {
	b, rec := newBackend(t)
	b.Units = UnitSpec{LocalDNS: true}

	var sawUnits bool

	b.Install = func(_ context.Context, r isolation.NodeRoot) error {
		_, err := os.Stat(filepath.Join(b.SystemdDir, UnitKubelet))
		sawUnits = err == nil

		require.Equal(t, "v1", r.Slot())
		require.Equal(t, filepath.Join(b.Layout.VersionDir("v1"), "bin", "kubelet"), r.Path("/usr/local/bin/kubelet"))

		return nil
	}

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	require.False(t, sawUnits, "artifacts are installed before the units that run them")
	require.Equal(t, []string{"systemctl daemon-reload"}, rec.calls)

	unit, err := os.ReadFile(filepath.Join(b.SystemdDir, UnitKubelet))
	require.NoError(t, err)
	require.Contains(t, string(unit), "--config="+DefaultKubeletConfigPath)
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitLocalDNS))
}

func TestPrepareInstallFailureWritesNoUnits(t *testing.T) {
	b, rec := newBackend(t)
	b.Install = func(context.Context, isolation.NodeRoot) error { return errors.New("download failed") }

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.ErrorContains(t, err, "download failed")
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.Empty(t, rec.calls)
}

func TestStackIncludesLocalDNSOnlyWhenInstalled(t *testing.T) {
	b, rec := newBackend(t)
	ctx := context.Background()
	r := root("v1")

	require.NoError(t, b.Start(ctx, r))
	require.Equal(t, []string{
		"systemctl enable --now " + UnitContainerd,
		"systemctl enable --now " + UnitKubelet,
	}, rec.calls)

	rec.calls = nil
	b.Units = UnitSpec{LocalDNS: true}
	_, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	rec.calls = nil

	require.NoError(t, b.Start(ctx, r))
	require.Equal(t, []string{
		"systemctl enable --now " + UnitLocalDNS,
		"systemctl enable --now " + UnitContainerd,
		"systemctl enable --now " + UnitKubelet,
	}, rec.calls)

	rec.calls = nil

	require.NoError(t, b.Stop(ctx, r))
	require.Equal(t, []string{
		"systemctl stop " + UnitKubelet,
		"systemctl stop " + UnitContainerd,
		"systemctl stop " + UnitLocalDNS,
	}, rec.calls)

	rec.calls = nil

	require.NoError(t, b.Restart(ctx, r))
	require.Equal(t, []string{
		"systemctl restart " + UnitLocalDNS,
		"systemctl restart " + UnitContainerd,
		"systemctl restart " + UnitKubelet,
	}, rec.calls)
}

func TestStartRunsGatesBetweenUnitsAndStopsOnFailure(t *testing.T) {
	b, rec := newBackend(t)
	b.Units = UnitSpec{LocalDNS: true}
	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	rec.calls = nil

	var order []string

	b.AfterStart = map[string]func(context.Context) error{
		UnitLocalDNS: func(context.Context) error {
			order = append(order, "dns-ready after "+strconv.Itoa(len(rec.calls))+" calls")
			return nil
		},
		UnitContainerd: func(context.Context) error { return errors.New("import failed") },
	}

	err = b.Start(context.Background(), root("v1"))
	require.ErrorContains(t, err, "import failed")
	require.Equal(t, []string{"dns-ready after 1 calls"}, order, "the gate ran before containerd started")
	require.NotContains(t, rec.calls, "systemctl enable --now "+UnitKubelet, "kubelet does not start behind a failed gate")
}

func TestRestartRunsGates(t *testing.T) {
	b, _ := newBackend(t)

	called := false
	b.AfterRestart = map[string]func(context.Context) error{UnitKubelet: func(context.Context) error { called = true; return errors.New("not ready") }}

	require.ErrorContains(t, b.Restart(context.Background(), root("v1")), "not ready")
	require.True(t, called)
}

func TestDestroyRemovesLocalDNSDropInsAndSkipsAbsentUnits(t *testing.T) {
	b, rec := newBackend(t)
	b.Units = UnitSpec{LocalDNS: true}
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	rec.calls = nil

	require.NoError(t, b.Destroy(ctx, r))

	entries, err := os.ReadDir(b.SystemdDir)
	require.NoError(t, err)
	require.Empty(t, entries, "units and drop-in directories are gone")
	require.Contains(t, rec.calls, "systemctl disable "+UnitLocalDNS)

	// A never-installed node: nothing to stop or disable, only the reload.
	b2, rec2 := newBackend(t)
	require.NoError(t, b2.Destroy(ctx, root("v1")))
	require.Equal(t, []string{"systemctl daemon-reload"}, rec2.calls)
}

func fakeProc(t *testing.T, procs map[int]string) string {
	t.Helper()

	dir := t.TempDir()
	for pid, comm := range procs {
		p := filepath.Join(dir, strconv.Itoa(pid))
		require.NoError(t, os.MkdirAll(p, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(p, "comm"), []byte(comm+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(p, "cgroup"), []byte("0::/system.slice/"+UnitContainerd+"\n"), 0o644))
	}

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "self"), 0o755))

	return dir
}

func TestKillShimsOnlyKillsContainerdShims(t *testing.T) {
	var killed []int

	kill := func(pid int) error { killed = append(killed, pid); return nil }

	proc := fakeProc(t, map[int]string{10: "containerd-shim", 11: "containerd", 12: "bash", 13: "containerd-shim-r", 14: "containerd-shim", 15: "containerd-shim", 16: "containerd-shim", 17: "containerd-shim"})

	// A shim of another containerd (the distro's, docker's) stays in that
	// containerd's cgroup and is not the agent's. Neither is a shim whose
	// cgroup cannot be read.
	require.NoError(t, os.WriteFile(filepath.Join(proc, "14", "cgroup"), []byte("0::/system.slice/containerd.service\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "15", "cgroup"), []byte("0::/system.slice/unbounded-containerd.service.bak\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(proc, "16", "cgroup")))
	// cgroup v1 names the unit in each controller's line.
	require.NoError(t, os.WriteFile(filepath.Join(proc, "17", "cgroup"), []byte("12:pids:/system.slice/"+UnitContainerd+"\n1:name=systemd:/system.slice/"+UnitContainerd+"\n"), 0o644))

	require.NoError(t, killShims(proc, kill))
	require.ElementsMatch(t, []int{10, 13, 17}, killed)

	require.NoError(t, killShims(filepath.Join(proc, "missing"), kill), "no procfs is not an error")

	require.ErrorContains(t, killShims(proc, func(int) error { return errors.New("denied") }), "denied")
}

func TestDestroyKillsShimsOnlyWhenProcIsSet(t *testing.T) {
	var killed []int

	b, _ := newBackend(t)
	b.Kill = func(pid int) error { killed = append(killed, pid); return nil }
	b.Proc = fakeProc(t, map[int]string{7: "containerd-shim"})

	require.NoError(t, b.Destroy(context.Background(), root("v1")))
	require.Equal(t, []int{7}, killed)

	killed = nil
	b.Proc = ""

	require.NoError(t, b.Destroy(context.Background(), root("v1")))
	require.Empty(t, killed)
}

// The upgrade hook prepares the host for the new version; the rollback hook,
// not the upgrade hook, prepares it for the previous one, so the previous
// version never starts on host networking set up for the new configuration.
func TestUpgradeRunsTheUpgradeHookForTheNewVersionAndTheRollbackHookForThePrevious(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	var hooks []string

	record := func(name string) func(context.Context) error {
		return func(context.Context) error {
			cur, err := b.Layout.Current()
			require.NoError(t, err)

			hooks = append(hooks, name+":"+cur)

			return nil
		}
	}
	b.BeforeUpgradeStart = record("upgrade")
	b.BeforeRollbackStart = record("rollback")

	err := b.Upgrade(context.Background(), root("v2"), func(context.Context) error { return errors.New("never ready") })
	require.ErrorContains(t, err, "rolled back to v1")
	require.Equal(t, []string{"upgrade:v2", "rollback:v1"}, hooks)
	require.NotEmpty(t, rec.calls)
}

func TestUpgradeRollbackRestoresConfigurationBeforeThePreviousVersionStarts(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	var order []string

	b.BeforeRollbackStart = func(context.Context) error {
		cur, err := b.Layout.Current()
		require.NoError(t, err)

		order = append(order, "restore with link "+cur+" after "+rec.calls[len(rec.calls)-1])

		return nil
	}

	err := b.Upgrade(context.Background(), root("v2"), func(context.Context) error {
		order = append(order, "wait")
		return errors.New("never ready")
	})
	require.ErrorContains(t, err, "rolled back to v1")
	require.Len(t, order, 2)
	require.Equal(t, "wait", order[0])
	require.Contains(t, order[1], "restore with link v1", "the link is flipped back first")
	require.Contains(t, order[1], "stop", "the new version is stopped first")
}

func TestUpgradeRollbackHookIsNotRunOnSuccess(t *testing.T) {
	b, _ := newBackend(t)
	installVersions(t, b, "v1", "v2")

	b.BeforeRollbackStart = func(context.Context) error {
		t.Fatal("a successful upgrade has nothing to roll back")
		return nil
	}

	require.NoError(t, b.Upgrade(context.Background(), root("v2"), nil))
}

func TestUpgradeRollbackHookFailureIsReportedAndTheRollbackContinues(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	b.BeforeRollbackStart = func(context.Context) error { return errors.New("read-only file system") }

	err := b.Upgrade(context.Background(), root("v2"), func(context.Context) error { return errors.New("never ready") })
	require.ErrorContains(t, err, "restore configuration for v1")
	require.ErrorContains(t, err, "read-only file system")

	cur, cerr := b.Layout.Current()
	require.NoError(t, cerr)
	require.Equal(t, "v1", cur, "the link is back and the previous version is still started")
	require.NotEmpty(t, rec.calls)
}

func TestUpgradeBeforeStartHookFailureRollsBack(t *testing.T) {
	b, _ := newBackend(t)
	installVersions(t, b, "v1", "v2")

	n := 0
	b.BeforeUpgradeStart = func(context.Context) error {
		n++
		if n == 1 {
			return errors.New("cleanup failed")
		}

		return nil
	}

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "cleanup failed")

	cur, cerr := b.Layout.Current()
	require.NoError(t, cerr)
	require.Equal(t, "v1", cur)
}

func TestExecPrefersTheActiveVersionsBinary(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1")

	bin := filepath.Join(b.Layout.BinDir(), "ctr")
	require.NoError(t, os.WriteFile(filepath.Join(b.Layout.VersionDir("v1"), "bin", "ctr"), []byte("#!/bin/sh\n"), 0o755))

	_, err := b.Exec(context.Background(), root("v1"), isolation.ExecSpec{Args: []string{"ctr", "version"}})
	require.NoError(t, err)
	require.Equal(t, bin+" version", rec.calls[len(rec.calls)-1])

	_, err = b.Exec(context.Background(), root("v1"), isolation.ExecSpec{Args: []string{"journalctl", "-n1"}})
	require.NoError(t, err)
	require.Equal(t, "journalctl -n1", rec.calls[len(rec.calls)-1], "commands the version does not ship resolve on PATH")

	_, err = b.Exec(context.Background(), root("v1"), isolation.ExecSpec{Args: []string{"/bin/true"}})
	require.NoError(t, err)
	require.Equal(t, "/bin/true ", rec.calls[len(rec.calls)-1])
}

func TestAgentUnitMapsLocalDNS(t *testing.T) {
	t.Parallel()

	require.Equal(t, UnitLocalDNS, agentUnit("localdns"))
	require.Equal(t, UnitKubelet, agentUnit("kubelet"))
	require.Equal(t, "custom.service", agentUnit("custom.service"))
}

// TestDefaultBackendConformance runs the shared suite against the backend
// NewDefault returns, not a hand built one: every default it sets must leave
// the backend conformant.
func TestDefaultBackendConformance(t *testing.T) {
	isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
		h := &hostUnits{active: map[string]bool{}, fail: map[string]error{}}

		bin := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\necho log line\n"), 0o755))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		// NewDefault's defaults with the layout and runner injected the way
		// the agent injects them. Proc is left at the default to prove Destroy
		// tolerates a real /proc.
		b := NewDefault(slog.New(slog.DiscardHandler))
		b.Kill = func(int) error { return nil }
		b.Layout = Layout{Base: filepath.Join(t.TempDir(), "node")}
		b.SystemdDir = t.TempDir()
		b.Run = h.run
		b.Units = UnitSpec{LocalDNS: true}

		return b, isolationtest.Options{
			Spec:         isolation.NodeSpec{Slot: "v1.34.0", Version: "v1.34.0"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			Fail: func(op isolationtest.Op, err error) {
				verb := map[isolationtest.Op]string{
					isolationtest.OpPrepare: "daemon-reload",
					isolationtest.OpStart:   "enable",
					isolationtest.OpStop:    "stop",
					isolationtest.OpRestart: "restart",
				}[op]

				h.mu.Lock()
				h.fail[verb] = err
				h.mu.Unlock()
			},
		}
	})
}

func TestPreflightCheckPathsOverride(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	b := &Backend{Host: &HostFS{Proc: filepath.Join(t.TempDir(), "proc")}, CheckPaths: []string{file}}
	require.ErrorContains(t, b.Preflight(), "not a directory")

	b.CheckPaths = []string{t.TempDir()}
	require.NoError(t, b.Preflight())
}

func TestUpgradeStopFailureRestoresConfigurationBeforeTheStackStartsAgain(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	rec.fail["systemctl stop "+UnitKubelet] = errors.New("stuck")

	var order []string

	b.BeforeRollbackStart = func(context.Context) error {
		order = append(order, "restore")
		return nil
	}

	b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if strings.HasPrefix(call, "systemctl enable --now") {
			order = append(order, "start")
		}

		return rec.run(ctx, l, name, args...)
	}

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "stuck")
	require.NotEmpty(t, order)
	require.Equal(t, "restore", order[0], "the files are put back before anything starts")
	require.Contains(t, order, "start", "the stack is started again")

	cur, _ := b.Layout.Current()
	require.Equal(t, "v1", cur, "the version did not change")
}

func TestUpgradeStopFailureReportsAConfigurationRestoreFailure(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	rec.fail["systemctl stop "+UnitKubelet] = errors.New("stuck")
	b.BeforeRollbackStart = func(context.Context) error { return errors.New("read-only file system") }

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "stuck")
	require.ErrorContains(t, err, "restore configuration: read-only file system")
}
