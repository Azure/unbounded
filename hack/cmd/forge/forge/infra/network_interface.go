// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/validate"
)

type NetworkInterfaceManager struct {
	Client *armnetwork.InterfacesClient
	Logger *slog.Logger
}

func (m *NetworkInterfaceManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.Interface) (*armnetwork.Interface, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("NetworkInterfaceManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("NetworkInterfaceManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.getNetworkInterface(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("NetworkInterfaceManager.CreateOrUpdate: get network interface: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing network interface, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Network interface already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("NetworkInterfaceManager.CreateOrUpdate: update network interface: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("NetworkInterfaceManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.Interface, nil
}

func (m *NetworkInterfaceManager) logger(name string) *slog.Logger {
	return m.Logger.With("network_interface", name)
}

func (m *NetworkInterfaceManager) getNetworkInterface(ctx context.Context, rg, name string) (*armnetwork.Interface, error) {
	r, err := m.Client.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.Interface, nil
}
