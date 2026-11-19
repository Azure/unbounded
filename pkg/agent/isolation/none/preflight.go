// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HostFS locates the host paths preflight inspects. Tests point the fields at
// a temporary tree; production uses DefaultHostFS.
type HostFS struct {
	// UnitDirs is retained for compatibility; installed unit files are not conflicts.
	UnitDirs []string
	// BinDirs is retained for compatibility; installed binaries are not conflicts.
	BinDirs []string
	// Proc is the procfs mount.
	Proc string
	// Cgroup is the cgroup filesystem mount. Empty skips the cgroup check.
	Cgroup string
}

// DefaultHostFS is the real host.
var DefaultHostFS = HostFS{
	UnitDirs: []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"},
	BinDirs:  []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"},
	Proc:     "/proc",
	Cgroup:   "/sys/fs/cgroup",
}

// foreign names the components that must not be running outside agent ownership.
var foreign = []struct{ unit, binary string }{
	{"kubelet.service", "kubelet"},
	{"containerd.service", "containerd"},
}

// DetectUnmanaged reports running processes outside the agent's own base.
// Installed unit files and binaries are harmless after the units are masked.
func (h HostFS) DetectUnmanaged(ownedBase string) []string {
	return h.runningProcesses(ownedBase, false)
}

// DetectUnmanagedBeforeDeactivation allows processes in the service cgroup
// that bootstrap will stop, but reports processes outside those units.
func (h HostFS) DetectUnmanagedBeforeDeactivation(ownedBase string) []string {
	return h.runningProcesses(ownedBase, true)
}

func (h HostFS) runningProcesses(ownedBase string, allowService bool) []string {
	entries, err := os.ReadDir(h.Proc)
	if err != nil {
		return nil
	}

	var findings []string

	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}

		comm, err := os.ReadFile(filepath.Join(h.Proc, e.Name(), "comm"))
		if err != nil {
			continue
		}

		name := strings.TrimSpace(string(comm))
		for _, f := range foreign {
			if name != f.binary {
				continue
			}

			exe, _ := os.Readlink(filepath.Join(h.Proc, e.Name(), "exe")) //nolint:errcheck // an unreadable exe is treated as foreign
			if ownedBase != "" && strings.HasPrefix(exe, ownedBase+string(filepath.Separator)) {
				continue
			}

			if allowService {
				if ServiceOwnsProcess(h.Proc, e.Name(), f.unit, f.binary) {
					continue
				}
			}

			findings = append(findings, fmt.Sprintf("%s is already running as pid %s", f.binary, e.Name()))
		}
	}

	return findings
}

// ServiceOwnsProcess verifies the process name and exact systemd service
// cgroup; unreadable process metadata is not evidence of ownership.
func ServiceOwnsProcess(proc, pid, unit, binary string) bool {
	dir := filepath.Join(proc, pid)
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil || strings.TrimSpace(string(comm)) != binary {
		return false
	}

	cgroup, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	return err == nil && inServiceCgroup(string(cgroup), unit)
}

func inServiceCgroup(cgroup, unit string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}

		for _, part := range strings.Split(fields[2], "/") {
			if part == unit {
				return true
			}
		}
	}

	return false
}

// CheckWritable fails when any of paths cannot be written. A path that does not
// exist is checked through its nearest existing parent.
func CheckWritable(paths ...string) error {
	var errs []error

	for _, p := range paths {
		dir := p
		for {
			info, err := os.Stat(dir)
			if err == nil {
				if !info.IsDir() {
					errs = append(errs, fmt.Errorf("%s is not a directory", dir))
				} else if err := probeWritable(dir); err != nil {
					errs = append(errs, fmt.Errorf("%s is not writable: %w", p, err))
				}

				break
			}

			parent := filepath.Dir(dir)
			if parent == dir {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
				break
			}

			dir = parent
		}
	}

	return errors.Join(errs...)
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".unbounded-write-check-*")
	if err != nil {
		return err
	}

	name := f.Name()

	_ = f.Close() //nolint:errcheck // probe file only

	return os.Remove(name) // a failed removal leaves a probe file behind, so surface it
}
