package azsdk

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armfeatures"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armlocks"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// ClientSet contains all necessary Azure API clients used throughout the core parts of
// the codebase.
type ClientSet struct {
	// REQUIRED: CloudName is the name of the Azure Cloud. Callers should configure
	// this before calling Configure().
	CloudName string

	// TenantID is ID the of Azure tenant. It is automatically set during
	// Configure()
	TenantID string

	// SubscriptionID is the ID of the Azure subscription all subscription
	// oriented clients are configured to communicate with. Callers should
	// configure this before calling Configure(). If left empty then the
	// value of `AZURE_SUBSCRIPTION_ID` environment variable is consulted
	// which is legacy conforming behavior but might not be desirable if
	// multiple clients are needed that talk to different subscriptions.
	SubscriptionID string

	// The user agent string to use. When unset the default user agent of
	// "aksiknife: <version>" is used when Configure() is called.
	UserAgent string

	// Chain configures the auth source chain to use for Azure SDK clients. If
	// not configured then the default azsdk.ChainFromEnv() is used.
	Chain []CredSource

	// Policies are additional custom policies to be applied to all Azure SDK
	// clients. Policies are applied after the default policies so they can
	// override default behavior if needed.
	//
	// Note: Policies are applied in the order they are provided in this slice.
	Policies []policy.Policy

	// The API version to use for Key Vault data plane operations.
	// When not set, the current version of the SDK will be used.
	// Bleu currently uses 7.5.
	KeyVaultDataPlaneAPIVersion string

	authenticatedIdentityClaims *TokenClaims
	credential                  azcore.TokenCredential
	clientOptions               *arm.ClientOptions

	// A client for getting Azure Compute Resource SKUs.
	ComputeResourceSKUClientV2 *armcompute.ResourceSKUsClient

	// A client for interacting with Azure Compute Disks.
	ComputeDisksClientV2 *armcompute.DisksClient

	// A client for interacting with Azure Compute Galleries.
	ComputeGalleryClientV2 *armcompute.GalleriesClient

	// A client for interacting with Azure Compute Gallery Images.
	ComputeGalleryImageClientV2 *armcompute.GalleryImagesClient

	// A client for interacting with Azure Compute Gallery Image Versions.
	ComputeGalleryImageVersionClientV2 *armcompute.GalleryImageVersionsClient

	// A client for interacting with Azure Compute Images.
	ComputeImageClientV2 *armcompute.ImagesClient

	// A client for interacting with Azure Compute Snapshots.
	ComputeSnapshotClientV2 *armcompute.SnapshotsClient

	// A client for interacting with Azure Compute Virtual Machines.
	ComputeVMClientV2 *armcompute.VirtualMachinesClient

	// A client for interacting with Azure Compute Virtual Machine Scale Sets.
	ComputeVMScaleSetClientV2 *armcompute.VirtualMachineScaleSetsClient

	// A client for interacting with Azure Compute Virtual Machine Scale Set Extensions.
	ComputeVMScaleSetExtensionClientV2 *armcompute.VirtualMachineScaleSetExtensionsClient

	// A client for interacting with Azure Compute Virtual Machine Scale Set VMs.
	ComputeVMScaleSetVMClientV2 *armcompute.VirtualMachineScaleSetVMsClient

	// A client for interacting with Azure Compute Usage.
	ComputeUsageClientV2 *armcompute.UsageClient

	// A client for interacting with Azure DNS Zones.
	DNSZonesClientV2 *armdns.ZonesClient

	// A client for interacting with Azure DNS RecordSets.
	DNSRecordSetsClientV2 *armdns.RecordSetsClient

	// A client for interacting with Azure Private DNS Zones.
	PrivateDNSZonesClientV2 *armprivatedns.PrivateZonesClient

	// A client for interacting with Azure Private DNS RecordSets.
	PrivateDNSRecordSetsClientV2 *armprivatedns.RecordSetsClient

	// A client for interacting with Azure Private DNS Virtual Network Links.
	PrivateDNSVirtualNetworkLinksClientV2 *armprivatedns.VirtualNetworkLinksClient

	// A client for interacting with Azure ARM feature flag registration.
	FeaturesClientV2 *armfeatures.Client

	// A client for interacting with User-Assigned Managed Identities.
	IdentitiesClientV2 *armmsi.UserAssignedIdentitiesClient

	// A client for interacting with Azure Key Vault.
	KeyVaultClientV2 *armkeyvault.VaultsClient

	KeyVaultSecretsClientV2 *armkeyvault.SecretsClient

	// A client for interacting with AKS Managed Clusters.
	ManagedClustersClient *armcontainerservice.ManagedClustersClient

	// A client for interacting with AKS Agent Pools.
	ManagedClusterAgentPoolsClient *armcontainerservice.AgentPoolsClient

	// A client for interacting with Azure Network Load Balancers.
	NetworkLoadBalancersClientV2 *armnetwork.LoadBalancersClient

	// A client for interacting with Azure Network Load Balancer Frontend IPs.
	NetworkLoadBalancerFrontendIPsClientV2 *armnetwork.LoadBalancerFrontendIPConfigurationsClient

	// A client for interacting with Azure Network Load Balancer Backend Address Pools.
	NetworkLoadBalancerBackendAddressPoolsClientV2 *armnetwork.LoadBalancerBackendAddressPoolsClient

	// A client for interacting with Azure Network Load Balancer Network Interfaces.
	NetworkLoadBalancerNetworkInterfacesClientV2 *armnetwork.LoadBalancerNetworkInterfacesClient

	// A client for interacting with Azure Network Load Balancer Inbound NAT Rules.
	NetworkLoadBalancerInboundNATRulesClientV2 *armnetwork.InboundNatRulesClient

	// A client for interacting with Azure Network Load Balancer Probes.
	NetworkLoadBalancerProbesClientV2 *armnetwork.LoadBalancerProbesClient

	// A client for interacting with Azure Network Public IP Addresses.
	NetworkPublicIPAddressesClientV2 *armnetwork.PublicIPAddressesClient

	// A client for interacting with Azure Network Route Tables.
	NetworkRouteTablesClientV2 *armnetwork.RouteTablesClient

	// A client for interacting with Azure Network Security Groups.
	NetworkSecurityGroupsClientV2 *armnetwork.SecurityGroupsClient

	// A client for interacting with Azure network security perimeter associations.
	NetworkSecurityPerimeterAssociationsClient *armnetwork.SecurityPerimeterAssociationsClient

	// A client for interacting with Azure Network Security Rules.
	NetworkSecurityRulesClientV2 *armnetwork.SecurityRulesClient

	// A client for interacting with Azure Network Subnets.
	NetworkSubnetsClientV2 *armnetwork.SubnetsClient

	// A client for interacting with Azure Network Interfaces.
	NetworkInterfacesClientV2 *armnetwork.InterfacesClient

	// A client for interacting with Azure Virtual Networks.
	NetworkVirtualNetworksClientV2 *armnetwork.VirtualNetworksClient

	// A client for interacting with Azure Virtual Network Peerings.
	NetworkVirtualNetworkPeeringsClientV2 *armnetwork.VirtualNetworkPeeringsClient

	// A client for interacting with Azure RBAC Role Assignments.
	RBACRoleAssignmentsClientV2 *armauthorization.RoleAssignmentsClient

	// A client for interacting with Azure RBAC Role Definitions.
	RBACRoleDefinitionsClientV2 *armauthorization.RoleDefinitionsClient

	// A client for interacting with Azure subscriptions.
	SubscriptionsClientV2 *armsubscriptions.Client

	// A client for interacting with Azure management locks.
	ManagementLocksClientV2 *armlocks.ManagementLocksClient

	// A client for interacting with Azure resources.
	ResourceClientV2 *armresources.Client

	// A client for interacting with Azure resource deployments.
	ResourceDeploymentClientV2 *armresources.DeploymentsClient

	// A client for interacting with Azure resource groups.
	ResourceGroupsClientV2 *armresources.ResourceGroupsClient

	// A client for interacting with Azure resource providers.
	ResourceProvidersClientV2 *armresources.ProvidersClient

	// A client for interacting with Azure resource tags.
	ResourceTagsClientV2 *armresources.TagsClient

	// A client for interacting with Azure Storage Accounts.
	StorageAccountsClientV2 *armstorage.AccountsClient

	// A client for interacting with Azure Storage Blob Services.
	StorageBlobServicesClientV2 *armstorage.BlobServicesClient

	// A client for interacting with Azure Storage SKUs.
	StorageSKUsClientV2 *armstorage.SKUsClient
}

