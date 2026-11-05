// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package noneops is the composition root for the none isolation backend. The
// backend package cannot import the phase packages that install and configure
// the node stack, so this package resolves the goal state for a host-native
// node, binds the phases to it and injects them into none.Backend as hooks.
//
// Layout of a none node. The agent owns /opt/unbounded/node: each installed
// version is a directory holding bin and libexec, and a current symlink selects
// the active one. Everything else is at the same host path the nspawn machine
// sees (/etc/containerd, /var/lib/kubelet, /opt/cni, /run/containerd), so
// unbounded-net-node, gantry and their charts do not change per mode.
//
// Slot. A none node has no blue/green slot. The slot recorded in applied-config
// (the file name <slot>-applied-config.json) is always kube1, so discovery,
// the uninstall script and the appliedconfig reader work unchanged. The NodeRoot
// slot is the version directory name, which is what Upgrade flips.
package noneops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Slot is the slot name a none node records its applied config under.
const Slot = goalstates.NSpawnMachineKube1

// Params bind a Node to one agent configuration.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides
	// GoalState, when set, is used instead of resolving one. Bootstrap passes
	// the goal state it has already resolved so GPU and device discovery happens
	// once.
	GoalState *goalstates.MachineGoalState

	// Overrides and Hooks are left zero in production, which is the real host.
	// Tests set them to run a node against a temporary tree and a fake host.
	Overrides
	hostnode.Hooks
}

// Overrides are the none-specific seams of Params.
type Overrides struct {
	// Layout is the agent-owned directory. The zero value is the default.
	Layout none.Layout
	// SystemdDir is where units are written. Empty means /etc/systemd/system.
	SystemdDir string
	// Run executes host commands. Nil uses the real host.
	Run none.Runner
	// Proc is the procfs Destroy scans for container shims. Empty means /proc.
	Proc string
	// CgroupRoot is the cgroup v2 mount Destroy kills the pods' cgroups under.
	// Empty means /sys/fs/cgroup when Proc is also empty, and none otherwise.
	CgroupRoot string

	// Download replaces the artifact installers. Nil downloads for real.
	Download func(ctx context.Context) error
	// SetupGPU replaces the host GPU setup that runs before the stack starts.
	SetupGPU func(ctx context.Context) error
	// WaitKubelet overrides the kubelet readiness wait. Nil uses the phase the
	// nspawn mode uses.
	WaitKubelet func(ctx context.Context) error
}

// Node is a none node bound to one configuration.
type Node struct {
	p Params

	once    sync.Once
	gs      *goalstates.MachineGoalState
	err     error
	backend *none.Backend
	version string
}

// New returns a Node. Goal state resolution is deferred to first use so that
// operations that never need it (Stop, Status) work without a configuration.
func New(p Params) *Node {
	if p.Log == nil {
		p.Log = slog.Default()
	}

	return &Node{p: p}
}

// Version is the version directory name of this configuration.
func (n *Node) Version() (string, error) {
	if err := n.resolve(); err != nil {
		return "", err
	}

	return n.version, nil
}

// GoalState is the resolved goal state with the host-native overrides applied.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

// Backend is the none backend wired to this node's phases.
func (n *Node) Backend() (*none.Backend, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend, nil
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		n.gs, n.err = n.resolveGoalState()
		if n.err != nil {
			return
		}

		n.version = VersionFor(n.gs.RootFS)
		n.backend = n.newBackend()
		n.applyOverrides()
	})

	return n.err
}

func (n *Node) resolveGoalState() (*goalstates.MachineGoalState, error) {
	return hostnode.GoalStateSource{
		Log: n.p.Log, Config: n.p.Config, Downloads: n.p.Downloads, GoalState: n.p.GoalState, Resolve: n.p.Resolve,
	}.For(Slot)
}

