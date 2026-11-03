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

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Runner runs a host command and returns its stdout. It is a seam for tests.
type Runner func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error)

// Backend runs the node stack directly on the host.
type Backend struct {
	Log    *slog.Logger
	Layout Layout
	// SystemdDir is where units are written. Empty means SystemdDir.
	SystemdDir string
	// Host is used by preflight. The zero value is DefaultHostFS.
	Host *HostFS
	// Run executes host commands. Nil uses executil.OutputCmd.
	Run Runner
	// Units is the template input written by Prepare.
	Units UnitSpec

	// Install places the version's artifacts and node configuration. Prepare
	// calls it after the version directory exists and before the units are
	// written. Nil installs nothing, which is what the conformance harness and
	// unit tests want.
	Install func(ctx context.Context, r isolation.NodeRoot) error
	// AfterStart runs after the named unit has been started by Start, for
	// readiness gates such as waiting for LocalDNS between CoreDNS and
	// containerd. A failure stops Start.
	AfterStart map[string]func(ctx context.Context) error
	// AfterRestart is AfterStart for Restart.
	AfterRestart map[string]func(ctx context.Context) error
	// BeforeUpgradeStart runs during Upgrade after the old version has stopped
	// and the link has flipped, before the new version starts, and again before
	// the previous version starts on rollback. The agent uses it to clean up
	// host networking as repave does.
	BeforeUpgradeStart func(ctx context.Context) error
	// BeforeRollbackStart runs during a failed Upgrade before the previous
	// version starts again, so it starts on the configuration it was running
	// with: after the link has flipped back on a rollback, and without a flip
	// when the stop or the activation failed. A failure is reported with the
	// rollback and does not stop it.
	BeforeRollbackStart func(ctx context.Context) error

	// BeforeStart runs at the start of Start and Restart, before any unit is
	// touched. The agent uses it to prepare GPU support, which depends on the
	// devices and driver present at that start. A failure stops the start.
	BeforeStart func(ctx context.Context) error
	// HostRoot prefixes the host paths grants name. Empty is the real host;
	// tests point it at a directory tree.
	HostRoot string
	// GPUHostPaths are host paths that must exist when a grant asks for NVIDIA
	// devices, because the stack cannot run GPU workloads without them. The
	// agent sets the NVIDIA Container Toolkit's tools; empty checks nothing.
	GPUHostPaths []string
	// CheckPaths replaces Requirements().WritablePaths in Preflight when set.
	// Tests and offline tooling use it to probe a temporary tree instead of the
	// real /etc, /opt and /var.
	CheckPaths []string
	// LookPath resolves host commands for PreflightStatic. Nil skips the
	// command check; the agent sets it to exec.LookPath.
	LookPath func(file string) (string, error)
	// Proc is the procfs mount Destroy scans for containerd shims that outlive
	// the stopped units. Empty skips the scan, so tests and tools that build a
	// Backend never signal processes by accident; the agent sets it to /proc.
	Proc string
	// CgroupRoot is the cgroup v2 mount Destroy kills the pods' cgroups under:
	// ending a containerd shim leaves the pod's processes running. Empty skips
	// it for the same reason as Proc; the agent sets it to /sys/fs/cgroup.
	CgroupRoot string
}

var _ isolation.Backend = (*Backend)(nil)

func init() {
	// deps is a *Backend carrying the agent's wiring (layout, hooks, runner).
	// Without it the registry returns a backend with production defaults, which
	// is what a caller that only needs Status, Exec or Logs wants.
	isolation.Register(isolation.ModeNone, func(deps any) (isolation.Backend, error) {
		b := &Backend{Log: slog.Default(), Proc: "/proc", CgroupRoot: "/sys/fs/cgroup"}
		if d, ok := deps.(*Backend); ok && d != nil {
			*b = *d
		}

		return b, nil
	})
}

// nodeUnits are the units every node has, in start order.
var nodeUnits = []string{UnitContainerd, UnitKubelet}

