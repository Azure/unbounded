// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package container is the isolation backend that runs the node stack in a
// privileged OCI container with systemd as PID 1. The agent unpacks the rootfs
// image, generates an OCI runtime bundle (config.json), and an agent-owned
// systemd unit supervises it with runc. The host's docker and containerd are
// not used.
//
// PROVISIONAL: the Azure Container Linux node facts in the design (section 13)
// have not been collected. The backend is exercised only against fake runtimes
// and stubbed hosts, never on a real node. Comments marked PROVISIONAL name
// assumptions that have not been verified there.
//
// The agent wires the backend in its mode composition table. Its operations on
// a live host (unpack, start, repave, reset) are composed by
// cmd/agent/internal/containerops, which the daemon and bootstrap use.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// Runner runs a host command and returns its stdout. It is a seam for tests,
// shared with the none backend.
type Runner = none.Runner

// Streamer starts a long running host command and returns its stdout.
type Streamer func(ctx context.Context, name string, args ...string) (io.ReadCloser, error)

// Unpacker materializes the image rootfs into dir. It is injected because the
// OCI unpack code lives in the phase packages, which depend on this package.
type Unpacker func(ctx context.Context, n isolation.NodeSpec, dir string) error

// Backend runs the node stack in a privileged OCI container.
type Backend struct {
	Log    *slog.Logger
	Layout Layout
	// RuntimePath is the runc executable (only runc is supported). Empty means
	// DefaultRuntimePath.
	RuntimePath string
	// CheckRuntime verifies the runtime executable exists before the unit is
	// started, so a missing runtime is a clear error instead of a unit systemd
	// quietly skips. Nil skips the check; the agent sets it.
	CheckRuntime func(path string) error
	// KernelRelease reports the running kernel's release string, for choosing
	// recursive read-only binds. Nil reads <SourceRoot>/proc/sys/kernel/osrelease;
	// an unknown kernel keeps plain "ro".
	KernelRelease func() string
	// HelperPath is the agent binary providing node-lifecycle hooks.
	HelperPath string
	// SystemdDir is where the unit is written. Empty means SystemdDir.
	SystemdDir string
	// Host is used by preflight and SELinux detection. Nil means DefaultHostFS.
	Host *HostFS
	// Run executes host commands. Nil uses executil.OutputCmd.
	Run Runner
	// Stream starts streaming commands (logs). Nil uses os/exec.
	Stream Streamer
	// Unpack materializes the rootfs. Nil requires the rootfs to exist already.
	Unpack Unpacker
	// StatDevice describes a granted device node. Nil stats the real device.
	StatDevice func(path string) (DeviceInfo, error)
	// MountInfoPath is read before removing a rootfs. Empty means /proc/self/mountinfo.
	MountInfoPath string
	// SourceRoot prefixes same-path source directories created by Prepare.
	// Empty means the real host root; tests use a temporary directory.
	SourceRoot string
	// WritableCheck overrides the writable path probe. Nil uses none.CheckWritable.
	WritableCheck func(paths ...string) error
	// RuntimeProvisioned tells Preflight that the agent installs the runtime
	// later in bootstrap, so a missing runtime file is expected on a clean host.
	// The runtime is still required to be executable when it is present.
	RuntimeProvisioned bool
	// RefreshGrants, when set, replaces the recorded grants at Regenerate. The
	// pre-start hook uses it to follow devices and driver libraries that a host
	// reboot or driver change moved since Prepare. It receives the grants
	// recorded by Prepare and returns the ones to render and record from now on.
	RefreshGrants func(ctx context.Context, recorded []isolation.Grant) ([]isolation.Grant, error)
	// UnitRequires lists host units the supervising unit requires and orders
	// after, such as the LocalDNS network unit.
	UnitRequires []string
	// GoneTimeout bounds how long Stop waits for the runtime to report the
	// container gone, before and again after killing it. Zero is
	// DefaultGoneTimeout.
	GoneTimeout time.Duration
	// PollInterval is the wait between runtime state checks. Zero is
	// DefaultPollInterval.
	PollInterval time.Duration
}

