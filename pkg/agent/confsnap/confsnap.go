// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package confsnap keeps a byte-for-byte copy of the host configuration files
// that more than one node stack writes, so a failed migration or upgrade can put
// them back. A snapshot is a directory: a blob per regular file and a manifest
// that records every path, its kind, mode and owner. The manifest is written
// last and atomically, so a snapshot that exists is complete and a crash while
// taking one leaves nothing to restore.
//
// Snapshots hold credentials (kubelet client keys), so the directory and the
// blobs are private to the owner.
package confsnap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/google/renameio/v2"

	"github.com/Azure/unbounded/internal/fsutil"
)

// ErrNoSnapshot is returned by Restore when dir holds no complete snapshot.
var ErrNoSnapshot = errors.New("no configuration snapshot")

const (
	manifestName    = "manifest.json"
	blobDir         = "blobs"
	manifestVersion = 1
)

// Kind is what a path was when the snapshot was taken.
type Kind string

const (
	// Absent means the path did not exist; restoring removes it.
	Absent  Kind = "absent"
	File    Kind = "file"
	Dir     Kind = "dir"
	Symlink Kind = "symlink"
)

type entry struct {
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Mode   uint32 `json:"mode,omitempty"`
	UID    int    `json:"uid,omitempty"`
	GID    int    `json:"gid,omitempty"`
	Blob   string `json:"blob,omitempty"`
	Target string `json:"target,omitempty"`
}

type manifest struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
	Entries []entry  `json:"entries"`
}

// ErrSnapshotExists is returned by Take when dir already holds a complete
// snapshot. That snapshot is the only record of a configuration a crash may
// have interrupted the restore of, so it is never overwritten: the caller
// restores or discards it first.
var ErrSnapshotExists = errors.New("a configuration snapshot already exists")

// Take records the state of paths, which are absolute in-node paths, below
// root (empty means the real filesystem root) into dir. A complete snapshot
// already in dir is refused with ErrSnapshotExists; an incomplete one, which a
// crash during an earlier Take leaves, is replaced. The snapshot is durable
// once Take returns: every saved file and the manifest are synced, the
// manifest last, so a manifest on disk always names files that are there. A directory is recorded with everything below it; symbolic
// links are recorded, not followed. A path that does not exist is recorded as
// absent. Sockets, devices and other special files are an error, since they
// cannot be put back.
func Take(dir, root string, paths []string) (err error) {
	if Exists(dir) {
		return fmt.Errorf("%w in %s", ErrSnapshotExists, dir)
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear snapshot directory: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, blobDir), 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}

	// A snapshot that is not complete is not a snapshot, and its copies of
	// kubelet keys must not outlive the failure: nothing records them, so
	// nothing would remove them.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir) //nolint:errcheck // best effort; the error being returned is the cause
		}
	}()

	m := manifest{Version: manifestVersion}
	seen := map[string]bool{}

	for _, p := range paths {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}

		seen[p] = true
		m.Roots = append(m.Roots, p)

		if err := record(&m, dir, root, p); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot manifest: %w", err)
	}

	if err := fsutil.SyncDir(filepath.Join(dir, blobDir)); err != nil {
		return fmt.Errorf("sync snapshot files: %w", err)
	}

	if err := fsutil.WriteFileDurable(filepath.Join(dir, manifestName), data, 0o600); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}

	return nil
}

func record(m *manifest, dir, root, p string) error {
	host := filepath.Join(root, p)

	info, err := os.Lstat(host)
	if errors.Is(err, fs.ErrNotExist) {
		m.Entries = append(m.Entries, entry{Path: p, Kind: Absent})
		return nil
	}

	if err != nil {
		return fmt.Errorf("stat %s: %w", host, err)
	}

	if !info.IsDir() {
		return recordOne(m, dir, p, host, info)
	}

	return filepath.WalkDir(host, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(host, path)
		if err != nil {
			return err
		}

		return recordOne(m, dir, filepath.Join(p, rel), path, info)
	})
}

func recordOne(m *manifest, dir, p, host string, info fs.FileInfo) error {
	e := entry{Path: p, Mode: uint32(info.Mode().Perm()), UID: -1, GID: -1}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.UID, e.GID = int(st.Uid), int(st.Gid)
	}

	switch mode := info.Mode(); {
	case mode.IsDir():
		e.Kind = Dir
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(host)
		if err != nil {
			return fmt.Errorf("read link %s: %w", host, err)
		}

		e.Kind, e.Target = Symlink, target
	case mode.IsRegular():
		data, err := os.ReadFile(host)
		if err != nil {
			return fmt.Errorf("read %s: %w", host, err)
		}

		e.Kind = File
		e.Blob = fmt.Sprintf("%06d", len(m.Entries))

		if err := writeSynced(filepath.Join(dir, blobDir, e.Blob), data); err != nil {
			return fmt.Errorf("save %s: %w", host, err)
		}
	default:
		return fmt.Errorf("cannot snapshot %s: unsupported file type %s", host, mode.Type())
	}

	m.Entries = append(m.Entries, e)

	return nil
}

// writeSynced writes a new snapshot file and syncs its contents. The directory
// entry is synced once for all files before the manifest is written.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close() //nolint:errcheck // the write error is the one to report

		return err
	}

	return errors.Join(f.Sync(), f.Close())
}