// applyOverrides points the goal state at the host: the root maps installer
// directories into the version, commands run on the host, and the binaries the
// generated configuration names are reached through the current link.
func (n *Node) applyOverrides() {
	root := n.root()

	n.gs.RootFS.RootOverride = root
	n.gs.NodeStart.RootOverride = root
	n.gs.NodeStart.ExecOverride = func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
		res, err := n.backend.Exec(ctx, root, isolation.ExecSpec{Args: args})

		return res.Stdout, err
	}

	bin := n.p.Layout.BinDir()
	n.gs.NodeStart.Containerd.ContainerdBinPath = filepath.Join(bin, "containerd")
	n.gs.NodeStart.Containerd.RuncBinaryPath = filepath.Join(bin, "runc")
	n.gs.NodeStart.Kubelet.KubeletBinPath = filepath.Join(bin, "kubelet")
}

func (n *Node) root() none.Root {
	return none.Root{Layout: n.p.Layout, Version: n.version, HostRoot: n.p.HostRoot}
}

// VersionFor names the version directory for a rootfs goal state: the
// Kubernetes version plus a digest of everything else that decides which
// binaries are installed, so changing only the containerd or CNI version, the
// architecture or a download override yields a new directory while an unchanged
// configuration reuses the installed one.
func VersionFor(r *goalstates.RootFS) string {
	inputs := struct {
		Containerd, RunC, CNI, CoreDNS, Arch string
		CoreDNSEnabled                       bool
		Downloads                            *goalstates.DownloadOverrides
	}{r.ContainerdVersion, r.RunCVersion, r.CNIPluginVersion, r.LocalDNS.CoreDNSVersion, r.HostArch, r.LocalDNS.Enabled, r.Downloads}

	data, _ := json.Marshal(inputs) //nolint:errcheck // plain data always marshals
	sum := sha256.Sum256(data)

	return strings.TrimPrefix(r.KubernetesVersion, "v") + "-" + hex.EncodeToString(sum[:4])
}

// setupGPU prepares the host for GPU workloads. It does nothing on a node
// without NVIDIA GPUs.
func (n *Node) setupGPU(ctx context.Context) error {
	if n.p.SetupGPU != nil {
		return n.p.SetupGPU(ctx)
	}

	return nodestart.SetupNVIDIAOnHost(n.p.Log, n.gs.NodeStart).Do(ctx)
}

func (n *Node) waitLocalDNS(ctx context.Context) error {
	return n.p.AwaitLocalDNS(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) waitKubelet(ctx context.Context) error {
	if n.p.WaitKubelet != nil {
		return n.p.WaitKubelet(ctx)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, none.UnitKubelet).Do(ctx)
}

func (n *Node) newBackend() *none.Backend {
	proc, cgroups := n.p.Proc, n.p.CgroupRoot
	if proc == "" {
		proc = "/proc"

		if cgroups == "" {
			cgroups = "/sys/fs/cgroup"
		}
	}

	return &none.Backend{
		Log:        n.p.Log,
		Layout:     n.p.Layout,
		SystemdDir: n.p.SystemdDir,
		Run:        n.p.Run,
		Proc:       proc,
		CgroupRoot: cgroups,
		Units: none.UnitSpec{
			KubeletConfigPath: goalstates.KubeletConfigurationPath,
			LocalDNS:          n.gs.NodeStart.LocalDNS.Enabled,
		},
		Install: n.install,
		// The stack is the host: GPU devices and libraries are already visible,
		// so there is nothing to bind. What GPU workloads need is the toolkit's
		// CDI spec, rebuilt at every start from the devices present then.
		BeforeStart:  n.setupGPU,
		HostRoot:     n.p.HostRoot,
		GPUHostPaths: nodestart.NVIDIAToolkitPaths(n.gs.NodeStart.NvidiaToolkit),
		AfterStart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
			none.UnitContainerd: func(ctx context.Context) error {
				return nodestart.ImportContainerImages(n.p.Log, n.gs.NodeStart).Do(ctx)
			},
		},
		AfterRestart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
		},
	}
}