// Defaults for Stop's check that the container is gone.
const (
	DefaultGoneTimeout  = 30 * time.Second
	DefaultPollInterval = 500 * time.Millisecond
)

var _ isolation.Backend = (*Backend)(nil)

// NewDefault returns a backend with production defaults, which is what a
// caller that only needs Status, Exec or Logs wants.
func NewDefault(log *slog.Logger) *Backend {
	return &Backend{Log: log}
}

// Mode implements isolation.Backend.
func (*Backend) Mode() isolation.Mode { return isolation.ModeContainer }

// Requirements implements isolation.Backend. The runtime artifact is installed
// under /opt/unbounded by the agent; cgroup v2 and the SELinux label are checked
// by Preflight rather than expressed here.
func (b *Backend) Requirements() isolation.Requirements {
	return isolation.Requirements{
		HostCommands:  []string{"systemctl", b.runtimePath()},
		WritablePaths: []string{"/etc", "/opt", "/var"},
		After:         []string{"network-online.target"},
	}
}

func (b *Backend) runtimePath() string {
	if b.RuntimePath == "" {
		return DefaultRuntimePath
	}

	return b.RuntimePath
}

func (b *Backend) host() HostFS {
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

func (b *Backend) checkWritable(paths ...string) error {
	if b.WritableCheck != nil {
		return b.WritableCheck(paths...)
	}

	return none.CheckWritable(paths...)
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

func (b *Backend) root(slot string) isolation.NodeRoot {
	return Root{SlotName: slot, Dir: b.Layout.RootfsDir(slot), HostRoot: b.SourceRoot}
}

// NodeRoot returns the node root of a slot without preparing it.
func (b *Backend) NodeRoot(slot string) (isolation.NodeRoot, error) {
	if err := ValidSlot(slot); err != nil {
		return nil, err
	}

	return b.root(slot), nil
}

func (b *Backend) statDevice(path string) (DeviceInfo, error) {
	if b.StatDevice != nil {
		return b.StatDevice(path)
	}

	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return DeviceInfo{}, err
	}

	typ := "c"

	switch st.Mode & unix.S_IFMT {
	case unix.S_IFBLK:
		typ = "b"
	case unix.S_IFCHR:
	default:
		return DeviceInfo{}, fmt.Errorf("%s is not a device node", path)
	}

	return DeviceInfo{Type: typ, Major: int64(unix.Major(uint64(st.Rdev))), Minor: int64(unix.Minor(uint64(st.Rdev))), Mode: uint32(st.Mode & 0o777)}, nil //nolint:gosec // device numbers and mode bits fit
}

// RenderBundle writes config.json for the slot from the grants, the current
// SELinux state and the devices on the host. It is idempotent and atomic.
func (b *Backend) RenderBundle(slot string, grants []isolation.Grant) error {
	in := BundleInput{
		Slot:      slot,
		RootfsDir: b.Layout.RootfsDir(slot),
		SELinux:   b.host().SELinuxMode(),
		Grants:    slices.Clone(grants),
		Devices:   map[string]DeviceInfo{},
	}

	for i, g := range grants {
		if g.Kind == isolation.GrantMount {
			// The device paths a device directory allows that exist now get
			// an explicit rule; one that is missing, or a device class, does
			// not, and the container's allow-all baseline covers it.
			for _, allow := range g.DeviceAllow {
				if !filepath.IsAbs(allow) {
					continue
				}

				if info, err := b.statDevice(allow); err == nil {
					in.Devices[allow] = info
				}
			}

			continue
		}

		if g.Kind != isolation.GrantDevice || g.Source == "" {
			continue
		}

		// A device grant can name a directory of device nodes (for example an
		// NVIDIA device directory); there is no node to describe, so it is
		// exposed as a bind mount.
		if fi, err := os.Stat(filepath.Join(b.SourceRoot, g.Source)); err == nil && fi.IsDir() {
			in.Grants[i].Kind = isolation.GrantMount

			continue
		}

		info, err := b.statDevice(g.Source)
		if err != nil {
			return fmt.Errorf("device grant %s: %w", g.Source, err)
		}

		in.Devices[g.Source] = info
	}

	in.ExtraBinds = b.extraBinds()
	in.RecursiveReadOnly = b.recursiveReadOnly()

	data, err := RenderConfig(in)
	if err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(b.Layout.ConfigPath(slot), data, 0o644)
}

// recursiveReadOnly reports whether read-only binds can be "rro": the installed
// runtime and the kernel must both support it. Anything unknown, such as a
// runtime that is not installed yet, keeps "ro"; the pre-start hook renders the
// bundle again, so the answer follows the host.
func (b *Backend) recursiveReadOnly() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := b.run(ctx, b.runtimePath(), "--version")
	if err != nil {
		b.logger().Debug("runtime version unknown; keeping plain read-only binds", "error", err)

		return false
	}

	kernel := ""

	if b.KernelRelease != nil {
		kernel = b.KernelRelease()
	} else if data, err := os.ReadFile(filepath.Join(b.SourceRoot, "/proc/sys/kernel/osrelease")); err == nil {
		kernel = strings.TrimSpace(string(data))
	}

	return supportsRecursiveReadOnly(out, kernel)
}

