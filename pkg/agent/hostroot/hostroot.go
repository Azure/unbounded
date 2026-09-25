// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostroot locates the directory that holds the agent's own host-side
// files: its binaries and the helpers systemd units run. It does not cover the
// config, state, logs, units, or anything inside the nspawn machine.
//
// New installations use Path, which is writable on every supported host,
// including those that mount /usr read-only such as Azure Container Linux.
// Hosts installed by an agent released before Path keep their files under
// LegacyPath, and Migrate points Path at them with a symlink. Every path is
// resolved through Resolve, so on such a host it names the files where they
// already are, and the units and recovery script the older agent wrote stay
// valid for it as well as for the new one.
package hostroot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	// Path is where the agent's host-side files live.
	Path = "/opt/unbounded"

	// LegacyPath is where agents released before Path installed them.
	LegacyPath = "/usr/local"
)

// Resolve returns the directory Path refers to on this host, with symlinks
// resolved: LegacyPath on a migrated host, and Path itself on any other.
//
// Paths built from it are compared with symlink targets, which are resolved,
// so they have to be resolved too. Building them from an unresolved Path on a
// migrated host would name /opt/unbounded/bin/unbounded-agent-blue while the
// current link resolves to /usr/local/bin/unbounded-agent-blue, and the two
// would never compare equal.
func Resolve() string {
	return canonical(Path)
}

// Planned returns the directory Resolve will return once Migrate has run with
// the same markers. It changes nothing, so scripts that place files before the
// agent runs can ask where to put them.
func Planned(markers ...string) string {
	return planned(Path, LegacyPath, markers)
}

func planned(root, legacy string, markers []string) string {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) && holdsAny(legacy, markers) {
		return canonical(legacy)
	}

	return canonical(root)
}

// canonical resolves symlinks in the longest leading part of path that exists
// and appends the rest. It gives the same answer before and after the missing
// part is created, so paths resolved before a first install still match the
// ones resolved after it.
func canonical(path string) string {
	path = filepath.Clean(path)

	missing := ""

	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, missing)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return path
		}

		missing = filepath.Join(filepath.Base(current), missing)
	}
}

// Migrate points Path at LegacyPath on a host whose agent installation is
// under LegacyPath. markers are paths relative to the root whose presence
// under LegacyPath identifies such an installation: the product's own binary
// layout, not files a fresh installation also creates there.
//
// It is idempotent and does nothing on a host without a legacy installation.
// It refuses a host with a legacy installation where Path is also a directory,
// because either could be the live one, and a symlink left by an older agent's
// reset is removed so a fresh installation gets a real directory.
//
// Commands that change the host call it first, before any path is resolved: a
// path resolved on an unmigrated legacy host names Path, where nothing is
// installed. Reset is the exception. It removes the files under both roots, so
// it works on a host this refuses, which is when an operator is told to run it.
func Migrate(log *slog.Logger, markers ...string) error {
	return migrate(log, Path, LegacyPath, markers)
}

func migrate(log *slog.Logger, root, legacy string, markers []string) error {
	info, err := os.Lstat(root)

	switch {
	case errors.Is(err, os.ErrNotExist):
		if !holdsAny(legacy, markers) {
			return nil
		}

		return linkLegacy(log, root, legacy)
	case err != nil:
		return fmt.Errorf("inspect %s: %w", root, err)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(root)
		if err != nil {
			return fmt.Errorf("read %s: %w", root, err)
		}

		// Only the link this package creates is ours to remove. A link an
		// operator made, to put the root on another filesystem, is theirs.
		if target != legacy || holdsAny(legacy, markers) {
			return nil
		}

		// An older agent's reset removes the files but not the link it never
		// knew about. Left in place, it would put a fresh installation back
		// under LegacyPath, which is read-only on some hosts.
		log.Info("removing a host root link with no installation behind it", "path", root, "target", target)

		if err := removeAndSync(root); err != nil {
			return err
		}

		return nil
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", root)
	case !holdsAny(legacy, markers):
		return nil
	case holdsAny(root, markers):
		return fmt.Errorf("the agent is installed under both %s and %s; run reset, then install again", legacy, root)
	default:
		return fmt.Errorf("the agent is installed under %s, but %s also exists; remove %s if nothing uses it, or run reset", legacy, root, root)
	}
}

