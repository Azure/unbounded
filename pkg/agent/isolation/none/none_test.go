// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

func TestRenderedUnitsGolden(t *testing.T) {
	spec := UnitSpec{BinDir: "/opt/unbounded/node/current/bin", KubeletConfigPath: "/var/lib/kubelet/config.yaml"}
	isolationtest.Golden(t, UnitContainerd, RenderContainerd(spec))
	isolationtest.Golden(t, UnitKubelet, RenderKubelet(spec))
}

func TestUnitsAreAgentOwned(t *testing.T) {
	spec := UnitSpec{BinDir: "/b", KubeletConfigPath: "/c"}

	// The kubelet must depend on the agent's containerd, never the distro's.
	k := string(RenderKubelet(spec))
	require.Contains(t, k, UnitContainerd)
	require.NotContains(t, strings.ReplaceAll(k, UnitContainerd, ""), "containerd.service")
	require.NotEqual(t, "kubelet.service", UnitKubelet)
	require.NotEqual(t, "containerd.service", UnitContainerd)
}

func TestWriteUnitsAtomicAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	spec := UnitSpec{BinDir: "/b", KubeletConfigPath: "/c"}

	for range 2 {
		paths, err := WriteUnits(dir, spec)
		require.NoError(t, err)
		require.Len(t, paths, 2)
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "no temp files are left behind")
}

// ---- layout ----

func TestLayoutEnsureFlipAndCurrent(t *testing.T) {
	l := Layout{Base: t.TempDir()}

	cur, err := l.Current()
	require.NoError(t, err)
	require.Empty(t, cur)

	_, err = l.Ensure("v1.34.0")
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(l.VersionDir("v1.34.0"), "bin"))

	prev, err := l.Flip("v1.34.0")
	require.NoError(t, err)
	require.Empty(t, prev)

	_, err = l.Ensure("v1.35.0")
	require.NoError(t, err)

	prev, err = l.Flip("v1.35.0")
	require.NoError(t, err)
	require.Equal(t, "v1.34.0", prev)

	cur, err = l.Current()
	require.NoError(t, err)
	require.Equal(t, "v1.35.0", cur)

	vs, err := l.Versions()
	require.NoError(t, err)
	require.Equal(t, []string{"v1.34.0", "v1.35.0"}, vs, "the current link is not a version")
}

func TestLayoutRejectsBadVersions(t *testing.T) {
	l := Layout{Base: t.TempDir()}

	for _, v := range []string{"", "current", ".", "..", "../x", "a/b"} {
		_, err := l.Ensure(v)
		require.Error(t, err, "version %q", v)
	}

	_, err := l.Flip("v9")
	require.ErrorContains(t, err, "not installed")
}

func TestLayoutRemove(t *testing.T) {
	l := Layout{Base: t.TempDir()}
	_, _ = l.Ensure("a")
	_, _ = l.Ensure("b")
	_, err := l.Flip("a")
	require.NoError(t, err)

	require.ErrorContains(t, l.Remove("a"), "active")
	require.NoError(t, l.Remove("b"))
	require.NoDirExists(t, l.VersionDir("b"))
	require.NoError(t, l.RemoveAll())
	require.NoDirExists(t, l.base())
}

func TestLayoutDefaultBase(t *testing.T) {
	require.Equal(t, DefaultBase+"/current", Layout{}.CurrentLink())
	require.Equal(t, DefaultBase+"/current/bin", Layout{}.BinDir())
}

// ---- preflight ----