var (
	runtimeVersionRE = regexp.MustCompile(`version\s+(\d+)\.(\d+)`)
	kernelReleaseRE  = regexp.MustCompile(`^(\d+)\.(\d+)`)
)

// supportsRecursiveReadOnly reports whether "rro" is available: runc 1.1 or
// newer, which understands the option, on Linux 5.12 or newer, which has the
// mount_setattr call it uses. runcVersion is `runc --version` output.
func supportsRecursiveReadOnly(runcVersion, kernelRelease string) bool {
	rm := runtimeVersionRE.FindStringSubmatch(runcVersion)
	km := kernelReleaseRE.FindStringSubmatch(strings.TrimSpace(kernelRelease))

	if rm == nil || km == nil {
		return false
	}

	return atLeast(rm[1], rm[2], 1, 1) && atLeast(km[1], km[2], 5, 12)
}

// atLeast reports whether major.minor is at least wantMajor.wantMinor.
func atLeast(major, minor string, wantMajor, wantMinor int) bool {
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)

	if err1 != nil || err2 != nil {
		return false
	}

	return ma > wantMajor || ma == wantMajor && mi >= wantMinor
}

// extraBinds are the nspawn machine's other host binds. A bind whose source is
// absent is left out, because the runtime refuses a bundle with a missing
// source; the pre-start hook re-renders the bundle, so a source that appears
// later (staged offline archives) is picked up on the next start.
func (b *Backend) extraBinds() []ExtraBind {
	var out []ExtraBind

	for _, e := range []ExtraBind{
		{Source: ImageArchiveHostDir, Target: ImageArchiveDir, ReadOnly: true},
		{Source: KernelModulesDir, Target: KernelModulesDir, ReadOnly: true},
	} {
		if _, err := os.Stat(filepath.Join(b.SourceRoot, e.Source)); err != nil {
			continue
		}

		out = append(out, e)
	}

	return out
}

// Regenerate re-renders config.json from the NodeSpec recorded by Prepare. The
// node-lifecycle pre-start hook calls it so SELinux or device changes since the
// last start are picked up. When RefreshGrants is set the grants it returns
// replace the recorded ones, and are recorded, so the next start begins from
// them.
func (b *Backend) Regenerate(ctx context.Context, slot string) error {
	if err := ValidSlot(slot); err != nil {
		return err
	}

	data, err := os.ReadFile(b.Layout.SpecPath(slot))
	if err != nil {
		return fmt.Errorf("read recorded node spec: %w", err)
	}

	var n isolation.NodeSpec
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("decode recorded node spec: %w", err)
	}

	// /run is a tmpfs, so the same-path sources under it are gone after a host
	// reboot, and runc fails the start when a bind source does not exist.
	if err := b.ensureSamePathDirs(); err != nil {
		return err
	}

	if b.RefreshGrants != nil {
		grants, err := b.RefreshGrants(ctx, n.Grants)
		if err != nil {
			return fmt.Errorf("refresh grants: %w", err)
		}

		if !reflect.DeepEqual(grants, n.Grants) {
			n.Grants = grants

			specData, err := json.Marshal(n)
			if err != nil {
				return err
			}

			if err := fsutil.WriteFileAtomic(b.Layout.SpecPath(slot), specData, 0o644); err != nil {
				return err
			}
		}
	}

	return b.RenderBundle(slot, n.Grants)
}