func (c *ClientSet) Credential() azcore.TokenCredential {
	return c.credential
}

func (c *ClientSet) CurrentIdentityObjectID() string {
	if c.authenticatedIdentityClaims == nil {
		return ""
	}

	return c.authenticatedIdentityClaims.ObjectID
}

func (c *ClientSet) CurrentIdentityType() string {
	if c.authenticatedIdentityClaims == nil {
		return ""
	}

	return c.authenticatedIdentityClaims.IDType
}

func (c *ClientSet) Configure() error {
	if c.CloudName == "" {
		return fmt.Errorf("cloud is not set")
	}

	cc, err := CloudConfig(c.CloudName)
	if err != nil {
		return fmt.Errorf("get cloud config for cloud %q: %w", c.CloudName, err)
	}

	var opts *azcore.ClientOptions

	c.credential, c.authenticatedIdentityClaims, opts, err = getTokenCredential(c.CloudName, c.Chain, &cc)
	if err != nil {
		return fmt.Errorf("get token credential: %w", err)
	}

	c.clientOptions, c.UserAgent, err = configureClientOptions(c.UserAgent, *opts, c.Policies)
	if err != nil {
		return fmt.Errorf("configure client options: %w", err)
	}

	c.TenantID = c.authenticatedIdentityClaims.TenantID

	if c.SubscriptionID == "" {
		c.SubscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}

	c.defaultAPIVersions()

	// Start configuring factories and clients for working with Azure resources.
	if err = c.configureComputeClients(); err != nil {
		return err
	}

	if err = c.configureContainerServiceClients(); err != nil {
		return err
	}

	if err = c.configureDNSClients(); err != nil {
		return err
	}

	if err = c.configureIdentityClients(); err != nil {
		return err
	}

	if err = c.configureKeyVaultClients(); err != nil {
		return err
	}

	if err = c.configureNetworkClients(); err != nil {
		return err
	}

	if err = c.configureRBACClients(); err != nil {
		return err
	}

	if err = c.configureResourceClients(); err != nil {
		return err
	}

	if err = c.configureStorageClients(); err != nil {
		return err
	}

	return nil
}