func fakeHost(t *testing.T) (HostFS, string) {
	t.Helper()

	root := t.TempDir()
	h := HostFS{
		UnitDirs: []string{filepath.Join(root, "units")},
		BinDirs:  []string{filepath.Join(root, "bin")},
		Proc:     filepath.Join(root, "proc"),
	}

	for _, d := range append(append([]string{}, h.UnitDirs...), h.BinDirs[0], h.Proc) {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	return h, root
}

func TestDetectUnmanagedCleanHost(t *testing.T) {
	h, _ := fakeHost(t)
	require.Empty(t, h.DetectUnmanaged("/opt/unbounded/node"))
}

func TestDetectUnmanagedFindsUnitsBinariesAndProcesses(t *testing.T) {
	h, root := fakeHost(t)

	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], "kubelet.service"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(h.BinDirs[0], "containerd"), nil, 0o755))

	proc := filepath.Join(h.Proc, "4242")
	require.NoError(t, os.MkdirAll(proc, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "comm"), []byte("kubelet\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(root, "usr/bin/kubelet"), filepath.Join(proc, "exe")))

	// Non numeric proc entries and unrelated processes are ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(h.Proc, "self"), 0o755))
	other := filepath.Join(h.Proc, "7")
	require.NoError(t, os.MkdirAll(other, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(other, "comm"), []byte("sshd\n"), 0o644))

	got := strings.Join(h.DetectUnmanaged("/opt/unbounded/node"), "\n")
	require.Contains(t, got, "kubelet unit")
	require.Contains(t, got, "containerd binary")
	require.Contains(t, got, "kubelet is already running as pid 4242")
	require.NotContains(t, got, "sshd")
}

func TestDetectUnmanagedIgnoresAgentOwnedProcess(t *testing.T) {
	h, _ := fakeHost(t)
	base := t.TempDir()

	proc := filepath.Join(h.Proc, "9")
	require.NoError(t, os.MkdirAll(proc, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "comm"), []byte("containerd\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(base, "v1", "bin", "containerd"), filepath.Join(proc, "exe")))

	require.Empty(t, h.DetectUnmanaged(base))
}

// A unit masked to /dev/null cannot start; the nspawn mode leaves the distro's
// containerd and kubelet masked, and a mask is not a foreign stack. A dangling
// link or a link anywhere else still is.
func TestDetectUnmanagedTreatsAMaskedUnitAsAbsent(t *testing.T) {
	h, _ := fakeHost(t)
	require.NoError(t, os.Symlink(os.DevNull, filepath.Join(h.UnitDirs[0], "kubelet.service")))
	require.NoError(t, os.Symlink(os.DevNull, filepath.Join(h.UnitDirs[0], "containerd.service")))
	require.Empty(t, h.DetectUnmanaged("/opt/unbounded/node"))

	for _, u := range []string{"kubelet.service", "containerd.service"} {
		require.NoError(t, os.Remove(filepath.Join(h.UnitDirs[0], u)))
	}

	real := filepath.Join(t.TempDir(), "containerd.service")
	require.NoError(t, os.WriteFile(real, nil, 0o644))
	require.NoError(t, os.Symlink(real, filepath.Join(h.UnitDirs[0], "containerd.service")))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(h.UnitDirs[0], "kubelet.service")))

	got := strings.Join(h.DetectUnmanaged("/opt/unbounded/node"), "\n")
	require.Contains(t, got, "containerd unit")
	require.Contains(t, got, "kubelet unit")
}

// Flip is durable: the link change is synced to the base directory, and a
// version that is not installed is refused before anything changes.
func TestFlipSyncsAndRefusesAMissingVersion(t *testing.T) {
	l := Layout{Base: filepath.Join(t.TempDir(), "node")}
	_, err := l.Ensure("v1")
	require.NoError(t, err)

	prev, err := l.Flip("v1")
	require.NoError(t, err)
	require.Empty(t, prev)

	_, err = l.Flip("v2")
	require.ErrorContains(t, err, "not installed")

	cur, err := l.Current()
	require.NoError(t, err)
	require.Equal(t, "v1", cur)
}

func TestDetectUnmanagedAgentUnitsAreNotForeign(t *testing.T) {
	h, _ := fakeHost(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], UnitKubelet), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], UnitContainerd), nil, 0o644))
	require.Empty(t, h.DetectUnmanaged("/opt/unbounded/node"))
}

