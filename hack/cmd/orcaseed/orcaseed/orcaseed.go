// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package orcaseed implements the `orcaseed` developer tool used by
// the Orca dev harness to populate the in-cluster Azurite origin
// container with synthetic or operator-supplied content. Four
// subcommands:
//
//	generate  - synthesise N blobs of size S each (random bytes;
//	            optionally seeded for reproducibility).
//	upload    - upload a single file from disk.
//	list      - print the blobs currently in the container.
//	delete    - remove blobs (optional --prefix filter).
//
// All subcommands share connection-shape flags (--endpoint,
// --account, --account-key, --container) defaulting to the dev
// harness's NodePort-exposed Azurite at localhost:30100. The
// well-known Azurite dev key is the default --account-key value;
// it is a public Microsoft-documented constant, not a secret.
package orcaseed

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Run is the entrypoint invoked by cmd/orcaseed/main.go. Wires the
// cobra command tree, parses flags, dispatches to the chosen
// subcommand. On error prints to stderr and exits non-zero.
func Run() {
	g := defaultGlobalFlags()

	root := &cobra.Command{
		Use:           "orcaseed",
		Short:         "Populate the Orca dev-harness origin container",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.PersistentFlags().StringVar(&g.endpoint, "endpoint", g.endpoint,
		"Azure Blob endpoint URL (path-style, account-included)")
	root.PersistentFlags().StringVar(&g.account, "account", g.account,
		"Storage account name")
	root.PersistentFlags().StringVar(&g.accountKey, "account-key", g.accountKey,
		"Shared key for the account (default: well-known Azurite dev key)")
	root.PersistentFlags().StringVar(&g.containerName, "container", g.containerName,
		"Container to operate against")
	root.PersistentFlags().BoolVar(&g.ensureContainer, "ensure-container", g.ensureContainer,
		"Create the container if it does not already exist")

	root.AddCommand(newGenerateCmd(g))
	root.AddCommand(newUploadCmd(g))
	root.AddCommand(newListCmd(g))
	root.AddCommand(newDeleteCmd(g))

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
