// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"strings"
)

const AgentUpgradeBinaryName = "unbounded-agent"

// AgentUpgradePaths describes the host-side blue-green agent binary layout.
type AgentUpgradePaths struct {
	BinaryPath        string
	BluePath          string
	GreenPath         string
	CurrentPath       string
	LastGoodPath      string
	SignalPath        string
	CurrentTargetPath string
}

// ResolvedAgentUpgradePaths returns the host-side agent binary paths after
// applying environment overrides.
func ResolvedAgentUpgradePaths() (AgentUpgradePaths, error) {
	paths := AgentUpgradePaths{
		BinaryPath:   resolveDaemonBinaryPath(EnvDaemonBinary, DaemonBinaryPath),
		BluePath:     resolveDaemonBinaryPath(EnvDaemonBinaryBlue, DaemonBinaryBluePath),
		GreenPath:    resolveDaemonBinaryPath(EnvDaemonBinaryGreen, DaemonBinaryGreenPath),
		CurrentPath:  resolveDaemonBinaryPath(EnvDaemonBinaryCurrent, DaemonBinaryCurrentPath),
		LastGoodPath: resolveDaemonBinaryPath(EnvDaemonBinaryLastGood, DaemonBinaryLastGoodPath),
		SignalPath:   resolveDaemonBinaryPath(EnvDaemonAgentUpgradeSignalPath, DaemonAgentUpgradeSignalPath),
	}
	targetPath, err := filepath.EvalSymlinks(paths.CurrentPath)
	if err != nil {
		if os.IsNotExist(err) {
			paths.CurrentTargetPath = paths.BinaryPath
			return paths, nil
		}

		return paths, err
	}

	paths.CurrentTargetPath = targetPath

	return paths, nil
}

func resolveDaemonBinaryPath(envName, defaultPath string) string {
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		return path
	}

	return defaultPath
}

// NextTargetPath returns the inactive blue-green binary path.
func (p AgentUpgradePaths) NextTargetPath() string {
	if p.CurrentTargetPath == p.BluePath {
		return p.GreenPath
	}

	return p.BluePath
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
