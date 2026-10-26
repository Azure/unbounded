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

	"github.com/Azure/unbounded/internal/gantry/config"
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

// agentRuntimeDeps keeps startup privilege tests independent of the real
// process credentials and file ownership.
type agentRuntimeDeps struct {
	geteuid    func() int
	setuid     func(uid int) error
	procStatus func() ([]byte, error)
	lchown     func(root *os.Root, name string, uid, gid int) error
}

// prepareAgentRuntime readies the process before the agent serves anything.
func prepareAgentRuntime(c *config.Config, logger *slog.Logger) error {
	return prepareAgentRuntimeWith(c, logger, agentRuntimeDeps{
		geteuid:    os.Geteuid,
		setuid:     syscall.Setuid,
		procStatus: func() ([]byte, error) { return os.ReadFile("/proc/self/status") },
		lchown:     (*os.Root).Lchown,
	})
}

// prepareAgentRuntimeWith gives agentUID the libp2p identity directory and then
// switches the process to agentUID. The directory is a hostPath that kubelet
// creates root-owned, so the agent starts as root to re-own it and switches
// before serving anything. A process that starts unprivileged changes nothing.
func prepareAgentRuntimeWith(c *config.Config, logger *slog.Logger, deps agentRuntimeDeps) error {
	if deps.geteuid() != 0 {
		return nil
	}

	if c.Libp2pIdentityPath != "" {
		dir := filepath.Dir(c.Libp2pIdentityPath)
		// Re-owning is recursive, so a relative path or the filesystem root
		// would reach far beyond the agent's own state.
		if !filepath.IsAbs(dir) || dir == "/" {
			return fmt.Errorf("libp2p_identity_path %q must be an absolute path below /", c.Libp2pIdentityPath)
		}

		if err := prepareIdentityDir(dir, deps.lchown); err != nil {
			return fmt.Errorf("prepare libp2p identity directory %s: %w", dir, err)
		}

		logger.Info("prepared libp2p identity directory",
			slog.String("path", dir),
			slog.Int("uid", agentUID),
			slog.Int("gid", identityDirGID),
		)
	}

	if err := dropRootPrivileges(agentUID, deps); err != nil {
		return fmt.Errorf("drop root privileges: %w", err)
	}

	logger.Info("dropped root privileges", slog.Int("uid", agentUID), slog.Int("gid", os.Getgid()))

	return nil
}

// prepareIdentityDir is chown -R agentUID:identityDirGID followed by chmod 0700
// on dir. The walk never follows symlinks, and os.Root keeps every change
// inside dir.
func prepareIdentityDir(dir string, lchown func(*os.Root, string, int, int) error) (err error) {
	// Created parents stay traversable; dir itself is restricted below.
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	root, err := os.OpenRoot(dir)
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

// dropRootPrivileges switches every thread of the process to uid. The kernel
// clears all capabilities when a root process takes a non-zero UID, and /proc
// confirms it before the agent continues. Groups stay as the container runtime
// set them, so runAsGroup and fsGroup still grant access to the containerd
// socket.
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
