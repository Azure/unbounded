// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package buildinfo provides build metadata (version, commit, build time)
// for all unbounded-net binaries. Version is read from a container metadata
// file at startup; commit and build time come from Go's embedded VCS info
// via debug.ReadBuildInfo.
package buildinfo

import (
	"os"
	"runtime/debug"
	"strings"
)

var (
	// Version is the release version tag (e.g. "v1.2.3").
	// Set from /etc/unbounded-net/version at startup, falling back to ldflags
	// (for local dev builds), then "dev".
	Version = "dev"

	// Commit is the VCS revision from debug.ReadBuildInfo.
	Commit = "unknown"

	// BuildTime is the VCS commit time from debug.ReadBuildInfo.
	BuildTime = "unknown"
)

// versionFilePath is the path to the version file written by the container build.
const versionFilePath = "/etc/unbounded-net/version"

func init() {
	// Try reading the version from the container metadata file first.
	if data, err := os.ReadFile(versionFilePath); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			Version = v
		}
	}

	// Extract commit and build time from Go's embedded VCS info.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if len(setting.Value) > 7 {
					Commit = setting.Value[:7]
				} else if setting.Value != "" {
					Commit = setting.Value
				}
			case "vcs.time":
				if setting.Value != "" {
					BuildTime = setting.Value
				}
			}
		}
	}
}
