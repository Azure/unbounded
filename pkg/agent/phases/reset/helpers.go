// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// removeFileIfExists ignores absence but propagates substantive removal errors.
func removeFileIfExists(log *slog.Logger, path string) error {
	return removeIfExists(log, path, "file", os.Lstat, os.Remove)
}

// removeAllIfExists propagates removal failures so reset retains ownership.
func removeAllIfExists(log *slog.Logger, path string) error {
	return removeIfExists(log, path, "directory", os.Lstat, os.RemoveAll)
}

// removeIfExists checks for the path before removing it.
//
// Reset sweeps the default install prefix as well as the configured one, and on
// a host with a read-only /usr the default is on a read-only filesystem. There,
// unlinking a path that does not exist returns EROFS rather than ENOENT, because
// the kernel checks the parent for write access before it looks up the name. So
// the absence has to be established first, or reset fails on a file that was
// never there.
//
// Lstat rather than Stat, so a dangling symlink still counts as present and is
// removed.
func removeIfExists(
	log *slog.Logger,
	path, kind string,
	lstat func(string) (os.FileInfo, error),
	remove func(string) error,
) error {
	if _, err := lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove "+kind, "path", path, "error", err)
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}
