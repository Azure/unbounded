// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostroot implements the host root that pkg/agent/hostroot exposes,
// and the parts of it only the unbounded agent uses: its own layout and
// removing the root on reset.
package hostroot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/renameio/v2"

	"github.com/Azure/unbounded/internal/fsutil"
)

const (
	// Path is where the agent's host-side files live.
	//
	// Its parent is not the agent's. Hosts stage files for the agent there,
	// such as offline artifacts and OCI layouts, and the parent may already
	// be a directory, or a mount, when an older agent's installation is
	// linked. So the agent creates the parent when it is missing, never
	// changes one that exists, and never removes it.
	Path = "/opt/unbounded/agent"

	// LegacyPath is where agents released before Path installed them.
	LegacyPath = "/usr/local"

	// stagingSuffix names the directory beside the root that a move copies the
	// legacy files into before it replaces the link.
	stagingSuffix = ".staging"

	// movingMarker is written into the staging directory, and so arrives in
	// the root with it. While it is there the move has not finished, and the
	// legacy files are expected to be present alongside the new ones.
	movingMarker = ".moving"
)

// State is what the root is on this host.
type State string

const (
	// StateAbsent is a host without the root: not installed, or installed
	// only under LegacyPath by an older agent.
	StateAbsent State = "absent"
	// StateLinked is a root that links to LegacyPath, where an older agent
	// installed the files.
	StateLinked State = "linked"
	// StateMoving is a real directory whose move from LegacyPath has not
	// finished.
	StateMoving State = "moving"
	// StateInstalled is a real directory holding the installation.
	StateInstalled State = "installed"
	// StateOther is anything else, such as a link an operator made.
	StateOther State = "other"
)

// LegacyReleased is documented in pkg/agent/hostroot.
func LegacyReleased() (bool, error) {
	return legacyReleased(Path, LegacyPath)
}

func legacyReleased(root, legacy string) (bool, error) {
	current, err := state(root, legacy)
	if err != nil {
		return false, err
	}

	switch current {
	case StateInstalled:
		return true, nil
	case StateOther:
	case StateAbsent, StateLinked, StateMoving:
		return false, nil
	}

	// A link an operator made releases the legacy root unless it leads back
	// there. Anything else in the way, or a link that leads nowhere, says
	// nothing about where the agent runs from.
	info, err := os.Lstat(root)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", root, err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, nil //nolint:nilerr // A dangling link releases nothing.
	}

	if target, err := os.Stat(resolved); err != nil || !target.IsDir() {
		return false, nil //nolint:nilerr // Neither does a link to something that is not a directory.
	}

	return !atOrUnder(resolved, legacy), nil
}

func state(root, legacy string) (State, error) {
	info, err := os.Lstat(root)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return StateAbsent, nil
	case err != nil:
		return "", fmt.Errorf("inspect %s: %w", root, err)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(root)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", root, err)
		}

		if target == legacy {
			return StateLinked, nil
		}

		return StateOther, nil
	case !info.IsDir():
		return StateOther, nil
	}

	if holdsAny(root, []string{movingMarker}) {
		return StateMoving, nil
	}

	return StateInstalled, nil
}

// Resolve is documented in pkg/agent/hostroot.
func Resolve() string {
	return canonical(Path)
}

// Planned is documented in pkg/agent/hostroot.
func Planned(markers ...string) string {
	return planned(Path, LegacyPath, markers)
}

