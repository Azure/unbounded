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
	// installation has them.
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
	// before it returns. The files under LegacyPath are removed after it.
	RewriteUnits func(context.Context) error
	// Restart restarts the daemon, whose own binary the move removed.
	Restart func(context.Context) error
}

// ReconcileMove moves a host an older agent installed from LegacyPath into a
// real directory at Path, once that cannot strand a rollback, and finishes a
// move that was interrupted. It reports whether it restarted the daemon. The
// caller runs it from the daemon, after Migrate, while holding whatever keeps
// an upgrade or a reset from changing the layout.
//
// Each run on a linked host records the running binary's digest. Older agents
// never do, so a linked host is moved only when neither the current nor the
// last-good binary predates the host root; until then the upgrade from the
// older agent can still roll back to it, and it needs its files under
// LegacyPath. That is at the first run after the AgentUpgrade that pushes the
// older agent out of last-good.
//
// Every path the units name stays valid at each step: the files under
// LegacyPath stay until RewriteUnits has pointed the units at the new ones. A
// move interrupted after the copy is in place is finished at the next run; one
// interrupted before it starts over.
func ReconcileMove(ctx context.Context, log *slog.Logger, opts MoveOptions) (bool, error) {
	return reconcileMove(ctx, log, Path, LegacyPath, opts, os.Executable, restoreLabels)
}

func reconcileMove(
	ctx context.Context,
	log *slog.Logger,
	root, legacy string,
	opts MoveOptions,
	executable func() (string, error),
	relabel func(context.Context, *slog.Logger, string),
) (bool, error) {
	current, err := state(root, legacy)
	if err != nil {
		return false, err
	}

	switch current {
	case StateMoving:
		log.Info("finishing the move of the agent's files to the host root", "path", root)

		return completeMove(ctx, log, root, legacy, opts)
	case StateLinked:
	case StateAbsent, StateInstalled, StateOther:
		return false, nil
	}

	self, err := executable()
	if err != nil {
		return false, fmt.Errorf("resolve the daemon's executable: %w", err)
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

	log.Info("moving the agent's files to the host root", "from", legacy, "to", root)

	if err := move(ctx, log, root, legacy, opts.Files, opts.Subdirs, relabel); err != nil {
		return false, err
	}

	return completeMove(ctx, log, root, legacy, opts)
}

func completeMove(ctx context.Context, log *slog.Logger, root, legacy string, opts MoveOptions) (bool, error) {
	// The units first, so nothing names the legacy files when they go.
	if err := opts.RewriteUnits(ctx); err != nil {
		return false, err
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

	// The running daemon's binary is gone from disk. It keeps running, but
	// anything that copies its own executable would find nothing.
	if err := opts.Restart(ctx); err != nil {
		return false, err
	}

	return true, nil
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
