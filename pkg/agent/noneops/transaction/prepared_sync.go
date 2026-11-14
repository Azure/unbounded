// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

// Startup intent must not outlive the prepared binaries/configuration it names.
// Installers sync file contents; this also persists directory entries and removals.
func (n *Node) syncPrepared(version string) error {
	systemd := unitDir(n.p.SystemdDir)

	paths := []string{n.p.Layout.VersionDir(version), filepath.Join(systemd, "multi-user.target.wants")}
	for _, unit := range []string{
		none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS, none.UnitKubeletLaunchCheck,
		goalstates.LocalDNSNetworkUnit, goalstates.LocalDNSSliceUnit, goalstates.NFTablesFlushUnit,
	} {
		paths = append(paths, filepath.Join(systemd, unit), none.DropInDir(systemd, unit))
	}

	for _, path := range append(n.configSnapshotPaths(), noneops.ModulesLoadPath,
		goalstates.ResolveHostPaths().LocalDNSNetworkHelper, goalstates.NFTablesClearPath) {
		paths = append(paths, filepath.Join(n.p.HostRoot, path))
	}

	for _, path := range paths {
		if err := syncPreparedPath(path); err != nil {
			return err
		}
	}

	return nil
}

func syncPreparedPath(path string) error {
	var dirs []string

	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}

		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		f, err := os.Open(path) //nolint:gosec // agent-owned prepared artifacts and configuration
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			return err
		}

		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return err
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		if err := fsutil.SyncDir(dirs[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if err := fsutil.SyncDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}
