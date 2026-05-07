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

type SubnetManager struct {
	Client *armnetwork.SubnetsClient
	Logger *slog.Logger
}

func (m *SubnetManager) CreateOrUpdate(ctx context.Context, rgName, vnetName string, desired armnetwork.Subnet) (*armnetwork.Subnet, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("SubnetManager.CreateOrUpdate: %w", err)
	}

	if err := validate.Empty(vnetName, "vnetName"); err != nil {
		return nil, fmt.Errorf("SubnetManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, vnetName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("SubnetManager.CreateOrUpdate: get subnet: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing subnet, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Subnet already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, vnetName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("SubnetManager.CreateOrUpdate: update subnet: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("SubnetManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.Subnet, nil
}

func (m *SubnetManager) Get(ctx context.Context, rg, vnetName, name string) (*armnetwork.Subnet, error) {
	r, err := m.Client.Get(ctx, rg, vnetName, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.Subnet, nil
}

func (m *SubnetManager) logger(name string) *slog.Logger {
	return m.Logger.With("subnet", name)
}
