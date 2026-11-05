// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package containerops is the composition root for the container isolation
// backend. The backend package cannot import the phase packages that unpack the
// OS image, provision the rootfs and configure the node stack, so this package
// binds them to a goal state and wires them into container.Backend.
//
// PROVISIONAL: the container mode is experimental. It is implemented against
// the design (section 8.3) and stubbed hosts; nothing here has run against a
// real runc, systemd or Azure Container Linux node. Comments marked
// PROVISIONAL name assumptions that are not verified.
//
// Layout of a container node. A slot (kube1 or kube2, blue/green as for nspawn)
// is an unpacked OS image under /var/lib/unbounded/containers/<slot> that runs
// systemd as PID 1 in an OCI container supervised by an agent-owned unit. The
// in-rootfs configuration phases are the nspawn ones, pointed at the container
// root through the goal state's RootOverride and, for commands, ExecOverride
// (which runs "<runtime> exec"). Paths under the same-path bind mounts
// (/var/lib/kubelet, /opt/cni, /etc/containerd/certs.d, /run/containerd,
// /sys/fs/bpf) resolve to the host directory so the agent writes where the
// container reads; see container.Root.
//
// Shared state. Because those directories are shared by both slots, a repave
// rewrites the kubelet configuration, kubeconfig and CNI binaries the previous
// slot also sees. Upgrade saves the shared configuration files first and puts
// them back before the previous slot starts again on a rollback. The previous
// slot's rootfs-private state (containerd data, unit drop-ins, binaries) is not
// touched until the new slot is healthy.
package containerops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

// Slot is the slot a first install builds.
const Slot = goalstates.NSpawnMachineKube1

// Params bind a Node to one agent configuration and one slot.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides
	// Slot is kube1 or kube2. Empty means kube1.
	Slot string
	// GoalState, when set, is used instead of resolving one. Bootstrap passes
	// the goal state it has already resolved so GPU and device discovery happens
	// once.
	GoalState *goalstates.MachineGoalState

	// Overrides and Hooks are left zero in production, which is the real host.
	// Tests set them to run a node against a temporary tree and a fake host.
	Overrides
	hostnode.Hooks
}

// Overrides are the container-specific seams of Params.
type Overrides struct {
	// Layout, SystemdDir, RuntimePath and HelperPath are as in container.Backend.
	// An empty HelperPath is the agent's rollback-stable lifecycle helper.
	Layout      container.Layout
	SystemdDir  string
	RuntimePath string
	HelperPath  string
	// Run executes host commands. Nil uses the real host.
	Run container.Runner
	// MountInfoPath is the mount table. Empty means /proc/self/mountinfo.
	MountInfoPath string
	// Host locates the SELinux and cgroup interfaces. Nil is the real host.
	Host *container.HostFS

	// Unpack, InstallRuntime, InstallHelper and ProvisionRootfs replace the
	// real artifact work. Nil does it for real.
	Unpack          func(ctx context.Context, dir string) error
	InstallRuntime  func(ctx context.Context) error
	InstallHelper   func(ctx context.Context) error
	ProvisionRootfs func(ctx context.Context) error

	// PollInterval is the wait between readiness polls. Zero means one second.
	PollInterval time.Duration
	// ReadyTimeout bounds the wait for systemd in the container. Zero means two
	// minutes.
	ReadyTimeout time.Duration
}

// Node is a container node bound to one configuration and slot.
type Node struct {
	p       Params
	slot    string
	backend *container.Backend

	// snapshot is the shared configuration saved by a running Upgrade, until it
	// is restored or discarded.
	snapshot *hostnode.Snapshot

	once sync.Once
	gs   *goalstates.MachineGoalState
	err  error
}

// New returns a Node. Goal state resolution is deferred to first use so that
// operations that never need it (Stop, Status, Destroy) work without a
// configuration.
func New(p Params) *Node {
	if p.Log == nil {
		p.Log = slog.Default()
	}

	slot := p.Slot
	if slot == "" {
		slot = Slot
	}

	helper := p.HelperPath
	if helper == "" {
		helper = goalstates.ResolveHostPaths().NSpawnLifecycleBinary
	}

	// The real runtime is checked before a start. A caller that points the
	// runtime elsewhere (a test, a tool) supplies its own and is not checked.
	var checkRuntime func(string) error
	if p.RuntimePath == "" {
		checkRuntime = func(path string) error {
			_, err := os.Stat(filepath.Join(p.HostRoot, path))

			return err
		}
	}

	return &Node{p: p, slot: slot, backend: &container.Backend{
		Log:           p.Log,
		Layout:        p.Layout,
		RuntimePath:   p.RuntimePath,
		CheckRuntime:  checkRuntime,
		HelperPath:    helper,
		SystemdDir:    p.SystemdDir,
		Host:          p.Host,
		Run:           p.Run,
		SourceRoot:    p.HostRoot,
		MountInfoPath: p.MountInfoPath,
	}}
}

