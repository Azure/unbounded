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
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove file", "path", path, "error", err)
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

// removeAllIfExists removes a path and propagates errors so ownership survives.
func removeAllIfExists(log *slog.Logger, path string) error {
	if err := os.RemoveAll(path); err != nil {
		log.Warn("failed to remove directory", "path", path, "error", err)
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}