// stack lists the agent-owned units in start order, preceded by LocalDNS when
// its unit is installed. Stop uses the reverse. The unit file is the source of
// truth, so a Backend built in a new process after a restart of the daemon sees
// the same stack as the one that prepared it.
func (b *Backend) stack() []string {
	if b.unitExists(UnitLocalDNS) {
		return append([]string{UnitLocalDNS}, nodeUnits...)
	}

	return nodeUnits
}

func (b *Backend) unitExists(unit string) bool {
	_, err := os.Lstat(filepath.Join(b.unitDir(), unit))

	return err == nil
}

// Mode implements isolation.Backend.
func (*Backend) Mode() isolation.Mode { return isolation.ModeNone }

// Requirements implements isolation.Backend. /usr need not be writable.
func (b *Backend) Requirements() isolation.Requirements {
	return isolation.Requirements{
		HostCommands:  []string{"systemctl"},
		WritablePaths: []string{"/etc", b.Layout.base(), "/var"},
		After:         []string{"network-online.target"},
	}
}

// Preflight refuses to proceed when the host already has a kubelet or
// containerd the agent does not own, or a required path is read only. It
// mutates nothing. It is PreflightStatic followed by PreflightOwnership.
func (b *Backend) Preflight() error {
	return errors.Join(b.PreflightStatic(), b.PreflightOwnership())
}

// PreflightStatic checks what the host must provide whatever it runs now: the
// cgroup filesystem, the required host commands (when LookPath is set) and
// writable /etc, the node base and /var. None of it depends on the absence of
// a kubelet or containerd, so it is valid on a host that is a running node.
// A migration runs it before it resets or stops anything.
func (b *Backend) PreflightStatic() error {
	host := b.hostFS()

	var errs []error

	if host.Cgroup != "" {
		if _, err := os.Stat(host.Cgroup); err != nil {
			errs = append(errs, fmt.Errorf("cgroup filesystem is required at %s: %w", host.Cgroup, err))
		}
	}

	if b.LookPath != nil {
		for _, cmd := range b.Requirements().HostCommands {
			if _, err := b.LookPath(cmd); err != nil {
				errs = append(errs, fmt.Errorf("required command %q not found: %w", cmd, err))
			}
		}
	}

	paths := b.Requirements().WritablePaths
	if b.CheckPaths != nil {
		paths = b.CheckPaths
	}

	if err := CheckWritable(paths...); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// PreflightOwnership refuses a host that already has a kubelet or containerd
// the agent does not own (unit, binary or running process). It fails on a host
// that is a running node, so a migration runs it only after the reset.
func (b *Backend) PreflightOwnership() error {
	findings := b.hostFS().DetectUnmanaged(b.Layout.base())
	if len(findings) == 0 {
		return nil
	}

	errs := make([]error, 0, len(findings)+1)
	for _, f := range findings {
		errs = append(errs, errors.New(f))
	}

	errs = append(errs, errors.New("use isolation mode nspawn or container, or remove the existing kubelet and containerd"))

	return errors.Join(errs...)
}

func (b *Backend) hostFS() HostFS {
	if b.Host != nil {
		return *b.Host
	}

	return DefaultHostFS
}

func (b *Backend) unitDir() string {
	if b.SystemdDir == "" {
		return SystemdDir
	}

	return b.SystemdDir
}

func (b *Backend) logger() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}

	return b.Log
}

func (b *Backend) run(ctx context.Context, name string, args ...string) (string, error) {
	if b.Run != nil {
		return b.Run(ctx, b.logger(), name, args...)
	}

	return executil.OutputCmd(ctx, b.logger(), name, args...)
}

func (b *Backend) systemctl(ctx context.Context, args ...string) error {
	_, err := b.run(ctx, "systemctl", args...)
	return err
}

