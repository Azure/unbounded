// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package version exposes build-time version metadata.
//
// The variables Version, GitCommit, and BuildTime are intended to be set at
// compile time via -ldflags:
//
//	go build -ldflags "-X github.com/Azure/unbounded-kube/internal/version.Version=v1.0.0
//	                    -X github.com/Azure/unbounded-kube/internal/version.GitCommit=abc1234
//	                    -X github.com/Azure/unbounded-kube/internal/version.BuildTime=2026-04-21T00:00:00Z"
package version

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is the semantic version of the binary.
// Set at build time; defaults to "dev" for local builds.
var Version = "dev"

// GitCommit is the git commit SHA the binary was built from.
// Set at build time; defaults to "unknown" for local builds.
var GitCommit = "unknown"

// BuildTime is the UTC timestamp the binary was built at (RFC 3339).
// Set at build time; defaults to "unknown" for local builds.
var BuildTime = "unknown"

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", Version, GitCommit, BuildTime)
}

// Command returns a cobra command that prints the version string.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(String())
		},
	}
}
