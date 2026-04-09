// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/cluster"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/Azure/unbounded-kube/internal/kube"
)

const (
	tagBastionDisableDirectAccess = "forge.bastion.disable-direct-access"
	bastionPoolName               = "bastion"
)

type MachinePool struct {
	Name              string
	Count             int
	Size              string
	UserData          string
	SSHUser           string
	SSHKeyPair        *infra.SSHKeyPair
	BackendPort       int32
	FrontendPortStart int32
	FrontendPortEnd   int32
}

type Datacenter struct {
	AzureCli          *azsdk.ClientSet
	KubeCli           kubernetes.Interface
	Kubectl           kube.KubectlFunc
	Logger            *slog.Logger
	Name              string
	Location          string
	WorkerNodeCIDR    string
	PeerWithCluster   bool
	ClusterDetails    *cluster.ClusterDetails
	DataDir           cluster.DataDir
	ExtraMachinePools []MachinePool

	SSHBastion                    bool
	SSHBastionVMSize              string
	SSHBastionDisableDirectAccess bool
}

func (d *Datacenter) ApplyDatacenterSite(ctx context.Context) error {
	if d.Name == "" {
		return fmt.Errorf("datacenter name is required")
	}

	l := d.Logger.With("dc", "azuredev", "name", d.Name)
	rgMan := infra.ResourceGroupManager{
		Client: d.AzureCli.ResourceGroupsClientV2,
		Logger: l,
	}

	l.Info("Applying datacenter resource group")

	rgDesired := armresources.ResourceGroup{
		Name:     to.Ptr(d.Name),
		Location: to.Ptr(d.Location),
	}

	if d.SSHBastionDisableDirectAccess {
		rgDesired.Tags = map[string]*string{
			tagBastionDisableDirectAccess: to.Ptr("1"),
		}
	}

	rg, err := rgMan.CreateOrUpdate(ctx, rgDesired)
	if err != nil {
		return fmt.Errorf("error creating datacenter resource group: %v", err)
	}

	l.Info("Applying datacenter networking configuration")

	if d.PeerWithCluster {
		l.Info("Datacenter will be peered with cluster")
	}

	dcNetMan := datacenterNetworkManager{
		azureCli:               d.AzureCli,
		logger:                 l,
		resourceGroup:          rg,
		mainVirtualNetworkCIDR: d.WorkerNodeCIDR,
		clusterResourceGroup:   d.ClusterDetails.NodesResourceGroup,
		clusterVirtualNetwork:  d.ClusterDetails.VirtualNetwork,
		peeringWithCluster:     d.PeerWithCluster,
	}

	if _, err := dcNetMan.CreateOrUpdate(ctx); err != nil {
		return fmt.Errorf("error creating datacenter networking: %w", err)
	}

	l.Info("Applied datacenter networking configuration")

	dcFrontendMan := datacenterFrontendManager{
		azureCli:      d.AzureCli,
		logger:        l,
		resourceGroup: rg,
	}

	if _, err := dcFrontendMan.CreateOrUpdate(ctx); err != nil {
		return fmt.Errorf("error creating datacenter frontend: %w", err)
	}

	l.Info("Applied datacenter frontend configuration")

	// the key vault stores the ssh private and public keys used for working on the SSH inventory and bootstrapping
	// system.
	dcSecretsMan := datacenterSecretsManager{
		azureCli:      d.AzureCli,
		logger:        l,
		resourceGroup: rg,
	}

	if err := dcSecretsMan.CreateOrUpdate(ctx); err != nil {
		return fmt.Errorf("error creating datacenter secrets storage: %w", err)
	}

	l.Info("Applied datacenter secrets storage")

	return nil
}

