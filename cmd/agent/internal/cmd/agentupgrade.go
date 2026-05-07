// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

func newCmdRecordAgentUpgradeFailureSignal() *cobra.Command {
	var message string

	cmd := &cobra.Command{
		Use:    "record-agent-upgrade-failure-signal",
		Short:  "Record AgentUpgrade daemon recovery failure signal",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return daemon.RecordAgentUpgradeFailureSignal(message)
		},
	}

	cmd.Flags().StringVar(&message, "message", "", "failure message to record")

	return cmd
}
