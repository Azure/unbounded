// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Azure/unbounded/internal/fsutil"
)

// MoveOptions describes what ReconcileMove moves and how the agent follows it.
type MoveOptions struct {
	// Files are the agent's files, relative to the root, that a move copies
	// from LegacyPath and then removes there.
	Files []string
	// Dirs are directories, relative to the root, that hold only the agent's
	// files. A move removes them under LegacyPath once Files are gone there,
	// unless something else is left in them. Shared directories, such as bin,
	// do not belong here.
	Dirs []string
	// Subdirs are created under the root after the move, as a fresh
	// installation has them, and the root's SELinux labels are restored.
	Subdirs []string
	// Record is where the digests of daemon binaries that know the host root
	// are recorded. It is removed once the host is moved; reset has to remove
	// it as well.
	Record string
	// SignalPath is the AgentUpgrade signal. While it exists an upgrade has not
	// been reported, and the host is not moved.
	SignalPath string
	// CurrentPath and LastGoodPath are the daemon's blue-green links. The host
	// is moved once both resolve to recorded binaries.
	CurrentPath, LastGoodPath string
	// RewriteUnits points every unit and script that names the agent's files
	// at the files under the root, reloads systemd, and makes them durable
	// before it returns. It runs on every pass of an unfinished move, so it
	// has to be idempotent. A move that is undone calls it once more with the
	// root linked to LegacyPath again, to point them back.
	RewriteUnits func(context.Context) error
	// Verify runs the daemon binary from the copy under root, before any unit
	// names it, and fails if it cannot run there, for example because
	// something denies running programs from that filesystem. The move is
	// then undone. It is required.
	Verify func(ctx context.Context, root string) error
	// Restart restarts the daemon from the rewritten units, so it runs from
	// the root. It may only queue the restart; the caller must not carry on
	// as the running daemon when ReconcileMove reports it. systemd counts
	// this planned restart against the unit's start limit, so a caller should
	// clear the unit's start limit first, with systemctl reset-failed <unit>,
	// or a refused restart strands the move.
	Restart func(context.Context) error
}

// moveHost is what a move asks of the host, which tests replace.
type moveHost struct {
	// executable returns the running daemon's executable.
	executable func() (string, error)
	// relabel restores the SELinux labels under a root.
	relabel func(context.Context, *slog.Logger, string)
	// noexec reports whether the filesystem holding a path is mounted
	// without permission to run programs from it.
	noexec func(string) (bool, error)
}

// ReconcileMove moves a host an older agent installed from LegacyPath into a
// real directory at Path, once that cannot strand a rollback, and finishes a
// move that was interrupted. The caller runs it from the daemon, after Migrate,
// while holding whatever keeps an upgrade or a reset from changing the layout.
//
// Each run on a linked host records the running binary's digest. Older agents
// never do, so a linked host is moved only when neither the current nor the
// last-good binary predates the host root; until then the upgrade from the
// older agent can still roll back to it, and it needs its files under
// LegacyPath. That is at the first run after the AgentUpgrade that pushes the
// older agent out of last-good.
//
// A move takes two daemon starts. The first copies the files into the root,
// restores its labels, checks the daemon runs from the copy, points the units
// at it, and restarts the daemon. The second, running from the root, removes
// the files under LegacyPath. So nothing is removed until the daemon has been
// restarted from the copy, and the daemon never runs from a binary that is
// gone. A failed restart leaves the move to the next start, with every file
// the old and new units name in place.
//
// A host whose root cannot run programs is not moved. Where the filesystem is
// mounted noexec no copy is made; where MoveOptions.Verify fails, the copy is
// replaced with the link again and the units pointed back at LegacyPath. The
// host stays linked either way, and the next start tries again.
//
// It reports whether it restarted the daemon, which it may only have queued.
// The caller must then wait to be replaced rather than carry on.
func ReconcileMove(ctx context.Context, log *slog.Logger, opts MoveOptions) (bool, error) {
	return reconcileMove(ctx, log, Path, LegacyPath, opts, moveHost{
		executable: os.Executable,
		relabel:    restoreLabels,
		noexec:     fsutil.MountedNoexec,
	})
}