// install places the version's artifacts: the same Kubernetes, CRI, CNI and
// CoreDNS installers the nspawn rootfs uses, pointed at the host root, plus the
// kernel modules the node needs and the LocalDNS supervisor script.
func (n *Node) install(ctx context.Context, _ isolation.NodeRoot) error {
	rf := n.gs.RootFS

	if n.p.Download != nil {
		if err := n.p.Download(ctx); err != nil {
			return err
		}

		return n.writeSupervisor()
	}

	tasks := []phases.Task{
		rootfs.DownloadKubeBinaries(n.p.Log, rf),
		rootfs.DownloadCRIBinaries(n.p.Log, rf),
		rootfs.DownloadCNIBinaries(n.p.Log, rf),
		rootfs.InstallCoreDNS(n.p.Log, rf),
		n.kernelModulesTask(),
	}

	if err := phases.Parallel(n.p.Log, tasks...).Do(ctx); err != nil {
		return err
	}

	return n.writeSupervisor()
}

func (n *Node) writeSupervisor() error {
	if !n.gs.RootFS.LocalDNS.Enabled {
		return nil
	}

	return n.writeSupervisorWith(rootfs.LocalDNSSupervisorScript(filepath.Join(n.p.Layout.BinDir(), "coredns")))
}

// writeSupervisorWith replaces the supervisor script with a new file. The
// version being prepared can be the active one, whose supervisor a running
// LocalDNS unit is reading; truncating it in place would change what that
// shell executes, while a rename leaves the open file as it was.
func (n *Node) writeSupervisorWith(script []byte) error {
	path := n.gs.RootFS.Root().Path(goalstates.LocalDNSSupervisorPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LocalDNS supervisor directory: %w", err)
	}

	if err := fsutil.WriteFileAtomic(path, script, 0o755); err != nil {
		return fmt.Errorf("write LocalDNS supervisor: %w", err)
	}

	return os.Chmod(path, 0o755) //nolint:gosec // executable by the service user
}

// Prepare installs the version and writes the units. It does not activate.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend.Prepare(ctx, isolation.NodeSpec{
		Slot:    n.version,
		Version: n.gs.RootFS.KubernetesVersion,
		Grants:  n.gs.RootFS.Grants(),
	})
}

// Provision installs the version, writes the units and points the current link
// at it. Bootstrap uses it for the first install, where there is no previous
// version to keep running.
func (n *Node) Provision(ctx context.Context) error {
	r, err := n.Prepare(ctx)
	if err != nil {
		return err
	}

	if _, err := n.backend.Activate(r); err != nil {
		return fmt.Errorf("activate %s: %w", r.Slot(), err)
	}

	return nil
}

// Configure writes the node configuration at its host paths and prepares the
// shared LocalDNS network. It reports whether containerd or kubelet
// configuration differed from what was on disk, so a reapply against a running
// node restarts only what changed.
func (n *Node) Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	gs := n.gs.NodeStart
	containerd := nodestart.ConfigureContainerdOnHost(gs, none.ContainerdConfigPath)
	kubelet := nodestart.ConfigureKubeletOnHost(gs, none.UnitKubelet)

	if err := phases.Parallel(n.p.Log, containerd, kubelet, nodestart.ConfigureLocalDNS(gs)).Do(ctx); err != nil {
		return false, false, err
	}

	if err := n.setupNetwork(ctx); err != nil {
		return false, false, err
	}

	return containerd.Changed(), kubelet.Changed(), nil
}

// Running reports whether the whole stack is active.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := n.resolve(); err != nil {
		return false, err
	}

	return hostnode.Running(ctx, n.backend, n.root())
}

// Start brings the stack up and waits for kubelet to bootstrap. When the stack
// was already running and its configuration changed, the changed service is
// restarted, containerd first.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	r := n.root()

	if err := n.backend.Start(ctx, r); err != nil {
		return err
	}

	err := hostnode.RestartChanged(ctx, n.p.Log, wasRunning,
		[]hostnode.ChangedUnit{{Unit: none.UnitContainerd, Changed: containerdChanged}, {Unit: none.UnitKubelet, Changed: kubeletChanged}},
		func(ctx context.Context, unit string) error {
			if _, err := n.backend.Exec(ctx, r, isolation.ExecSpec{Args: []string{"systemctl", "restart", unit}}); err != nil {
				return fmt.Errorf("systemctl restart %s: %w", unit, err)
			}

			return nil
		})
	if err != nil {
		return err
	}

	return n.waitBootstrap(ctx)
}

