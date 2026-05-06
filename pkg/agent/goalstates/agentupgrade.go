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
		BinaryPath:   getEnvOrDefault(EnvDaemonBinary, DaemonBinaryPath),
		BluePath:     getEnvOrDefault(EnvDaemonBinaryBlue, DaemonBinaryBluePath),
		GreenPath:    getEnvOrDefault(EnvDaemonBinaryGreen, DaemonBinaryGreenPath),
		CurrentPath:  getEnvOrDefault(EnvDaemonBinaryCurrent, DaemonBinaryCurrentPath),
		LastGoodPath: getEnvOrDefault(EnvDaemonBinaryLastGood, DaemonBinaryLastGoodPath),
	}
}

func getEnvOrDefault(name, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}

	return defaultValue
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
