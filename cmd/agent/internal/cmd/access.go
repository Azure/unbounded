// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// nodeAccess is the active node and the backend that operates its stack.
type nodeAccess struct {
	Backend isolation.Backend
	Slot    isolation.SlotRef
}

// accessResolver finds the active node. It is a seam for tests.
type accessResolver func(*slog.Logger) (*nodeAccess, error)

// backendForMode builds the backend of a mode from the mode composition table.
// Exec and logs need no goal state, so the backend is built from the applied
// config alone.
func backendForMode(mode isolation.Mode, log *slog.Logger, cfg *provision.AgentConfig) (isolation.Backend, error) {
	m, err := agentModes().lookup(mode)
	if err != nil {
		return nil, fmt.Errorf("isolation mode %q is not supported by exec and logs: %w", mode, err)
	}

	return m.newBackend(log, cfg), nil
}

// resolveActiveNode finds the running node the way the daemon does, from the
// applied config, and builds the backend of the mode it records.
func resolveActiveNode(log *slog.Logger) (*nodeAccess, error) {
	active, err := daemon.FindActiveMachine(log)
	if err != nil {
		return nil, fmt.Errorf("no node has been bootstrapped on this host, or its applied config cannot be read: %w", err)
	}

	b, err := backendForMode(active.Mode, log, active.Config)
	if err != nil {
		return nil, err
	}

	return &nodeAccess{Backend: b, Slot: isolation.SlotName(active.Name)}, nil
}

func runExec(ctx context.Context, log *slog.Logger, resolve accessResolver, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("exec requires a command, for example: exec -- crictl ps")
	}

	n, err := resolve(log)
	if err != nil {
		return err
	}

	res, err := n.Backend.Exec(ctx, n.Slot, isolation.ExecSpec{Args: args})
	if res.Stdout != "" {
		if _, werr := fmt.Fprintln(stdout, res.Stdout); werr != nil && err == nil {
			err = werr
		}
	}

	return err
}

func runLogs(ctx context.Context, log *slog.Logger, resolve accessResolver, stdout io.Writer, opts isolation.LogOptions) error {
	if opts.Lines < 0 {
		return errors.New("--lines must not be negative")
	}

	n, err := resolve(log)
	if err != nil {
		return err
	}

	rc, err := n.Backend.Logs(ctx, n.Slot, opts)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(stdout, rc)
	closeErr := rc.Close()

	// A stream the user ended with Ctrl-C was cut short on purpose.
	if ctx.Err() != nil {
		return nil
	}

	if copyErr != nil {
		return copyErr
	}

	// The command that produced the stream may have failed, for example
	// journalctl against a machine that is not running.
	return closeErr
}

func newCmdExec(cmdCtx *CommandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "exec -- <command> [args...]",
		Short: "Run a command in the node stack of this host's Kubernetes node",
		Long: `Run a command where the node stack (containerd, kubelet) runs, whatever the
isolation mode: inside the nspawn machine, or on the host for mode none. The
mode is read from the applied agent config.

Example:
  unbounded-agent exec -- crictl ps`,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return runExec(ctx, cmdCtx.Logger, resolveActiveNode, cmd.OutOrStdout(), args)
		},
	}
}

func newCmdLogs(cmdCtx *CommandContext) *cobra.Command {
	var opts isolation.LogOptions

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show logs of the node stack of this host's Kubernetes node",
		Long: `Show the journal of the node stack, whatever the isolation mode. Without
--unit, all node stack units are shown.

Examples:
  unbounded-agent logs --unit kubelet --lines 200
  unbounded-agent logs --unit containerd --follow`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return runLogs(ctx, cmdCtx.Logger, resolveActiveNode, cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Unit, "unit", "", "unit to show, for example kubelet or containerd (default: all)")
	cmd.Flags().BoolVarP(&opts.Follow, "follow", "f", false, "keep streaming new entries")
	cmd.Flags().IntVarP(&opts.Lines, "lines", "n", 0, "show only the last N entries")

	return cmd
}
