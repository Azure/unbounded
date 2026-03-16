package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"k8s.io/client-go/kubernetes"
)

type ClusterDetails struct {
	NodesResourceGroup    *armresources.ResourceGroup
	KubernetesCluster     *armcontainerservice.ManagedCluster
	VirtualNetwork        *armnetwork.VirtualNetwork
	KubeCli               kubernetes.Interface
	Kubectl               func(context.Context) *exec.Cmd
	KubeconfigData        []byte
	KubeCACertificateData []byte
	GatewayPoolName       string
}

func NewGetClusterDetails(azCli *azsdk.ClientSet, logger *slog.Logger, dataDir DataDir, clusterName string) *GetClusterDetails {
	return &GetClusterDetails{
		Logger:  logger,
		Name:    clusterName,
		DataDir: dataDir,
		ManagedClusterGetter: &infra.AzureKubernetesClusterManager{
			ManagedClustersCli: azCli.ManagedClustersClient,
			Logger:             logger,
		},
		GetVirtualNetwork: (&infra.VirtualNetworkManager{
			Client: azCli.NetworkVirtualNetworksClientV2,
			Logger: logger,
		}).GetVirtualNetworkByNamePrefix,
		GetResourceGroup: (&infra.ResourceGroupManager{
			Client: azCli.ResourceGroupsClientV2,
			Logger: logger,
		}).Get,
	}
}

type GetClusterDetails struct {
	ManagedClusterGetter interface {
		Get(ctx context.Context, rg, name string) (*armcontainerservice.ManagedCluster, error)
		GetUserCredentials(ctx context.Context, rgName, name string) ([]byte, error)
	}

	GetResourceGroup  func(ctx context.Context, name string) (*armresources.ResourceGroup, error)
	GetVirtualNetwork func(ctx context.Context, rg, name string) (*armnetwork.VirtualNetwork, error)
	Logger            *slog.Logger
	DataDir           DataDir
	Name              string
}

func (g *GetClusterDetails) Get(ctx context.Context) (*ClusterDetails, error) {
	const vnetPrefix = "aks-vnet-"

	g.Logger.Info("Getting cluster")

	mc, err := g.ManagedClusterGetter.Get(ctx, g.Name, g.Name)
	if err != nil {
		return nil, fmt.Errorf("get cluster: %w", err)
	}

	g.Logger.Info("Getting cluster node resource group")

	rg, err := g.GetResourceGroup(ctx, *mc.Properties.NodeResourceGroup)
	if err != nil {
		return nil, fmt.Errorf("get cluster node resource group: %w", err)
	}

	g.Logger.Info("Getting cluster node virtual network")

	vnet, err := g.GetVirtualNetwork(ctx, *rg.Name, vnetPrefix)
	if err != nil {
		return nil, fmt.Errorf("get management cluster node virtual network: %w", err)
	}

	g.Logger.Info("Get cluster kubeconfig")

	kubeConfig, err := g.ManagedClusterGetter.GetUserCredentials(ctx, g.Name, g.Name)
	if err != nil {
		return nil, fmt.Errorf("get kubernetes user credentials: %w", err)
	}

	kubeconfigPath, err := saveKubeconfig(g.DataDir.UnboundedForgePath("kubeconfig"), kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("save kubeconfig: %w", err)
	}

	g.Logger.Info("Kubeconfig saved", "path", kubeconfigPath)

	kubeCli, _, err := kube.ClientAndConfigFromBytes(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client from kubeconfig: %w", err)
	}

	kubeCA, err := kube.GetRootKubernetesCA(kubeCli)
	if err != nil {
		return nil, fmt.Errorf("get kubernetes cluster CA certificate: %w", err)
	}

	return &ClusterDetails{
		KubeconfigData:        kubeConfig,
		KubeCACertificateData: kubeCA,
		KubeCli:               kubeCli,
		Kubectl:               kube.Kubectl(nil, kubeconfigPath),
		NodesResourceGroup:    rg,
		VirtualNetwork:        vnet,
		KubernetesCluster:     mc,
		GatewayPoolName:       defaultGatewayPoolName,
	}, nil
}