func TestPreflightRefusesUnmanagedStack(t *testing.T) {
	h, _ := fakeHost(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.UnitDirs[0], "containerd.service"), nil, 0o644))

	b := &Backend{Layout: Layout{Base: filepath.Join(t.TempDir(), "node")}, Host: &h}
	err := b.Preflight()
	require.ErrorContains(t, err, "not managed by the agent")
	require.ErrorContains(t, err, "use isolation mode nspawn")
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, CheckWritable(dir, filepath.Join(dir, "not", "yet", "created")))

	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	require.ErrorContains(t, CheckWritable(file), "not a directory")

	ro := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(ro, 0o555))

	if os.Geteuid() != 0 {
		require.ErrorContains(t, CheckWritable(ro), "not writable")
	}
}

// ---- backend ----

type recorder struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
}

func (r *recorder) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)

	if err, ok := r.fail[call]; ok {
		return "", err
	}

	return "out", nil
}

func newBackend(t *testing.T) (*Backend, *recorder) {
	t.Helper()

	rec := &recorder{fail: map[string]error{}}
	b := &Backend{
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Layout:     Layout{Base: filepath.Join(t.TempDir(), "node")},
		SystemdDir: t.TempDir(),
		Run:        rec.run,
		Units:      UnitSpec{KubeletConfigPath: "/var/lib/kubelet/config.yaml"},
	}

	return b, rec
}

func TestBackendBasics(t *testing.T) {
	b, _ := newBackend(t)
	require.Equal(t, isolation.ModeNone, b.Mode())

	req := b.Requirements()
	require.Contains(t, req.HostCommands, "systemctl")
	require.NotContains(t, req.WritablePaths, "/usr", "a read-only /usr is supported")
}

func TestPrepareWritesUnitsAndLayout(t *testing.T) {
	b, rec := newBackend(t)

	r, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	require.Equal(t, "v1", r.Slot())
	require.Equal(t, "/etc/containerd/config.toml", r.Path("/etc/containerd/config.toml"), "paths are the identity")

	require.FileExists(t, filepath.Join(b.SystemdDir, UnitContainerd))
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.DirExists(t, filepath.Join(b.Layout.VersionDir("v1"), "bin"))
	require.Equal(t, []string{"systemctl daemon-reload"}, rec.calls)

	unit, err := os.ReadFile(filepath.Join(b.SystemdDir, UnitKubelet))
	require.NoError(t, err)
	require.Contains(t, string(unit), b.Layout.BinDir()+"/kubelet", "units run through the current link")

	cur, err := b.Layout.Current()
	require.NoError(t, err)
	require.Empty(t, cur, "Prepare must not activate")
}

func TestPrepareRejectsBadVersion(t *testing.T) {
	b, _ := newBackend(t)
	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "../x"})
	require.Error(t, err)
}

func TestStartStopRestartOrder(t *testing.T) {
	b, rec := newBackend(t)
	ctx := context.Background()
	r := root("v1")

	require.NoError(t, b.Start(ctx, r))
	require.NoError(t, b.Restart(ctx, r))
	require.NoError(t, b.Stop(ctx, r))

	require.Equal(t, []string{
		"systemctl enable --now " + UnitContainerd,
		"systemctl enable --now " + UnitKubelet,
		"systemctl restart " + UnitContainerd,
		"systemctl restart " + UnitKubelet,
		"systemctl stop " + UnitKubelet,
		"systemctl stop " + UnitContainerd,
	}, rec.calls)
}

func TestStartStopsAtFirstFailure(t *testing.T) {
	b, rec := newBackend(t)
	rec.fail["systemctl enable --now "+UnitContainerd] = errors.New("boom")

	require.ErrorContains(t, b.Start(context.Background(), root("v1")), "boom")
	require.Len(t, rec.calls, 1, "kubelet must not start without containerd")
}

func TestStopAttemptsEveryUnit(t *testing.T) {
	b, rec := newBackend(t)
	rec.fail["systemctl stop "+UnitKubelet] = errors.New("stuck")

	require.ErrorContains(t, b.Stop(context.Background(), root("v1")), "stuck")
	require.Len(t, rec.calls, 2, "containerd is still stopped")
}

