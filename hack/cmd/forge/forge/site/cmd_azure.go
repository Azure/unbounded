// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package site

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/cluster"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
)

const (
	azureDefaultCloud        = "AzurePublicCloud"
	azureDefaultSubscription = "44654aed-2753-4b88-9142-af7132933b6b"
	azureDefaultLocation     = "canadacentral"
)

func azureSiteCommandGroup(parent *cobra.Command, siteCmdContext *siteCommandContext) {
	site := &azuredev.Datacenter{
		WorkerNodeCIDR: "10.1.0.0/16",
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
	kp := &infra.SSHKeyPair{}

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

			if err := site.ApplyDatacenterSite(cmd.Context()); err != nil {
				return fmt.Errorf("apply datacenter site: %w", err)
			}

			if site.SSHBastion {
				kp.Logger = site.Logger

				// no need to make this complicated and create per-Site SSH keys for development.
				if kp.PublicKeyPath == "" {
					kp.PublicKeyPath = dataDir.UnboundedForgePath("ssh", fmt.Sprintf("%s.pub", siteCmdContext.clusterName))
				}

				// The bastion pool needs its SSH key pair resolved before provisioning so that
				// GetOrGenerate is called with the correct path.
				if err := kp.GetOrGenerate(); err != nil {
					return fmt.Errorf("get or generate SSH key pair for bastion: %w", err)
				}

				if err := site.ApplySSHBastion(ctx, kp); err != nil {
					return fmt.Errorf("apply ssh bastion: %w", err)
				}
			}

			return nil
		},
	}

	c.Flags().StringVar(&siteCmdContext.CloudName, "azure", azureDefaultCloud, "Azure cloud name")
	c.Flags().StringVar(&siteCmdContext.SubscriptionID, "subscription", azureDefaultSubscription, "Azure subscription ID")
	c.Flags().StringVar(&siteCmdContext.Location, "location", azureDefaultLocation, "Azure location")
	c.Flags().StringVar(&site.WorkerNodeCIDR, "worker-node-cidr", site.WorkerNodeCIDR, "CIDR range to use for work nodes")
	c.Flags().BoolVar(&site.SSHBastion, "ssh-bastion", site.SSHBastion, "Provision an SSH bastion (jump host) for the site")
	c.Flags().StringVar(&site.SSHBastionVMSize, "ssh-bastion-vm-size", "Standard_D2ads_v6", "VM size to use for the SSH bastion")
	c.Flags().BoolVar(&site.SSHBastionDisableDirectAccess, "ssh-bastion-disable-direct-access", site.SSHBastionDisableDirectAccess, "Disable direct SSH access to worker pools, forcing access through the bastion")
	c.Flags().StringVar(&kp.PublicKeyPath, "ssh-public-key", "", "SSH public key (leave empty to generate a new key pair)")
	c.Flags().StringVar(&kp.PrivateKeyPath, "ssh-private-key", "", "SSH private key (leave empty to generate a new key pair)")

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
	c.Flags().StringVar(&mp.SSHKeyPair.PublicKeyPath, "ssh-public-key", "", "SSH public key (leave empty to generate a new key pair)")
	c.Flags().StringVar(&mp.SSHKeyPair.PrivateKeyPath, "ssh-private-key", "", "SSH private key (leave empty to generate a new key pair)")
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
		outputFormat              string
		namespace                 string
		matchPrefix               string
		machinaBastion            bool
		machinaSSHSecretRef       string
		machBastionSSHSecret      string
		machinaSSHUsername        string
		machinaBastionSSHUsername string
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

			ctx := cmd.Context()

			// Check whether the bastion disable-direct-access tag is set on the RG.
			rgMan := infra.ResourceGroupManager{
				Client: site.AzureCli.ResourceGroupsClientV2,
				Logger: siteCmdContext.Logger,
			}

			rg, err := rgMan.Get(ctx, site.Name)
			if err != nil {
				return fmt.Errorf("get resource group: %w", err)
			}

			directAccessDisabled := rgTagEquals(rg.Tags, "forge.bastion.disable-direct-access", "1")

			// Determine whether we need bastion-aware inventory (private IPs via VMSS query).
			useDirectQuery := directAccessDisabled || machinaBastion

			inventoryGetter := azuredev.InventoryGetter{
				AzureCli:          site.AzureCli,
				ResourceGroupName: site.Name,
				LoadBalancerName:  "frontend",
				SSHBackendPort:    22,
			}

			var inventory *azuredev.Inventory

			if useDirectQuery {
				// Workers may have no NAT rules, or caller wants private IPs with bastion.
				// Query VMSS instances directly for private IPs.
				inventory, err = inventoryGetter.GetDirect(ctx)
				if err != nil {
					return fmt.Errorf("get direct inventory: %w", err)
				}
			} else {
				// Standard NAT-based inventory with public IPs.
				inventory, err = inventoryGetter.Get(ctx)
				if err != nil {
					return fmt.Errorf("get inventory: %w", err)
				}
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

			switch outputFormat {
			case "machina":
				var bastionHost string
				if machinaBastion && inventory.Bastion != nil {
					bastionHost = machineHost(inventory.Bastion.IPAddress, inventory.Bastion.Port)
				}

				opts := MachinaInventoryOptions{
					Site:               siteCmdContext.siteName,
					BastionHost:        bastionHost,
					SSHUsername:        machinaSSHUsername,
					BastionSSHUsername: machinaBastionSSHUsername,
				}

				if machinaSSHSecretRef != "" {
					ref, err := parseSecretKeyRef(machinaSSHSecretRef)
					if err != nil {
						return fmt.Errorf("parse --machina-ssh-secret-ref: %w", err)
					}

					opts.SSHSecretRef = &ref
				}

				if machBastionSSHSecret != "" {
					ref, err := parseSecretKeyRef(machBastionSSHSecret)
					if err != nil {
						return fmt.Errorf("parse --machina-bastion-ssh-secret-ref: %w", err)
					}

					opts.BastionSSHSecretRef = &ref
				}

				return WriteInventoryAsMachina(cmd.OutOrStdout(), inventory, opts)
			case "ssh":
				sshUser := "kubedev"
				privateKeyPath := dataDir.UnboundedForgePath("ssh", siteCmdContext.clusterName)
				sshConfigPath := dataDir.UnboundedForgePath("ssh", siteCmdContext.siteName)

				// Render the SSH config into a buffer so we can write it to both file and stdout.
				var buf bytes.Buffer
				if err := WriteInventoryAsSSH(&buf, inventory, siteCmdContext.siteName, sshUser, privateKeyPath); err != nil {
					return fmt.Errorf("render ssh config: %w", err)
				}

				// Write the config to the data directory.
				if err := os.MkdirAll(filepath.Dir(sshConfigPath), 0o700); err != nil {
					return fmt.Errorf("create ssh config directory: %w", err)
				}

				if err := os.WriteFile(sshConfigPath, buf.Bytes(), 0o600); err != nil {
					return fmt.Errorf("write ssh config: %w", err)
				}

				// Write to stdout with a comment header indicating the file path.
				w := cmd.OutOrStdout()

				if _, err := fmt.Fprintf(w, "# SSH config for site %q (written to %s)\n", siteCmdContext.siteName, sshConfigPath); err != nil {
					return err
				}

				if _, err := fmt.Fprintf(w, "# Usage: ssh -F %s <host>\n", sshConfigPath); err != nil {
					return err
				}

				if _, err := w.Write(buf.Bytes()); err != nil {
					return err
				}

				return nil
			default:
				if inventory.Bastion != nil {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "bastion => %s:%d\n", inventory.Bastion.IPAddress, inventory.Bastion.Port); err != nil {
						return err
					}
				}

				for _, vm := range inventory.Machines {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s => %s:%d\n", vm.Name, vm.IPAddress, vm.Port); err != nil {
						return err
					}
				}

				return nil
			}
		},
	}

	c.Flags().StringVarP(&outputFormat, "output", "o", "", "Output format (machina, ssh)")
	c.Flags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace for machina output")
	c.Flags().StringVar(&matchPrefix, "match-prefix", "", "Only include machines whose VM name starts with this prefix")
	c.Flags().BoolVar(&machinaBastion, "machina-bastion", false, "When used with --output=machina, configure each Machine CR with spec.ssh.bastion using the bastion's public IP")
	c.Flags().StringVar(&machinaSSHSecretRef, "machina-ssh-secret-ref", "", "Secret reference for spec.ssh.privateKeyRef in format [$namespace/]$name[:$key] (default namespace: machina-system)")
	c.Flags().StringVar(&machBastionSSHSecret, "machina-bastion-ssh-secret-ref", "", "Secret reference for spec.ssh.bastion.privateKeyRef in format [$namespace/]$name[:$key] (default namespace: machina-system)")
	c.Flags().StringVar(&machinaSSHUsername, "machina-ssh-username", "kubedev", "SSH username for spec.ssh.username on each Machine CR")
	c.Flags().StringVar(&machinaBastionSSHUsername, "machina-bastion-ssh-username", "kubedev", "SSH username for spec.ssh.bastion.username on each Machine CR")

	return c
}

// rgTagEquals returns true if the resource group tags contain the given key with
// the given value.
func rgTagEquals(tags map[string]*string, key, value string) bool {
	if tags == nil {
		return false
	}

	v, ok := tags[key]

	return ok && v != nil && *v == value
}
