// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

func NewCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the token watcher controller manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			return RunManager(ctrl.SetupSignalHandler(), cfg)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "Path to the configuration file (required)")

	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(fmt.Sprintf("mark flag required: %v", err))
	}

	return cmd
}