// Backend is the container backend wired to this node. Its Unpack is bound on
// first use of the goal state.
func (n *Node) Backend() *container.Backend { return n.backend }

// GoalState is the resolved goal state with the container overrides applied.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		if err := container.ValidSlot(n.slot); err != nil {
			n.err = err
			return
		}

		n.gs, n.err = n.resolveGoalState()
		if n.err != nil {
			return
		}

		n.applyOverrides()
	})

	return n.err
}

func (n *Node) resolveGoalState() (*goalstates.MachineGoalState, error) {
	return hostnode.GoalStateSource{
		Log: n.p.Log, Config: n.p.Config, Downloads: n.p.Downloads, GoalState: n.p.GoalState, Resolve: n.p.Resolve,
	}.For(n.slot)
}

// applyOverrides points the goal state at the container: the root is the
// slot's container root, commands run through the runtime's exec, and the
// backend unpacks the image and requires the LocalDNS network unit.
func (n *Node) applyOverrides() {
	root, _ := n.backend.NodeRoot(n.slot) //nolint:errcheck // the slot was validated above

	n.gs.RootFS.RootOverride = root
	n.gs.NodeStart.RootOverride = root
	n.gs.NodeStart.InContainer = true
	n.gs.NodeStart.ExecOverride = func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
		res, err := n.backend.Exec(ctx, root, isolation.ExecSpec{Args: args})

		return res.Stdout, err
	}

	n.backend.Unpack = func(ctx context.Context, _ isolation.NodeSpec, dir string) error { return n.unpack(ctx, dir) }

	if n.gs.NodeStart.LocalDNS.Enabled {
		n.backend.UnitRequires = []string{goalstates.LocalDNSNetworkUnit}
	}
}

func (n *Node) root() isolation.NodeRoot {
	r, _ := n.backend.NodeRoot(n.slot) //nolint:errcheck // the slot is validated by resolve and by every backend call

	return r
}

func (n *Node) runtimePath() string {
	if n.p.RuntimePath != "" {
		return n.p.RuntimePath
	}

	return container.DefaultRuntimePath
}

// unpack materializes the OS image into dir. It is the owned variant: a slot
// whose previous unpack was interrupted (no completion marker) is cleaned and
// rebuilt, and a completed one is kept. Callers must never point it at a slot
// that runs a node.
func (n *Node) unpack(ctx context.Context, dir string) error {
	if n.p.Unpack != nil {
		return n.p.Unpack(ctx, dir)
	}

	rf := n.gs.RootFS

	return oci.DownloadOwnedRootFS(n.p.Log, dir, rf.HostArch, rf.OCIImage).Do(ctx)
}

// Prepare builds the slot: installs the host runtime and lifecycle helper,
// unpacks the image, renders the bundle and unit, and provisions the rootfs
// (Kubernetes, CRI and CNI binaries and OS configuration). It does not start
// anything. It is safe to repeat on a slot that is not running.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	if err := n.installRuntime(ctx); err != nil {
		return nil, err
	}

	if err := n.installHelper(ctx); err != nil {
		return nil, err
	}

	r, err := n.backend.Prepare(ctx, isolation.NodeSpec{
		Slot:    n.slot,
		Version: n.gs.RootFS.KubernetesVersion,
		Grants:  n.gs.RootFS.Grants(),
	})
	if err != nil {
		return nil, err
	}

	if err := n.provisionRootfs(ctx); err != nil {
		return nil, err
	}

	return r, nil
}

func (n *Node) installRuntime(ctx context.Context) error {
	if n.p.InstallRuntime != nil {
		return n.p.InstallRuntime(ctx)
	}

	return rootfs.InstallHostRuntime(n.p.Log, n.gs.RootFS, n.runtimePath()).Do(ctx)
}

func (n *Node) installHelper(ctx context.Context) error {
	if n.p.InstallHelper != nil {
		return n.p.InstallHelper(ctx)
	}

	return rootfs.EnsureNSpawnLifecycleHelper().Do(ctx)
}

