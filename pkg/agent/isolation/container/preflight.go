// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// MinSystemdVersion is the oldest systemd the container unit works on. It
// uses DelegateSubgroup=, RestartSteps= and RestartMaxDelaySec= (systemd 254).
// Older systemd ignores those keys, so runc stays in the unit's own cgroup and
// the container starts without the memory and io controllers.
const MinSystemdVersion = 254

// systemdVersionPattern matches the first line of `systemctl --version`, such
// as "systemd 255 (255.4-1ubuntu8)" or "systemd 252 (252.16-1.amzn2023)".
var systemdVersionPattern = regexp.MustCompile(`^systemd (\d+)\b`)

// parseSystemdVersion returns the major version from `systemctl --version`
// output.
func parseSystemdVersion(out string) (int, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")

	m := systemdVersionPattern.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return 0, fmt.Errorf("unrecognized systemctl --version output %q", first)
	}

	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("unrecognized systemd version %q: %w", m[1], err)
	}

	return v, nil
}

// checkSystemdVersion refuses a host whose systemd is older than
// MinSystemdVersion.
func (b *Backend) checkSystemdVersion(ctx context.Context) error {
	out, err := b.run(ctx, "systemctl", "--version")
	if err != nil {
		return fmt.Errorf("reading the systemd version (systemctl --version): %w", err)
	}

	v, err := parseSystemdVersion(out)
	if err != nil {
		return fmt.Errorf("reading the systemd version: %w", err)
	}

	if v < MinSystemdVersion {
		return fmt.Errorf("systemd %d is too old: the container mode needs systemd %d or newer "+
			"(DelegateSubgroup=, RestartSteps= and RestartMaxDelaySec= in its unit)", v, MinSystemdVersion)
	}

	return nil
}

// HostFS locates the host files preflight inspects. Tests point the fields at
// a temporary tree; production uses DefaultHostFS.
type HostFS struct {
	// CgroupControllers exists only on a cgroup v2 unified hierarchy.
	CgroupControllers string
	// SELinuxEnforce holds "1" when enforcing and "0" when permissive. Absent
	// means SELinux is disabled.
	SELinuxEnforce string
	// SELinuxContext is the kernel's context validation interface. Writing a
	// context to it and reading it back fails when the type is unknown.
	SELinuxContext string
}

// DefaultHostFS is the real host.
var DefaultHostFS = HostFS{
	CgroupControllers: "/sys/fs/cgroup/cgroup.controllers",
	SELinuxEnforce:    "/sys/fs/selinux/enforce",
	SELinuxContext:    "/sys/fs/selinux/context",
}

// SELinuxMode reads the host SELinux state.
func (h HostFS) SELinuxMode() SELinuxMode {
	data, err := os.ReadFile(h.SELinuxEnforce)
	if err != nil {
		return SELinuxDisabled
	}

	if strings.TrimSpace(string(data)) == "1" {
		return SELinuxEnforcing
	}

	return SELinuxPermissive
}

// CheckLabel verifies the kernel accepts the spc_t label. PROVISIONAL: the
// context interface behavior is assumed from the SELinux kernel API and has
// not been checked on Azure Container Linux.
func (h HostFS) CheckLabel() error {
	f, err := os.OpenFile(h.SELinuxContext, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("selinux label check: %w", err)
	}

	defer f.Close() //nolint:errcheck // read/write probe only

	if _, err := f.WriteString(SELinuxLabel); err != nil {
		return fmt.Errorf("selinux does not accept label %s: %w", SELinuxLabel, err)
	}

	buf := make([]byte, 512)
	if n, _ := f.ReadAt(buf, 0); n == 0 { //nolint:errcheck // io.EOF after a short read is expected
		return fmt.Errorf("selinux does not accept label %s", SELinuxLabel)
	}

	return nil
}

// Preflight fails when the host cannot run the container backend: a missing
// or non-executable runtime, no cgroup v2, systemd older than
// MinSystemdVersion, an SELinux label the kernel does not accept while
// enforcing, or a read only required path. It mutates nothing.
func (b *Backend) Preflight() error {
	host := b.host()

	var errs []error

	info, err := os.Stat(b.runtimePath())

	switch {
	case err != nil && b.RuntimeProvisioned && errors.Is(err, os.ErrNotExist):
		// The agent installs the runtime during bootstrap, after preflight.
	case err != nil:
		errs = append(errs, fmt.Errorf("container runtime %s: %w", b.runtimePath(), err))
	case info.IsDir() || info.Mode()&0o111 == 0:
		errs = append(errs, fmt.Errorf("container runtime %s is not executable", b.runtimePath()))
	}

	if _, err := os.Stat(host.CgroupControllers); err != nil {
		errs = append(errs, fmt.Errorf("cgroup v2 is required (%s): %w", host.CgroupControllers, err))
	}

	if err := b.checkSystemdVersion(context.Background()); err != nil {
		errs = append(errs, err)
	}

	if host.SELinuxMode() == SELinuxEnforcing {
		if err := host.CheckLabel(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := b.checkWritable(b.Requirements().WritablePaths...); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