// Exists reports whether dir holds a complete snapshot.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, manifestName))

	return err == nil
}

// Discard removes the snapshot.
func Discard(dir string) error {
	return os.RemoveAll(dir)
}

// Restore puts every recorded path back as it was when the snapshot was taken:
// the same bytes, mode, owner and links. Files that were created below a
// recorded directory since are removed, and a path recorded as absent is
// removed. It is idempotent, and leaves a file alone when it already matches.
// Every path is attempted, and the errors are joined.
func Restore(dir, root string) error {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoSnapshot
	}

	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("decode snapshot manifest: %w", err)
	}

	if m.Version != manifestVersion {
		return fmt.Errorf("snapshot manifest has unsupported version %d", m.Version)
	}

	var errs []error

	for _, r := range m.Roots {
		errs = append(errs, restoreRoot(m, dir, root, r)...)
	}

	return errors.Join(errs...)
}

func restoreRoot(m manifest, dir, root, r string) []error {
	var under []entry

	for _, e := range m.Entries {
		if e.Path == r || strings.HasPrefix(e.Path, r+"/") {
			under = append(under, e)
		}
	}

	sort.SliceStable(under, func(i, j int) bool { return under[i].Path < under[j].Path })

	if len(under) == 1 && under[0].Kind == Absent {
		if err := os.RemoveAll(filepath.Join(root, r)); err != nil {
			return []error{fmt.Errorf("remove %s: %w", filepath.Join(root, r), err)}
		}

		return nil
	}

	var errs []error

	if len(under) > 0 && under[0].Kind == Dir {
		errs = append(errs, prune(root, r, under)...)
	}

	for _, e := range under {
		if err := restoreEntry(dir, root, e); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// prune removes what was created below the directory r since the snapshot.
func prune(root, r string, keep []entry) []error {
	want := make(map[string]bool, len(keep))
	for _, e := range keep {
		want[e.Path] = true
	}

	var (
		errs   []error
		remove []string
	)

	host := filepath.Join(root, r)

	base := root
	if base == "" {
		base = "/"
	}

	walkErr := filepath.WalkDir(host, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return relErr
		}

		if !want["/"+filepath.ToSlash(rel)] {
			remove = append(remove, path)

			if d.IsDir() {
				return filepath.SkipDir
			}
		}

		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("scan %s: %w", host, walkErr))
	}

	for _, path := range remove {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}

	return errs
}

func restoreEntry(dir, root string, e entry) error {
	host := filepath.Join(root, e.Path)

	switch e.Kind {
	case Dir:
		return restoreDir(host, e)
	case Symlink:
		return restoreSymlink(host, e)
	case File:
		return restoreFile(dir, host, e)
	case Absent:
		return os.RemoveAll(host)
	default:
		return fmt.Errorf("snapshot entry %s has unknown kind %q", e.Path, e.Kind)
	}
}

func restoreDir(host string, e entry) error {
	if info, err := os.Lstat(host); err == nil && !info.IsDir() {
		if err := os.RemoveAll(host); err != nil {
			return fmt.Errorf("replace %s: %w", host, err)
		}
	}

	if err := os.MkdirAll(host, fs.FileMode(e.Mode)); err != nil {
		return fmt.Errorf("create %s: %w", host, err)
	}

	return applyMeta(host, e)
}

func restoreSymlink(host string, e entry) error {
	if cur, err := os.Readlink(host); err == nil && cur == e.Target {
		return nil
	}

	if err := os.RemoveAll(host); err != nil {
		return fmt.Errorf("replace %s: %w", host, err)
	}

	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		return err
	}

	if err := os.Symlink(e.Target, host); err != nil {
		return fmt.Errorf("link %s: %w", host, err)
	}

	return nil
}

func restoreFile(dir, host string, e entry) error {
	want, err := os.ReadFile(filepath.Join(dir, blobDir, e.Blob))
	if err != nil {
		return fmt.Errorf("read snapshot of %s: %w", e.Path, err)
	}

	if info, err := os.Lstat(host); err == nil {
		if info.Mode().IsRegular() {
			if have, err := os.ReadFile(host); err == nil && bytes.Equal(have, want) {
				return applyMeta(host, e)
			}
		} else if err := os.RemoveAll(host); err != nil {
			return fmt.Errorf("replace %s: %w", host, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		return err
	}

	if err := renameio.WriteFile(host, want, fs.FileMode(e.Mode), renameio.WithTempDir(filepath.Dir(host))); err != nil {
		return fmt.Errorf("restore %s: %w", host, err)
	}

	return applyMeta(host, e)
}

// applyMeta sets the recorded mode and owner when they differ.
func applyMeta(host string, e entry) error {
	info, err := os.Lstat(host)
	if err != nil {
		return err
	}

	if info.Mode().Perm() != fs.FileMode(e.Mode) {
		if err := os.Chmod(host, fs.FileMode(e.Mode)); err != nil {
			return fmt.Errorf("chmod %s: %w", host, err)
		}
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || e.UID < 0 || e.GID < 0 || (int(st.Uid) == e.UID && int(st.Gid) == e.GID) {
		return nil
	}

	if err := os.Lchown(host, e.UID, e.GID); err != nil {
		return fmt.Errorf("chown %s: %w", host, err)
	}

	return nil
}
