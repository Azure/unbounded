// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/kube"
)

const (
	defaultGatewayPoolName          = "gw-main"
	defaultGatewayPoolAgentPoolName = "gwmain"
)

type DatacenterConfig struct {
	Provider    string
	ConfigFile  string
	ExtraParams map[string]string
}

type CreateCluster struct {
	Azure                *azsdk.ClientSet
	Name                 string
	Location             string
	Logger               *slog.Logger
	SystemPoolNodeSKU    string
	SystemPoolNodeCount  int32
	GatewayPoolNodeSKU   string
	GatewayPoolNodeCount int32
	SSHDir               string
	DataDir              DataDir
	DatacenterConfig     *DatacenterConfig
}

type CreateClusterOutput struct {
	ClusterName            string
	FQDN                   string
	NodePoolsResourceGroup string
	ResourceGroup          string
	SubscriptionID         string
	KubeconfigPath         string
}

func (cc *CreateCluster) Do(ctx context.Context) (*CreateClusterOutput, error) {
	cc.Logger = cc.Logger.With("cluster", cc.Name, "stage", "create-cluster")

	rg, err := cc.createResourceGroup(ctx)
	if err != nil {
		return nil, err
	}

	cc.DataDir.UnboundedForge = *rg.Name

	if cc.SSHDir == "" {
		cc.SSHDir = cc.DataDir.UnboundedForgePath("ssh")
	}

	if err := os.MkdirAll(cc.DataDir.UnboundedForgePath(), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	cc.Logger.Info("Created data directory", "path", cc.DataDir)

	_, sshPublicKey, err := infra.CreateKeyPair(4096, cc.SSHDir, *rg.Name)
	if err != nil {
		return nil, fmt.Errorf("create SSH key pair: %w", err)
	}

	sshPublicKeyData, err := os.ReadFile(sshPublicKey)
	if err != nil {
		return nil, fmt.Errorf("read SSH public key data: %w", err)
	}

	cluster, kubeCli, kubeconfigPath, err := cc.createCluster(ctx, *rg.Name, sshPublicKeyData)
	if err != nil {
		return nil, err
	}

	kubectl := kube.Kubectl(os.Environ(), kubeconfigPath)

	out := &CreateClusterOutput{
		ClusterName:            *cluster.Name,
		FQDN:                   *cluster.Properties.Fqdn,
		NodePoolsResourceGroup: *cluster.Properties.NodeResourceGroup,
		ResourceGroup:          *rg.Name,
		SubscriptionID:         cc.Azure.SubscriptionID,
		KubeconfigPath:         kubeconfigPath,
	}

	cc.Logger.Info("Applying datacenter node bootstrap token")

	if _, err := applyBootstrapToken(ctx, cc.Logger, kubeCli, kubectl, *cluster.Name, cc.DataDir); err != nil {
		return nil, fmt.Errorf("error applying node bootstrap token: %w", err)
	}

	return out, nil
}

func (cc *CreateCluster) createResourceGroup(ctx context.Context) (*armresources.ResourceGroup, error) {
	mgr := &infra.ResourceGroupManager{
		Client: cc.Azure.ResourceGroupsClientV2,
		Logger: cc.Logger,
	}

	cc.Logger.Info("Applying resource group")

	rg, err := mgr.CreateOrUpdate(ctx, armresources.ResourceGroup{
		Name:     to.Ptr(cc.Name),
		Location: to.Ptr(cc.Location),
	})
	if err != nil {
		return nil, fmt.Errorf("creating resource group: %w", err)
	}

	return rg, nil
}

func (cc *CreateCluster) createCluster(ctx context.Context, rgName string, sshPublicKey []byte) (*armcontainerservice.ManagedCluster, kubernetes.Interface, string, error) {
	mgr := &infra.AzureKubernetesClusterManager{
		ManagedClustersCli: cc.Azure.ManagedClustersClient,
		Logger:             cc.Logger,
	}

	clusterSpec, err := infra.NewManagedCluster(cc.Logger, cc.Name, cc.Location).
		DNSPrefix(cc.Name).
		KubernetesVersion("1.34.3").
		NodePoolResourceGroupName(fmt.Sprintf("%s-nodes", rgName)).
		ServiceCIDR("10.0.0.0/16").
		WithSSHKey(sshPublicKey).
		EnableOIDCIssuer().
		WithAgentPool(
			infra.NewManagedClusterAgentPool("system", cc.SystemPoolNodeSKU, cc.SystemPoolNodeCount).
				NodePublicIP().
				SystemPool().
				Build()).
		WithAgentPool(newGatewayAgentPool("main", cc.GatewayPoolNodeSKU, cc.GatewayPoolNodeCount)).
		Build()
	if err != nil {
		return nil, nil, "", fmt.Errorf("create cluster spec: %w", err)
	}

	cc.Logger.Info("Applying cluster")

	cluster, err := mgr.CreateOrUpdate(ctx, rgName, clusterSpec)
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating cluster: %w", err)
	}

	cc.Logger.Info("Get cluster kubeconfig")

	b, err := mgr.GetUserCredentials(ctx, rgName, *cluster.Name)
	if err != nil {
		return nil, nil, "", fmt.Errorf("get kubernetes user credentials: %w", err)
	}

	kubeconfigPath, err := saveKubeconfig(cc.DataDir.UnboundedForgePath("kubeconfig"), b)
	if err != nil {
		return nil, nil, "", fmt.Errorf("save kubeconfig: %w", err)
	}

	cc.Logger.Info("Kubeconfig saved", "path", kubeconfigPath)

	cc.Logger.Info("Setup kubernetes client using kubeconfig")

	kubeCli, _, err := kube.ClientAndConfigFromBytes(b)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create kubernetes client: %w", err)
	}

	return cluster, kubeCli, kubeconfigPath, nil
}
