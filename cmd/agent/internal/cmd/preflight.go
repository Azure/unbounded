// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

type preflightHandler struct {
	cmdCtx                *CommandContext
	configPath            string
	ignorePreflightErrors []string
	failOnWarnings        bool
	output                string
	writer                io.Writer
}

func newCmdPreflight(cmdCtx *CommandContext) *cobra.Command {
	handler := &preflightHandler{cmdCtx: cmdCtx, writer: os.Stdout}

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Run non-mutating preflight checks",
		Long:  "Run non-mutating preflight checks for the host and agent configuration before node bootstrap.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.configPath, "config", "", "Path to agent config file")
	cmd.Flags().StringSliceVar(
		&handler.ignorePreflightErrors,
		"ignore-preflight-errors",
		nil,
		"Comma-separated preflight check names whose errors should be reported as warnings",
	)
	cmd.Flags().BoolVar(&handler.failOnWarnings, "fail-on-warnings", false, "Fail when any preflight warning is returned")
	cmd.Flags().StringVar(&handler.output, "output", "text", "Output format: text or json")

	return cmd
}

func (h *preflightHandler) execute(ctx context.Context) error {
	h.cmdCtx.Setup()
	logger := h.cmdCtx.Logger

	if h.configPath != "" {
		oldConfigPath := os.Getenv(configFileEnv)
		defer os.Setenv(configFileEnv, oldConfigPath) //nolint:errcheck // best effort restore

		if err := os.Setenv(configFileEnv, h.configPath); err != nil {
			return err
		}
	}

	cfg, err := loadConfig(logger)
	if err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate agent config: %w", err)
	}

	downloads, _, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, cfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	goalState, err := goalstates.ResolveMachine(
		logger,
		&cfg.AgentConfig,
		goalstates.NSpawnMachineKube1,
		downloads,
	)
	if err != nil {
		return fmt.Errorf("resolve goal state: %w", err)
	}

	checks := preflight.Flatten(
		host.Preflight(logger, cfg.AgentConfig, goalState),
		nodestart.Preflight(logger, cfg.AgentConfig, goalState),
		rootfs.Preflight(logger, cfg.AgentConfig, goalState),
	)

	opts := preflight.Options{
		IgnoreErrors:   h.ignorePreflightErrors,
		FailOnWarnings: h.failOnWarnings,
	}
	report := preflight.Run(ctx, checks, opts)

	switch strings.ToLower(h.output) {
	case "", "text":
		if err := writePreflightText(h.writer, report); err != nil {
			return err
		}
	case "json":
		enc := json.NewEncoder(h.writer)
		enc.SetIndent("", "  ")

		if err := enc.Encode(report); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported output format %q", h.output)
	}

	return report.Err(h.failOnWarnings)
}

func writePreflightText(w io.Writer, report preflight.Report) error {
	if _, err := fmt.Fprintln(w, "[preflight] Running unbounded-agent pre-flight checks"); err != nil {
		return err
	}

	var errors []preflight.Result

	for _, result := range report.Checks {
		switch result.Severity {
		case preflight.SeverityOK:
			if err := writePreflightResult(w, "OK", result); err != nil {
				return err
			}
		case preflight.SeverityError:
			errors = append(errors, result)
		case preflight.SeverityWarning:
			if err := writePreflightResult(w, "WARNING", result); err != nil {
				return err
			}
		}
	}

	if len(errors) == 0 {
		return nil
	}

	if _, err := fmt.Fprintln(w, "[preflight] Some fatal errors occurred:"); err != nil {
		return err
	}

	for _, result := range errors {
		if _, err := fmt.Fprintf(w, "\t[ERROR %s]: %s", result.Name, result.Message); err != nil {
			return err
		}

		if result.Target != "" {
			if _, err := fmt.Fprintf(w, " (target: %s)", result.Target); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(
		w,
		"[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`",
	)

	return err
}

func writePreflightResult(w io.Writer, status string, result preflight.Result) error {
	if _, err := fmt.Fprintf(w, "\t[%s %s]: %s", status, result.Name, result.Message); err != nil {
		return err
	}

	if result.Target != "" {
		if _, err := fmt.Fprintf(w, " (target: %s)", result.Target); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w)

	return err
}
