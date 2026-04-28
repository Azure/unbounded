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

type PublicIPAddressManager struct {
	Client *armnetwork.PublicIPAddressesClient
	Logger *slog.Logger
}

func (m *PublicIPAddressManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("PublicIPAddressManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("PublicIPAddressManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("PublicIPAddressManager.CreateOrUpdate: get public IP address: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing public IP address, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Public IP address already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("PublicIPAddressManager.CreateOrUpdate: update public IP address: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("PublicIPAddressManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.PublicIPAddress, nil
}

func (m *PublicIPAddressManager) logger(name string) *slog.Logger {
	return m.Logger.With("public_ip_address", name)
}

func (m *PublicIPAddressManager) Get(ctx context.Context, rg, name string) (*armnetwork.PublicIPAddress, error) {
	r, err := m.Client.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.PublicIPAddress, nil
}
