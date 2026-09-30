// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostroot locates the directory that holds the agent's own host-side
// files: its binaries and the helpers systemd units run. It does not cover the
// config, state, logs, units, or anything inside the nspawn machine.
//
// New installations use Path, which is writable on every supported host,
// including those that mount /usr read-only such as Azure Container Linux.
//
// Hosts installed by an agent released before Path keep their files under
// LegacyPath at first, and Migrate points Path at them with a symlink. Every
// path is resolved through Resolve, so on such a host it names the files where
// they already are, and the units and recovery script the older agent wrote
// stay valid for it as well as for the new one. That is what lets the upgrade
// from the older agent roll back to it.
//
// Once no older agent is left to roll back to, the daemon moves the files into
// a real directory at Path: Stage copies them beside it, Swap replaces the link
// with the copy, and FinishMove marks the move done once the units name the new
// paths and the old files are gone. A move interrupted after Swap is resumed
// from the marker Stage writes.
package hostroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	// Path is where the agent's host-side files live.
	Path = "/opt/unbounded"

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

// CurrentState reports what the root is on this host.
func CurrentState() (State, error) {
	return state(Path, LegacyPath)
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

	moving, err := isMoving(root)
	if err != nil {
		return "", err
	}

	if moving {
		return StateMoving, nil
	}

	return StateInstalled, nil
}

func isMoving(root string) (bool, error) {
	_, err := os.Lstat(filepath.Join(root, movingMarker))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", filepath.Join(root, movingMarker), err)
	}

	return true, nil
}

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
// because either could be the live one, unless that directory is a move from
// LegacyPath that has not finished. A symlink left by an older agent's reset is
// removed so a fresh installation gets a real directory.
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
	}

	// The legacy files stay until the units no longer name them, so a move
	// that has not finished has an installation under both roots on purpose.
	moving, err := isMoving(root)
	if err != nil {
		return err
	}

	switch {
	case moving:
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
// a link pointing somewhere else is left for whoever made it. What an
// unfinished move left behind, the staging copy and the marker, goes too.
func Remove(log *slog.Logger) error {
	return remove(log, Path, LegacyPath)
}

func remove(log *slog.Logger, root, legacy string) error {
	if err := discardStaging(root); err != nil {
		return err
	}

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

	if err := removeAndSync(filepath.Join(root, movingMarker)); err != nil {
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

// Stage copies files, given relative to the root, from LegacyPath into a
// staging directory beside Path, and marks the copy as a move in progress.
// Files that are not there are skipped. Nothing that is in use changes: the
// link at Path, the files under LegacyPath, and the units that name them stay
// as they are until Swap.
//
// Symlinks are recreated rather than copied, and a target under LegacyPath is
// rewritten to the same file under Path, so the blue-green links in the copy
// lead to the copy. The target names Path as it will resolve once the copy is
// in place, because that is how the agent writes and compares them.
//
// A staging directory left by an earlier attempt is replaced. It is not in use
// by anything, and the files it copied may have changed since.
func Stage(files []string) error {
	return stage(Path, LegacyPath, files)
}

func stage(root, legacy string, files []string) error {
	staging := root + stagingSuffix

	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove %s: %w", staging, err)
	}

	if err := mkdirMode(staging, 0o755); err != nil {
		return err
	}

	final := filepath.Join(canonical(filepath.Dir(root)), filepath.Base(root))
	prefixes := []string{filepath.Clean(legacy), canonical(legacy)}
	dirs := []string{staging}

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

		dir := filepath.Dir(dst)
		if err := mkdirMode(dir, 0o755); err != nil {
			return err
		}

		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
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
			if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is neither a file nor a symlink", src)
		}
	}

	// Last, so a staging directory with the marker is a complete copy.
	if err := createSynced(filepath.Join(staging, movingMarker)); err != nil {
		return err
	}

	for _, dir := range dirs {
		if err := syncDir(dir); err != nil {
			return err
		}
	}

	return syncDir(filepath.Dir(root))
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

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // One of the agent's own files under the legacy root.
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}

	defer in.Close() //nolint:errcheck // Read-only handle.

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // The package's own staging directory.
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	_, copyErr := io.Copy(out, in)
	if copyErr != nil {
		copyErr = fmt.Errorf("copy %s to %s: %w", src, dst, copyErr)
	}

	// The umask applies to the create; the copy has to match the original.
	return errors.Join(copyErr, out.Chmod(mode), out.Sync(), out.Close())
}

