// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/validate"
)

type VirtualNetworkPeeringsManager struct {
	Client *armnetwork.VirtualNetworkPeeringsClient
	Logger *slog.Logger
}

func (m *VirtualNetworkPeeringsManager) CreateOrUpdate(ctx context.Context, rgName, vnet string, desired armnetwork.VirtualNetworkPeering) (*armnetwork.VirtualNetworkPeering, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, vnet, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("VirtualNetworkPeeringsManager.CreateOrUpdate: get virtual network peering: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing virtual network peering, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Virtual network already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, vnet, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: update virtual network: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualNetworkManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.VirtualNetworkPeering, nil
}

func (m *VirtualNetworkPeeringsManager) Get(ctx context.Context, rg, vnetName, peeringName string) (*armnetwork.VirtualNetworkPeering, error) {
	r, err := m.Client.Get(ctx, rg, vnetName, peeringName, nil)
	if err != nil {
		return nil, err
	}

	return &r.VirtualNetworkPeering, nil
}

func (m *VirtualNetworkPeeringsManager) logger(name string) *slog.Logger {
	return m.Logger.With("virtual_network_peering", name)
}
