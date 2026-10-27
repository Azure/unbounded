// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/gantry/config"
	gantrylog "github.com/Azure/unbounded/internal/gantry/log"
	"github.com/Azure/unbounded/internal/version"
)

// identityDir is the libp2p identity hostPath that gantry init prepares. It
// must match the DaemonSet's mount, and libp2p_identity_path must point inside
// it.
const identityDir = "/var/lib/gantry/libp2p"

// newCmdRoot builds the gantry command. runAgent and prepare implement gantry
// agent and gantry init; tests replace them.
func newCmdRoot(runAgent func(args []string) error, prepare func(dir string, logger *slog.Logger) error) *cobra.Command {
	root := &cobra.Command{
		Use:     "gantry",
		Short:   "Gantry peer-to-peer OCI distribution agent",
		Version: buildInfo(),
		// main reports every error, the agent's own included, the same way.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Usage(); err != nil {
				return err
			}

			return errors.New("gantry: subcommand required")
		},
	}

	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newCmdAgent(runAgent), newCmdInit(prepare), newCmdVersion())

	return root
}

// buildInfo is what gantry version and gantry --version print.
func buildInfo() string {
	return fmt.Sprintf("gantry %s %s/%s (go %s, commit %s, built %s)",
		version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(),
		version.GitCommit, version.BuildTime)
}

func newCmdAgent(runAgent func(args []string) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent [flags]",
		Short: "Run the Gantry P2P agent",
		// The agent parses its own flags, layered over its YAML file and
		// environment, so they reach it untouched.
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runAgent(args)
		},
	}

	// cobra cannot list flags it does not parse.
	cmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		writeAgentHelp(cmd.OutOrStdout())
	})

	return cmd
}

func writeAgentHelp(w io.Writer) {
	fs, _ := buildAgentFlagSet(config.NewDefault())
	fs.SetOutput(w)

	_, _ = fmt.Fprint(w, "Usage: gantry agent [flags]\n\nFlags:\n") //nolint:errcheck // best-effort write

	fs.PrintDefaults()
}

func newCmdInit(prepare func(dir string, logger *slog.Logger) error) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Prepare the libp2p identity directory and drop root",
		Long: "Run as root, init gives UID 65532 " + identityDir + " and everything in it,\n" +
			"restricts the directory to that user, and switches the process to it.\n" +
			"Run as any other user, it changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return prepare(identityDir, gantrylog.New(cmd.ErrOrStderr(), "info", "json"))
		},
	}
}

func newCmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), buildInfo())

			return err
		},
	}
}
