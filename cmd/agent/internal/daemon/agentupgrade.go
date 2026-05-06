// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeBinaryMode           = 0o755
)

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	paths := daemonAgentUpgradePaths()
	currentTarget, err := resolveSymlink(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}
	upgrade := paths.ResolveAgentUpgrade(downloadURL, currentTarget)

	if err := agentbinary.InstallFromTarGz(ctx, upgrade.DownloadURL, upgrade.TargetBinaryPath, upgrade.BinaryName, agentUpgradeBinaryMode); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", upgrade.TargetBinaryPath, err)
	}

	if err := updateSymlink(upgrade.LastGoodLinkPath, upgrade.PreviousBinaryPath); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := updateSymlink(upgrade.CurrentLinkPath, upgrade.TargetBinaryPath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	log.Info("staged upgraded daemon binary",
		"url", upgrade.DownloadURL,
		"previous", upgrade.PreviousBinaryPath,
		"current", upgrade.TargetBinaryPath,
	)

	return nil
}

func daemonAgentUpgradePaths() goalstates.AgentUpgradePaths {
	return goalstates.AgentUpgradePaths{
		BinaryPath:   daemonBinaryPath(),
		BluePath:     daemonBinaryBluePath(),
		GreenPath:    daemonBinaryGreenPath(),
		CurrentPath:  daemonBinaryCurrentPath(),
		LastGoodPath: daemonBinaryLastGoodPath(),
	}
}

func ensureDaemonBinaryLinks(log *slog.Logger) error {
	paths := daemonAgentUpgradePaths()

	if _, err := filepath.EvalSymlinks(paths.CurrentPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve current daemon binary symlink: %w", err)
		}
		target, targetErr := initialDaemonBinaryTarget(paths)
		if targetErr != nil {
			return targetErr
		}
		if err := updateSymlink(paths.CurrentPath, target); err != nil {
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
		if err := updateSymlink(paths.LastGoodPath, currentTarget); err != nil {
			return fmt.Errorf("initialize last-good daemon symlink: %w", err)
		}
	}

	if currentTarget != paths.BinaryPath {
		if err := updateSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
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

func resolveSymlink(path string) (string, error) {
	targetPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		return targetPath, nil
	}

	if os.IsNotExist(err) {
		return daemonBinaryPath(), nil
	}

	return "", err
}

func daemonBinaryPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonBinary)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryPath
}

func daemonBinaryBluePath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonBinaryBlue)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryBluePath
}

func daemonBinaryGreenPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonBinaryGreen)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryGreenPath
}

func daemonBinaryCurrentPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonBinaryCurrent)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryCurrentPath
}

func daemonBinaryLastGoodPath() string {
	if path := strings.TrimSpace(os.Getenv(goalstates.EnvDaemonBinaryLastGood)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryLastGoodPath
}

func updateSymlink(linkPath, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o750); err != nil {
		return err
	}

	tmpPath := fmt.Sprintf("%s.tmp", linkPath)
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(targetPath, tmpPath); err != nil {
		return err
	}

	return os.Rename(tmpPath, linkPath)
}
