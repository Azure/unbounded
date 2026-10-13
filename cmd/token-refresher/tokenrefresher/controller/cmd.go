// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/logger"
)

func NewCommand() *cobra.Command {
	var (
		configPath string
		logOpts    logger.Options
	)

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the token watcher controller manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			logOpts.Setup(os.Stderr)

			return RunManager(ctrl.SetupSignalHandler(), cfg)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to the configuration file (required)")
	logOpts.BindFlags(cmd.Flags())

	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(fmt.Sprintf("mark flag required: %v", err))
	}

	return cmd
}
