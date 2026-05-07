// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/infra"
)

type DeleteCluster struct {
	Azure  *azsdk.ClientSet
	Name   string
	Logger *slog.Logger
}

func (dc *DeleteCluster) Do(ctx context.Context) error {
	l := dc.Logger.With("op", "delete")

	rgm := infra.ResourceGroupManager{
		Client: dc.Azure.ResourceGroupsClientV2,
		Logger: l,
	}

	name := dc.Name

	l.Info("Deleting resource group", "name", name)

	if err := rgm.Delete(ctx, name); err != nil {
		return fmt.Errorf("delete resource group: %w", err)
	}

	return nil
}
