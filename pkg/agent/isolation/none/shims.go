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
	"syscall"
)

// shimComm is the process name of the per-pod containerd shim. containerd runs
// with KillMode=process so that pods survive a containerd restart, which also
// means they survive stopping the unit. Tearing the node down has to end them.
// The name is truncated to 15 characters in /proc/<pid>/comm.
const shimComm = "containerd-shim"

// killShims sends SIGKILL to every containerd shim visible under proc. The
// preflight guarantees the host had no containerd of its own, so every shim
// belongs to the node stack the agent installed. A process that exits between
// the scan and the signal is not an error.
func killShims(proc string) error {
	entries, err := os.ReadDir(proc)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("scan processes: %w", err)
	}

	var errs []error

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		comm, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm"))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(comm)), shimComm) {
			continue
		}

		if err := killProcess(pid); err != nil {
			errs = append(errs, fmt.Errorf("kill containerd shim %d: %w", pid, err))
		}
	}

	return errors.Join(errs...)
}

// killProcess is a seam so tests never signal a real process.
var killProcess = func(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

// podCgroupNames are the top-level cgroups kubelet puts every pod in: the
// systemd driver names it kubepods.slice, the cgroupfs driver kubepods.
var podCgroupNames = []string{"kubepods.slice", "kubepods"}

// killPodCgroups ends every process in the pods' cgroups under root (the
// cgroup v2 mount). SIGKILL on a containerd shim does not do it: the pod's
// processes are reparented and keep running, holding their mounts and files
// while the node's data is removed. cgroup.kill (Linux 5.14) kills the whole
// subtree at once; without it each process listed in the subtree is signaled.
// An absent cgroup is nothing to kill.
func killPodCgroups(root string) error {
	var errs []error

	for _, name := range podCgroupNames {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		err := writeExisting(filepath.Join(dir, "cgroup.kill"), "1")
		if err == nil {
			continue
		}

		if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("kill cgroup %s: %w", dir, err))

			continue
		}

		if err := killCgroupProcesses(dir); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// killCgroupProcesses signals every process in dir and its descendants, for
// kernels without cgroup.kill.
func killCgroupProcesses(dir string) error {
	var errs []error

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)

			return nil
		}

		for _, f := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				continue
			}

			if err := killProcess(pid); err != nil {
				errs = append(errs, fmt.Errorf("kill pod process %d: %w", pid, err))
			}
		}

		return nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("scan cgroup %s: %w", dir, err))
	}

	return errors.Join(errs...)
}

// writeExisting writes to a file that must already exist: a control file, never
// created by the write, so a kernel without it is reported as ErrNotExist.
func writeExisting(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close() //nolint:errcheck // the write error is the one to report

		return err
	}

	return f.Close()
}
