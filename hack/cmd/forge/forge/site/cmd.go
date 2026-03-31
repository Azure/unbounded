package site

import (
	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/cmd"
)

type siteCommandContext struct {
	*cmd.CommandContext
	clusterName string
	siteName    string
}

func CommandGroup(parent *cobra.Command, parentCmdCtx *cmd.CommandContext) {
	cmdCtx := &siteCommandContext{
		CommandContext: parentCmdCtx,
	}

	siteGroup := &cobra.Command{
		Use:   "site",
		Short: "Manage datacenter sites",
	}

	siteGroup.PersistentFlags().StringVar(&cmdCtx.siteName, "site", "", "The name of the site")
	siteGroup.PersistentFlags().StringVar(&cmdCtx.clusterName, "cluster", "", "The name of the cluster")

	azureSiteCommandGroup(siteGroup, cmdCtx)

	parent.AddCommand(siteGroup)
}
