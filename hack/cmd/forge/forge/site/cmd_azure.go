package site

import (
	"fmt"
	"strings"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/cluster"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
	"github.com/spf13/cobra"
)

const (
	azureDefaultCloud        = "AzurePublicCloud"
	azureDefaultSubscription = "44654aed-2753-4b88-9142-af7132933b6b"
	azureDefaultLocation     = "canadacentral"
)

func azureSiteCommandGroup(parent *cobra.Command, siteCmdContext *siteCommandContext) {
	site := &azuredev.Datacenter{
		WorkerNodeCIDR: "10.1.0.0/16",
		WorkerPodCIDR:  "100.125.0.0/16",
	}

	g := &cobra.Command{
		Use: "azure",
	}

	parent.AddCommand(g)

	g.AddCommand(
		addSiteCmd(siteCmdContext, site),
		addPoolCmd(siteCmdContext, site),
		addInventoryCmd(siteCmdContext, site))
}

func addSiteCmd(siteCmdContext *siteCommandContext, site *azuredev.Datacenter) *cobra.Command {
	mp := azuredev.MachinePool{
		Name:              "worker",
		Count:             0,
		Size:              "standard_d2ads_v6",
		SSHKeyPair:        &infra.SSHKeyPair{},
		BackendPort:       22,
		FrontendPortStart: 22001,
		FrontendPortEnd:   22999,
	}

	c := &cobra.Command{
		Use:   "add",
		Short: "Add a new Azure-backed datacenter site",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := siteCmdContext.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			dataDir := cluster.DataDir{
				Root:           siteCmdContext.DataDir,
				UnboundedForge: siteCmdContext.clusterName,
				Site:           siteCmdContext.siteName,
			}

			site.AzureCli = &siteCmdContext.AzureCli
			site.Logger = siteCmdContext.Logger
			site.DataDir = dataDir
			site.Name = siteCmdContext.siteName
			site.Location = siteCmdContext.Location

			ctx := cmd.Context()

			clusterDetails, err := cluster.NewGetClusterDetails(site.AzureCli, site.Logger, dataDir, siteCmdContext.clusterName).Get(ctx)
			if err != nil {
				return fmt.Errorf("get cluster details: %w", err)
			}

			site.ClusterDetails = clusterDetails
			site.KubeCli = clusterDetails.KubeCli
			site.Kubectl = clusterDetails.Kubectl

			bootstrapTok, err := kube.GetBootstrapToken(cmd.Context(), site.KubeCli)
			if err != nil {
				return fmt.Errorf("get forge node bootstrap token: %w", err)
			}

			site.BootstrapToken = bootstrapTok

			mp.SSHKeyPair.Logger = site.Logger

			// no need to make this complicated and create per-Site SSH keys for development.
			if mp.SSHKeyPair.PublicKeyPath == "" {
				mp.SSHKeyPair.PublicKeyPath = dataDir.UnboundedForgePath("ssh", fmt.Sprintf("%s.pub", siteCmdContext.clusterName))
			}

			if err := site.ApplyDatacenterSite(cmd.Context()); err != nil {
				return fmt.Errorf("apply datacenter site: %w", err)
			}

			userData, err := site.GetWorkerUserData()
			if err != nil {
				return fmt.Errorf("get worker user data: %w", err)
			}

			mp.UserData = userData

			if err := site.ApplyDatacenterSitePool(cmd.Context(), mp); err != nil {
				return fmt.Errorf("apply datacenter site machine pool: %w", err)
			}

			return nil
		},
	}

	c.Flags().StringVar(&siteCmdContext.CloudName, "azure", azureDefaultCloud, "Azure cloud name")
	c.Flags().StringVar(&siteCmdContext.SubscriptionID, "subscription", azureDefaultSubscription, "Azure subscription ID")
	c.Flags().StringVar(&siteCmdContext.Location, "location", azureDefaultLocation, "Azure location")
	c.Flags().IntVar(&mp.Count, "worker-vm-count", mp.Count, "Number of worker nodes to create")
	c.Flags().StringVar(&mp.Size, "worker-vm-size", mp.Size, "VM size to use for worker nodes")
	c.Flags().StringVar(&site.WorkerNodeCIDR, "worker-node-cidr", site.WorkerNodeCIDR, "CIDR range to use for work nodes")
	c.Flags().StringVar(&site.WorkerPodCIDR, "worker-pod-cidr", site.WorkerPodCIDR, "CIDR range to use for pods on worker nodes")
	c.Flags().StringVar(&mp.SSHKeyPair.PublicKeyPath, "worker-ssh-public-key", "", "SSH public key (set empty or 'auto' to generate a new key pair)")
	c.Flags().BoolVar(&site.AddUnboundedCNISiteConfig, "add-unbounded-cni-site", site.AddUnboundedCNISiteConfig, "Add an unbounded-cni site configuration automatically")

	return c
}

