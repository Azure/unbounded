// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/logger"
)

// NewCommand creates a new controller command.
func NewCommand() *cobra.Command {
	var (
		configPath string
		logOpts    logger.Options
	)

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the machina controller manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			logOpts.Setup(os.Stderr)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Handle shutdown signals
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				cancel()
			}()

			return RunManager(ctx, cfg)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to the configuration file (required)")
	logOpts.BindFlags(cmd.Flags())

	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(fmt.Sprintf("mark flag required: %v", err))
	}

	return cmd
}
