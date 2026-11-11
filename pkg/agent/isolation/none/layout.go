// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package none is the isolation backend that runs the node stack directly on
// the host as agent-owned systemd units. Artifacts live in versioned
// directories under an agent-owned base, selected by a "current" symlink, so an
// upgrade is a symlink flip that can be flipped back.
//
// The agent's node stack operations (cmd/agent/internal/noneops) build the
// Backend with its install and readiness hooks.
package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/fsutil"
)

// DefaultBase is the agent-owned directory that holds versioned artifacts.
const DefaultBase = "/opt/unbounded/node"

const currentName = "current"

// Layout is the on-disk arrangement of versioned artifacts.
type Layout struct {
	// Base is the agent-owned directory. Empty means DefaultBase.
	Base string
}

func (l Layout) base() string {
	if l.Base == "" {
		return DefaultBase
	}

	return l.Base
}

// VersionDir is the directory holding the artifacts of one version.
func (l Layout) VersionDir(version string) string {
	return filepath.Join(l.base(), version)
}

// CurrentLink is the symlink that selects the active version.
func (l Layout) CurrentLink() string { return filepath.Join(l.base(), currentName) }

// BinDir is where the active version's executables are found. Units refer to
// binaries through the current link so a flip changes what they run.
func (l Layout) BinDir() string { return filepath.Join(l.CurrentLink(), "bin") }

// validVersion rejects values that could escape the base directory or collide
// with the current link.
func validVersion(version string) error {
	switch {
	case version == "":
		return errors.New("version is empty")
	case version == currentName:
		return fmt.Errorf("version %q is reserved", version)
	case version == "." || version == "..",
		strings.ContainsAny(version, "/\x00"):
		return fmt.Errorf("invalid version %q", version)
	}

	return nil
}

// Ensure creates the version directory (and its bin directory) if needed,
// makes the base, version and bin directories 0755, and returns the version
// directory's path. It is idempotent.
func (l Layout) Ensure(version string) (string, error) {
	if err := validVersion(version); err != nil {
		return "", err
	}

	dir := l.VersionDir(version)
	for _, d := range []string{l.base(), dir, filepath.Join(dir, "bin")} {
		if err := MkdirTraversable(d); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}

	return dir, nil
}

// MkdirTraversable creates dir with any missing parents and makes dir, and each
// parent it created, 0755 whatever the umask. The node's LocalDNS runs as a
// dynamic user, which has to traverse the base and version directories to
// reach its binaries; under the umask 027 of a hardened host, plain MkdirAll
// leaves them 0750 and the unit fails to execute. A parent that already
// existed keeps its mode: it is not the agent's to change.
func MkdirTraversable(dir string) error {
	dir = filepath.Clean(dir)

	created := []string{dir}

	for p := filepath.Dir(dir); p != filepath.Dir(p); p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		created = append(created, p)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, p := range created {
		if err := os.Chmod(p, 0o755); err != nil { //nolint:gosec // traversable by the LocalDNS service user, by design
			return err
		}
	}

	return nil
}

// Current returns the active version, or "" when nothing is active yet.
func (l Layout) Current() (string, error) {
	target, err := os.Readlink(l.CurrentLink())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read %s: %w", l.CurrentLink(), err)
	}

	return filepath.Base(target), nil
}

// Flip atomically points the current link at an installed version and returns
// the version it replaced ("" if there was none). The new link is built beside
// the old one and renamed over it, so there is no moment without a link.
func (l Layout) Flip(version string) (string, error) {
	if err := validVersion(version); err != nil {
		return "", err
	}

	if info, err := os.Stat(l.VersionDir(version)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("version %q is not installed", version)
	}

	prev, err := l.Current()
	if err != nil {
		return "", err
	}

	tmp := l.CurrentLink() + ".new"
	_ = os.Remove(tmp) //nolint:errcheck // a stale temp link from an interrupted flip

	// A relative target keeps the layout relocatable.
	if err := os.Symlink(version, tmp); err != nil {
		return "", fmt.Errorf("create link: %w", err)
	}

	if err := os.Rename(tmp, l.CurrentLink()); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best effort cleanup of the unused link

		return "", fmt.Errorf("activate version %q: %w", version, err)
	}

	// The flip is what decides the version the node boots into, so it must
	// survive a power loss once Flip returns.
	if err := fsutil.SyncDir(l.base()); err != nil {
		return "", fmt.Errorf("activate version %q: sync %s: %w", version, l.base(), err)
	}

	return prev, nil
}

// Versions lists installed versions in sorted order.
func (l Layout) Versions() ([]string, error) {
	entries, err := os.ReadDir(l.base())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() && e.Name() != currentName {
			out = append(out, e.Name())
		}
	}

	sort.Strings(out)

	return out, nil
}

// ErrVersionActive is returned by Remove for the active version.
var ErrVersionActive = errors.New("version is active")

// Remove deletes one installed version. The active version cannot be removed.
func (l Layout) Remove(version string) error {
	if err := validVersion(version); err != nil {
		return err
	}

	cur, err := l.Current()
	if err != nil {
		return err
	}

	if cur == version {
		return fmt.Errorf("version %q: %w", version, ErrVersionActive)
	}

	return os.RemoveAll(l.VersionDir(version))
}

// RemoveAll deletes the whole base directory, active version included.
func (l Layout) RemoveAll() error { return os.RemoveAll(l.base()) }