// ensureSamePathDirs creates the host directories the container binds at the
// same path.
func (b *Backend) ensureSamePathDirs() error {
	for _, s := range SamePathMounts {
		if err := os.MkdirAll(filepath.Join(b.SourceRoot, s.Path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", s.Path, err)
		}
	}

	return nil
}

// Prepare materializes the slot: rootfs, same-path source directories,
// config.json, the recorded node spec and the supervising unit. It is safe to
// repeat.
func (b *Backend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if err := ValidSlot(n.Slot); err != nil {
		return nil, err
	}

	rootfs := b.Layout.RootfsDir(n.Slot)

	if b.Unpack != nil {
		if err := b.Unpack(ctx, n, rootfs); err != nil {
			return nil, fmt.Errorf("unpack rootfs: %w", err)
		}
	} else if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("rootfs %s is not present and no unpacker is configured", rootfs)
	}

	if err := maskUnits(rootfs); err != nil {
		return nil, err
	}

	if err := b.ensureSamePathDirs(); err != nil {
		return nil, err
	}

	specData, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}

	if err := fsutil.WriteFileAtomic(b.Layout.SpecPath(n.Slot), specData, 0o644); err != nil {
		return nil, err
	}

	if err := b.RenderBundle(n.Slot, n.Grants); err != nil {
		return nil, err
	}

	if err := b.writeUnit(n.Slot); err != nil {
		return nil, err
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return nil, fmt.Errorf("daemon-reload: %w", err)
	}

	return b.root(n.Slot), nil
}

// MaskedUnits are masked in the container's rootfs. udev belongs to the host:
// the container sees the host's devices but must not run a second udev that
// processes their events and rewrites their nodes and permissions.
var MaskedUnits = []string{
	"systemd-udevd.service",
	"systemd-udevd-control.socket",
	"systemd-udevd-kernel.socket",
	"systemd-udev-trigger.service",
	"systemd-udev-settle.service",
}

// maskUnits links each MaskedUnits entry to /dev/null under the rootfs's
// /etc/systemd/system, which is what `systemctl mask` does. It is idempotent.
func maskUnits(rootfs string) error {
	dir := filepath.Join(rootfs, "etc/systemd/system")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mask units: %w", err)
	}

	for _, unit := range MaskedUnits {
		path := filepath.Join(dir, unit)

		if target, err := os.Readlink(path); err == nil && target == os.DevNull {
			continue
		}

		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("mask %s: %w", unit, err)
		}

		if err := os.Symlink(os.DevNull, path); err != nil {
			return fmt.Errorf("mask %s: %w", unit, err)
		}
	}

	return nil
}

func (b *Backend) writeUnit(slot string) error {
	unit := RenderUnit(UnitSpec{
		Slot:        slot,
		RuntimePath: b.runtimePath(),
		BundleDir:   b.Layout.BundleDir(slot),
		HelperPath:  b.HelperPath,
		After:       b.Requirements().After,
		Requires:    b.UnitRequires,
	})
	if err := fsutil.WriteFileAtomic(filepath.Join(b.unitDir(), UnitName(slot)), unit, 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	return nil
}

// RewriteUnit re-renders the supervising unit of a prepared slot and reloads
// systemd, without restarting a running container. It is how a moved helper
// path reaches the hooks of an existing unit.
func (b *Backend) RewriteUnit(ctx context.Context, slot string) error {
	if err := ValidSlot(slot); err != nil {
		return err
	}

	if err := b.writeUnit(slot); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}

	return nil
}