func (d *Datacenter) ApplyDatacenterSitePool(ctx context.Context, mp MachinePool) error {
	if d.Name == "" {
		return fmt.Errorf("datacenter name is required")
	}

	if mp.Name == "" {
		return fmt.Errorf("machine pool name is required")
	}

	// each pool needs to be prefixed with the site name otherwise you end up with problems joining
	// nodes to Kubernetes since pools across datacenters can have the same name.
	if !strings.HasPrefix(mp.Name, fmt.Sprintf("%s-", d.Name)) {
		mp.Name = fmt.Sprintf("%s-%s", d.Name, mp.Name)
	}

	if mp.SSHUser == "" {
		mp.SSHUser = "kubedev"
	}

	if mp.SSHKeyPair.PublicKeyPath == "" {
		mp.SSHKeyPair.PublicKeyPath = d.DataDir.SitePath("ssh", fmt.Sprintf("%s_id_rsa.pub", mp.Name))
	}

	l := d.Logger.With("dc", "azuredev", "name", d.Name)
	rgMan := infra.ResourceGroupManager{
		Client: d.AzureCli.ResourceGroupsClientV2,
		Logger: l,
	}

	rg, err := rgMan.Get(ctx, d.Name)
	if err != nil {
		return fmt.Errorf("error getting datacenter resource group: %v", err)
	}

	dcNetMan := datacenterNetworkManager{
		azureCli:               d.AzureCli,
		logger:                 l,
		resourceGroup:          rg,
		mainVirtualNetworkCIDR: d.WorkerNodeCIDR,
		clusterResourceGroup:   d.ClusterDetails.NodesResourceGroup,
		clusterVirtualNetwork:  d.ClusterDetails.VirtualNetwork,
		peeringWithCluster:     d.PeerWithCluster,
	}

	dcVirtualNetwork, err := dcNetMan.GetVirtualNetwork(ctx)
	if err != nil {
		return fmt.Errorf("error creating datacenter networking: %w", err)
	}

	mainSubnet, err := getSubnetByName("main", dcVirtualNetwork.Properties.Subnets)
	if err != nil {
		return fmt.Errorf("error getting main pool subnet: %w", err)
	}

	dcFrontendMan := datacenterFrontendManager{
		azureCli:      d.AzureCli,
		logger:        l,
		resourceGroup: rg,
	}

	frontendIP, err := dcFrontendMan.CreateOrUpdatePublicIP(ctx, mp.Name)
	if err != nil {
		return fmt.Errorf("error creating or updating datacenter frontend public IP: %w", err)
	}

	frontendLoadBalancer, err := dcFrontendMan.GetFrontend(ctx)
	if err != nil {
		return fmt.Errorf("error getting datacenter frontend load balancer: %w", err)
	}

	var (
		frontendName     = fmt.Sprintf("fe-%s", mp.Name)
		backendPoolName  = fmt.Sprintf("be-%s", mp.Name)
		inboundRuleName  = fmt.Sprintf("in-%s", mp.Name)
		outboundRuleName = fmt.Sprintf("out-%s", mp.Name)
	)

	frontend := getLoadBalancerFrontendByName(frontendName, frontendLoadBalancer.loadBalancer.Properties.FrontendIPConfigurations)
	if frontend == nil {
		frontend = &armnetwork.FrontendIPConfiguration{
			Name: to.Ptr(frontendName),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: frontendIP,
			},
		}

		putLoadBalancerFrontend(frontendLoadBalancer.loadBalancer, frontend)
	}

	backend := getLoadBalancerBackendPoolByName(backendPoolName, frontendLoadBalancer.loadBalancer.Properties.BackendAddressPools)
	if backend == nil {
		backend = &armnetwork.BackendAddressPool{
			Name: to.Ptr(backendPoolName),
		}

		putLoadBalancerBackendPool(frontendLoadBalancer.loadBalancer, backend)
	}

	// When the bastion disable-direct-access tag is set on the resource group, skip creating inbound NAT
	// rules for non-bastion pools. This forces SSH access to go through the bastion host.
	isBastionPool := strings.Contains(mp.Name, bastionPoolName)
	directAccessDisabled := hasResourceGroupTag(rg, tagBastionDisableDirectAccess, "1")

	if !directAccessDisabled || isBastionPool {
		inboundRule := getLoadBalancerInboundNatRuleByName(inboundRuleName, frontendLoadBalancer.loadBalancer.Properties.InboundNatRules)
		if inboundRule == nil {
			inboundRule = &armnetwork.InboundNatRule{
				Name: to.Ptr(inboundRuleName),
				Properties: &armnetwork.InboundNatRulePropertiesFormat{
					Protocol:               to.Ptr(armnetwork.TransportProtocolTCP),
					FrontendPortRangeStart: to.Ptr(mp.FrontendPortStart),
					FrontendPortRangeEnd:   to.Ptr(mp.FrontendPortEnd),
					BackendPort:            to.Ptr(mp.BackendPort),
					FrontendIPConfiguration: &armnetwork.SubResource{
						ID: to.Ptr(fmt.Sprintf("%s/frontendIPConfigurations/%s", *frontendLoadBalancer.loadBalancer.ID, *frontend.Name)),
					},
					BackendAddressPool: &armnetwork.SubResource{
						ID: to.Ptr(fmt.Sprintf("%s/backendAddressPools/%s", *frontendLoadBalancer.loadBalancer.ID, *backend.Name)),
					},
					EnableFloatingIP:     to.Ptr(false),
					EnableTCPReset:       to.Ptr(true),
					IdleTimeoutInMinutes: to.Ptr[int32](4),
				},
			}

			putLoadBalancerInboundNatRule(frontendLoadBalancer.loadBalancer, inboundRule)
		}
	} else {
		l.Info("Skipping inbound NAT rule for pool (direct SSH access disabled via bastion tag)", "pool", mp.Name)
	}

	outboundRule := getLoadBalancerOutboundRuleByName(outboundRuleName, frontendLoadBalancer.loadBalancer.Properties.OutboundRules)
	if outboundRule == nil {
		outboundRule = &armnetwork.OutboundRule{
			Name: to.Ptr(outboundRuleName),
			Properties: &armnetwork.OutboundRulePropertiesFormat{
				Protocol: to.Ptr(armnetwork.LoadBalancerOutboundRuleProtocolAll),
				BackendAddressPool: &armnetwork.SubResource{
					ID: to.Ptr(fmt.Sprintf("%s/backendAddressPools/%s", *frontendLoadBalancer.loadBalancer.ID, backendPoolName)),
				},
				FrontendIPConfigurations: []*armnetwork.SubResource{
					{
						ID: to.Ptr(fmt.Sprintf("%s/frontendIPConfigurations/%s", *frontendLoadBalancer.loadBalancer.ID, *frontend.Name)),
					},
				},
				EnableTCPReset:       to.Ptr(true),
				IdleTimeoutInMinutes: to.Ptr[int32](4),
			},
		}

		putLoadBalancerOutboundRule(frontendLoadBalancer.loadBalancer, outboundRule)
	}

	lbMan := infra.LoadBalancerManager{
		Client: d.AzureCli.NetworkLoadBalancersClientV2,
		Logger: d.Logger,
	}

	frontendLoadbalancer, err := lbMan.CreateOrUpdate(ctx, *rg.Name, *frontendLoadBalancer.loadBalancer)
	if err != nil {
		return fmt.Errorf("create or update public IP addresses: %w", err)
	}

	backend = getLoadBalancerBackendPoolByName(backendPoolName, frontendLoadbalancer.Properties.BackendAddressPools)
	if backend == nil {
		return fmt.Errorf("error getting load balancer backend pool for cluster: %w", err)
	}

	dcSecretsMan := datacenterSecretsManager{
		azureCli:      d.AzureCli,
		logger:        l,
		resourceGroup: rg,
	}

	if err := mp.SSHKeyPair.GetOrGenerate(); err != nil {
		return fmt.Errorf("error getting or generating SSH key pair: %w", err)
	}

	sshPublicKey, err := mp.SSHKeyPair.PublicKey()
	if err != nil {
		return fmt.Errorf("error getting SSH public key: %w", err)
	}

	tags := map[string]*string{
		tagSSHPrivateKeySecret: to.Ptr(fmt.Sprintf("%s-ssh", mp.Name)),
		tagSSHPublicKeySecret:  to.Ptr(fmt.Sprintf("%s-ssh-public", mp.Name)),
		tagSSHUserSecret:       to.Ptr(fmt.Sprintf("%s-ssh-user", mp.Name)),
	}

	if err := dcSecretsMan.PutSSHSecrets(ctx, tags, mp.SSHUser, mp.SSHKeyPair); err != nil {
		return fmt.Errorf("error putting datacenter SSH secrets: %w", err)
	}

	dcComputeMan := &datacenterComputeManager{
		azureCli:      d.AzureCli,
		logger:        l,
		resourceGroup: rg,
	}

	_, err = dcComputeMan.createOrUpdate(ctx, machinePoolConfig{
		name: mp.Name,
		sku: &armcompute.SKU{
			Capacity: to.Ptr(int64(mp.Count)),
			Tier:     to.Ptr("Standard"),
			Name:     to.Ptr(mp.Size),
		},
		adminUser:                      mp.SSHUser,
		adminSSHPublicKey:              sshPublicKey,
		userData:                       mp.UserData,
		subnet:                         mainSubnet,
		loadBalancerBackendAddressPool: backend,
		tags:                           tags,
	})
	if err != nil {
		return fmt.Errorf("error creating or updating machine pool %q: %w", mp.Name, err)
	}

	return nil
}