func (n *Node) provisionRootfs(ctx context.Context) error {
	if n.p.ProvisionRootfs != nil {
		return n.p.ProvisionRootfs(ctx)
	}

	rf := n.gs.RootFS

	return phases.Parallel(n.p.Log,
		rootfs.DownloadKubeBinaries(n.p.Log, rf),
		rootfs.DownloadCRIBinaries(n.p.Log, rf),
		rootfs.DownloadCNIBinaries(n.p.Log, rf),
		rootfs.ConfigureOS(rf),
		rootfs.DisableResolved(rf),
		rootfs.ConfigureLocalDNS(n.p.Log, rf),
	).Do(ctx)
}

// Configure writes the node configuration into the container root (and the
// same-path host directories) and prepares the shared LocalDNS network. It
// reports whether containerd or kubelet configuration differed from what was
// there, so a reapply against a running node restarts only what changed.
func (n *Node) Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	gs := n.gs.NodeStart
	containerd := nodestart.ConfigureContainerdTracked(gs)
	kubelet := nodestart.ConfigureKubeletTracked(gs)

	if err := phases.Parallel(n.p.Log, containerd, kubelet, nodestart.ConfigureLocalDNS(gs)).Do(ctx); err != nil {
		return false, false, err
	}

	if err := n.setupNetwork(ctx); err != nil {
		return false, false, err
	}

	return containerd.Changed(), kubelet.Changed(), nil
}

// Running reports whether the container is up.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := container.ValidSlot(n.slot); err != nil {
		return false, err
	}

	return hostnode.Running(ctx, n.backend, n.root())
}

// Start brings the node up: starts the container, waits for systemd inside it,
// then starts containerd and kubelet and waits for kubelet to bootstrap. When
// the container was already running and its configuration changed, the changed
// service is restarted, containerd first.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	gs := n.gs.NodeStart

	return phases.Serial(n.p.Log,
		hostnode.Func("start-container", func(ctx context.Context) error { return n.backend.Start(ctx, n.root()) }),
		hostnode.Func("wait-container-ready", n.waitReady),
		hostnode.Func("wait-localdns", n.waitLocalDNS),
		nodestart.StartContainerd(n.p.Log, gs),
		nodestart.ImportContainerImages(n.p.Log, gs),
		nodestart.StartKubelet(n.p.Log, gs),
		hostnode.Func("restart-reconfigured-services", func(ctx context.Context) error {
			return n.restartChanged(ctx, wasRunning, containerdChanged, kubeletChanged)
		}),
		nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, gs),
	).Do(ctx)
}

// restartChanged restarts the services whose configuration changed. When this
// start booted the container (wasRunning is false) the services have already
// read the configuration written before it.
func (n *Node) restartChanged(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	return hostnode.RestartChanged(ctx, n.p.Log, wasRunning,
		[]hostnode.ChangedUnit{
			{Unit: goalstates.SystemdUnitContainerd, Changed: containerdChanged},
			{Unit: goalstates.SystemdUnitKubelet, Changed: kubeletChanged},
		},
		func(ctx context.Context, unit string) error {
			if _, err := n.backend.Exec(ctx, n.root(), isolation.ExecSpec{Args: []string{"systemctl", "restart", unit}}); err != nil {
				return fmt.Errorf("systemctl restart %s in %s: %w", unit, n.slot, err)
			}

			return nil
		}, "slot", n.slot)
}

func (n *Node) waitLocalDNS(ctx context.Context) error {
	return n.p.AwaitLocalDNS(ctx, n.p.Log, n.gs.NodeStart)
}

// waitReady waits until systemd inside the container accepts commands and has
// finished starting. PROVISIONAL: "running" and "degraded" are systemd's
// SystemState values once startup is done; a degraded system is accepted
// because an unrelated failed unit must not block the node.
func (n *Node) waitReady(ctx context.Context) error {
	return waitSystemd(ctx, n.backend, n.root(), n.p.PollInterval, n.p.ReadyTimeout)
}

// waitSystemd polls systemd in the container until it has finished booting.
func waitSystemd(ctx context.Context, b *container.Backend, root isolation.NodeRoot, interval, timeout time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}

	if timeout <= 0 {
		timeout = 2 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last string

	for {
		res, err := b.Exec(ctx, root, isolation.ExecSpec{Args: []string{"systemctl", "show", "--property=SystemState", "--value"}})

		switch state := strings.TrimSpace(res.Stdout); {
		case err != nil:
			last = err.Error()
		case state == "running" || state == "degraded":
			return nil
		default:
			last = "system state " + state
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s not ready after %s: %s", root.Slot(), timeout, last)
		case <-time.After(interval):
		}
	}
}

