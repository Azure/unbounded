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
	// UnitDirs are directories searched for systemd unit files.
	UnitDirs []string
	// BinDirs are directories searched for executables.
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

// foreign names the components that must not already be on the host.
var foreign = []struct{ unit, binary string }{
	{"kubelet.service", "kubelet"},
	{"containerd.service", "containerd"},
}

// DetectUnmanaged returns one finding per kubelet or containerd the agent does
// not own: a unit file, an executable on the standard paths, or a running
// process. The agent refuses to start with any finding, since two kubelets or
// two containerds on one host fight over the same sockets and directories.
//
// Binaries below the agent's own base are not findings, and a process whose
// executable lives below ownedBase is the agent's own.
func (h HostFS) DetectUnmanaged(ownedBase string) []string {
	var findings []string

	for _, f := range foreign {
		for _, dir := range h.UnitDirs {
			path := filepath.Join(dir, f.unit)
			if _, err := os.Lstat(path); err == nil && !masked(path) {
				findings = append(findings, fmt.Sprintf("%s unit %s exists and is not managed by the agent", f.binary, path))
			}
		}

		for _, dir := range h.BinDirs {
			path := filepath.Join(dir, f.binary)
			if _, err := os.Lstat(path); err == nil {
				findings = append(findings, fmt.Sprintf("%s binary %s exists and is not managed by the agent", f.binary, path))
			}
		}
	}

	findings = append(findings, h.runningProcesses(ownedBase)...)

	return findings
}

// masked reports whether the unit file at path is a mask: a link to /dev/null.
// A masked unit cannot start, so it is no kubelet or containerd of its own. The
// nspawn mode masks the distro's containerd and kubelet, which is what a host
// that once ran nspawn keeps.
func masked(path string) bool {
	target, err := filepath.EvalSymlinks(path)

	return err == nil && target == os.DevNull
}

func (h HostFS) runningProcesses(ownedBase string) []string {
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

			findings = append(findings, fmt.Sprintf("%s is already running as pid %s", f.binary, e.Name()))
		}
	}

	return findings
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
