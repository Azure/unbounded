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
	agentBinaryArchiveName           = "unbounded-agent"

	envDaemonBinary         = "UNBOUNDED_AGENT_DAEMON_BINARY"
	envDaemonBinaryBlue     = "UNBOUNDED_AGENT_DAEMON_BINARY_BLUE"
	envDaemonBinaryGreen    = "UNBOUNDED_AGENT_DAEMON_BINARY_GREEN"
	envDaemonBinaryCurrent  = "UNBOUNDED_AGENT_DAEMON_BINARY_CURRENT"
	envDaemonBinaryLastGood = "UNBOUNDED_AGENT_DAEMON_BINARY_LAST_GOOD"
)

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	currentTarget, err := resolveSymlink(daemonBinaryCurrentPath())
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	inactivePath := daemonBinaryBluePath()
	if currentTarget == daemonBinaryBluePath() {
		inactivePath = daemonBinaryGreenPath()
	}

	if err := agentbinary.InstallFromTarGz(ctx, downloadURL, inactivePath, agentBinaryArchiveName, 0o755); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", inactivePath, err)
	}

	if err := updateSymlink(daemonBinaryLastGoodPath(), currentTarget); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := updateSymlink(daemonBinaryCurrentPath(), inactivePath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	log.Info("staged upgraded daemon binary",
		"url", downloadURL,
		"previous", currentTarget,
		"current", inactivePath,
	)

	return nil
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
	if path := strings.TrimSpace(os.Getenv(envDaemonBinary)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryPath
}

func daemonBinaryBluePath() string {
	if path := strings.TrimSpace(os.Getenv(envDaemonBinaryBlue)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryBluePath
}

func daemonBinaryGreenPath() string {
	if path := strings.TrimSpace(os.Getenv(envDaemonBinaryGreen)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryGreenPath
}

func daemonBinaryCurrentPath() string {
	if path := strings.TrimSpace(os.Getenv(envDaemonBinaryCurrent)); path != "" {
		return path
	}

	return goalstates.DaemonBinaryCurrentPath
}

func daemonBinaryLastGoodPath() string {
	if path := strings.TrimSpace(os.Getenv(envDaemonBinaryLastGood)); path != "" {
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