// Start enables and starts the unit. Starting a running unit is a no-op. The
// other slot's unit is disabled first, so only this slot comes back at boot,
// and a failed state left by the unit's start limit is cleared, so the agent
// can always start a slot again.
func (b *Backend) Start(ctx context.Context, r isolation.SlotRef) error {
	slot := r.Slot()
	if err := ValidSlot(slot); err != nil {
		return err
	}

	if b.CheckRuntime != nil {
		if err := b.CheckRuntime(b.runtimePath()); err != nil {
			return fmt.Errorf("container runtime %s is not installed: %w", b.runtimePath(), err)
		}
	}

	if other := UnitName(OtherSlot(slot)); b.unitPresent(OtherSlot(slot)) {
		if err := b.systemctl(ctx, "disable", other); err != nil {
			return fmt.Errorf("disable %s before starting %s: %w", other, UnitName(slot), err)
		}
	}

	if err := b.systemctl(ctx, "reset-failed", UnitName(slot)); err != nil {
		b.logger().Debug("reset-failed (best effort)", "unit", UnitName(slot), "error", err)
	}

	if err := b.systemctl(ctx, "enable", "--now", UnitName(slot)); err != nil {
		return fmt.Errorf("start %s: %w", UnitName(slot), err)
	}

	return nil
}

func (b *Backend) unitPresent(slot string) bool {
	_, err := os.Lstat(filepath.Join(b.unitDir(), UnitName(slot)))

	return err == nil
}

// Stop stops and disables the unit, so the slot does not come back at boot,
// and returns only once the runtime reports the container gone. A container
// that outlives the stop is killed and deleted; one that survives that too is
// an error, because it still holds the host paths both slots share. Stopping a
// stopped container succeeds.
func (b *Backend) Stop(ctx context.Context, r isolation.SlotRef) error {
	slot := r.Slot()
	if err := ValidSlot(slot); err != nil {
		return err
	}

	var errs []error

	if err := b.systemctl(ctx, "stop", UnitName(slot)); err != nil {
		errs = append(errs, fmt.Errorf("stop %s: %w", UnitName(slot), err))
	}

	if b.unitPresent(slot) {
		if err := b.systemctl(ctx, "disable", UnitName(slot)); err != nil {
			errs = append(errs, fmt.Errorf("disable %s: %w", UnitName(slot), err))
		}
	}

	if err := b.ensureGone(ctx, slot); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ensureGone waits for the runtime to report the slot's container gone. If it
// does not go, it is killed and its state deleted, and waited for again. The
// stale runtime state of a container that is gone is removed.
func (b *Backend) ensureGone(ctx context.Context, slot string) error {
	if b.waitGone(ctx, slot) {
		b.deleteContainer(ctx, slot)

		return nil
	}

	id := ContainerID(slot)
	b.logger().Warn("container outlived its unit's stop; killing it", "container", id)

	if _, err := b.run(ctx, b.runtimePath(), "kill", "--all", id, "KILL"); err != nil {
		b.logger().Debug("runtime kill (best effort)", "container", id, "error", err)
	}

	b.deleteContainer(ctx, slot)

	if b.waitGone(ctx, slot) {
		return nil
	}

	return fmt.Errorf("container %s is still alive after stopping, killing and deleting it", id)
}

// waitGone polls containerAlive until it reports false or GoneTimeout passes.
func (b *Backend) waitGone(ctx context.Context, slot string) bool {
	timeout := b.GoneTimeout
	if timeout <= 0 {
		timeout = DefaultGoneTimeout
	}

	interval := b.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	deadline := time.Now().Add(timeout)

	for {
		if !b.containerAlive(ctx, slot) {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		select {
		case <-ctx.Done():
			return !b.containerAlive(context.WithoutCancel(ctx), slot)
		case <-time.After(interval):
		}
	}
}

// deleteContainer removes leftover runtime state. A container that does not
// exist is the normal case, so failure is only logged.
func (b *Backend) deleteContainer(ctx context.Context, slot string) {
	if _, err := b.run(ctx, b.runtimePath(), "delete", "--force", ContainerID(slot)); err != nil {
		b.logger().Debug("runtime delete (best effort)", "slot", slot, "error", err)
	}
}

// Restart restarts the container only, never the host.
func (b *Backend) Restart(ctx context.Context, r isolation.SlotRef) error {
	if err := ValidSlot(r.Slot()); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "restart", UnitName(r.Slot())); err != nil {
		return fmt.Errorf("restart %s: %w", UnitName(r.Slot()), err)
	}

	return nil
}

// SlotExists reports whether the slot has a rootfs or a supervising unit on the
// host, even a half built one.
func (b *Backend) SlotExists(slot string) bool {
	if ValidSlot(slot) != nil {
		return false
	}

	for _, p := range []string{b.Layout.RootfsDir(slot), filepath.Join(b.unitDir(), UnitName(slot))} {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}

	return false
}

// Destroy stops the container, then removes the unit, the bundle, the rootfs
// and the slot's bpffs mount. Nothing is removed while the runtime still has
// a live container for the slot: it owns its rootfs, and the binds inside its
// private mount namespace are not in the host mount table that removeRootfs
// checks, so removing the files could delete host data through them. A later
// destroy removes them once the container is gone. The rootfs is also kept
// while anything is mounted below it on the host.
func (b *Backend) Destroy(ctx context.Context, r isolation.SlotRef) error {
	slot := r.Slot()
	if err := ValidSlot(slot); err != nil {
		return err
	}

	var errs []error

	// A slot whose unit was never written (an interrupted Prepare) or is
	// already gone has nothing for systemd to stop; asking would fail with
	// "not loaded" and hide the cleanup that follows.
	unitPresent := b.unitPresent(slot)
	if unitPresent {
		if err := b.Stop(ctx, r); err != nil {
			errs = append(errs, err)
		}
	} else if err := b.ensureGone(ctx, slot); err != nil {
		errs = append(errs, err)
	}

	// Checked whatever the stop reported: a stop that claimed success is not
	// taken on trust before files are deleted.
	if b.containerAlive(ctx, slot) {
		errs = append(errs, fmt.Errorf("container %s is still alive; its unit, bundle and rootfs are kept", ContainerID(slot)))

		return errors.Join(errs...)
	}

	if unitPresent {
		if err := os.Remove(filepath.Join(b.unitDir(), UnitName(slot))); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}

		if err := b.systemctl(ctx, "daemon-reload"); err != nil {
			errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
		}
	}

	if err := b.removeTree(b.Layout.BundleDir(slot)); err != nil {
		errs = append(errs, err)
	}

	if err := b.removeRootfs(slot); err != nil {
		errs = append(errs, err)
	}

	if err := b.removeBPFFS(ctx, slot); err != nil {
		errs = append(errs, err)
	}

	// With the last slot gone the layout directories are empty. Leaving them
	// makes the next bootstrap's existing-deployment check refuse the host.
	// os.Remove fails harmlessly while another slot or file remains.
	for _, dir := range []string{b.Layout.BundleDir(""), b.Layout.RootfsDir("")} {
		_ = os.Remove(dir) //nolint:errcheck // best effort; a non-empty directory is expected
	}

	return errors.Join(errs...)
}

