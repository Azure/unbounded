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
	var (
		clientID   string
		registries []string
	)

	provider := &cobra.Command{
		Use: "acr-credential-provider", Short: "Serve one kubelet ACR managed-identity credential request", Hidden: true,
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := registryauth.NewProvider(clientID)
			if err != nil {
				return err
			}

			return p.Run(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), registries)
		},
	}
	provider.Flags().StringVar(&clientID, "client-id", "", "Optional user-assigned managed identity client ID")
	provider.Flags().StringArrayVar(&registries, "registry", nil, "Allowed ACR login server (repeatable)")

	var opts registryauth.NodeOptions

	node := &cobra.Command{
		Use: "registry-auth-node", Short: "Reconcile operator-owned ACR authentication on an AKS Flex node", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return registryauth.RunNode(ctx, opts)
		},
	}
	node.Flags().StringVar(&opts.Root, "host-root", "/host", "Mounted node root filesystem")
	node.Flags().StringVar(&opts.ConfigPath, "config", "/etc/registry-auth/config.json", "Projected node-auth desired state")
	node.Flags().StringVar(&opts.Listen, "listen", ":9096", "Node-auth health listener")
	node.Flags().StringVar(&opts.Namespace, "namespace", os.Getenv("POD_NAMESPACE"), "Restart Lease namespace")
	node.Flags().StringVar(&opts.Identity, "identity", os.Getenv("POD_UID"), "Unique identity for restart coordination")

	return []*cobra.Command{provider, node}
}