func TestStatus(t *testing.T) {
	b, rec := newBackend(t)

	st, err := b.Status(context.Background(), root("v1"))
	require.NoError(t, err)
	require.True(t, st.Running)

	rec.fail["systemctl is-active --quiet "+UnitKubelet] = errors.New("inactive")

	st, err = b.Status(context.Background(), root("v1"))
	require.NoError(t, err)
	require.False(t, st.Running)
	require.Contains(t, st.Detail, UnitKubelet)
}

func TestExec(t *testing.T) {
	b, rec := newBackend(t)

	res, err := b.Exec(context.Background(), root("v1"), isolation.ExecSpec{Args: []string{"ldconfig", "-p"}})
	require.NoError(t, err)
	require.Equal(t, "out", res.Stdout)
	require.Equal(t, []string{"ldconfig -p"}, rec.calls)

	_, err = b.Exec(context.Background(), root("v1"), isolation.ExecSpec{})
	require.Error(t, err)
}

func TestDestroyRemovesEverythingTheAgentInstalled(t *testing.T) {
	b, rec := newBackend(t)
	ctx := context.Background()

	r, err := b.Prepare(ctx, isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	_, err = b.Activate(r)
	require.NoError(t, err)

	rec.calls = nil

	require.NoError(t, b.Destroy(ctx, r))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitContainerd))
	require.NoDirExists(t, b.Layout.base())
	require.Contains(t, rec.calls, "systemctl disable "+UnitKubelet)
	require.Equal(t, "systemctl daemon-reload", rec.calls[len(rec.calls)-1])
}

func TestDestroyIsIdempotent(t *testing.T) {
	b, _ := newBackend(t)
	require.NoError(t, b.Destroy(context.Background(), root("v1")))
	require.NoError(t, b.Destroy(context.Background(), root("v1")))
}

func TestDestroyReportsFailuresButContinues(t *testing.T) {
	b, rec := newBackend(t)
	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	rec.fail["systemctl stop "+UnitKubelet] = errors.New("stuck")

	err = b.Destroy(context.Background(), root("v1"))
	require.ErrorContains(t, err, "stuck")
	require.ErrorContains(t, err, "keeping the installed versions")

	// The unit that did not stop keeps its file, so it can still be named and
	// stopped, and so do the binaries it runs. The rest is removed.
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubelet), "a unit whose stop failed is not removed")
	require.NotContains(t, rec.calls, "systemctl disable "+UnitKubelet)
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitContainerd), "removal proceeds for the units that stopped")
	require.DirExists(t, b.Layout.VersionDir("v1"))

	// Once it stops, a second Destroy finishes the job.
	delete(rec.fail, "systemctl stop "+UnitKubelet)
	require.NoError(t, b.Destroy(context.Background(), root("v1")))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.NoDirExists(t, b.Layout.base())
}

// ---- upgrade ----

func installVersions(t *testing.T, b *Backend, current string, others ...string) {
	t.Helper()

	for _, v := range append([]string{current}, others...) {
		_, err := b.Layout.Ensure(v)
		require.NoError(t, err)
	}

	_, err := b.Layout.Flip(current)
	require.NoError(t, err)
}

func TestUpgradeSuccess(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	ready := 0

	require.NoError(t, b.Upgrade(context.Background(), root("v2"), func(context.Context) error { ready++; return nil }))

	cur, _ := b.Layout.Current()
	require.Equal(t, "v2", cur)
	require.Equal(t, 1, ready)
	require.Equal(t, []string{
		"systemctl stop " + UnitKubelet,
		"systemctl stop " + UnitContainerd,
		"systemctl daemon-reload",
		"systemctl enable --now " + UnitContainerd,
		"systemctl enable --now " + UnitKubelet,
	}, rec.calls, "the units are written for the new version after the old stack stopped")
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
}