// root is the NodeRoot of a version, with the default layout. Backend methods
// that only need the slot use it; the Layout-aware Root is returned by Prepare.
func root(version string) isolation.NodeRoot {
	return Root{Version: version}
}

// Prepare installs the version directory and writes the agent-owned units. The
// artifacts themselves are placed in the returned layout's bin directory by the
// installers; the current link is not touched until Activate.
func (b *Backend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	// Refuse a grant the host-native stack cannot honor before anything is
	// written, so a failed Prepare leaves the node as it was.
	if err := b.checkGrants(n.Grants); err != nil {
		return nil, fmt.Errorf("grants: %w", err)
	}

	if _, err := b.Layout.Ensure(n.Slot); err != nil {
		return nil, err
	}

	r := Root{Layout: b.Layout, Version: n.Slot}

	if b.Install != nil {
		if err := b.Install(ctx, r); err != nil {
			return nil, fmt.Errorf("install %s: %w", n.Slot, err)
		}
	}

	units := b.Units
	if units.BinDir == "" {
		units.BinDir = b.Layout.BinDir()
	}

	if units.KubeletConfigPath == "" {
		units.KubeletConfigPath = DefaultKubeletConfigPath
	}

	if !units.LocalDNS {
		if err := b.retireLocalDNS(ctx); err != nil {
			return nil, err
		}
	}

	if _, err := WriteUnits(b.unitDir(), units); err != nil {
		return nil, fmt.Errorf("write units: %w", err)
	}

	if _, err := WriteDropIns(b.unitDir(), units); err != nil {
		return nil, fmt.Errorf("write unit drop-ins: %w", err)
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return nil, fmt.Errorf("daemon-reload: %w", err)
	}

	return r, nil
}

// retireLocalDNS stops and disables a LocalDNS unit left by a configuration
// that had LocalDNS on, before WriteUnits deletes its file. The stack is derived
// from the unit files, so a deleted unit would never be stopped again and
// CoreDNS would keep the node's listeners. A unit that cannot be stopped is not
// deleted.
func (b *Backend) retireLocalDNS(ctx context.Context) error {
	if !b.unitExists(UnitLocalDNS) {
		return nil
	}

	if err := b.systemctl(ctx, "stop", UnitLocalDNS); err != nil {
		return fmt.Errorf("stop %s before removing it: %w", UnitLocalDNS, err)
	}

	if err := b.systemctl(ctx, "disable", UnitLocalDNS); err != nil {
		return fmt.Errorf("disable %s before removing it: %w", UnitLocalDNS, err)
	}

	return nil
}

// Activate points the current link at the root's version and returns the
// version it replaced.
func (b *Backend) Activate(r isolation.NodeRoot) (string, error) {
	return b.Layout.Flip(r.Slot())
}

// Start starts the stack in order, running the AfterStart gate of each unit
// before the next one starts.
func (b *Backend) Start(ctx context.Context, _ isolation.NodeRoot) error {
	if err := b.beforeStart(ctx); err != nil {
		return err
	}

	for _, u := range b.stack() {
		if err := b.systemctl(ctx, "enable", "--now", u); err != nil {
			return fmt.Errorf("start %s: %w", u, err)
		}

		if gate := b.AfterStart[u]; gate != nil {
			if err := gate(ctx); err != nil {
				return fmt.Errorf("wait after starting %s: %w", u, err)
			}
		}
	}

	return nil
}

// Stop stops the stack in reverse order. Every unit is attempted so a failure
// on one does not leave the others running.
func (b *Backend) Stop(ctx context.Context, _ isolation.NodeRoot) error {
	var errs []error

	stack := b.stack()
	for i := len(stack) - 1; i >= 0; i-- {
		if err := b.systemctl(ctx, "stop", stack[i]); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", stack[i], err))
		}
	}

	return errors.Join(errs...)
}

