// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

func Run() {
	cmdCtx := &CommandContext{
		LogFormat: "text",
	}

	root := &cobra.Command{
		Use:   "agent",
		Short: "Unbounded Kubernetes Node Agent",
	}

	root.PersistentFlags().BoolVar(&cmdCtx.Debug, "debug", false, "enable debug-level logging")
	root.PersistentFlags().StringVar(&cmdCtx.LogFormat, "log-format", cmdCtx.LogFormat, "log format: text or json")
	root.PersistentFlags().BoolVar(&cmdCtx.LogNoColor, "no-color", false, "disable color in log output")

	root.AddCommand(
		newCmdStart(cmdCtx),
		newCmdPreflight(cmdCtx),
		newCmdDaemon(cmdCtx),
		newCmdReset(cmdCtx),
		newCmdVersion(),
		newCmdNSpawnLifecycle(cmdCtx),
		newCmdHostAgentUpgrade(cmdCtx),
		newCmdRecordAgentUpgradeFailureSignal(),
	)

	if err := root.Execute(); err != nil {
		// The daemon standing down for an unfinished installation is an
		// ordinary state, not a fault. It has already said so in the journal,
		// and the unit treats this code as success so systemd leaves it alone
		// rather than restarting it into its start limit.
		if !errors.Is(err, daemon.ErrDeferred) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		os.Exit(commandExitCode(err))
	}
}

// exitCodeError makes the process exit with code rather than 1. A command
// returns one only when its unit names the code.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }

func (e *exitCodeError) Unwrap() error { return e.err }

// commandExitCode returns the exit code for a command's error:
// daemon.DeferredExitCode for a deferred daemon, the code of an exitCodeError,
// and 1 for anything else.
func commandExitCode(err error) int {
	if errors.Is(err, daemon.ErrDeferred) {
		return daemon.DeferredExitCode
	}

	var coded *exitCodeError
	if errors.As(err, &coded) {
		return coded.code
	}

	return 1
}
