// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeBinaryMode           = 0o755
	agentUpgradeVerifyTimeout        = 30 * time.Second
)

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	paths := goalstates.ResolvedAgentUpgradePaths()
	currentTarget, err := resolveSymlink(paths.CurrentPath, paths.BinaryPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}
	upgrade := paths.ResolveAgentUpgrade(downloadURL, currentTarget)

	if err := agentbinary.InstallFromTarGz(ctx, upgrade.DownloadURL, upgrade.TargetBinaryPath, upgrade.BinaryName, agentUpgradeBinaryMode); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", upgrade.TargetBinaryPath, err)
	}

	if err := verifyAgentUpgradeBinary(ctx, upgrade.TargetBinaryPath); err != nil {
		return err
	}

	if err := agentbinary.UpdateSymlink(upgrade.LastGoodLinkPath, upgrade.PreviousBinaryPath); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := agentbinary.UpdateSymlink(upgrade.CurrentLinkPath, upgrade.TargetBinaryPath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	log.Info("staged upgraded daemon binary",
		"url", upgrade.DownloadURL,
		"previous", upgrade.PreviousBinaryPath,
		"current", upgrade.TargetBinaryPath,
	)

	return nil
}

func verifyAgentUpgradeBinary(ctx context.Context, path string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, agentUpgradeVerifyTimeout)
	defer cancel()

	output, err := exec.CommandContext(verifyCtx, path, "version").CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("verify upgraded daemon binary %s: %w: %s", path, err, details)
		}
		return fmt.Errorf("verify upgraded daemon binary %s: %w", path, err)
	}

	return nil
}

func recordPendingAgentUpgradeOperation(operationName string) error {
	return writeFile(agentUpgradeOperationSignalPath(), []byte(operationName+"\n"), 0o600)
}

func clearPendingAgentUpgradeOperation(log *slog.Logger) {
	if err := os.Remove(agentUpgradeOperationSignalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to clear pending AgentUpgrade operation signal", "error", err)
	}
}

func agentUpgradeOperationSignalPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonAgentUpgradeOperationPath)); path != "" {
		return path
	}

	return goalstates.DaemonAgentUpgradeOperationPath
}

func agentUpgradeFailureSignalPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonAgentUpgradeFailurePath)); path != "" {
		return path
	}

	return goalstates.DaemonAgentUpgradeFailurePath
}

func ensureDaemonBinaryLinks(log *slog.Logger) error {
	paths := goalstates.ResolvedAgentUpgradePaths()

	if _, err := filepath.EvalSymlinks(paths.CurrentPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve current daemon binary symlink: %w", err)
		}
		target, targetErr := initialDaemonBinaryTarget(paths)
		if targetErr != nil {
			return targetErr
		}
		if err := agentbinary.UpdateSymlink(paths.CurrentPath, target); err != nil {
			return fmt.Errorf("initialize current daemon symlink: %w", err)
		}
	}

	currentTarget, err := filepath.EvalSymlinks(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	if _, err := filepath.EvalSymlinks(paths.LastGoodPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve last-good daemon binary symlink: %w", err)
		}
		if err := agentbinary.UpdateSymlink(paths.LastGoodPath, currentTarget); err != nil {
			return fmt.Errorf("initialize last-good daemon symlink: %w", err)
		}
	}

	if currentTarget != paths.BinaryPath {
		// Do not replace the compatibility path when the current symlink
		// already resolves to that path. That preserves legacy installs and
		// avoids creating a BinaryPath -> CurrentPath -> BinaryPath loop.
		if err := agentbinary.UpdateSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
			return fmt.Errorf("initialize daemon compatibility symlink: %w", err)
		}
	}

	log.Info("daemon binary links initialized",
		"current", paths.CurrentPath,
		"last_good", paths.LastGoodPath,
	)

	return nil
}

func initialDaemonBinaryTarget(paths goalstates.AgentUpgradePaths) (string, error) {
	for _, path := range []string{paths.BluePath, paths.GreenPath, paths.BinaryPath} {
		if isExecutableFile(path) {
			return path, nil
		}
	}

	return "", fmt.Errorf("no executable agent binary found for daemon link initialization")
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func resolveSymlink(path, fallbackPath string) (string, error) {
	targetPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		return targetPath, nil
	}

	if os.IsNotExist(err) {
		return fallbackPath, nil
	}

	return "", err
}