func (b *Backend) mountInfoPath() string {
	if b.MountInfoPath == "" {
		return "/proc/self/mountinfo"
	}

	return b.MountInfoPath
}

// removeBPFFS unmounts and removes the slot's private bpffs mount, which the
// unit's ExecStartPre created on the host.
func (b *Backend) removeBPFFS(ctx context.Context, slot string) error {
	dir := filepath.Join(b.SourceRoot, BPFFSPath(slot))

	mounted, err := mountsBelow(b.mountInfoPath(), dir)
	if err != nil {
		return fmt.Errorf("check bpffs mount %s: %w", dir, err)
	}

	if slices.Contains(mounted, dir) {
		if _, err := b.run(ctx, "umount", dir); err != nil {
			return fmt.Errorf("unmount bpffs %s: %w", dir, err)
		}
	}

	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove bpffs mount point %s: %w", dir, err)
	}

	return nil
}

// containerAlive reports whether the runtime still has a container for the
// slot that has not stopped. A runtime that does not know the container means
// it is gone. Output that cannot be parsed is treated as alive, because the
// answer is used to decide whether files may be deleted.
func (b *Backend) containerAlive(ctx context.Context, slot string) bool {
	out, err := b.run(ctx, b.runtimePath(), "state", ContainerID(slot))
	if err != nil {
		return false
	}

	var st runtimeState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return true
	}

	return st.Status != "stopped"
}

