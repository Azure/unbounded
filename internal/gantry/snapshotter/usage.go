// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package snapshotter

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/containerd/v2/core/snapshots"
)

// hardlink identifies a file by inode so a link farm is not counted twice.
type hardlink struct {
	dev uint64
	ino uint64
}

// diskUsage measures what a local snapshot directory actually costs on the
// node. Allocated blocks are used rather than apparent size so a sparse file
// and a hole punched file report what the filesystem gave them.
//
// A missing directory reports zero rather than failing: a RACER backed
// snapshot has no local directory at all, and asking a committed snapshot for
// its usage must still work.
func diskUsage(ctx context.Context, root string) (snapshots.Usage, error) {
	var (
		usage snapshots.Usage
		seen  map[hardlink]struct{}
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				// The tree can change underneath the walk; a file that has just
				// been removed costs nothing.
				return nil
			}

			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		stat, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}

		if stat.Nlink > 1 && !fi.IsDir() {
			key := hardlink{dev: uint64(stat.Dev), ino: stat.Ino}
			if _, dup := seen[key]; dup {
				return nil
			}

			if seen == nil {
				seen = map[hardlink]struct{}{}
			}

			seen[key] = struct{}{}
		}

		usage.Inodes++
		usage.Size += stat.Blocks * 512

		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return snapshots.Usage{}, nil
		}

		return snapshots.Usage{}, err
	}

	return usage, nil
}
