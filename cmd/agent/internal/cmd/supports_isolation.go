// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// SupportsIsolationCommand is the subcommand an agent binary answers to say
// whether it can operate an isolation mode. The daemon runs it on an agent
// upgrade candidate, and the recovery script on the last-good binary, before
// either is allowed to run a node in a mode other than nspawn. A binary that
// predates the subcommand fails it, which is the answer wanted: it cannot
// operate those modes.
const SupportsIsolationCommand = daemon.SupportsIsolationCommand

func newCmdSupportsIsolation() *cobra.Command {
	return &cobra.Command{
		Use:   SupportsIsolationCommand + " <mode>",
		Short: "Exit zero if this agent can operate the isolation mode",
		Long: `Exit zero if this agent binary can bootstrap and operate a node in the named
isolation mode, and non-zero otherwise. It reads nothing from the host.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(_ *cobra.Command, args []string) error {
			return supportsIsolation(args[0])
		},
	}
}

// supportsIsolation returns nil when this agent can operate mode.
func supportsIsolation(mode string) error {
	m, err := isolation.ParseMode(mode)
	if err != nil {
		return err
	}

	// The empty string parses as the default mode; the question names a mode.
	if mode == "" {
		return fmt.Errorf("no isolation mode named")
	}

	_, err = agentModes().lookup(m)

	return err
}