// planned follows what migrate does to the root, without doing it.
func planned(root, legacy string, markers []string) string {
	current, err := state(root, legacy)
	if err != nil {
		return canonical(root)
	}

	installed := holdsAny(legacy, markers)

	switch {
	case current == StateAbsent && installed:
		// Migrate links it.
		return canonical(legacy)
	case current == StateLinked && !installed:
		// Migrate removes a link with no installation behind it, and a fresh
		// installation makes a directory in its place.
		return filepath.Join(canonical(filepath.Dir(root)), filepath.Base(root))
	default:
		return canonical(root)
	}
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

// Migrate is documented in pkg/agent/hostroot. Reset does not call it: it
// removes the files under both roots, so it works on a host this refuses,
// which is when an operator is told to run it.
func Migrate(log *slog.Logger, markers ...string) error {
	return migrate(log, Path, LegacyPath, markers)
}

func migrate(log *slog.Logger, root, legacy string, markers []string) error {
	current, err := state(root, legacy)
	if err != nil {
		return err
	}

	switch current {
	case StateAbsent:
		if !holdsAny(legacy, markers) {
			return nil
		}

		// The link goes inside the parent, which may already hold other files;
		// see Path.
		if err := mkdirMode(filepath.Dir(root), 0o755); err != nil {
			return err
		}

		// Atomic, so a concurrent migration replaces an identical link.
		if err := renameio.Symlink(legacy, root); err != nil {
			return fmt.Errorf("link %s to %s: %w", root, legacy, err)
		}

		log.Info("linked the host root to the existing installation", "path", root, "target", legacy)

		return nil
	case StateLinked:
		if holdsAny(legacy, markers) {
			return nil
		}

		// An older agent's reset removes the files but not the link it never
		// knew about. Left in place, it would put a fresh installation back
		// under LegacyPath, which is read-only on some hosts.
		log.Info("removing a host root link with no installation behind it", "path", root, "target", legacy)

		return removeIfExists(root)
	case StateOther:
		// A link an operator made, to put the root on another filesystem, is
		// theirs. Anything else is in the way.
		if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is not a directory", root)
		}

		return nil
	case StateMoving:
		// The legacy files stay until the units no longer name them, so a move
		// that has not finished has an installation under both roots on purpose.
		return nil
	case StateInstalled:
	}

	switch {
	case !holdsAny(legacy, markers):
		return nil
	case holdsAny(root, markers):
		return fmt.Errorf("the agent is installed under both %s and %s; run reset, then install again", legacy, root)
	default:
		return fmt.Errorf("the agent is installed under %s, but %s also exists; remove %s if nothing uses it, or run reset", legacy, root, root)
	}
}

func holdsAny(root string, markers []string) bool {
	for _, marker := range markers {
		if _, err := os.Lstat(filepath.Join(root, marker)); err == nil {
			return true
		}
	}

	return false
}

// Prepare is documented in pkg/agent/hostroot.
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

	for _, dir := range append([]string{""}, subdirs...) {
		if err := mkdirMode(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}

	relabel(ctx, log, root)

	return nil
}

