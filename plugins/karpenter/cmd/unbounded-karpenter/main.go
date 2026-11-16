// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/Azure/unbounded/plugins/karpenter/internal/controller"
	"github.com/Azure/unbounded/plugins/karpenter/internal/lifecycle"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
	"github.com/Azure/unbounded/plugins/karpenter/internal/provider"
)

const (
	// envPriceCurrency selects the ISO 4217 currency whose offering prices
	// are used as Karpenter prices. Karpenter's operator owns the command
	// line, so the plugin's settings are environment variables.
	envPriceCurrency = "UNBOUNDED_PRICE_CURRENCY"
)

func main() {
	nodeclass.AddToScheme(scheme.Scheme)

	ctx, manager := operator.NewOperator()

	if err := provider.RegisterIndexes(ctx, manager.GetFieldIndexer()); err != nil {
		fmt.Fprintf(os.Stderr, "configure capacity indexes: %v\n", err)
		os.Exit(1)
	}

	cloudProvider, err := provider.New(manager.GetCache(), manager.GetAPIReader(), manager.GetClient(), provider.Options{
		PriceCurrency: os.Getenv(envPriceCurrency),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure cloud provider: %v\n", err)
		os.Exit(1)
	}

	allocations := &lifecycle.Controller{
		Client:   manager.GetClient(),
		Reader:   manager.GetAPIReader(),
		Recorder: manager.GetEventRecorderFor("unbounded-karpenter"), //nolint:staticcheck // SA1019: the upstream operator uses the same recorder API.
		Marker:   cloudProvider,
	}

	manager.WithControllers(ctx, controller.Controllers(ctx, manager, cloudProvider, allocations)...).Start(ctx)
}