// ApplySSHBastion provisions a single-instance VMSS bastion pool that serves as an SSH jump host for
// the datacenter site. The bastion reuses the same SSH key pair as the worker pools and is placed in
// the same subnet. After provisioning, an SSH config file is written to the forge data directory.
func (d *Datacenter) ApplySSHBastion(ctx context.Context, workerSSHKeyPair *infra.SSHKeyPair) error {
	l := d.Logger.With("dc", "azuredev", "name", d.Name)

	vmSize := d.SSHBastionVMSize
	if vmSize == "" {
		vmSize = "Standard_D2ads_v6"
	}

	bastionPool := MachinePool{
		Name:              bastionPoolName,
		Count:             1,
		Size:              vmSize,
		SSHKeyPair:        workerSSHKeyPair,
		BackendPort:       22,
		FrontendPortStart: 22,
		FrontendPortEnd:   22,
		// No UserData -- the bastion does not join Kubernetes.
	}

	l.Info("Applying SSH bastion pool")

	if err := d.ApplyDatacenterSitePool(ctx, bastionPool); err != nil {
		return fmt.Errorf("apply bastion pool: %w", err)
	}

	// Resolve the bastion's public IP address from the frontend IP created for its pool.
	bastionPublicIPName := fmt.Sprintf("%s-%s", d.Name, bastionPoolName)

	ipMan := infra.PublicIPAddressManager{
		Client: d.AzureCli.NetworkPublicIPAddressesClientV2,
		Logger: l,
	}

	bastionIP, err := ipMan.Get(ctx, d.Name, bastionPublicIPName)
	if err != nil {
		return fmt.Errorf("get bastion public IP: %w", err)
	}

	if bastionIP.Properties == nil || bastionIP.Properties.IPAddress == nil {
		return fmt.Errorf("bastion public IP address not yet allocated")
	}

	publicIPAddress := *bastionIP.Properties.IPAddress

	sshUser := "kubedev"

	privateKeyPath := workerSSHKeyPair.PrivateKeyPath
	if privateKeyPath == "" {
		privateKeyPath = strings.TrimSuffix(workerSSHKeyPair.PublicKeyPath, ".pub")
	}

	sshConfigPath := d.DataDir.UnboundedForgePath("ssh", d.Name)
	if err := writeSSHConfig(sshConfigPath, d.Name, publicIPAddress, d.WorkerNodeCIDR, sshUser, privateKeyPath); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}

	l.Info("SSH bastion provisioned",
		"publicIP", publicIPAddress,
		"sshConfig", sshConfigPath,
		"usage", fmt.Sprintf("ssh -F %s <worker-private-ip>", sshConfigPath),
	)

	return nil
}

