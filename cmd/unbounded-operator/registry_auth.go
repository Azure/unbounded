// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/registryauth"
)

func registryAuthCommands() []*cobra.Command {
	var opts registryauth.NodeOptions

	node := &cobra.Command{
		Use: "registry-auth-node", Short: "Install and configure the stock ACR provider on an opted-in Flex node", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return registryauth.RunNode(ctx, opts)
		},
	}
	node.Flags().StringVar(&opts.Root, "host-root", "/host", "Mounted node root filesystem")
	node.Flags().StringVar(&opts.ConfigJSON, "config-json", os.Getenv("REGISTRY_AUTH_CONFIG"), "Inline non-secret node-auth desired state")
	node.Flags().StringVar(&opts.Listen, "listen", ":9096", "Node-auth health listener")
	node.Flags().StringVar(&opts.ProviderPath, "provider-binary", registryauth.ProviderBinary, "Stock upstream ACR provider executable")

	return []*cobra.Command{node}
}