func createSynced(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // The package's own marker.
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	return errors.Join(f.Sync(), f.Close())
}

// Swap puts the copy Stage made in place of the link at Path, then creates
// any of subdirs the copy lacks and restores SELinux labels, as Prepare does
// for a new installation, so a moved host ends up laid out like a fresh one.
//
// os.Rename cannot put a directory over a symlink, so the link is removed
// first. Until the rename, the root does not exist. Nothing depends on it in
// that window: the units, the blue-green links and the recovery script all
// name LegacyPath, whose files are still there. A command that runs in the
// window finds a legacy installation and no root, and links it again; the
// rename then fails, and the next attempt starts over.
func Swap(ctx context.Context, log *slog.Logger, subdirs ...string) error {
	return swap(ctx, log, Path, LegacyPath, subdirs, restoreLabels)
}

func swap(
	ctx context.Context,
	log *slog.Logger,
	root, legacy string,
	subdirs []string,
	relabel func(context.Context, *slog.Logger, string),
) error {
	staging := root + stagingSuffix

	complete, err := isMoving(staging)
	if err != nil {
		return err
	}

	if !complete {
		return fmt.Errorf("%s is not a complete copy of %s", staging, legacy)
	}

	current, err := state(root, legacy)
	if err != nil {
		return err
	}

	switch current {
	case StateLinked:
		if err := os.Remove(root); err != nil {
			return fmt.Errorf("remove %s: %w", root, err)
		}
	case StateAbsent:
	case StateMoving, StateInstalled, StateOther:
		return fmt.Errorf("%s is %s, not a link to %s", root, current, legacy)
	}

	if err := os.Rename(staging, root); err != nil {
		return fmt.Errorf("move %s to %s: %w", staging, root, err)
	}

	if err := syncDir(filepath.Dir(root)); err != nil {
		return err
	}

	log.Info("moved the agent's files into the host root", "path", root, "from", legacy)

	return prepare(ctx, log, root, subdirs, relabel)
}

// FinishMove marks the move finished. Call it once nothing names the files
// under LegacyPath any more and they have been removed.
func FinishMove() error {
	return finishMove(Path)
}

func finishMove(root string) error {
	return removeAndSync(filepath.Join(root, movingMarker))
}

// DiscardStaging removes a copy that Stage made and Swap never put in place.
func DiscardStaging() error {
	return discardStaging(Path)
}

func discardStaging(root string) error {
	staging := root + stagingSuffix

	if _, err := os.Lstat(staging); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %s: %w", staging, err)
	}

	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove %s: %w", staging, err)
	}

	return syncDir(filepath.Dir(root))
}

// RemoveSeed removes seed, a path relative to LegacyPath where install scripts
// place the agent binary for agents that predate Path. It does so only on a
// host installed under a real directory at Path, where nothing under
// LegacyPath is an installation, and only when seed is a regular file, which
// is what the scripts write. Anything else there is left alone.
func RemoveSeed(log *slog.Logger, seed string, markers ...string) error {
	return removeSeed(log, Path, LegacyPath, seed, markers)
}

func removeSeed(log *slog.Logger, root, legacy, seed string, markers []string) error {
	current, err := state(root, legacy)
	if err != nil {
		return err
	}

	if current != StateInstalled || holdsAny(legacy, markers) {
		return nil
	}

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

	return removeAndSync(path)
}
