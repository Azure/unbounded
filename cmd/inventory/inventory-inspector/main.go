// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/lib/pq"
	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/inventory/inspector"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	config := inspector.Config{}

	rootCmd := &cobra.Command{
		Use:     "inventory-inspector",
		Short:   "Inspect collected inventory data for conflicts",
		Version: version.Version + " (commit: " + version.GitCommit + ")",
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if config.Debug {
				level = slog.LevelDebug
			}

			logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
			slog.SetDefault(logger)

			sslMode := pq.SSLMode(os.Getenv("POSTGRES_SSL_MODE"))
			switch sslMode {
			case "", pq.SSLModeDisable, pq.SSLModeAllow, pq.SSLModePrefer, pq.SSLModeRequire, pq.SSLModeVerifyCA, pq.SSLModeVerifyFull:
			default:
				return fmt.Errorf("invalid POSTGRES_SSL_MODE %q", sslMode)
			}

			config.DbConn = pq.Config{
				Host:            os.Getenv("POSTGRES_HOST"),
				Database:        os.Getenv("POSTGRES_DB_NAME"),
				User:            os.Getenv("POSTGRES_USER"),
				Password:        os.Getenv("POSTGRES_PASSWORD"),
				ApplicationName: "inventory-inspector",
				SSLMode:         sslMode,
			}

			if portStr := os.Getenv("POSTGRES_PORT"); portStr != "" {
				port, err := strconv.ParseUint(portStr, 10, 16)
				if err != nil {
					return fmt.Errorf("invalid POSTGRES_PORT %q: %w", portStr, err)
				}

				config.DbConn.Port = uint16(port)
			}

			return inspector.Execute(cmd.Context(), config)
		},
	}

	rootCmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug output")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
