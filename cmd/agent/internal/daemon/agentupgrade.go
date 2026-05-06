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

	if err := agentbinary.InstallFromTarGz(ctx, upgrade.DownloadURL, upgrade.TargetBinaryPath, upgrade.BinaryName, upgrade.BinaryMode); err != nil {
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
	paths := goalstates.DefaultAgentUpgradePaths()
	paths.BinaryPath = daemonBinaryPath()
	paths.BluePath = daemonBinaryBluePath()
	paths.GreenPath = daemonBinaryGreenPath()
	paths.CurrentPath = daemonBinaryCurrentPath()
	paths.LastGoodPath = daemonBinaryLastGoodPath()

	return paths
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