func TestUpgradeRollsBackWhenKubeletNeverReady(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	err := b.Upgrade(context.Background(), root("v2"), func(context.Context) error { return errors.New("not Ready") })
	require.ErrorContains(t, err, "not Ready")
	require.ErrorContains(t, err, "rolled back to v1")

	cur, _ := b.Layout.Current()
	require.Equal(t, "v1", cur, "current flipped back")

	// Stop, flip, write units, start, wait fails, stop, flip back, start. The
	// rollback rewrites no units: RollbackUnits is not set.
	require.Len(t, rec.calls, 9)
	require.Equal(t, "systemctl enable --now "+UnitKubelet, rec.calls[len(rec.calls)-1])
}

func TestUpgradeRollsBackWhenStartFails(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	first := true
	rec.fail["systemctl enable --now "+UnitContainerd] = errors.New("bad binary")

	// Only the first start fails; the rollback start must succeed.
	b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "systemctl enable --now "+UnitContainerd && !first {
			return "", nil
		}

		if call == "systemctl enable --now "+UnitContainerd {
			first = false
			return "", errors.New("bad binary")
		}

		return rec.run(ctx, l, name, args...)
	}

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "bad binary")
	require.ErrorContains(t, err, "rolled back to v1")

	cur, _ := b.Layout.Current()
	require.Equal(t, "v1", cur)
}

func TestUpgradeRollbackFailureIsReported(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	rec.fail["systemctl enable --now "+UnitKubelet] = errors.New("kubelet will not start")

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "rollback to v1 failed")
}

func TestUpgradeToUninstalledVersionKeepsRunning(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1")

	err := b.Upgrade(context.Background(), root("v9"), nil)
	require.ErrorContains(t, err, "not installed")

	cur, _ := b.Layout.Current()
	require.Equal(t, "v1", cur)
	require.Equal(t, "systemctl enable --now "+UnitKubelet, rec.calls[len(rec.calls)-1], "the stopped stack is started again")
}

func TestUpgradeFirstInstallHasNothingToRollBackTo(t *testing.T) {
	b, _ := newBackend(t)
	_, err := b.Layout.Ensure("v1")
	require.NoError(t, err)

	err = b.Upgrade(context.Background(), root("v1"), func(context.Context) error { return errors.New("not Ready") })
	require.ErrorContains(t, err, "not Ready")
	require.NotContains(t, err.Error(), "rolled back")
}

func TestUpgradeRollbackSurvivesCanceledContext(t *testing.T) {
	b, _ := newBackend(t)
	installVersions(t, b, "v1", "v2")

	ctx, cancel := context.WithCancel(context.Background())

	var rollbackCtxErr error

	calls := 0
	b.Run = func(c context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		calls++
		// Calls 1-2 stop, 3-4 start; the second stop pair is the rollback.
		if calls > 4 {
			rollbackCtxErr = c.Err()
		}

		return "", nil
	}

	err := b.Upgrade(ctx, root("v2"), func(context.Context) error { cancel(); return ctx.Err() })
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, rollbackCtxErr)
}

// NewDefault is the production wiring: it reaps under the real /proc and
// cgroup mount and uses the default layout.
func TestNewDefault(t *testing.T) {
	b := NewDefault(nil)
	require.Equal(t, isolation.ModeNone, b.Mode())
	require.Equal(t, "/proc", b.Proc)
	require.Equal(t, "/sys/fs/cgroup", b.CgroupRoot)
	require.Equal(t, DefaultBase, b.Layout.base())
	require.Nil(t, b.Kill)
}

// An upgrade ends the old pods after the stack stops and before the hook that
// cleans host networking runs, so no pod outlives the networking it uses.
func TestUpgradeEndsThePodsBeforeCleaningHostNetworking(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	var order []string

	b.Proc = fakeProc(t, map[int]string{7: "containerd-shim"})
	b.Kill = func(pid int) error { order = append(order, fmt.Sprintf("kill %d", pid)); return nil }
	b.BeforeUpgradeStart = func(context.Context) error { order = append(order, "cleanup network"); return nil }

	require.NoError(t, b.Upgrade(context.Background(), root("v2"), nil))
	require.Equal(t, []string{"kill 7", "cleanup network"}, order)
	require.Equal(t, "systemctl stop "+UnitContainerd, rec.calls[1], "the stack stopped first")
}

