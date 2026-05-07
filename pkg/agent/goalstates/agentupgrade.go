// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"strings"
)

const AgentUpgradeBinaryName = "unbounded-agent"

// AgentUpgradePaths describes the host-side blue-green agent binary layout.
type AgentUpgradePaths struct {
	BinaryPath   string
	BluePath     string
	GreenPath    string
	CurrentPath  string
	LastGoodPath string
}

// DefaultAgentUpgradePaths returns the production host-side agent binary paths.
func DefaultAgentUpgradePaths() AgentUpgradePaths {
	return AgentUpgradePaths{
		BinaryPath:   DaemonBinaryPath,
		BluePath:     DaemonBinaryBluePath,
		GreenPath:    DaemonBinaryGreenPath,
		CurrentPath:  DaemonBinaryCurrentPath,
		LastGoodPath: DaemonBinaryLastGoodPath,
	}
}

// ResolvedAgentUpgradePaths returns the host-side agent binary paths after
// applying environment overrides.
func ResolvedAgentUpgradePaths() AgentUpgradePaths {
	return AgentUpgradePaths{
		BinaryPath:   resolveDaemonBinaryPath(EnvDaemonBinary, DaemonBinaryPath),
		BluePath:     resolveDaemonBinaryPath(EnvDaemonBinaryBlue, DaemonBinaryBluePath),
		GreenPath:    resolveDaemonBinaryPath(EnvDaemonBinaryGreen, DaemonBinaryGreenPath),
		CurrentPath:  resolveDaemonBinaryPath(EnvDaemonBinaryCurrent, DaemonBinaryCurrentPath),
		LastGoodPath: resolveDaemonBinaryPath(EnvDaemonBinaryLastGood, DaemonBinaryLastGoodPath),
	}
}

func resolveDaemonBinaryPath(envName, defaultPath string) string {
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		return path
	}

	return defaultPath
}

// AgentUpgrade captures the desired host-side binary state for one upgrade.
type AgentUpgrade struct {
	DownloadURL        string
	BinaryName         string
	PreviousBinaryPath string
	TargetBinaryPath   string
	CurrentLinkPath    string
	LastGoodLinkPath   string
}

// ResolveAgentUpgrade returns the target blue-green binary state for an upgrade.
func (p AgentUpgradePaths) ResolveAgentUpgrade(downloadURL, previousBinaryPath string) AgentUpgrade {
	targetPath := p.BluePath
	if previousBinaryPath == p.BluePath {
		targetPath = p.GreenPath
	}

	return AgentUpgrade{
		DownloadURL:        downloadURL,
		BinaryName:         AgentUpgradeBinaryName,
		PreviousBinaryPath: previousBinaryPath,
		TargetBinaryPath:   targetPath,
		CurrentLinkPath:    p.CurrentPath,
		LastGoodLinkPath:   p.LastGoodPath,
	}
}

// InitialDaemonBinaryTarget returns the first executable binary that can seed
// the current daemon binary link.
func (p AgentUpgradePaths) InitialDaemonBinaryTarget() (string, error) {
	for _, path := range []string{p.BluePath, p.GreenPath, p.BinaryPath} {
		if isExecutableFile(path) {
			return path, nil
		}
	}

	return "", os.ErrNotExist
}

// isExecutableFile reports whether path is a regular executable file.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// AgentUpgradeSignalPaths describes host-side AgentUpgrade signal file paths.
type AgentUpgradeSignalPaths struct {
	OperationPath string
	FailurePath   string
}

// DefaultAgentUpgradeSignalPaths returns the production host-side AgentUpgrade
// signal file paths.
func DefaultAgentUpgradeSignalPaths() AgentUpgradeSignalPaths {
	return AgentUpgradeSignalPaths{
		OperationPath: DaemonAgentUpgradeOperationPath,
		FailurePath:   DaemonAgentUpgradeFailurePath,
	}
}

// ResolvedAgentUpgradeSignalPaths returns the host-side AgentUpgrade signal
// file paths after applying environment overrides.
func ResolvedAgentUpgradeSignalPaths() AgentUpgradeSignalPaths {
	return AgentUpgradeSignalPaths{
		OperationPath: resolveDaemonBinaryPath(EnvDaemonAgentUpgradeOperationPath, DaemonAgentUpgradeOperationPath),
		FailurePath:   resolveDaemonBinaryPath(EnvDaemonAgentUpgradeFailurePath, DaemonAgentUpgradeFailurePath),
	}
}