// linkLegacy creates root as a symlink to legacy. It is built under a
// temporary name and renamed into place, so root is never half-made, and a
// concurrent migration renames an identical link over it.
func linkLegacy(log *slog.Logger, root, legacy string) error {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", parent, err)
	}

	temp := fmt.Sprintf("%s.migrating-%d", root, os.Getpid())
	_ = os.Remove(temp) //nolint:errcheck // Leftover from an interrupted migration by this PID; absence is expected.

	if err := os.Symlink(legacy, temp); err != nil {
		return fmt.Errorf("link %s to %s: %w", root, legacy, err)
	}

	if err := os.Rename(temp, root); err != nil {
		_ = os.Remove(temp) //nolint:errcheck // Best-effort cleanup; the rename error is returned.
		return fmt.Errorf("link %s to %s: %w", root, legacy, err)
	}

	if err := syncDir(parent); err != nil {
		return err
	}

	log.Info("linked the host root to the existing installation", "path", root, "target", legacy)

	return nil
}

func holdsAny(root string, markers []string) bool {
	for _, marker := range markers {
		if _, err := os.Lstat(filepath.Join(root, marker)); err == nil {
			return true
		}
	}

	return false
}

// Prepare creates the root and the given subdirectories as a new installation
// needs them, with mode 0755 regardless of the umask, and restores their
// SELinux labels where the policy tools are present. On a migrated host the
// root is the existing installation and is left as it is.
//
// The labels matter because a directory takes its parent's label when it is
// created. Under /opt that is usr_t, while the policy expects bin_t under
// /opt/*/bin; files created later inherit the directory's label.
func Prepare(ctx context.Context, log *slog.Logger, subdirs ...string) error {
	return prepare(ctx, log, Path, subdirs, restoreLabels)
}

func prepare(
	ctx context.Context,
	log *slog.Logger,
	root string,
	subdirs []string,
	relabel func(context.Context, *slog.Logger, string),
) error {
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	for _, dir := range append([]string{root}, prefixed(root, subdirs)...) {
		if err := mkdirMode(dir, 0o755); err != nil {
			return err
		}
	}

	relabel(ctx, log, root)

	return nil
}

func prefixed(root string, subdirs []string) []string {
	out := make([]string, 0, len(subdirs))
	for _, dir := range subdirs {
		out = append(out, filepath.Join(root, dir))
	}

	return out
}

// mkdirMode creates dir, and its parents, and sets its mode. An existing
// directory keeps its mode, which may have been chosen by whoever made it.
func mkdirMode(dir string, mode os.FileMode) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}

	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set mode of %s: %w", dir, err)
	}

	return nil
}

func restoreLabels(ctx context.Context, log *slog.Logger, root string) {
	restorecon, err := exec.LookPath("restorecon")
	if err != nil {
		return
	}

	if out, err := exec.CommandContext(ctx, restorecon, "-R", root).CombinedOutput(); err != nil { //nolint:gosec // Fixed tool and the package's own root.
		log.Warn("could not restore SELinux labels on the host root", "path", root, "error", err, "output", string(out))
	}
}

// Remove removes the host root once reset has removed the files in it. A
// link this package created is removed, and a real directory is removed along
// with its now empty subdirectories. Anything not empty is left in place, and
// a link pointing somewhere else is left for whoever made it.
func Remove(log *slog.Logger) error {
	return remove(log, Path, LegacyPath)
}

func remove(log *slog.Logger, root, legacy string) error {
	info, err := os.Lstat(root)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect %s: %w", root, err)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(root)
		if err != nil {
			return fmt.Errorf("read %s: %w", root, err)
		}

		if target != legacy {
			return nil
		}

		log.Info("removing the host root link", "path", root)

		return removeAndSync(root)
	case !info.IsDir():
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read %s: %w", root, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeIfEmpty(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}

	return removeIfEmpty(root)
}

func removeIfEmpty(dir string) error {
	err := os.Remove(dir)
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return nil
	}

	return fmt.Errorf("remove %s: %w", dir, err)
}

func removeAndSync(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // The package's own directory.
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}

	return errors.Join(f.Sync(), f.Close())
}