func reconcileMove(
	ctx context.Context,
	log *slog.Logger,
	root, legacy string,
	opts MoveOptions,
	host moveHost,
) (bool, error) {
	current, err := state(root, legacy)
	if err != nil {
		return false, err
	}

	switch current {
	case StateMoving, StateLinked:
	case StateAbsent, StateInstalled, StateOther:
		return false, nil
	}

	if opts.Verify == nil {
		return false, errors.New("hostroot: MoveOptions.Verify is required")
	}

	self, err := host.executable()
	if err != nil {
		return false, fmt.Errorf("resolve the daemon's executable: %w", err)
	}

	if current == StateMoving {
		log.Info("finishing the move of the agent's files to the host root", "path", root)

		return completeMove(ctx, log, root, legacy, opts, self, host)
	}

	if err := recordDigest(opts.Record, self); err != nil {
		return false, err
	}

	ready, reason, err := moveReady(opts)
	if err != nil {
		return false, err
	}

	if !ready {
		log.Info("keeping the agent's files under the legacy root", "path", legacy, "reason", reason)

		return false, nil
	}

	// Verify would catch it too, but only after a full copy, at every start.
	parent := filepath.Dir(root)
	if noexec, err := host.noexec(parent); err != nil {
		return false, err
	} else if noexec {
		log.Warn("keeping the agent's files under the legacy root: the host root's filesystem is mounted noexec",
			"path", parent, "legacy", legacy)

		return false, nil
	}

	log.Info("moving the agent's files to the host root", "from", legacy, "to", root)

	if err := move(log, root, legacy, opts.Files); err != nil {
		return false, err
	}

	return completeMove(ctx, log, root, legacy, opts, self, host)
}

// completeMove finishes a move whose copy is in place at root. self is the
// running daemon's executable.
func completeMove(
	ctx context.Context,
	log *slog.Logger,
	root, legacy string,
	opts MoveOptions,
	self string,
	host moveHost,
) (bool, error) {
	// Laid out and labeled as a fresh installation is, before any unit runs
	// from it. A move interrupted after the rename has not done it yet, and
	// doing it again is harmless.
	if err := prepare(ctx, log, root, opts.Subdirs, host.relabel); err != nil {
		return false, err
	}

	// Before any unit names the copy: once one does, a daemon that cannot run
	// from it does not start again, and nothing rolls it back, since the
	// recovery unit only acts on an AgentUpgrade. A daemon already running
	// from the copy has shown it runs there.
	running := under(self, legacy)
	if running {
		if err := opts.Verify(ctx, root); err != nil {
			return false, undoMove(ctx, log, root, legacy, opts, err)
		}
	}

	if err := opts.RewriteUnits(ctx); err != nil {
		return false, err
	}

	// A daemon still running from the legacy files is restarted from the copy
	// before they go, and the restarted daemon removes them. Until then the
	// files the old units named are all in place, so a restart that fails
	// strands nothing, and the next start tries again.
	if running {
		log.Info("restarting the daemon from the host root; it removes the files under the legacy root",
			"path", root, "running", self)

		if err := opts.Restart(ctx); err != nil {
			return false, err
		}

		return true, nil
	}

	for _, rel := range opts.Files {
		if err := removeOwned(filepath.Join(legacy, rel)); err != nil {
			return false, err
		}
	}

	// Deepest first, so a directory nested in another is gone before its
	// parent is checked.
	dirs := slices.Clone(opts.Dirs)
	slices.SortStableFunc(dirs, func(a, b string) int {
		return strings.Count(filepath.Clean(b), string(filepath.Separator)) - strings.Count(filepath.Clean(a), string(filepath.Separator))
	})

	for _, rel := range dirs {
		if err := removeEmptyDir(log, filepath.Join(legacy, rel)); err != nil {
			return false, err
		}
	}

	// Before the marker goes: a crash could otherwise bring the legacy files
	// back without it, and Migrate refuses a host installed under both roots.
	if err := fsutil.SyncFilesystems(legacy); err != nil {
		return false, err
	}

	// Only a linked host reads it, and this one is no longer linked.
	if err := removeOwned(opts.Record); err != nil {
		return false, err
	}

	if err := removeIfExists(filepath.Join(root, movingMarker)); err != nil {
		return false, err
	}

	log.Info("moved the agent's files to the host root", "path", root, "from", legacy)

	return false, nil
}

// undoMove puts the link to legacy back in place of a copy the daemon cannot
// run from, and points the units back at legacy, which an earlier pass of the
// same move may have rewritten. The legacy files are all still there, since
// only a daemon running from the copy removes them. The record stays, so the
// next start tries the move again. It returns why the move was undone, and
// why undoing it failed if it did.
func undoMove(ctx context.Context, log *slog.Logger, root, legacy string, opts MoveOptions, cause error) error {
	log.Warn("the agent cannot run from the host root; keeping its files under the legacy root",
		"path", root, "legacy", legacy, "error", cause)

	err := relink(root, legacy)
	if err == nil {
		err = opts.RewriteUnits(ctx)
	}

	if err != nil {
		return fmt.Errorf("the agent cannot run from %s: %w; and pointing the host back at %s failed: %w", root, cause, legacy, err)
	}

	return fmt.Errorf("the agent cannot run from %s, so it stays linked to %s: %w", root, legacy, cause)
}