func (c *ClientSet) NewBlobStorageClient(accountName, storageEndpoint string, opts *azblob.ClientOptions) (*azblob.Client, error) {
	return azblob.NewClient(fmt.Sprintf("https://%s.blob.%s", accountName, storageEndpoint), c.credential, opts)
}

// Use a specific API version instead of what's shipped with the SDK, if not already set.
func (c *ClientSet) defaultAPIVersions() {
	// Set the default API version for Key Vault data plane operations if not already set.
	if c.clientOptions.APIVersion == "" {
		c.KeyVaultDataPlaneAPIVersion = "7.5"
	}
}

func fmtDefaultScope(endpoint string) string {
	return endpoint + "/.default"
}

func getAccessToken(ctx context.Context, credential azcore.TokenCredential, scopes []string) (string, error) {
	tok, err := credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: scopes,
	})
	if err != nil {
		return "", fmt.Errorf("getting raw access token: %w", err)
	}

	return tok.Token, nil
}

func getTokenCredential(cloudName string, chain []CredSource, cloudConfig *cloud.Configuration) (azcore.TokenCredential, *TokenClaims, *azcore.ClientOptions, error) {
	// environment := cloud.SDKEndpointsConfig()
	authConfig := AuthConfig{
		CloudName: cloudName,
		Chain:     chain,
	}

	credential, opts, err := Setup(authConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("setup azure token: %w", err)
	}

	accessToken, err := getAccessToken(context.Background(), credential, []string{
		fmtDefaultScope(cloudConfig.Services[cloud.ResourceManager].Endpoint),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("getting access token: %w", err)
	}

	tokenClaims, err := GetTokenClaims(accessToken)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get access token claims: %w", err)
	}

	return credential, tokenClaims, opts, nil
}

