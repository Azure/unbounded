package cluster

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/cmd"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/unboundedcni"
	"github.com/spf13/cobra"
)

func CommandGroup(parent *cobra.Command, cmdCtx *cmd.CommandContext) {
	clusterGroup := &cobra.Command{
		Use: "cluster",
	}

	createCommand(clusterGroup, cmdCtx)
	deleteCommand(clusterGroup, cmdCtx)
	validateCommand(clusterGroup, cmdCtx)

	parent.AddCommand(clusterGroup)
}

func createCommand(parent *cobra.Command, cmdCtx *cmd.CommandContext) {
	task := &CreateCluster{
		Location:               "canadacentral",
		SystemPoolNodeSKU:      "Standard_D2ads_v6",
		SystemPoolNodeCount:    2,
		GatewayPoolNodeSKU:     "Standard_D2ads_v6",
		GatewayPoolNodeCount:   2,
		UnboundedCNIReleaseURL: "https://github.com/azure-management-and-platforms/aks-unbounded-cni/releases/download/v0.7.1/unbounded-cni-manifests-v0.7.1.tar.gz",
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
	createCmd.Flags().StringVar(&task.UnboundedCNIReleaseURL, "unbounded-cni-release-url", task.UnboundedCNIReleaseURL, "URL to the unbounded-cni manifests tarball (https:// or file://)")
	createCmd.Flags().StringVar(&task.ControllerImage, "unbounded-cni-controller-image", "", "Override the controller container image in unbounded-cni manifests")
	createCmd.Flags().StringVar(&task.NodeImage, "unbounded-cni-node-image", "", "Override the node container image in unbounded-cni manifests")

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

func validateCommand(parent *cobra.Command, cmdCtx *cmd.CommandContext) {
	var (
		clusterName   string
		timeout       time.Duration
		retryInterval time.Duration
	)

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate cluster connectivity",
		Long:  "Validates connectivity within a cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmdCtx.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			dataDir := DataDir{Root: cmdCtx.DataDir}

			ctx := cmd.Context()

			clusterDetails, err := NewGetClusterDetails(&cmdCtx.AzureCli, cmdCtx.Logger, dataDir, clusterName).Get(ctx)
			if err != nil {
				return fmt.Errorf("get cluster details: %w", err)
			}

			kubeCli, restConfig, err := kube.ClientAndConfigFromBytes(clusterDetails.KubeconfigData)
			if err != nil {
				return fmt.Errorf("create kubernetes client: %w", err)
			}

			opts := unboundedcni.ValidateConnectivityOptions{
				Timeout:       timeout,
				RetryInterval: retryInterval,
			}

			if err := unboundedcni.ValidateConnectivity(ctx, cmdCtx.Logger, kubeCli, restConfig, opts); err != nil {
				return fmt.Errorf("validate connectivity: %w", err)
			}

			return nil
		},
	}

	validateCmd.Flags().StringVar(&clusterName, "name", "", "Name of the cluster")
	validateCmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Timeout for validation")
	validateCmd.Flags().DurationVar(&retryInterval, "retry-interval", 10*time.Second, "Retry interval when problems are detected")

	parent.AddCommand(validateCmd)
}