// waitBootstrap waits for kubelet's client credentials to appear. It runs
// through a serial group so a failure carries the task name the Machine
// condition reason is classified from.
func (n *Node) waitBootstrap(ctx context.Context) error {
	return phases.Serial(n.p.Log, nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, n.gs.NodeStart)).Do(ctx)
}

// Restart restarts the node stack in place (soft reboot): LocalDNS, containerd,
// then kubelet, waiting for LocalDNS after it restarts and for kubelet to be
// active at the end. The host is not rebooted.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.backend.Restart(ctx, n.root()); err != nil {
		return err
	}

	return n.waitKubelet(ctx)
}

// Upgrade installs this configuration's version beside the active one, writes
// the new configuration, and switches over: stop the stack, clean host
// networking as repave does, flip the current link, start, and wait for kubelet.
// If the new version does not come up the link is flipped back and the previous
// version is started again. The caller has already drained the node and removed
// it from the cluster, as for an nspawn repave, and persists the applied config
// after a nil return.
//
// Configuration lives at the same host paths for every version, so the files
// the stacks share are saved before anything is written (see confsnap) and put
// back on every failure: before the flip, and before the previous version
// starts on a rollback.
func (n *Node) Upgrade(ctx context.Context) (err error) {
	if err := n.resolve(); err != nil {
		return err
	}

	snapshot, err := hostnode.TakeSnapshot(n.p.Log, n.configSnapshotDir(), n.p.HostRoot)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil && !snapshot.Restored() {
			// A failure the backend did not roll back from, such as an install or
			// configuration error, or a stop that failed. The snapshot stays when
			// the restore fails, so it can be tried again.
			if rerr := snapshot.Restore(); rerr != nil {
				err = errors.Join(err, rerr)

				return
			}
		}

		snapshot.Discard()
	}()

	r, err := n.Prepare(ctx)
	if err != nil {
		return err
	}

	if _, _, err := n.Configure(ctx); err != nil {
		return err
	}

	n.backend.BeforeRollbackStart = func(context.Context) error { return snapshot.Restore() }

	first := true

	n.backend.BeforeUpgradeStart = func(ctx context.Context) error {
		if err := n.cleanupNetwork(ctx); err != nil {
			return err
		}

		if err := n.setupNetwork(ctx); err != nil {
			return err
		}

		if !first {
			return nil
		}

		first = false

		// A repaved node bootstraps fresh credentials, as the new nspawn machine
		// does. The old identity belongs to the Node object that was deleted.
		return hostnode.ResetKubeletIdentity(n.gs.NodeStart)
	}

	return n.backend.Upgrade(ctx, r, func(ctx context.Context) error {
		if err := n.waitKubelet(ctx); err != nil {
			return err
		}

		return n.waitBootstrap(ctx)
	})
}

func (n *Node) configSnapshotDir() string {
	return n.p.SnapshotDir("none-upgrade-config")
}

// PruneVersions removes every installed version except the active one. It runs
// after an upgrade succeeds, mirroring the removal of the old nspawn machine.
func (n *Node) PruneVersions() error {
	versions, err := n.p.Layout.Versions()
	if err != nil {
		return err
	}

	var errs []error

	for _, v := range versions {
		if err := n.p.Layout.Remove(v); err != nil && !errors.Is(err, none.ErrVersionActive) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (n *Node) setupNetwork(ctx context.Context) error {
	return n.p.SetupLocalDNSNetwork(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) cleanupNetwork(ctx context.Context) error {
	return n.p.CleanupHostNetwork(ctx, n.p.Log)
}

func (n *Node) kernelModulesTask() phases.Task {
	return LoadKernelModules(n.p.Log, n.p.Run)
}

func execSpec(args ...string) isolation.ExecSpec { return isolation.ExecSpec{Args: args} }
