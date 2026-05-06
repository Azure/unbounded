// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "github.com/Azure/unbounded/pkg/agent/internal/utilio"

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
		BinaryPath:   utilio.GetEnvOrDefault(EnvDaemonBinary, DaemonBinaryPath),
		BluePath:     utilio.GetEnvOrDefault(EnvDaemonBinaryBlue, DaemonBinaryBluePath),
		GreenPath:    utilio.GetEnvOrDefault(EnvDaemonBinaryGreen, DaemonBinaryGreenPath),
		CurrentPath:  utilio.GetEnvOrDefault(EnvDaemonBinaryCurrent, DaemonBinaryCurrentPath),
		LastGoodPath: utilio.GetEnvOrDefault(EnvDaemonBinaryLastGood, DaemonBinaryLastGoodPath),
	}
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
