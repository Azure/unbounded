// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/validate"
)

type ManagedClusterBuilder struct {
	logger *slog.Logger
	errors []error
	v      *armcontainerservice.ManagedCluster
}

func NewManagedCluster(logger *slog.Logger, name, location string) *ManagedClusterBuilder {
	return &ManagedClusterBuilder{
		logger: logger,
		v: &armcontainerservice.ManagedCluster{
			Name:     &name,
			Location: &location,
			Identity: &armcontainerservice.ManagedClusterIdentity{
				Type: to.Ptr(armcontainerservice.ResourceIdentityTypeSystemAssigned),
			},
			Properties: &armcontainerservice.ManagedClusterProperties{
				LinuxProfile: &armcontainerservice.LinuxProfile{
					AdminUsername: to.Ptr("azureuser"),
				},

				// NetworkPlugin is set to None because we deploy the unbounded-cni on the cluster.
				NetworkProfile: &armcontainerservice.NetworkProfile{
					NetworkPlugin: to.Ptr(armcontainerservice.NetworkPluginNone),
				},
			},
		},
		errors: []error{},
	}
}

func (b *ManagedClusterBuilder) DNSPrefix(prefix string) *ManagedClusterBuilder {
	// Normalize: replace non-alphanumeric characters with dash
	nonAlphanumeric := regexp.MustCompile(`[^a-zA-Z0-9-]`)
	normalized := nonAlphanumeric.ReplaceAllString(prefix, "-")

	// Collapse multiple consecutive dashes to a single dash
	multipleDashes := regexp.MustCompile(`-+`)
	normalized = multipleDashes.ReplaceAllString(normalized, "-")

	// Trim leading and trailing dashes
	normalized = strings.Trim(normalized, "-")

	b.v.Properties.DNSPrefix = &normalized

	return b
}

func (b *ManagedClusterBuilder) KubernetesVersion(kubeVersion string) *ManagedClusterBuilder {
	b.v.Properties.KubernetesVersion = &kubeVersion
	return b
}

func (b *ManagedClusterBuilder) NodePoolResourceGroupName(rgName string) *ManagedClusterBuilder {
	b.v.Properties.NodeResourceGroup = &rgName
	return b
}

func (b *ManagedClusterBuilder) EnableOIDCIssuer() *ManagedClusterBuilder {
	b.v.Properties.OidcIssuerProfile = &armcontainerservice.ManagedClusterOIDCIssuerProfile{
		Enabled: to.Ptr(true),
	}

	return b
}

//func (b *ManagedClusterBuilder) PodCIDR(cidr string) *ManagedClusterBuilder {
//	if b.v.Properties.NetworkProfile == nil {
//		b.v.Properties.NetworkProfile = &armcontainerservice.NetworkProfile{}
//	}
//
//	b.v.Properties.NetworkProfile.PodCidrs = []*string{&cidr}
//
//	return b
//}

func (b *ManagedClusterBuilder) ServiceCIDR(cidr string) *ManagedClusterBuilder {
	if b.v.Properties.NetworkProfile == nil {
		b.v.Properties.NetworkProfile = &armcontainerservice.NetworkProfile{}
	}

	b.v.Properties.NetworkProfile.ServiceCidr = &cidr

	return b
}

func (b *ManagedClusterBuilder) WithAgentPool(pool armcontainerservice.ManagedClusterAgentPoolProfile) *ManagedClusterBuilder {
	if b.v.Properties.AgentPoolProfiles == nil {
		b.v.Properties.AgentPoolProfiles = []*armcontainerservice.ManagedClusterAgentPoolProfile{}
	}

	b.v.Properties.AgentPoolProfiles = append(b.v.Properties.AgentPoolProfiles, &pool)

	return b
}

func (b *ManagedClusterBuilder) WithSSHKey(publicKeyData []byte) *ManagedClusterBuilder {
	if b.v.Properties.LinuxProfile == nil {
		b.v.Properties.LinuxProfile = &armcontainerservice.LinuxProfile{}
	}

	b.v.Properties.LinuxProfile.SSH = &armcontainerservice.SSHConfiguration{
		PublicKeys: []*armcontainerservice.SSHPublicKey{
			{
				KeyData: to.Ptr(string(publicKeyData)),
			},
		},
	}

	return b
}

func (b *ManagedClusterBuilder) Build() (armcontainerservice.ManagedCluster, error) {
	if len(b.errors) > 0 {
		errMsg := fmt.Sprintf("%d error(s) occurred", len(b.errors))
		for i, err := range b.errors {
			errMsg += fmt.Sprintf("\n%d. %s", i+1, err.Error())
		}

		return armcontainerservice.ManagedCluster{}, fmt.Errorf("ManagedClusterBuilder.Build: %s", errMsg)
	}

	return *b.v, nil
}