// writeSSHConfig writes an SSH configuration file that sets up ProxyJump through the bastion host
// for accessing worker nodes by their private IP addresses.
func writeSSHConfig(path, siteName, bastionIP, workerNodeCIDR, sshUser, privateKeyPath string) error {
	hostPattern, err := cidrToSSHPattern(workerNodeCIDR)
	if err != nil {
		return fmt.Errorf("convert CIDR to SSH host pattern: %w", err)
	}

	bastionHost := fmt.Sprintf("bastion-%s", siteName)

	config := fmt.Sprintf(`# SSH bastion config for site %s
# Usage: ssh -F %s <worker-private-ip>
Host %s
    HostName %s
    User %s
    IdentityFile %s
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null

Host %s
    User %s
    IdentityFile %s
    ProxyJump %s
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
`,
		siteName, path,
		bastionHost, bastionIP, sshUser, privateKeyPath,
		hostPattern, sshUser, privateKeyPath, bastionHost,
	)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create ssh config directory: %w", err)
	}

	return os.WriteFile(path, []byte(config), 0o600)
}

// cidrToSSHPattern converts a CIDR like "10.1.0.0/16" to an SSH Host pattern like "10.1.*".
// It replaces trailing zero octets (based on the mask) with wildcards.
func cidrToSSHPattern(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}

	ip := ipNet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("only IPv4 CIDRs are supported, got %q", cidr)
	}

	mask := ipNet.Mask

	var parts []string

	for i := 0; i < 4; i++ {
		switch mask[i] {
		case 0xff:
			parts = append(parts, fmt.Sprintf("%d", ip[i]))
		case 0:
			parts = append(parts, "*")
		default:
			// Partial mask octet (e.g. /12 or /20) -- use wildcard from here on.
			for j := i; j < 4; j++ {
				parts = append(parts, "*")
			}

			return strings.Join(parts, "."), nil
		}
	}

	return strings.Join(parts, "."), nil
}

func hasResourceGroupTag(rg *armresources.ResourceGroup, key, value string) bool {
	if rg.Tags == nil {
		return false
	}

	v, ok := rg.Tags[key]

	return ok && v != nil && *v == value
}