// A soft reboot restarts the units and leaves the pods running.
func TestRestartLeavesThePodsRunning(t *testing.T) {
	b, _ := newBackend(t)
	installVersions(t, b, "v1")

	killed := 0
	b.Proc = fakeProc(t, map[int]string{7: "containerd-shim"})
	b.Kill = func(int) error { killed++; return nil }

	require.NoError(t, b.Restart(context.Background(), root("v1")))
	require.Zero(t, killed)
}

// A pod that cannot be ended stops the upgrade before the flip, and the old
// version comes back up.
func TestUpgradeThatCannotEndThePodsRestartsTheOldVersion(t *testing.T) {
	b, rec := newBackend(t)
	installVersions(t, b, "v1", "v2")

	b.Proc = fakeProc(t, map[int]string{7: "containerd-shim"})
	b.Kill = func(int) error { return errors.New("denied") }

	err := b.Upgrade(context.Background(), root("v2"), nil)
	require.ErrorContains(t, err, "end the pods before upgrade")

	cur, _ := b.Layout.Current()
	require.Equal(t, "v1", cur)
	require.Equal(t, "systemctl enable --now "+UnitKubelet, rec.calls[len(rec.calls)-1])
}

// LocalDNS on, then an upgrade to a configuration without it that fails: the
// LocalDNS unit is retired only once the stack is down, and the rollback puts
// it back so the previous version starts with the LocalDNS it ran with.
func TestUpgradeRollbackRestoresTheLocalDNSUnitItRetired(t *testing.T) {
	b, rec := newBackend(t)
	b.Units.LocalDNS = true
	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	installVersions(t, b, "v1", "v2")

	previous := b.Units
	b.Units.LocalDNS = false
	b.RollbackUnits = &previous
	rec.calls = nil

	err = b.Upgrade(context.Background(), root("v2"), func(context.Context) error { return errors.New("not Ready") })
	require.ErrorContains(t, err, "rolled back to v1")

	require.Equal(t, "systemctl stop "+UnitLocalDNS, rec.calls[2], "LocalDNS stops with the stack, last")

	stops := 0

	for _, c := range rec.calls {
		if c == "systemctl stop "+UnitLocalDNS {
			stops++
		}
	}

	require.Equal(t, 1, stops, "the retirement does not stop it again")
	require.Contains(t, rec.calls, "systemctl disable "+UnitLocalDNS)
	require.FileExists(t, filepath.Join(b.SystemdDir, UnitLocalDNS), "the rollback wrote the LocalDNS unit back")
	require.Equal(t, "systemctl enable --now "+UnitKubelet, rec.calls[len(rec.calls)-1])
	require.Contains(t, rec.calls, "systemctl enable --now "+UnitLocalDNS, "the previous version starts LocalDNS again")
}

// LocalDNS off, then an upgrade that turns it on and fails: the rollback
// removes the LocalDNS unit the upgrade added.
func TestUpgradeRollbackRemovesTheLocalDNSUnitItAdded(t *testing.T) {
	b, rec := newBackend(t)
	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	installVersions(t, b, "v1", "v2")

	previous := b.Units
	b.Units.LocalDNS = true
	b.RollbackUnits = &previous
	rec.calls = nil

	err = b.Upgrade(context.Background(), root("v2"), func(context.Context) error { return errors.New("not Ready") })
	require.ErrorContains(t, err, "rolled back to v1")
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitLocalDNS))
	require.Equal(t, "systemctl enable --now "+UnitKubelet, rec.calls[len(rec.calls)-1])
	require.NotContains(t, rec.calls[len(rec.calls)-2:], "systemctl enable --now "+UnitLocalDNS)
}

// PrepareVersion installs without touching the units of the running node.
func TestPrepareVersionLeavesTheUnitsAlone(t *testing.T) {
	b, rec := newBackend(t)

	r, err := b.PrepareVersion(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	require.Equal(t, "v1", r.Slot())
	require.DirExists(t, b.Layout.VersionDir("v1"))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, UnitKubelet))
	require.Empty(t, rec.calls)
}