func addPoolCmd(siteCmdContext *siteCommandContext, site *azuredev.Datacenter) *cobra.Command {
	mp := azuredev.MachinePool{
		Count:             2,
		Size:              "standard_d2ads_v6",
		SSHKeyPair:        &infra.SSHKeyPair{},
		BackendPort:       22,
		FrontendPortStart: 22001,
		FrontendPortEnd:   22999,
	}

	c := &cobra.Command{
		Use:   "add-pool",
		Short: "Add a new machine pool to an existing Azure-backed datacenter site",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := siteCmdContext.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			dataDir := cluster.DataDir{
				Root:           siteCmdContext.DataDir,
				UnboundedForge: siteCmdContext.clusterName,
				Site:           siteCmdContext.siteName,
			}

			site.AzureCli = &siteCmdContext.AzureCli
			site.Logger = siteCmdContext.Logger
			site.DataDir = dataDir
			site.Name = siteCmdContext.siteName
			site.Location = siteCmdContext.Location

			clusterDetails, err := cluster.NewGetClusterDetails(site.AzureCli, site.Logger, dataDir, siteCmdContext.clusterName).Get(cmd.Context())
			if err != nil {
				return fmt.Errorf("get cluster details: %w", err)
			}

			site.ClusterDetails = clusterDetails
			site.KubeCli = clusterDetails.KubeCli
			site.Kubectl = clusterDetails.Kubectl

			bootstrapTok, err := kube.GetBootstrapToken(cmd.Context(), site.KubeCli)
			if err != nil {
				return fmt.Errorf("get forge node bootstrap token: %w", err)
			}

			site.BootstrapToken = bootstrapTok

			mp.SSHKeyPair.Logger = site.Logger

			// no need to make this complicated and create per-Site SSH keys for development.
			if mp.SSHKeyPair.PublicKeyPath == "" {
				mp.SSHKeyPair.PublicKeyPath = dataDir.UnboundedForgePath("ssh", fmt.Sprintf("%s.pub", siteCmdContext.clusterName))
			}

			return site.ApplyDatacenterSitePool(cmd.Context(), mp)
		},
	}

	c.Flags().StringVar(&siteCmdContext.CloudName, "azure", azureDefaultCloud, "Azure cloud name")
	c.Flags().StringVar(&siteCmdContext.SubscriptionID, "subscription", azureDefaultSubscription, "Azure subscription ID")
	c.Flags().StringVar(&siteCmdContext.Location, "location", azureDefaultLocation, "Azure location")
	c.Flags().StringVar(&mp.SSHKeyPair.PublicKeyPath, "ssh-public-key", "", "SSH public key (set empty or 'auto' to generate a new key pair)")
	c.Flags().StringVar(&mp.SSHKeyPair.PrivateKeyPath, "ssh-private-key", "", "SSH private key (set empty or 'auto' to generate a new key pair)")
	c.Flags().StringVar(&mp.Name, "name", mp.Name, "Name of the machine pool to add")
	c.Flags().IntVar(&mp.Count, "count", mp.Count, "Number of worker nodes to create in the pool")
	c.Flags().StringVar(&mp.Size, "size", mp.Size, "VM size to use for worker nodes in the pool")
	c.Flags().StringVar(&mp.SSHUser, "ssh-user", mp.SSHUser, "SSH user name for worker nodes in the pool")
	c.Flags().Int32Var(&mp.BackendPort, "ssh-backend-port", mp.BackendPort, "Backend SSH port")
	c.Flags().Int32Var(&mp.FrontendPortStart, "ssh-frontend-port-start", mp.FrontendPortStart, "Starting frontend port for SSH")
	c.Flags().Int32Var(&mp.FrontendPortEnd, "ssh-frontend-port-end", mp.FrontendPortEnd, "Ending frontend port for SSH")

	return c
}

func addInventoryCmd(siteCmdContext *siteCommandContext, site *azuredev.Datacenter) *cobra.Command {
	var (
		outputFormat string
		namespace    string
		matchPrefix  string
	)

	c := &cobra.Command{
		Use:   "inventory",
		Short: "Display the machine inventory for a site",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := siteCmdContext.Setup(); err != nil {
				return fmt.Errorf("setup command: %w", err)
			}

			dataDir := cluster.DataDir{
				Root:           siteCmdContext.DataDir,
				UnboundedForge: siteCmdContext.clusterName,
				Site:           siteCmdContext.siteName,
			}

			site.AzureCli = &siteCmdContext.AzureCli
			site.Logger = siteCmdContext.Logger
			site.DataDir = dataDir
			site.Name = siteCmdContext.siteName

			inventoryGetter := azuredev.InventoryGetter{
				AzureCli:          site.AzureCli,
				ResourceGroupName: site.Name,
				LoadBalancerName:  "frontend",
				SSHBackendPort:    22,
			}

			inventory, err := inventoryGetter.Get(cmd.Context())
			if err != nil {
				return fmt.Errorf("get inventory: %w", err)
			}

			// Filter machines by VM name prefix if requested.
			if matchPrefix != "" {
				filtered := make([]azuredev.Machine, 0, len(inventory.Machines))
				for _, m := range inventory.Machines {
					if strings.HasPrefix(m.Name, matchPrefix) {
						filtered = append(filtered, m)
					}
				}

				inventory.Machines = filtered
			}

			if outputFormat == "machina" {
				return WriteInventoryAsMachina(cmd.OutOrStdout(), inventory)
			}

			for _, vm := range inventory.Machines {
				fmt.Printf("%s => %s:%d\n", vm.Name, vm.IPAddress, vm.Port)
			}

			return nil
		},
	}

	c.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format (machina)")
	c.Flags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace for machina output")
	c.Flags().StringVar(&matchPrefix, "match-prefix", "", "Only include machines whose VM name starts with this prefix")

	return c
}
