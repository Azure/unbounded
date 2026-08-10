// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const hostAgentBinaryMode = 0o755

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
		Use:    "agent-upgrade",
		Short:  "Activate this executable as the host agent daemon",
		Long:   "Activate this executable as the host agent daemon without creating a MachineOperation.",
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
		Mode:          hostAgentBinaryMode,
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
	if _, err := fmt.Fprintln(w, "Agent upgrade mode: host-driven"); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "Kubernetes MachineOperation: not created"); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Candidate: %s\n", plan.CandidatePath); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Active binary: %s\n", plan.ActivePath); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Install target: %s\n", plan.TargetPath); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Current link: %s -> %s\n", plan.CurrentLinkPath, plan.TargetPath); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Last-good link: %s -> %s\n", plan.LastGoodLinkPath, plan.RollbackPath); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "Initialize managed layout: %t\n", plan.InitializeLayout); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "Planned actions:"); err != nil {
		return err
	}

	for _, action := range plan.Actions {
		if _, err := fmt.Fprintf(w, "  - %s\n", action); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w, "Preflight: no changes applied")

	return err
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
