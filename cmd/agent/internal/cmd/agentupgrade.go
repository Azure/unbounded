// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const hostAgentBinaryMode = 0o755

//go:embed assets/agent-upgrade-plan.txt.tmpl
var hostAgentUpgradePlanTemplateText string

var hostAgentUpgradePlanTemplate = template.Must(
	template.New("host-agent-upgrade-plan").Parse(hostAgentUpgradePlanTemplateText),
)

type hostAgentUpgradeHandler struct {
	cmdCtx       *CommandContext
	preflight    bool
	writer       io.Writer
	executable   func() (string, error)
	resolvedPath func() (goalstates.AgentUpgradePaths, error)
	newService   func(goalstates.AgentUpgradePaths) agentbinary.DaemonService
	geteuid      func() int
}

func newCmdHostAgentUpgrade(cmdCtx *CommandContext) *cobra.Command {
	handler := &hostAgentUpgradeHandler{
		cmdCtx:       cmdCtx,
		writer:       os.Stdout,
		executable:   os.Executable,
		resolvedPath: goalstates.ResolvedAgentUpgradePaths,
		geteuid:      os.Geteuid,
	}
	handler.newService = func(paths goalstates.AgentUpgradePaths) agentbinary.DaemonService {
		return daemon.NewHostDaemonActivationService(handler.cmdCtx.Logger, paths)
	}

	cmd := &cobra.Command{
		Use:   "agent-upgrade",
		Short: "Activate this executable as the host agent daemon",
		Long:  "Activate this executable as the host agent daemon without creating a MachineOperation.",
		// Host delivery automation invokes this command from a separately staged
		// candidate. Keep it out of normal user-facing help because the supported
		// interactive upgrade surface is the coordinated MachineOperation command.
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			handler.writer = cmd.OutOrStdout()
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().BoolVar(&handler.preflight, "preflight", false, "Show and validate the host activation plan without applying it")

	return cmd
}

func (h *hostAgentUpgradeHandler) execute(ctx context.Context) error {
	h.cmdCtx.Setup()

	candidatePath, err := h.executable()
	if err != nil {
		return fmt.Errorf("resolve candidate executable: %w", err)
	}

	candidatePath, err = resolveExecutablePath(candidatePath)
	if err != nil {
		return err
	}

	paths, err := h.resolvedPath()
	if err != nil {
		return fmt.Errorf("resolve agent binary paths: %w", err)
	}

	options := agentbinary.ActivationOptions{
		Layout: agentbinary.Layout{
			BinaryPath:   paths.BinaryPath,
			BluePath:     paths.BluePath,
			GreenPath:    paths.GreenPath,
			CurrentPath:  paths.CurrentPath,
			LastGoodPath: paths.LastGoodPath,
		},
		CandidatePath: candidatePath,
		BinaryMode:    hostAgentBinaryMode,
		LockPath:      goalstates.DaemonAgentUpgradeLockPath,
	}
	service := h.newService(paths)

	if h.preflight {
		plan, err := agentbinary.PreflightHostDaemonActivation(ctx, options, service)
		if err != nil {
			return err
		}

		return writeHostAgentUpgradePlan(h.writer, plan)
	}

	if h.geteuid() != 0 {
		return fmt.Errorf("host agent upgrade requires root privileges")
	}

	result, err := agentbinary.ActivateHostDaemon(ctx, h.cmdCtx.Logger, options, service)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(h.writer, "activated host agent daemon: %s -> %s\n", result.PreviousPath, result.CurrentPath)

	return err
}

func resolveExecutablePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute candidate executable path: %w", err)
	}

	return filepath.Clean(path), nil
}

func writeHostAgentUpgradePlan(w io.Writer, plan agentbinary.ActivationPlan) error {
	return hostAgentUpgradePlanTemplate.Execute(w, plan)
}

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
