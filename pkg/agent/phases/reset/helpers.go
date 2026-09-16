// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// ToolMissing distinguishes a package not installed yet from an installed tool
// failing to inspect or remove resources. Permission failures are not absence.
func ToolMissing(name string) bool {
	_, err := exec.LookPath(name)
	return errors.Is(err, exec.ErrNotFound)
}

// SystemdUnavailable permits offline cleanup when no host systemd is running.
func SystemdUnavailable() bool {
	if ToolMissing("systemctl") {
		return true
	}

	_, err := os.Stat("/run/systemd/system")

	return errors.Is(err, os.ErrNotExist)
}

// removeFileIfExists ignores absence but propagates substantive removal errors.
func removeFileIfExists(log *slog.Logger, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove file", "path", path, "error", err)
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

// removeAllIfExists propagates removal failures so reset retains ownership.
func removeAllIfExists(log *slog.Logger, path string) error {
	if err := os.RemoveAll(path); err != nil {
		log.Warn("failed to remove directory", "path", path, "error", err)
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}