func (b *Backend) removeTree(dir string) error {
	return os.RemoveAll(dir)
}

func (b *Backend) removeRootfs(slot string) error {
	dir := b.Layout.RootfsDir(slot)

	mounted, err := mountsBelow(b.mountInfoPath(), dir)
	if err != nil {
		return fmt.Errorf("refusing to remove %s: %w", dir, err)
	}

	if len(mounted) > 0 {
		return fmt.Errorf("refusing to remove %s: still mounted: %s", dir, strings.Join(mounted, ", "))
	}

	return os.RemoveAll(dir)
}

// mountsBelow returns mount points at or below dir from a mountinfo file.
func mountsBelow(mountinfo, dir string) ([]string, error) {
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return nil, err
	}

	var out []string

	prefix := dir + string(filepath.Separator)

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		mp := unescapeMountinfo(fields[4])
		if mp == dir || strings.HasPrefix(mp, prefix) {
			out = append(out, mp)
		}
	}

	return out, nil
}

// unescapeMountinfo decodes the octal escapes (\040 for a space, \011, \012,
// \134) the kernel writes in mountinfo paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))

				i += 3

				continue
			}
		}

		b.WriteByte(s[i])
	}

	return b.String()
}

// runtimeState is the subset of `runtime state` output used by Status.
type runtimeState struct {
	Status string `json:"status"`
}

// Status reports running when the unit is active and the runtime reports the
// container running.
func (b *Backend) Status(ctx context.Context, r isolation.SlotRef) (isolation.Status, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.Status{}, err
	}

	if _, err := b.run(ctx, "systemctl", "is-active", "--quiet", UnitName(r.Slot())); err != nil {
		return isolation.Status{Detail: UnitName(r.Slot()) + " is not active"}, nil //nolint:nilerr // an inactive unit is a status, not a failure
	}

	return b.RuntimeStatus(ctx, r)
}

// RuntimeStatus reports whether the OCI runtime considers the container
// running, without requiring the supervising unit to be active. The
// node-lifecycle post-start hook needs this form: it runs as ExecStartPost, when
// the unit is still activating and `systemctl is-active` fails, so Status would
// never report running from inside the hook. PROVISIONAL: the activating state
// during ExecStartPost is systemd's documented behavior, not observed here.
func (b *Backend) RuntimeStatus(ctx context.Context, r isolation.SlotRef) (isolation.Status, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.Status{}, err
	}

	out, err := b.run(ctx, b.runtimePath(), "state", ContainerID(r.Slot()))
	if err != nil {
		return isolation.Status{Detail: "runtime state: " + err.Error()}, nil //nolint:nilerr // an unreachable container is a status, not a failure
	}

	var st runtimeState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return isolation.Status{Detail: "runtime state: unparsable output"}, nil //nolint:nilerr // reported in Detail
	}

	if st.Status != "running" {
		return isolation.Status{Detail: "container is " + st.Status}, nil
	}

	return isolation.Status{Running: true}, nil
}

// Exec runs a command in the container via `<runtime> exec`.
func (b *Backend) Exec(ctx context.Context, r isolation.SlotRef, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.ExecResult{}, err
	}

	if len(cmd.Args) == 0 {
		return isolation.ExecResult{}, errors.New("exec: no command")
	}

	args := append([]string{"exec", ContainerID(r.Slot())}, cmd.Args...)
	out, err := b.run(ctx, b.runtimePath(), args...)

	return isolation.ExecResult{Stdout: out}, err
}

// Logs streams the container journal by running journalctl in the container.
func (b *Backend) Logs(ctx context.Context, r isolation.SlotRef, opts isolation.LogOptions) (io.ReadCloser, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return nil, err
	}

	args := []string{"exec", ContainerID(r.Slot()), "journalctl", "--no-pager"}
	if opts.Unit != "" {
		args = append(args, "--unit="+opts.Unit)
	}

	args = append(args, isolation.JournalTailArgs(opts)...)

	if b.Stream != nil {
		return b.Stream(ctx, b.runtimePath(), args...)
	}

	return isolation.StartLogStream(ctx, b.runtimePath(), args...)
}
