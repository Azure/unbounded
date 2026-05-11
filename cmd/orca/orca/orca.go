// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package orca wires the Orca cache binary together. It is invoked by
// cmd/orca/main.go and is responsible for parsing flags, loading the
// YAML config, and delegating to internal/orca/app for actual runtime
// wiring.
package orca

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/orca/app"
	"github.com/Azure/unbounded/internal/orca/config"
)

// Run is the entrypoint invoked by cmd/orca/main.go.
func Run() {
	root := &cobra.Command{
		Use:   "orca",
		Short: "Orca origin cache - S3-compatible read-only cache fronting Azure / S3 origins",
	}
	root.AddCommand(newServeCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Orca cache server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), configPath)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "/etc/orca/config.yaml",
		"path to YAML config file")

	return cmd
}

func serve(parent context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	level, err := resolveLogLevel(cfg.Logging.Level)
	if err != nil {
		return err
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(level)

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     levelVar,
		AddSource: true,
	}))
	slog.SetDefault(log)

	log.Info("orca starting",
		"config_path", configPath,
		"log_level", level.String(),
	)

	log.Info("config loaded",
		"origin_id", cfg.Origin.ID,
		"replicas_target", cfg.Cluster.TargetReplicas,
		"target_global", cfg.Origin.TargetGlobal,
		"internal_tls", cfg.Cluster.InternalTLS.Enabled,
		"client_auth", cfg.Server.Auth.Enabled,
	)

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a, err := app.Start(ctx, cfg, app.WithLogger(log))
	if err != nil {
		return err
	}

	if waitErr := a.Wait(ctx); waitErr != nil {
		log.Error("listener exited with error", "err", waitErr)
		cancel()
	} else {
		log.Info("shutdown signal received")
	}

	shutdownCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shCancel()

	_ = a.Shutdown(shutdownCtx) //nolint:errcheck // shutdown errors already logged inside App.Shutdown

	log.Info("orca stopped")

	return nil
}

// resolveLogLevel determines the effective slog.Level by consulting
// the ORCA_LOG_LEVEL environment variable first; if unset or empty,
// falls back to the YAML-configured value. An unrecognised value
// (from either source) returns a parse error so misconfiguration is
// surfaced at startup rather than silently degrading to info.
func resolveLogLevel(yamlLevel string) (slog.Level, error) {
	if env := strings.TrimSpace(os.Getenv("ORCA_LOG_LEVEL")); env != "" {
		level, err := config.ParseLogLevel(env)
		if err != nil {
			return 0, fmt.Errorf("ORCA_LOG_LEVEL: %w", err)
		}

		return level, nil
	}

	return config.ParseLogLevel(yamlLevel)
}
