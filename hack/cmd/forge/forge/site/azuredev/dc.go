package azuredev

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/cluster"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/unboundedcni"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes"
)

//go:embed assets
var assets embed.FS

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
	AzureCli                  *azsdk.ClientSet
	KubeCli                   kubernetes.Interface
	Kubectl                   func(context.Context) *exec.Cmd
	Logger                    *slog.Logger
	Name                      string
	Location                  string
	WorkerNodeCIDR            string
	WorkerPodCIDR             string
	PeerWithCluster           bool
	BootstrapToken            *kube.BootstrapToken
	ClusterDetails            *cluster.ClusterDetails
	DataDir                   cluster.DataDir
	ExtraMachinePools         []MachinePool
	AddUnboundedCNISiteConfig bool
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

	// Sometimes it's preferable to not provision the Site as part of setting up the DC. For example, because you
	// are developing a tool that wants to manage the site.
	if d.AddUnboundedCNISiteConfig {
		// Perform the CNI site configuration early as it will validate whether the CIDRs are
		// in use already and prevent creating a whole DC site with overlapping CIDRs which is
		// then annoying to clean up.
		l.Info("Applying datacenter site configuration")

		if err := d.installAndConfigureSiteCNI(ctx); err != nil {
			return fmt.Errorf("error installing unbounded-cni site cloudConfig: %w", err)
		}
	}

	l.Info("Applying datacenter resource group")

	rg, err := rgMan.CreateOrUpdate(ctx, armresources.ResourceGroup{
		Name:     to.Ptr(d.Name),
		Location: to.Ptr(d.Location),
	})
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

func (d *Datacenter) installAndConfigureSiteCNI(ctx context.Context) error {
	d.Logger.Info("Applying site configuration")

	cniPath := d.DataDir.SitePath("cni")
	if err := os.MkdirAll(cniPath, 0o700); err != nil {
		return fmt.Errorf("error creating site cni directory: %w", err)
	}

	if err := unboundedcni.ApplySiteManifest(ctx, d.Logger, d.Kubectl, cniPath, unboundedcni.SiteConfig{
		SiteName:  d.Name,
		NodeCIDRs: []string{d.WorkerNodeCIDR},
		PodCIDRs:  []string{d.WorkerPodCIDR},
	}); err != nil {
		return fmt.Errorf("error applying site %q manifest: %w", d.Name, err)
	}

	d.Logger.Info("Applying site gateway assignment", "site", d.Name, "gateway", d.ClusterDetails.GatewayPoolName)

	if err := unboundedcni.ApplySiteGatewayPoolAssignmentManifest(ctx, d.Logger, d.Kubectl, cniPath, unboundedcni.SiteGatewayPoolAssignment{
		SiteName:        d.Name,
		SiteNames:       []string{d.Name},
		GatewayPoolName: d.ClusterDetails.GatewayPoolName,
	}); err != nil {
		return fmt.Errorf("error applying site gateway assignment manifest: %w", err)
	}

	return nil
}

func (d *Datacenter) GetWorkerUserData() (string, error) {
	if d.BootstrapToken == nil {
		return "", fmt.Errorf("node bootstrap token is not set, cannot render worker user data")
	}

	bootstrapScript, err := assets.ReadFile("assets/worker.sh")
	if err != nil {
		return "", fmt.Errorf("read custom data file: %w", err)
	}

	runCmd := fmt.Sprintf("env API_SERVER=%q BOOTSTRAP_TOKEN=%q KUBE_VERSION=%q CLUSTER_RG=%q CA_CERT_BASE64=%q /tmp/bootstrap.sh",
		fmt.Sprintf("%s:443", *d.ClusterDetails.KubernetesCluster.Properties.Fqdn),
		d.BootstrapToken.String(),
		*d.ClusterDetails.KubernetesCluster.Properties.KubernetesVersion,
		*d.ClusterDetails.NodesResourceGroup.Name,
		base64.StdEncoding.EncodeToString(d.ClusterDetails.KubeCACertificateData))

	cc := cloudConfig{
		RunCmd: []string{runCmd},
		WriteFiles: []writeFile{
			{
				Path:        "/tmp/bootstrap.sh",
				Permissions: "0755",
				Content:     string(bootstrapScript),
			},
		},
	}

	ccRendered, err := renderCloudConfig(cc)
	if err != nil {
		return "", fmt.Errorf("render cloud config: %w", err)
	}

	return base64.StdEncoding.EncodeToString(ccRendered), nil
}

type writeFile struct {
	Content     string `yaml:"content"`
	Owner       string `yaml:"owner,omitempty"`
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
}

type cloudConfig struct {
	RunCmd     []string    `yaml:"runcmd"`
	WriteFiles []writeFile `yaml:"write_files"`
}

func renderCloudConfig(cc cloudConfig) ([]byte, error) {
	b, err := yaml.Marshal(cc)
	if err != nil {
		return b, fmt.Errorf("marshal cloud config: %w", err)
	}

	return append([]byte("#cloud-config\n"), b...), nil
}
