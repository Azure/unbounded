// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// agentUID is the user the agent serves as. The Racer dataplane creates the
	// Gantry origin socket directory for the same UID.
	agentUID = 65532

	// identityDirGID owns the libp2p identity directory and its contents.
	// identityDirMode gives the group no access, so only agentUID matters.
	identityDirGID              = 65532
	identityDirMode fs.FileMode = 0o700
)

// agentRuntimeDeps keeps gantry init tests independent of the real process
// credentials and file ownership.
type agentRuntimeDeps struct {
	geteuid    func() int
	setuid     func(uid int) error
	procStatus func() ([]byte, error)
	lchown     func(root *os.Root, name string, uid, gid int) error
}

// prepareAgentRuntime is gantry init: prepareAgentRuntimeWith with the real
// process credentials.
func prepareAgentRuntime(dir string, logger *slog.Logger) error {
	return prepareAgentRuntimeWith(dir, logger, agentRuntimeDeps{
		geteuid:    os.Geteuid,
		setuid:     syscall.Setuid,
		procStatus: func() ([]byte, error) { return os.ReadFile("/proc/self/status") },
		lchown:     (*os.Root).Lchown,
	})
}

// prepareAgentRuntimeWith gives agentUID the libp2p identity directory dir and
// then switches the process to agentUID. The directory is a hostPath that
// kubelet creates root-owned, so this needs root; a process that starts
// unprivileged changes nothing.
func prepareAgentRuntimeWith(dir string, logger *slog.Logger, deps agentRuntimeDeps) error {
	if deps.geteuid() != 0 {
		return nil
	}

	if err := prepareIdentityDir(dir, deps.lchown); err != nil {
		return fmt.Errorf("prepare libp2p identity directory %s: %w", dir, err)
	}

	logger.Info("prepared libp2p identity directory",
		slog.String("path", dir),
		slog.Int("uid", agentUID),
		slog.Int("gid", identityDirGID),
	)

	if err := dropRootPrivileges(agentUID, deps); err != nil {
		return fmt.Errorf("drop root privileges: %w", err)
	}

	logger.Info("dropped root privileges", slog.Int("uid", agentUID), slog.Int("gid", os.Getgid()))

	return nil
}

// prepareIdentityDir is chown -R agentUID:identityDirGID followed by chmod 0700
// on dir. No symlink is followed: openDirNoFollow refuses one anywhere in dir's
// path, the walk never descends through one, and os.Root keeps every change
// inside dir.
func prepareIdentityDir(dir string, lchown func(*os.Root, string, int, int) error) (err error) {
	opened, err := openDirNoFollow(dir)
	if err != nil {
		return err
	}

	// Reopening through /proc reaches the directory already open, whatever dir
	// names by now; reopening dir by name would follow a symlink again.
	root, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(opened.Fd())))
	_ = opened.Close() //nolint:errcheck // The root holds its own descriptor.

	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, root.Close()) }()

	err = fs.WalkDir(root.FS(), ".", func(name string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		return lchown(root, name, agentUID, identityDirGID)
	})
	if err != nil {
		return err
	}

	return root.Chmod(".", identityDirMode)
}

// openDirNoFollow opens dir one component at a time from /, the way racersdk
// opens its origin directory, and creates missing components as it goes. It
// refuses a symlink anywhere in the path: below a directory the agent owns, the
// agent could replace a component with a link to another host path.
func openDirNoFollow(dir string) (*os.File, error) {
	current, err := os.Open("/")
	if err != nil {
		return nil, err
	}

	path := "/"

	for _, part := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		path = filepath.Join(path, part)

		fd, err := openOrMkdirNoFollow(int(current.Fd()), part)
		_ = current.Close() //nolint:errcheck // Only the next component is kept.

		if err != nil {
			return nil, fmt.Errorf("open %s without following symlinks: %w", path, err)
		}

		current = os.NewFile(uintptr(fd), path)
	}

	return current, nil
}

// openOrMkdirNoFollow opens the directory name under parent without following
// a symlink, creating it first if it is missing. Created directories stay
// traversable; prepareIdentityDir restricts only the last one.
func openOrMkdirNoFollow(parent int, name string) (int, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

	fd, err := unix.Openat(parent, name, flags, 0)
	if !errors.Is(err, unix.ENOENT) {
		return fd, err
	}

	// Another process may create the name first; the open below still refuses
	// it if that is a symlink.
	if err := unix.Mkdirat(parent, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}

	return unix.Openat(parent, name, flags, 0)
}

// dropRootPrivileges switches every thread of the process to uid. The kernel
// clears all capabilities when a root process takes a non-zero UID, and /proc
// confirms it. Groups stay as the container runtime set them, so runAsGroup and
// fsGroup still grant access to the containerd socket.
func dropRootPrivileges(uid int, deps agentRuntimeDeps) error {
	if err := deps.setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}

	status, err := deps.procStatus()
	if err != nil {
		return fmt.Errorf("read process status: %w", err)
	}

	return verifyDroppedPrivileges(status, uid)
}

// verifyDroppedPrivileges checks a /proc/<pid>/status document: the real,
// effective, saved, and filesystem UIDs must all be uid, and the permitted and
// effective capability sets must be empty.
func verifyDroppedPrivileges(status []byte, uid int) error {
	fields := make(map[string]string)

	for line := range strings.Lines(string(status)) {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}

	uids := strings.Fields(fields["Uid"])
	if len(uids) != 4 {
		return fmt.Errorf("unexpected Uid line %q", fields["Uid"])
	}

	for _, id := range uids {
		if id != strconv.Itoa(uid) {
			return fmt.Errorf("process UIDs are %s, want all %d", strings.Join(uids, " "), uid)
		}
	}

	for _, set := range []string{"CapPrm", "CapEff"} {
		caps, err := strconv.ParseUint(fields[set], 16, 64)
		if err != nil {
			return fmt.Errorf("parse %s %q: %w", set, fields[set], err)
		}

		if caps != 0 {
			return fmt.Errorf("%s is %s, want no capabilities", set, fields[set])
		}
	}

	return nil
}