func configureClientOptions(userAgent string, opts policy.ClientOptions, _ []policy.Policy) (*arm.ClientOptions, string, error) {
	if userAgent == "" {
		userAgent = "unbounded-forge"
	}

	clientOptions := &arm.ClientOptions{
		ClientOptions: opts,
	}

	// TODO <phlombar@microsoft.com> 2026-02-03: None of this matters right now
	//
	// Apply policies to the client.
	//clientOptions.PerRetryPolicies = append(clientOptions.PerRetryPolicies, userAgentPolicy{
	//	userAgent: userAgent,
	//})
	//
	//// Apply any additional custom policies.
	//if len(policies) > 0 {
	//	clientOptions.PerRetryPolicies = append(clientOptions.PerRetryPolicies, policies...)
	//}
	//
	//// Don't register the resource provider for the client, assume they are already registered.
	//clientOptions.DisableRPRegistration = true
	//
	//// This overrides the default retry policy for transient errors.
	//clientOptions.Retry.ShouldRetry = shouldRetryWhen(
	//	shouldRetryOnStatusCode(defaultAzureTemporaryErrorStatusCodes()...),
	//	shouldRetryOnConnectionResetByPeerError,
	//)

	return clientOptions, userAgent, nil
}

func (c *ClientSet) configureComputeClients() error {
	f, err := armcompute.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create Compute client factory: %w", err)
	}

	c.ComputeResourceSKUClientV2 = f.NewResourceSKUsClient()
	c.ComputeDisksClientV2 = f.NewDisksClient()
	c.ComputeGalleryClientV2 = f.NewGalleriesClient()
	c.ComputeGalleryImageClientV2 = f.NewGalleryImagesClient()
	c.ComputeGalleryImageVersionClientV2 = f.NewGalleryImageVersionsClient()
	c.ComputeImageClientV2 = f.NewImagesClient()
	c.ComputeSnapshotClientV2 = f.NewSnapshotsClient()
	c.ComputeVMClientV2 = f.NewVirtualMachinesClient()
	c.ComputeUsageClientV2 = f.NewUsageClient()

	// Allow more retries than the default which is 3.
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
	}

	// Create VMSS clients with custom retry options.
	f, err = armcompute.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return fmt.Errorf("failed to create compute client factory with retries: %w", err)
	}

	c.ComputeVMScaleSetClientV2 = f.NewVirtualMachineScaleSetsClient()
	c.ComputeVMScaleSetExtensionClientV2 = f.NewVirtualMachineScaleSetExtensionsClient()
	c.ComputeVMScaleSetVMClientV2 = f.NewVirtualMachineScaleSetVMsClient()

	return nil
}

func (c *ClientSet) configureContainerServiceClients() error {
	f, err := armcontainerservice.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create Compute client factory: %w", err)
	}

	c.ManagedClustersClient = f.NewManagedClustersClient()
	c.ManagedClusterAgentPoolsClient = f.NewAgentPoolsClient()

	return nil
}

func (c *ClientSet) configureDNSClients() error {
	// Allow additional retries then the default and handle 409 Conflict errors.
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
	}

	dnsFactory, err := armdns.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return fmt.Errorf("failed to create dns factory: %w", err)
	}

	c.DNSZonesClientV2 = dnsFactory.NewZonesClient()
	c.DNSRecordSetsClientV2 = dnsFactory.NewRecordSetsClient()

	privateDNSFactory, err := armprivatedns.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create private dns factory: %w", err)
	}

	c.PrivateDNSZonesClientV2 = privateDNSFactory.NewPrivateZonesClient()
	c.PrivateDNSRecordSetsClientV2 = privateDNSFactory.NewRecordSetsClient()
	c.PrivateDNSVirtualNetworkLinksClientV2 = privateDNSFactory.NewVirtualNetworkLinksClient()

	return nil
}

func (c *ClientSet) configureIdentityClients() error {
	// Allow additional retries then the default and handle 499 ManagedClustersCli Closed Request.
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
	}

	identityFactory, err := armmsi.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return err
	}

	c.IdentitiesClientV2 = identityFactory.NewUserAssignedIdentitiesClient()

	return nil
}

func (c *ClientSet) configureKeyVaultClients() error {
	f, err := armkeyvault.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create key vault client factory: %w", err)
	}

	c.KeyVaultClientV2 = f.NewVaultsClient()

	c.KeyVaultSecretsClientV2 = f.NewSecretsClient()

	return nil
}