// Restart restarts the stack in start order. The host is never rebooted.
func (b *Backend) Restart(ctx context.Context, _ isolation.NodeRoot) error {
	if err := b.beforeStart(ctx); err != nil {
		return err
	}

	for _, u := range b.stack() {
		if err := b.systemctl(ctx, "restart", u); err != nil {
			return fmt.Errorf("restart %s: %w", u, err)
		}

		if gate := b.AfterRestart[u]; gate != nil {
			if err := gate(ctx); err != nil {
				return fmt.Errorf("wait after restarting %s: %w", u, err)
			}
		}
	}

	return nil
}

// Destroy stops and disables the units, removes them with their drop-ins,
// kills container shims containerd leaves running, and removes the version
// directories (the whole base directory). Units that were never installed are
// skipped, so destroying a half-installed node completes instead of failing on
// systemctl errors for units that do not exist.
func (b *Backend) Destroy(ctx context.Context, _ isolation.NodeRoot) error {
	var errs []error

	owned := []string{UnitLocalDNS, UnitContainerd, UnitKubelet}

	// Reverse start order: kubelet, containerd, LocalDNS.
	for i := len(owned) - 1; i >= 0; i-- {
		if !b.unitExists(owned[i]) {
			continue
		}

		if err := b.systemctl(ctx, "stop", owned[i]); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", owned[i], err))
		}
	}

	if b.Proc != "" {
		if err := killShims(b.Proc); err != nil {
			errs = append(errs, err)
		}
	}

	if b.CgroupRoot != "" {
		if err := killPodCgroups(b.CgroupRoot); err != nil {
			errs = append(errs, err)
		}
	}

	for _, u := range owned {
		if b.unitExists(u) {
			if err := b.systemctl(ctx, "disable", u); err != nil {
				errs = append(errs, fmt.Errorf("disable %s: %w", u, err))
			}
		}

		if err := os.Remove(filepath.Join(b.unitDir(), u)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}

		if err := os.RemoveAll(DropInDir(b.unitDir(), u)); err != nil {
			errs = append(errs, err)
		}
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
	}

	// The active version cannot be removed individually, so a destroy of the
	// whole node removes everything the agent installed.
	if err := b.Layout.RemoveAll(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Status reports running when every unit is active.
func (b *Backend) Status(ctx context.Context, _ isolation.NodeRoot) (isolation.Status, error) {
	for _, u := range b.stack() {
		if _, err := b.run(ctx, "systemctl", "is-active", "--quiet", u); err != nil {
			return isolation.Status{Detail: u + " is not active"}, nil //nolint:nilerr // an inactive unit is a status, not a failure
		}
	}

	return isolation.Status{Running: true}, nil
}

// Exec runs the command on the host, which is the node stack's environment.
func (b *Backend) Exec(ctx context.Context, _ isolation.NodeRoot, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	if len(cmd.Args) == 0 {
		return isolation.ExecResult{}, errors.New("exec: no command")
	}

	out, err := b.run(ctx, b.resolveCommand(cmd.Args[0]), cmd.Args[1:]...)

	return isolation.ExecResult{Stdout: out}, err
}

// resolveCommand prefers the active version's own binary for a bare command
// name, so exec sees the same ctr, crictl and kubectl the node runs. Anything
// else, including every command the version does not ship, resolves on the
// host's PATH as usual.
func (b *Backend) resolveCommand(name string) string {
	if strings.Contains(name, "/") {
		return name
	}

	candidate := filepath.Join(b.Layout.BinDir(), name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return candidate
	}

	return name
}

// Logs streams the journal for an agent-owned unit. With no unit it covers
// both.
func (b *Backend) Logs(ctx context.Context, _ isolation.NodeRoot, opts isolation.LogOptions) (io.ReadCloser, error) {
	args := []string{"--no-pager"}

	if opts.Unit != "" {
		args = append(args, "--unit="+agentUnit(opts.Unit))
	} else {
		for _, u := range b.stack() {
			args = append(args, "--unit="+u)
		}
	}

	args = append(args, isolation.JournalTailArgs(opts)...)

	return isolation.StartLogStream(ctx, "journalctl", args...)
}

// agentUnit maps the names users know to the agent-owned unit names.
func agentUnit(name string) string {
	switch name {
	case "kubelet", "kubelet.service":
		return UnitKubelet
	case "containerd", "containerd.service":
		return UnitContainerd
	case "localdns", "localdns.service":
		return UnitLocalDNS
	default:
		return name
	}
}

// Upgrade switches to an installed version: stop the stack, flip the link,
// start the stack, and wait for ready. If any step after the flip fails the
// link is flipped back and the previous version is started again. The node is
// briefly down.
func (b *Backend) Upgrade(ctx context.Context, r isolation.NodeRoot, waitReady func(context.Context) error) error {
	if err := b.Stop(ctx, r); err != nil {
		// Whatever stopped is restarted so a failed stop does not leave the
		// node down; the version has not changed.
		return errors.Join(fmt.Errorf("stop before upgrade: %w", err), b.restartUnchanged(ctx, r))
	}

	prev, err := b.Activate(r)
	if err != nil {
		return errors.Join(fmt.Errorf("activate %s: %w", r.Slot(), err), b.restartUnchanged(ctx, r))
	}

	err = b.beforeUpgradeStart(ctx)
	if err == nil {
		err = b.Start(ctx, r)
	}

	if err == nil && waitReady != nil {
		err = waitReady(ctx)
	}

	if err == nil {
		return nil
	}

	err = fmt.Errorf("upgrade to %s: %w", r.Slot(), err)

	if prev == "" {
		return err
	}

	// Roll back with a context that survives the cancellation that may have
	// caused the failure.
	rb := context.WithoutCancel(ctx)

	var rbErrs []error

	if serr := b.Stop(rb, r); serr != nil {
		rbErrs = append(rbErrs, serr)
	}

	if _, ferr := b.Layout.Flip(prev); ferr != nil {
		rbErrs = append(rbErrs, fmt.Errorf("flip back to %s: %w", prev, ferr))
	}

	if b.BeforeRollbackStart != nil {
		if herr := b.BeforeRollbackStart(rb); herr != nil {
			rbErrs = append(rbErrs, fmt.Errorf("restore configuration for %s: %w", prev, herr))
		}
	}

	if herr := b.beforeUpgradeStart(rb); herr != nil {
		rbErrs = append(rbErrs, fmt.Errorf("prepare host for %s: %w", prev, herr))
	}

	if serr := b.Start(rb, root(prev)); serr != nil {
		rbErrs = append(rbErrs, fmt.Errorf("start %s: %w", prev, serr))
	}

	if len(rbErrs) > 0 {
		return errors.Join(err, fmt.Errorf("rollback to %s failed: %w", prev, errors.Join(rbErrs...)))
	}

	return fmt.Errorf("%w (rolled back to %s)", err, prev)
}

// restartUnchanged starts the stack again after an upgrade failed before the
// version changed. The configuration is put back first, as on a rollback, so
// the running version does not start on files the upgrade already rewrote.
func (b *Backend) restartUnchanged(ctx context.Context, r isolation.NodeRoot) error {
	rb := context.WithoutCancel(ctx)

	var errs []error

	if b.BeforeRollbackStart != nil {
		if err := b.BeforeRollbackStart(rb); err != nil {
			errs = append(errs, fmt.Errorf("restore configuration: %w", err))
		}
	}

	if err := b.Start(rb, r); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (b *Backend) beforeStart(ctx context.Context) error {
	if b.BeforeStart == nil {
		return nil
	}

	if err := b.BeforeStart(ctx); err != nil {
		return fmt.Errorf("prepare host before start: %w", err)
	}

	return nil
}

func (b *Backend) beforeUpgradeStart(ctx context.Context) error {
	if b.BeforeUpgradeStart == nil {
		return nil
	}

	return b.BeforeUpgradeStart(ctx)
}
