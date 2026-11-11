// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// HostCNIBinDir is the host directory containerd looks for CNI plugins in. It
// stays the CNI default because cluster add-ons (unbounded-net's IPAM plugin,
// for one) install their plugins there from a hostPath mount.
const HostCNIBinDir = "/opt/cni/bin"

// CNIBinDir is where the active version's CNI plugins are, through the current
// link.
func (l Layout) CNIBinDir() string { return filepath.Join(l.CurrentLink(), cniBinSubdir) }

func (b *Backend) hostCNIBinDir() string { return filepath.Join(b.HostRoot, HostCNIBinDir) }

// LinkCNIPlugins links every CNI plugin of version into the host plugin
// directory, each to the plugin of the same name below the current link. A
// flip of the link therefore switches the plugins with the rest of the
// version, while plugins other components install in the host directory stay
// usable. A host file that is not a link, or a link that points outside the
// agent's base, belongs to someone else and is left alone. It is idempotent.
func (b *Backend) LinkCNIPlugins(version string) error {
	entries, err := os.ReadDir(filepath.Join(b.Layout.VersionDir(version), cniBinSubdir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("list the CNI plugins of %s: %w", version, err)
	}

	dir := b.hostCNIBinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	var errs []error

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		dest := filepath.Join(dir, e.Name())
		want := filepath.Join(b.Layout.CNIBinDir(), e.Name())

		switch target, err := os.Readlink(dest); {
		case err == nil && target == want:
			continue
		case err == nil && b.ownsLink(target):
			// A link of ours to another place; replace it below.
		case errors.Is(err, os.ErrNotExist):
		case err == nil:
			b.logger().Warn("leaving a CNI plugin link another component owns", "path", dest, "target", target)

			continue
		default:
			b.logger().Warn("leaving a CNI plugin another component installed", "path", dest)

			continue
		}

		if err := replaceLink(want, dest); err != nil {
			errs = append(errs, fmt.Errorf("link CNI plugin %s: %w", e.Name(), err))
		}
	}

	return errors.Join(errs...)
}

// UnlinkCNIPlugins removes the links LinkCNIPlugins made and nothing else, then
// the host plugin directory when that leaves it empty.
func (b *Backend) UnlinkCNIPlugins() error {
	dir := b.hostCNIBinDir()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	var errs []error

	for _, e := range entries {
		path := filepath.Join(dir, e.Name())

		target, err := os.Readlink(path)
		if err != nil || !b.ownsLink(target) {
			continue
		}

		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ownsLink reports whether a link target points into the agent's base.
func (b *Backend) ownsLink(target string) bool {
	return strings.HasPrefix(filepath.Clean(target), b.Layout.base()+string(filepath.Separator))
}

// replaceLink points dest at target atomically.
func replaceLink(target, dest string) error {
	tmp := dest + ".unbounded-new"
	_ = os.Remove(tmp) //nolint:errcheck // a stale temp link from an interrupted run

	if err := os.Symlink(target, tmp); err != nil {
		return err
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best effort cleanup of the unused link

		return err
	}

	return nil
}
