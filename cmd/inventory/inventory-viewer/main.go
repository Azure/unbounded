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

	inventoryviewer "github.com/Azure/unbounded/internal/inventory/viewer"
)

func main() {
	config := inventoryviewer.Config{}

	rootCmd := &cobra.Command{
		Use:   "inventory-viewer",
		Short: "Web interface for browsing inventory data",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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
				ApplicationName: "inventory-viewer",
				SSLMode:         sslMode,
			}

			if portStr := os.Getenv("POSTGRES_PORT"); portStr != "" {
				port, err := strconv.ParseUint(portStr, 10, 16)
				if err != nil {
					return fmt.Errorf("invalid POSTGRES_PORT %q: %w", portStr, err)
				}

				config.DbConn.Port = uint16(port)
			}

			return inventoryviewer.Execute(cmd.Context(), config)
		},
	}

	rootCmd.Flags().StringVar(&config.Addr, "addr", ":8080", "Address for the HTTP server to listen on")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
