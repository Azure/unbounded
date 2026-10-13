// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/cluster"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/cmd"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/site"
	"github.com/Azure/unbounded/internal/logger"
)

func Run() {
	rootCfg := cmd.CommandContext{
		CloudName:      "AzurePublicCloud",
		SubscriptionID: "",
		LogOpts:        logger.Options{Format: logger.FormatPrettyText, AttrOrder: []string{"cluster", "stage", "step"}},
		DataDir:        filepath.Join("~/.unbounded-forge"),
	}

	root := &cobra.Command{
		Use:               "forge [command]",
		Short:             "unbounded development tool",
		Long:              `unbounded development tool`,
		SilenceUsage:      true,
		PersistentPreRunE: setup(&rootCfg.DataDir),
	}

	cluster.CommandGroup(root, &rootCfg)
	site.CommandGroup(root, &rootCfg)

	root.PersistentFlags().StringVarP(&rootCfg.CloudName, "cloud", "a", rootCfg.CloudName, "Azure cloud name")
	root.PersistentFlags().StringVarP(&rootCfg.SubscriptionID, "subscription", "s", rootCfg.SubscriptionID, "Azure subscription ID")
	rootCfg.LogOpts.BindFlags(root.PersistentFlags())

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func setup(dataDir *string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if dataDir == nil {
			return fmt.Errorf("data dir is nil")
		}

		*dataDir = strings.TrimSpace(*dataDir)

		if strings.HasPrefix(*dataDir, "~") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("get user home dir: %w", err)
			}

			*dataDir = strings.Replace(*dataDir, "~", homeDir, 1)
		}

		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}

		return nil
	}
}
