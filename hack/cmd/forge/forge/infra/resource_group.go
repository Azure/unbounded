// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/validate"
)

type ResourceGroupManager struct {
	Client *armresources.ResourceGroupsClient
	Logger *slog.Logger
}

func (m *ResourceGroupManager) CreateOrUpdate(ctx context.Context, desired armresources.ResourceGroup) (*armresources.ResourceGroup, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: get resource group: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing resource group, applying modifications is necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Resource group already up-to-date")
		return current, nil
	}

	cuResp, err := m.Client.CreateOrUpdate(ctx, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.CreateOrUpdate: update resource group: %w", err)
	}

	return &cuResp.ResourceGroup, nil
}

func (m *ResourceGroupManager) Delete(ctx context.Context, name string) error {
	if err := validate.Empty(name, "name"); err != nil {
		return fmt.Errorf("ResourceGroupManager.Delete: %w", err)
	}

	m.logger(name)

	p, err := m.Client.BeginDelete(ctx, name, nil)
	if err != nil {
		if azsdk.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("ResourceGroupManager.Delete: %w", err)
	}

	if _, err := p.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("ResourceGroupManager.Delete: %w", err)
	}

	return nil
}

func (m *ResourceGroupManager) logger(name string) *slog.Logger {
	return m.Logger.With("resource_group", name)
}

func (m *ResourceGroupManager) Get(ctx context.Context, name string) (*armresources.ResourceGroup, error) {
	r, err := m.Client.Get(ctx, name, nil)
	if err != nil {
		return nil, fmt.Errorf("ResourceGroupManager.Get: %w", err)
	}

	return &r.ResourceGroup, nil
}
