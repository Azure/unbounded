// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/lib/pq"
	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/internal/inventory/aggregator"
)

func main() {
	config := aggregator.Config{}

	rootCmd := &cobra.Command{
		Use:   "inventory-aggregator",
		Short: "Aggregate and store node inventory data",
		RunE: func(cmd *cobra.Command, args []string) error {
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

	rootCmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug output")
	rootCmd.Flags().StringVar(&config.GRPCAddr, "grpc-addr", ":50051", "Address for the gRPC server to listen on")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
