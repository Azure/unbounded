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

	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

// nodeAccess is the active node and the backend that operates its stack.
type nodeAccess struct {
	Backend isolation.Backend
	Root    isolation.NodeRoot
}

// accessResolver finds the active node. It is a seam for tests.
type accessResolver func(*slog.Logger) (*nodeAccess, error)

// backendForMode builds the backend for a mode through the isolation registry.
// The registry hands each factory the deps it understands: the nspawn backend
// takes its logger, the none backend its logger and the host's /proc, and the
// container backend its logger (exec and logs go through the default runtime).
func backendForMode(mode isolation.Mode, log *slog.Logger) (isolation.Backend, error) {
	var deps any

	switch mode {
	case isolation.ModeNone:
		deps = &none.Backend{Log: log, Proc: "/proc"}
	case isolation.ModeContainer:
		deps = &container.Backend{Log: log}
	default:
		deps = &nspawn.Backend{Log: log}
	}

	b, err := isolation.New(mode, deps)
	if err != nil {
		// Say plainly which modes this binary cannot serve rather than leaking
		// the registry wording.
		return nil, fmt.Errorf("isolation mode %q is not supported by exec and logs: %w", mode, err)
	}

	return b, nil
}

// resolveActiveNode reads the applied config and builds the backend for the
// mode it records.
func resolveActiveNode(log *slog.Logger) (*nodeAccess, error) {
	applied, err := appliedconfig.Read()
	if err != nil {
		if errors.Is(err, appliedconfig.ErrNotFound) {
			return nil, fmt.Errorf("no node has been bootstrapped on this host: %w", err)
		}

		return nil, err
	}

	b, err := backendForMode(applied.Mode, log)
	if err != nil {
		return nil, err
	}

	return &nodeAccess{Backend: b, Root: isolation.DirRoot{SlotName: applied.Slot}}, nil
}

func runExec(ctx context.Context, log *slog.Logger, resolve accessResolver, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("exec requires a command, for example: exec -- crictl ps")
	}

	n, err := resolve(log)
	if err != nil {
		return err
	}

	res, err := n.Backend.Exec(ctx, n.Root, isolation.ExecSpec{Args: args})
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

	rc, err := n.Backend.Logs(ctx, n.Root, opts)
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
isolation mode: inside the nspawn machine, on the host for mode none, or inside
the container. The mode is read from the applied agent config.

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
