// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// newCmdAllocationLaunchCheck is the command unbounded-kubelet-launch-check
// runs before every start of the kubelet in isolation mode none on an
// allocation host. A final refusal exits with
// goalstates.AllocationLaunchRefusedExitCode, which the unit does not restart,
// so the kubelet's start fails rather than loops; any other failure exits 1
// and the unit checks again.
func newCmdAllocationLaunchCheck(cmdCtx *CommandContext) *cobra.Command {
	return &cobra.Command{
		Use:    noneops.LaunchCheckCommand,
		Short:  "Check the allocation before the kubelet starts",
		Hidden: true,
		Args:   cobra.NoArgs,
		// systemd runs it with fixed arguments, so a failure is never a usage
		// problem and the usage block is just noise in the journal.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmdCtx.Setup()

			log := cmdCtx.Logger
			name := goalstates.AppliedConfigName(isolation.ModeNone, noneops.Slot)
			loadApplied := func() (*config.AgentConfig, bool, error) {
				return loadAppliedConfig(log, goalstates.AppliedConfigPath(name), goalstates.AppliedConfigChecksumPath(name))
			}

			return finalRefusalExit(authorizeNoneStart(cmd.Context(), log, loadApplied, daemon.LoadAllocationLaunchConfig, daemon.VerifyAllocationLaunch))
		},
	}
}

// authorizeNoneStart runs the allocation launch guard before every start of
// the kubelet in isolation mode none on an allocation host. It runs in its own
// oneshot rather than in the kubelet unit, so a final refusal fails the
// kubelet's start rather than loops under the kubelet's Restart=always.
//
// It picks the config to check as authorizeNSpawnStart does: the applied
// config when it loads, otherwise the allocation launch record. Unlike the
// nspawn pre-start, which runs on every host, it refuses for good when it
// finds neither: the check exists only on an allocation host, so finding
// neither means the agent's configuration was removed under it.
func authorizeNoneStart(
	ctx context.Context,
	log *slog.Logger,
	loadApplied func() (*config.AgentConfig, bool, error),
	loadRecord func() (*config.AgentConfig, bool, error),
	verify func(context.Context, *slog.Logger, *config.AgentConfig) error,
) error {
	cfg, found, err := loadApplied()
	if err != nil {
		log.Warn("cannot load the applied config for the allocation launch check; checking for the allocation launch record instead",
			"error", err)
	}

	if err != nil || !found {
		record, recordFound, recordErr := loadRecord()
		if recordErr != nil {
			log.Error("allocation launch refused", "cause", recordErr)

			return fmt.Errorf("%w: %w", daemon.ErrAllocationLaunchRefused, recordErr)
		}

		if !recordFound {
			refusal := fmt.Errorf("%w: no applied config and no allocation launch record", daemon.ErrAllocationLaunchRefused)
			log.Error("allocation launch refused", "cause", refusal)

			return refusal
		}

		cfg = record
	}

	return verify(ctx, log, cfg)
}
