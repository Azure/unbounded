// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Long-running daemon for node lifecycle management",
		Long: "Long-running daemon that manages the nspawn machine lifecycle. " +
			"Runs as a systemd unit after initial provisioning.",
		// systemd runs this with fixed arguments, so a failure here is never a
		// usage problem and the flag listing is noise in the journal.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return quietWhenDeferred(cmd, daemon.Run(ctx, cmdCtx.Logger))
		},
	}

	return cmd
}

// quietWhenDeferred stops cobra reporting a deferred daemon as an error.
//
// Standing down because an installation owns the host is an ordinary state
// that the daemon has already recorded as a warning. Letting cobra print
// "Error:" over the top of that puts a fault in the journal where there is
// none, which is exactly the confusion this whole path exists to remove.
func quietWhenDeferred(cmd *cobra.Command, err error) error {
	if errors.Is(err, daemon.ErrDeferred) {
		cmd.SilenceErrors = true
	}

	return err
}
