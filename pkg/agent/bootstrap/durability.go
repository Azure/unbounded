// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFilesystems orders stage outputs before checkpoint advancement. A stage
// can create directories, symlinks and files on different mounts; syncing only
// the state directory would not persist those outputs. Flush each filesystem
// once, including metadata and newly created ancestor directory entries.
// This is intentionally stronger than per-file atomic replacement and occurs
// only at bootstrap/repair boundaries, not in normal reconciliation.
func SyncFilesystems(paths ...string) error {
	seen := map[uint64]bool{}

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open stage output %s: %w", path, err)
		}

		var stat unix.Stat_t

		err = unix.Fstat(int(f.Fd()), &stat)
		if err == nil && !seen[uint64(stat.Dev)] {
			err = unix.Syncfs(int(f.Fd()))
			seen[uint64(stat.Dev)] = true
		}

		closeErr := f.Close()

		if err != nil {
			return fmt.Errorf("sync stage filesystem %s: %w", path, err)
		}

		if closeErr != nil {
			return closeErr
		}
	}

	return nil
}