func (c *ClientSet) configureNetworkClients() error {
	f, err := armnetwork.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return err
	}

	c.NetworkLoadBalancersClientV2 = f.NewLoadBalancersClient()
	c.NetworkLoadBalancerFrontendIPsClientV2 = f.NewLoadBalancerFrontendIPConfigurationsClient()
	c.NetworkLoadBalancerBackendAddressPoolsClientV2 = f.NewLoadBalancerBackendAddressPoolsClient()
	c.NetworkLoadBalancerNetworkInterfacesClientV2 = f.NewLoadBalancerNetworkInterfacesClient()
	c.NetworkLoadBalancerInboundNATRulesClientV2 = f.NewInboundNatRulesClient()
	c.NetworkLoadBalancerProbesClientV2 = f.NewLoadBalancerProbesClient()
	c.NetworkRouteTablesClientV2 = f.NewRouteTablesClient()
	c.NetworkSecurityGroupsClientV2 = f.NewSecurityGroupsClient()
	c.NetworkSecurityPerimeterAssociationsClient = f.NewSecurityPerimeterAssociationsClient()
	c.NetworkSecurityRulesClientV2 = f.NewSecurityRulesClient()
	c.NetworkSubnetsClientV2 = f.NewSubnetsClient()
	c.NetworkInterfacesClientV2 = f.NewInterfacesClient()
	c.NetworkVirtualNetworksClientV2 = f.NewVirtualNetworksClient()
	c.NetworkVirtualNetworkPeeringsClientV2 = f.NewVirtualNetworkPeeringsClient()

	// Allow more retries and handle additional status codes then the default.
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
	}

	// Configure the public IP address client.
	f, err = armnetwork.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return err
	}

	c.NetworkPublicIPAddressesClientV2 = f.NewPublicIPAddressesClient()

	return nil
}

func (c *ClientSet) configureRBACClients() error {
	f, err := armauthorization.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return err
	}

	c.RBACRoleAssignmentsClientV2 = f.NewRoleAssignmentsClient()
	c.RBACRoleDefinitionsClientV2 = f.NewRoleDefinitionsClient()

	return nil
}

func (c *ClientSet) configureResourceClients() error {
	subscriptionsFactory, err := armsubscriptions.NewClientFactory(c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create Subscriptions client factory: %w", err)
	}

	c.SubscriptionsClientV2 = subscriptionsFactory.NewClient()

	locksFactory, err := armlocks.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create Locks client factory: %w", err)
	}

	c.ManagementLocksClientV2 = locksFactory.NewManagementLocksClient()

	resourcesFactory, err := armresources.NewClientFactory(c.SubscriptionID, c.credential, c.clientOptions)
	if err != nil {
		return fmt.Errorf("failed to create Resources client factory: %w", err)
	}

	c.ResourceClientV2 = resourcesFactory.NewClient()
	c.ResourceDeploymentClientV2 = resourcesFactory.NewDeploymentsClient()
	c.ResourceProvidersClientV2 = resourcesFactory.NewProvidersClient()
	c.ResourceTagsClientV2 = resourcesFactory.NewTagsClient()

	// Allow more retries and handle transient errors created by ARM locks not fully removed.
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
		RetryDelay: 5 * time.Second,
	}

	// Create a new Resources client factory for handling resource groups.
	resourceGroupFactory, err := armresources.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return fmt.Errorf("failed to create Resource Group client factory: %w", err)
	}

	c.ResourceGroupsClientV2 = resourceGroupFactory.NewResourceGroupsClient()

	return nil
}

func (c *ClientSet) configureStorageClients() error {
	optionsWithCustomRetry := c.cloneDefaultClientOptions()
	optionsWithCustomRetry.Retry = policy.RetryOptions{
		MaxRetries: 10,
		RetryDelay: 5 * time.Second,
	}

	f, err := armstorage.NewClientFactory(c.SubscriptionID, c.credential, optionsWithCustomRetry)
	if err != nil {
		return err
	}

	c.StorageAccountsClientV2 = f.NewAccountsClient()
	c.StorageBlobServicesClientV2 = f.NewBlobServicesClient()
	c.StorageSKUsClientV2 = f.NewSKUsClient()

	return nil
}

// Create a copy of the default client options, for customization with specific client factories.
// Don't use the `StatusCodes` field of the retry policy because it will have no effect.
func (c *ClientSet) cloneDefaultClientOptions() *arm.ClientOptions {
	return c.clientOptions.Clone()
}
