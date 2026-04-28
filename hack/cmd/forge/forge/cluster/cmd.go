// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/cmd"
)

func CommandGroup(parent *cobra.Command, cmdCtx *cmd.CommandContext) {
	clusterGroup := &cobra.Command{
		Use: "cluster",
	}

	createCommand(clusterGroup, cmdCtx)
	deleteCommand(clusterGroup, cmdCtx)

	parent.AddCommand(clusterGroup)
}

func createCommand(parent *cobra.Command, cmdCtx *cmd.CommandContext) {
	task := &CreateCluster{
		Location:             "canadacentral",
		SystemPoolNodeSKU:    "Standard_D2ads_v6",
		SystemPoolNodeCount:  2,
		GatewayPoolNodeSKU:   "Standard_D2ads_v6",
		GatewayPoolNodeCount: 2,
	}

	createCmd := &cobra.Command{
		Use: "create",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmdCtx.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			task.Azure = &cmdCtx.AzureCli
			task.Logger = cmdCtx.Logger
			task.DataDir = DataDir{Root: cmdCtx.DataDir}

			out, err := task.Do(cmd.Context())
			if err != nil {
				return fmt.Errorf("run task: %w", err)
			}

			b, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal output: %w", err)
			}

			fmt.Println(string(b))

			return nil
		},
	}

	createCmd.Flags().StringVar(&task.Name, "name", "", "Cluster name")
	createCmd.Flags().StringVar(&task.Location, "location", task.Location, "Azure location for the cluster")
	createCmd.Flags().StringVar(&task.SSHDir, "ssh-dir", task.SSHDir, "Directory to place SSH keys")
	createCmd.Flags().StringVar(&task.SystemPoolNodeSKU, "system-pool-node-sku", task.SystemPoolNodeSKU, "VM SKU for system node pool")
	createCmd.Flags().Int32Var(&task.SystemPoolNodeCount, "system-pool-node-count", task.SystemPoolNodeCount, "Number of nodes in the system node pool")
	createCmd.Flags().StringVar(&task.GatewayPoolNodeSKU, "gateway-pool-node-sku", task.GatewayPoolNodeSKU, "VM SKU for gateway node pool")
	createCmd.Flags().Int32Var(&task.GatewayPoolNodeCount, "gateway-pool-node-count", task.GatewayPoolNodeCount, "Number of nodes in the gateway node pool")

	parent.AddCommand(createCmd)
}

func deleteCommand(parent *cobra.Command, parentCfg *cmd.CommandContext) {
	task := &DeleteCluster{}

	deleteCmd := &cobra.Command{
		Use: "delete",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := parentCfg.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			task.Azure = &parentCfg.AzureCli
			task.Logger = parentCfg.Logger

			if err := task.Do(cmd.Context()); err != nil {
				return fmt.Errorf("run task: %w", err)
			}

			return nil
		},
	}

	deleteCmd.Flags().StringVar(&task.Name, "name", "", "Cluster name")

	parent.AddCommand(deleteCmd)
}