// relink replaces the copy at root with the link to legacy that Migrate makes.
// Units that name paths under root reach the legacy files through it, so they
// work whether or not they have been pointed back yet.
func relink(root, legacy string) error {
	aside := root + stagingSuffix

	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("remove %s: %w", aside, err)
	}

	// A link cannot replace a directory, so the copy is set aside first.
	// Until the link is in place there is no root, which Migrate would link
	// the same way.
	if err := os.Rename(root, aside); err != nil {
		return fmt.Errorf("move %s to %s: %w", root, aside, err)
	}

	if err := renameio.Symlink(legacy, root); err != nil {
		return fmt.Errorf("link %s to %s: %w", root, legacy, err)
	}

	if err := fsutil.SyncDir(filepath.Dir(root)); err != nil {
		return err
	}

	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("remove %s: %w", aside, err)
	}

	return nil
}

// atOrUnder reports whether path is the legacy root or under it, as given or
// with symlinks resolved.
func atOrUnder(path, legacy string) bool {
	clean := filepath.Clean(path)

	return clean == filepath.Clean(legacy) || clean == canonical(legacy) || under(clean, legacy)
}

// under reports whether path is under the legacy root, as given or with
// symlinks resolved: /proc/self/exe names the resolved path, and the legacy
// root is itself a link on some images.
func under(path, legacy string) bool {
	clean := filepath.Clean(path)

	for _, prefix := range []string{filepath.Clean(legacy), canonical(legacy)} {
		if strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// removeOwned removes one of the agent's files when it is there. Checking first
// is not an optimization: on a read-only filesystem, unlinking a path that is
// not there fails with EROFS rather than ENOENT.
func removeOwned(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return removeIfExists(path)
}

// removeEmptyDir removes one of the agent's directories when it is there and
// empty. Anything left in it is not the agent's, so the directory stays. Nor is
// anything other than a directory at the path: a link there is an operator's.
func removeEmptyDir(log *slog.Logger, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	if !info.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if len(entries) > 0 {
		log.Info("keeping a directory under the legacy root that holds files the agent did not install", "path", path)

		return nil
	}

	return removeIfExists(path)
}

// moveReady reports whether a linked host can be moved: no AgentUpgrade is
// waiting to be reported, and the current and last-good links resolve to
// binaries recorded in opts.Record. A binary that is not recorded has never run
// the code that records it, so it predates the host root.
func moveReady(opts MoveOptions) (bool, string, error) {
	if _, err := os.Stat(opts.SignalPath); err == nil {
		return false, "an AgentUpgrade has not been reported yet", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "", fmt.Errorf("inspect AgentUpgrade signal: %w", err)
	}

	known, err := loadDigests(opts.Record)
	if err != nil {
		return false, "", err
	}

	for _, slot := range [][2]string{{"current", opts.CurrentPath}, {"last-good", opts.LastGoodPath}} {
		name, link := slot[0], slot[1]

		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			return false, fmt.Sprintf("the %s binary does not resolve: %v", name, err), nil
		}

		digest, err := fileDigest(target)
		if err != nil {
			return false, "", err
		}

		if !known[digest] {
			return false, fmt.Sprintf("the %s binary %s predates the host root; the move follows the next AgentUpgrade", name, target), nil
		}
	}

	return true, "", nil
}

// recordDigest adds the digest of the binary at path to the record unless it is
// there.
func recordDigest(record, path string) error {
	digest, err := fileDigest(path)
	if err != nil {
		return err
	}

	known, err := loadDigests(record)
	if err != nil {
		return err
	}

	if known[digest] {
		return nil
	}

	known[digest] = true

	return fsutil.WriteFileDurable(record, []byte(strings.Join(slices.Sorted(maps.Keys(known)), "\n")+"\n"), 0o600)
}

// loadDigests reads the digests recorded at path. A missing file records none.
func loadDigests(path string) (map[string]bool, error) {
	known := map[string]bool{}

	data, err := os.ReadFile(path) //nolint:gosec // The agent's own record.
	if errors.Is(err, os.ErrNotExist) {
		return known, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	for _, field := range strings.Fields(string(data)) {
		known[field] = true
	}

	return known, nil
}

func fileDigest(path string) (string, error) {
	sum, err := fsutil.FileSHA256(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	return hex.EncodeToString(sum[:]), nil
}