// mkdirMode creates dir, and each parent it lacks, with mode whatever the
// umask. A directory that already exists keeps its mode, which may have been
// chosen by whoever made it; the root's parent, in particular, is not the
// agent's.
func mkdirMode(dir string, mode os.FileMode) error {
	_, err := os.Stat(dir)
	if err == nil {
		return nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}

	if err := mkdirMode(filepath.Dir(dir), mode); err != nil {
		return err
	}

	// Another command may have made it since it was checked. It is theirs
	// then, mode and all, as long as it is a directory: a link that appears
	// in that window is not followed.
	if err := os.Mkdir(dir, mode); errors.Is(err, os.ErrExist) {
		if info, err := os.Lstat(dir); err != nil {
			return fmt.Errorf("inspect %s: %w", dir, err)
		} else if !info.IsDir() {
			return fmt.Errorf("%s appeared while it was being created, and is not a directory", dir)
		}

		return nil
	} else if err != nil {
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

// Remove removes the host root once reset has removed the files in it. A link
// to LegacyPath, which Migrate makes, is removed, and a real directory is
// removed along with those of its immediate subdirectories that are empty.
// Anything not empty is left in place, and a link pointing somewhere else is
// left for whoever made it. What an
// unfinished move left behind, the staging copy and the marker, goes too. The
// root's parent stays, whatever is in it; see Path.
func Remove(log *slog.Logger) error {
	return remove(log, Path, LegacyPath)
}

func remove(log *slog.Logger, root, legacy string) error {
	if err := os.RemoveAll(root + stagingSuffix); err != nil {
		return fmt.Errorf("remove %s: %w", root+stagingSuffix, err)
	}

	current, err := state(root, legacy)
	if err != nil {
		return err
	}

	switch current {
	case StateAbsent, StateOther:
		return nil
	case StateLinked:
		log.Info("removing the host root link", "path", root)

		return removeIfExists(root)
	case StateMoving, StateInstalled:
	}

	if err := removeIfExists(filepath.Join(root, movingMarker)); err != nil {
		return err
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

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

// move copies files, given relative to the root, from legacy into a staging
// directory beside root and puts the copy in place of the link at root. The
// copy carries a marker that keeps the host in StateMoving until completeMove,
// which lays it out and labels it, removes the marker. Files that are not there
// are skipped.
//
// Symlinks are recreated rather than copied, and a target under LegacyPath is
// rewritten to the same file under Path, so the blue-green links in the copy
// lead to the copy. The target names Path as it will resolve once the copy is
// in place, because that is how the agent writes and compares them.
//
// Nothing that is in use changes: the units, the blue-green links and the
// recovery script all name LegacyPath, whose files stay. A staging directory
// left by an earlier attempt is replaced.
func move(log *slog.Logger, root, legacy string, files []string) error {
	if err := stage(root, legacy, files); err != nil {
		return err
	}

	// os.Rename cannot put a directory over a symlink, so the link goes first.
	// A command that runs before the rename finds a legacy installation and no
	// root, and links it again; the rename then fails, and the next attempt
	// starts over.
	if err := removeIfExists(root); err != nil {
		return err
	}

	if err := os.Rename(root+stagingSuffix, root); err != nil {
		return fmt.Errorf("move %s to %s: %w", root+stagingSuffix, root, err)
	}

	// The units are rewritten to name the new root next; it has to survive a
	// crash first.
	if err := fsutil.SyncDir(filepath.Dir(root)); err != nil {
		return err
	}

	log.Info("copied the agent's files into the host root", "path", root, "from", legacy)

	return nil
}

func stage(root, legacy string, files []string) error {
	staging := root + stagingSuffix

	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove %s: %w", staging, err)
	}

	// The root's parent, which is not the agent's; see Path.
	if err := mkdirMode(filepath.Dir(staging), 0o755); err != nil {
		return err
	}

	// Made here, never found: whatever is at the name now was put there since
	// it was removed, and the copy must not go where it leads.
	if err := os.Mkdir(staging, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", staging, err)
	}

	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("set mode of %s: %w", staging, err)
	}

	final := filepath.Join(canonical(filepath.Dir(root)), filepath.Base(root))
	prefixes := []string{filepath.Clean(legacy), canonical(legacy)}

	for _, rel := range files {
		src := filepath.Join(legacy, rel)

		info, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("inspect %s: %w", src, err)
		}

		dst := filepath.Join(staging, rel)
		if err := mkdirMode(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return fmt.Errorf("read %s: %w", src, err)
			}

			if err := os.Symlink(rebase(target, prefixes, final), dst); err != nil {
				return fmt.Errorf("link %s: %w", dst, err)
			}
		case info.Mode().IsRegular():
			if err := fsutil.InstallFile(src, dst, info.Mode().Perm()); err != nil {
				return fmt.Errorf("copy %s: %w", src, err)
			}
		default:
			return fmt.Errorf("%s is neither a file nor a symlink", src)
		}
	}

	if err := os.WriteFile(filepath.Join(staging, movingMarker), nil, 0o644); err != nil { //nolint:gosec // The package's own marker.
		return fmt.Errorf("mark %s: %w", staging, err)
	}

	// The copy has to be on disk before it replaces the link.
	return fsutil.SyncFilesystems(staging)
}

// rebase rewrites an absolute link target under one of prefixes to the same
// path under root. Any other target is kept.
func rebase(target string, prefixes []string, root string) string {
	if !filepath.IsAbs(target) {
		return target
	}

	clean := filepath.Clean(target)
	for _, prefix := range prefixes {
		if rest, ok := strings.CutPrefix(clean, prefix+string(filepath.Separator)); ok {
			return filepath.Join(root, rest)
		}
	}

	return target
}

// RemoveSeed removes SeedFile under LegacyPath. Call it only where
// LegacyReleased reports true, so nothing the agent runs is there. Only a
// regular file is removed, because that is what the scripts write; a link
// there is an operator's.
func RemoveSeed(log *slog.Logger) error {
	return removeSeed(log, LegacyPath, SeedFile)
}

func removeSeed(log *slog.Logger, legacy, seed string) error {
	path := filepath.Join(legacy, seed)

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	log.Info("removing the agent binary an install script seeded for older releases", "path", path)

	return removeIfExists(path)
}