// Stop gracefully stops the node: kubelet and containerd inside first so the
// container stop does not have to kill them, then the container. Stopping a
// stopped node succeeds.
func (n *Node) Stop(ctx context.Context) error {
	if err := container.ValidSlot(n.slot); err != nil {
		return err
	}

	running, err := n.Running(ctx)
	if err != nil {
		return err
	}

	if running {
		for _, unit := range []string{goalstates.SystemdUnitKubelet, goalstates.SystemdUnitContainerd} {
			if _, err := n.backend.Exec(ctx, n.root(), isolation.ExecSpec{Args: []string{"systemctl", "stop", unit}}); err != nil {
				n.p.Log.Warn("failed to pre-stop service (proceeding anyway)", "slot", n.slot, "unit", unit, "error", err)
			}
		}
	}

	return n.backend.Stop(ctx, n.root())
}

// Restart restarts the container only (soft reboot): the unit's hooks
// regenerate the bundle, systemd inside starts the enabled containerd and
// kubelet, and this waits for LocalDNS and an active kubelet. The host is not
// rebooted.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return phases.Serial(n.p.Log,
		hostnode.Func("restart-container", func(ctx context.Context) error { return n.backend.Restart(ctx, n.root()) }),
		hostnode.Func("wait-container-ready", n.waitReady),
		hostnode.Func("wait-localdns", n.waitLocalDNS),
		nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet),
	).Do(ctx)
}

// Destroy stops the node and removes its unit, bundle and rootfs.
func (n *Node) Destroy(ctx context.Context) error {
	if err := container.ValidSlot(n.slot); err != nil {
		return err
	}

	return n.backend.Destroy(ctx, n.root())
}

// Resume starts a slot that was stopped, for rollback: it restores the shared
// LocalDNS network, starts the container and waits for kubelet to be active.
// The slot's rootfs and unit are expected to be intact.
func (n *Node) Resume(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return phases.Serial(n.p.Log,
		hostnode.Func("setup-localdns-network", n.setupNetwork),
		hostnode.Func("start-container", func(ctx context.Context) error { return n.backend.Start(ctx, n.root()) }),
		hostnode.Func("wait-container-ready", n.waitReady),
		hostnode.Func("wait-localdns", n.waitLocalDNS),
		nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet),
	).Do(ctx)
}

// RewriteUnit re-renders the supervising unit for the current helper path and
// reloads systemd without restarting the container.
func (n *Node) RewriteUnit(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.installHelper(ctx); err != nil {
		return err
	}

	return n.backend.RewriteUnit(ctx, n.slot)
}