type ManagedClusterAgentPoolBuilder struct {
	v *armcontainerservice.ManagedClusterAgentPoolProfile
}

func NewManagedClusterAgentPool(name, vmSize string, count int32) *ManagedClusterAgentPoolBuilder {
	return &ManagedClusterAgentPoolBuilder{
		v: &armcontainerservice.ManagedClusterAgentPoolProfile{
			Name:   to.Ptr(name),
			Count:  to.Ptr(count),
			VMSize: to.Ptr(vmSize),
			OSType: to.Ptr(armcontainerservice.OSTypeLinux),
			OSSKU:  to.Ptr(armcontainerservice.OSSKUAzureLinux),
		},
	}
}

func (b *ManagedClusterAgentPoolBuilder) SystemPool() *ManagedClusterAgentPoolBuilder {
	b.v.Mode = to.Ptr(armcontainerservice.AgentPoolModeSystem)
	return b
}

func (b *ManagedClusterAgentPoolBuilder) User() *ManagedClusterAgentPoolBuilder {
	b.v.Mode = to.Ptr(armcontainerservice.AgentPoolModeUser)
	return b
}

func (b *ManagedClusterAgentPoolBuilder) NodePublicIP() *ManagedClusterAgentPoolBuilder {
	b.v.EnableNodePublicIP = to.Ptr(true)
	return b
}

func (b *ManagedClusterAgentPoolBuilder) WithAllowedPort(port armcontainerservice.PortRange) *ManagedClusterAgentPoolBuilder {
	if b.v.NetworkProfile == nil {
		b.v.NetworkProfile = &armcontainerservice.AgentPoolNetworkProfile{
			AllowedHostPorts: make([]*armcontainerservice.PortRange, 0),
		}
	}

	b.v.NetworkProfile.AllowedHostPorts = append(b.v.NetworkProfile.AllowedHostPorts, &port)

	return b
}

func (b *ManagedClusterAgentPoolBuilder) WithNodeLabels(labels map[string]string) *ManagedClusterAgentPoolBuilder {
	if b.v.NodeLabels == nil {
		b.v.NodeLabels = make(map[string]*string)
	}

	for k, v := range labels {
		b.v.NodeLabels[k] = to.Ptr(v)
	}

	return b
}

func (b *ManagedClusterAgentPoolBuilder) WithNodeTaints(taints []string) *ManagedClusterAgentPoolBuilder {
	if b.v.NodeTaints == nil {
		b.v.NodeTaints = make([]*string, 0)
	}

	for _, t := range taints {
		b.v.NodeTaints = append(b.v.NodeTaints, &t)
	}

	return b
}

func (b *ManagedClusterAgentPoolBuilder) Build() armcontainerservice.ManagedClusterAgentPoolProfile {
	return *b.v
}

type AzureKubernetesClusterManager struct {
	ManagedClustersCli *armcontainerservice.ManagedClustersClient
	Logger             *slog.Logger
}

func (m *AzureKubernetesClusterManager) CreateOrUpdate(ctx context.Context, rgName string, desired armcontainerservice.ManagedCluster) (*armcontainerservice.ManagedCluster, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.CreateOrUpdate: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing managed cluster, applying modifications is necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("AKS cluster already up-to-date")
		return current, nil
	}

	p, err := m.ManagedClustersCli.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.BeginCreateOrUpdate: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.ManagedCluster, nil
}

func (m *AzureKubernetesClusterManager) Get(ctx context.Context, rgName, name string) (*armcontainerservice.ManagedCluster, error) {
	r, err := m.ManagedClustersCli.Get(ctx, rgName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.Get: %w", err)
	}

	return &r.ManagedCluster, nil
}

// GetUserCredentials retrieves the kubeconfig credentials for the AKS cluster.
// Returns the kubeconfig as a byte slice that can be written to a file or used directly.
func (m *AzureKubernetesClusterManager) GetUserCredentials(ctx context.Context, rgName, name string) ([]byte, error) {
	l := m.logger(name)
	l.Info("Getting user credentials for AKS cluster")

	resp, err := m.ManagedClustersCli.ListClusterUserCredentials(ctx, rgName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.GetUserCredentials: %w", err)
	}

	if len(resp.Kubeconfigs) == 0 {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.GetUserCredentials: no kubeconfig returned")
	}

	// Return the first kubeconfig (typically the user credentials)
	kubeconfig := resp.Kubeconfigs[0]
	if kubeconfig.Value == nil {
		return nil, fmt.Errorf("AzureKubernetesClusterManager.GetUserCredentials: kubeconfig value is nil")
	}

	l.Info("Successfully retrieved kubeconfig credentials")

	return kubeconfig.Value, nil
}

func (m *AzureKubernetesClusterManager) logger(name string) *slog.Logger {
	return m.Logger.With("aks_cluster", name)
}
