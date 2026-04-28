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

type RouteTableManager struct {
	Client *armnetwork.RouteTablesClient
	Logger *slog.Logger
}

func (m *RouteTableManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.RouteTable) (*armnetwork.RouteTable, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("RouteTableManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("RouteTableManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.getRouteTable(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("RouteTableManager.CreateOrUpdate: get route table: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing route table, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Route table already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: update resource group: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.RouteTable, nil
}

func (m *RouteTableManager) getRouteTable(ctx context.Context, rg, name string) (*armnetwork.RouteTable, error) {
	r, err := m.Client.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.RouteTable, nil
}

func (m *RouteTableManager) logger(name string) *slog.Logger {
	return m.Logger.With("route_table", name)
}