func (n *Node) setupNetwork(ctx context.Context) error {
	return n.p.SetupLocalDNSNetwork(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) cleanupNetwork(ctx context.Context) error {
	return n.p.CleanupHostNetwork(ctx, n.p.Log)
}

// resetSharedKubeletState prepares the shared kubelet directory for a new slot.
// Pod volume mounts propagate from the stopped container to the host under
// /var/lib/kubelet and outlive it, so they are unmounted, and the previous
// kubelet identity is removed when the node authenticates with a bootstrap
// token, so the new slot bootstraps fresh credentials as a new nspawn machine
// does. Direct kubeconfig and exec credential modes are rewritten by Configure.
// PROVISIONAL: propagation of pod mounts to the host (rshared through the host
// root mount) is the design's assumption.
func (n *Node) resetSharedKubeletState(ctx context.Context) error {
	if err := hostnode.UnmountBelow(ctx, n.p.Log, n.p.Run, n.p.MountInfoPath, hostnode.KubeletDir); err != nil {
		return err
	}

	return hostnode.ResetKubeletIdentity(n.gs.NodeStart)
}

// Upgrade moves the node from old to this node's slot (blue/green): the new
// slot is built beside the running old one, then the old slot is stopped, host
// networking is cleaned as repave does, the shared kubelet state is reset, and
// the new slot is configured, started and waited on. The caller has already
// drained the node and removed it from the cluster, as for an nspawn repave,
// persists the applied config after a nil return, and then retires old.
//
// If anything fails the new slot is removed and the old one is started again,
// and the returned error says whether that worked. The files both slots share
// at the same host paths, such as the kubelet identity, are saved first (see
// confsnap) and restored before the old slot starts again.
func (n *Node) Upgrade(ctx context.Context, old *Node) (err error) {
	if n.slot == old.slot {
		return fmt.Errorf("upgrade needs a different slot than the running %s", old.slot)
	}

	// A slot left by an earlier failed or retired upgrade must not be reused:
	// its rootfs may be a different image. The old slot is untouched here.
	if n.backend.SlotExists(n.slot) {
		if err := n.Destroy(ctx); err != nil {
			return fmt.Errorf("remove stale slot %s: %w", n.slot, err)
		}
	}

	snapshot, err := hostnode.TakeSnapshot(n.p.Log, n.configSnapshotDir(), n.p.HostRoot)
	if err != nil {
		return err
	}

	n.snapshot = snapshot

	defer func() {
		// A snapshot that is still set was not restored, and is kept for that.
		if err == nil && n.snapshot != nil {
			n.snapshot.Discard()

			n.snapshot = nil
		}
	}()

	if _, err := n.Prepare(ctx); err != nil {
		// The old slot has not been touched.
		return n.abandon(ctx, fmt.Errorf("prepare slot %s: %w", n.slot, err))
	}

	if err := old.Stop(ctx); err != nil {
		return n.rollback(ctx, old, fmt.Errorf("stop slot %s: %w", old.slot, err))
	}

	if err := n.cleanupNetwork(ctx); err != nil {
		return n.rollback(ctx, old, fmt.Errorf("clean up network: %w", err))
	}

	if err := n.Activate(ctx); err != nil {
		return n.rollback(ctx, old, err)
	}

	return nil
}

// Activate brings a prepared slot up as the node's only running slot: it resets
// the shared kubelet state so the new kubelet bootstraps fresh credentials,
// configures the node, starts it and waits for kubelet to be active. The caller
// has stopped whatever ran before and cleaned host networking. It is the tail
// of Upgrade, shared with an isolation migration into this mode, and removes
// nothing on failure: the caller decides how to roll back.
func (n *Node) Activate(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.resetSharedKubeletState(ctx); err != nil {
		return err
	}

	containerdChanged, kubeletChanged, err := n.Configure(ctx)
	if err != nil {
		return fmt.Errorf("configure slot %s: %w", n.slot, err)
	}

	if err := n.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
		return fmt.Errorf("start slot %s: %w", n.slot, err)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet).Do(ctx)
}

// abandon removes the half built new slot after a failure before the old slot
// was touched.
func (n *Node) abandon(ctx context.Context, cause error) error {
	var errs []error

	if err := n.Destroy(context.WithoutCancel(ctx)); err != nil {
		errs = append(errs, fmt.Errorf("remove slot %s: %w", n.slot, err))
	}

	if err := n.restoreShared(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(append([]error{cause}, errs...)...)
}

func (n *Node) configSnapshotDir() string {
	return n.p.SnapshotDir("container-upgrade-config")
}

// restoreShared puts the saved shared configuration back and drops the
// snapshot. A failure keeps it.
func (n *Node) restoreShared() error {
	if n.snapshot == nil {
		return nil
	}

	if err := n.snapshot.Restore(); err != nil {
		return err
	}

	n.snapshot.Discard()

	n.snapshot = nil

	return nil
}

// rollback removes the new slot and starts the old one again.
func (n *Node) rollback(ctx context.Context, old *Node, cause error) error {
	ctx = context.WithoutCancel(ctx)

	n.p.Log.Error("upgrade failed, restoring the previous slot", "failed_slot", n.slot, "restore_slot", old.slot, "error", cause)

	var errs []error

	if err := n.Destroy(ctx); err != nil {
		errs = append(errs, fmt.Errorf("remove slot %s: %w", n.slot, err))
	}

	if err := n.cleanupNetwork(ctx); err != nil {
		errs = append(errs, fmt.Errorf("clean up network: %w", err))
	}

	// The new slot is gone, so nothing writes the shared files any more, and the
	// old slot starts on the files it was running with.
	if err := n.restoreShared(); err != nil {
		errs = append(errs, err)
	}

	if err := old.Resume(ctx); err != nil {
		errs = append(errs, fmt.Errorf("restart previous slot %s: %w", old.slot, err))
	}

	if len(errs) > 0 {
		return errors.Join(append([]error{cause}, errs...)...)
	}

	return fmt.Errorf("%w (previous slot %s restored)", cause, old.slot)
}
