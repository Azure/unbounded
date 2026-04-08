// Package version exposes build-time version metadata.
//
// The variables Version and GitCommit are intended to be set at compile time
// via -ldflags:
//
//	go build -ldflags "-X github.com/project-unbounded/unbounded-kube/internal/version.Version=v1.0.0
//	                    -X github.com/project-unbounded/unbounded-kube/internal/version.GitCommit=abc1234"
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

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("%s (commit: %s)", Version, GitCommit)
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
