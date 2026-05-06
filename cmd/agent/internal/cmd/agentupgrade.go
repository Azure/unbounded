// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func newCmdRecordAgentUpgradeFailureSignal() *cobra.Command {
	var operationPath string
	var failurePath string
	var message string

	cmd := &cobra.Command{
		Use:    "record-agent-upgrade-failure-signal",
		Short:  "Record AgentUpgrade daemon recovery failure signal",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if strings.TrimSpace(operationPath) == "" {
				return fmt.Errorf("operation path is required")
			}
			if strings.TrimSpace(failurePath) == "" {
				return fmt.Errorf("failure path is required")
			}

			return daemon.RecordAgentUpgradeFailureSignal(operationPath, failurePath, message)
		},
	}

	cmd.Flags().StringVar(&operationPath, "operation-path", goalstates.DaemonAgentUpgradeOperationPath, "path to the pending AgentUpgrade operation signal")
	cmd.Flags().StringVar(&failurePath, "failure-path", goalstates.DaemonAgentUpgradeFailurePath, "path to the AgentUpgrade failure signal")
	cmd.Flags().StringVar(&message, "message", "", "failure message to record")

	return cmd
}
