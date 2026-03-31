package infra

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/validate"
)

type VirtualNetworkManager struct {
	Client *armnetwork.VirtualNetworksClient
	Logger *slog.Logger
}

func (m *VirtualNetworkManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.VirtualNetwork) (*armnetwork.VirtualNetwork, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: get virtual network: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing virtual network, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Virtual network already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: update virtual network: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.VirtualNetwork, nil
}

func (m *VirtualNetworkManager) Get(ctx context.Context, rg, name string) (*armnetwork.VirtualNetwork, error) {
	r, err := m.Client.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.VirtualNetwork, nil
}

func (m *VirtualNetworkManager) GetVirtualNetworkByNamePrefix(ctx context.Context, rg, prefix string) (*armnetwork.VirtualNetwork, error) {
	pager := m.Client.NewListPager(rg, nil)

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("VirtualNetworkManager.GetNetworkByNameInResourceGroup: listing virtual networks: %w", err)
		}

		for i := range page.Value {
			if strings.HasPrefix(*page.Value[i].Name, prefix) {
				return page.Value[i], nil
			}
		}
	}

	return nil, fmt.Errorf("no virtual networks with prefix %q found in resource group %s", prefix, rg)
}

func (m *VirtualNetworkManager) logger(name string) *slog.Logger {
	return m.Logger.With("virtual_network", name)
}
