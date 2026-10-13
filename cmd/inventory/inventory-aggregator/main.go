// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/lib/pq"
	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/inventory/aggregator"
	"github.com/Azure/unbounded/internal/logger"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if err := newCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	config := aggregator.Config{}
	logOpts := logger.Options{Format: logger.FormatJSON}

	rootCmd := &cobra.Command{
		Use:     "inventory-aggregator",
		Short:   "Aggregate and store node inventory data",
		Version: version.Version + " (commit: " + version.GitCommit + ")",
		RunE: func(cmd *cobra.Command, args []string) error {
			logOpts.Setup(os.Stderr)

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
				ApplicationName: "inventory-aggregator",
				SSLMode:         sslMode,
			}

			if portStr := os.Getenv("POSTGRES_PORT"); portStr != "" {
				port, err := strconv.ParseUint(portStr, 10, 16)
				if err != nil {
					return fmt.Errorf("invalid POSTGRES_PORT %q: %w", portStr, err)
				}

				config.DbConn.Port = uint16(port)
			}

			return aggregator.Run(cmd.Context(), config)
		},
	}

	logOpts.BindFlags(rootCmd.Flags())
	rootCmd.Flags().StringVar(&config.GRPCAddr, "grpc-addr", ":50051", "Address for the gRPC server to listen on")

	return rootCmd
}
