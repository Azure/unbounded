// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package controller registers the reusable MachineOperation provider
// controller with a controller-runtime manager.
package controller

import (
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalmachineops "github.com/Azure/unbounded/internal/machineops"
	"github.com/Azure/unbounded/pkg/machineops"
)

// ClusterInfo contains bootstrap data used by host replacement operations.
type ClusterInfo = internalmachineops.ClusterInfo

// Options scopes and tunes a MachineOperation provider controller.
type Options struct {
	SiteName                    string
	ProviderName                string
	MaxConcurrentReconciles     int
	ProviderPollInterval        time.Duration
	ProviderStallAfter          time.Duration
	ProviderStalledPollInterval time.Duration
	APIServerEndpoint           string
	CredentialSecretNamespace   string
	ClusterInfo                 *ClusterInfo
}

// AddToManager validates provider registrations and adds one MachineOperation
// reconciler to mgr.
func AddToManager(mgr ctrl.Manager, providers []*machineops.Provider, options Options) error {
	if mgr == nil {
		return fmt.Errorf("controller-runtime manager is required")
	}

	if err := validateProviders(providers, options.ProviderName); err != nil {
		return err
	}

	directClient, err := client.New(mgr.GetConfig(), client.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return fmt.Errorf("create MachineOperation client: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	reconciler := &internalmachineops.MachineOperationReconciler{
		Client:                      directClient,
		RESTMapper:                  mgr.GetRESTMapper(),
		Providers:                   providers,
		SiteName:                    options.SiteName,
		ProviderName:                options.ProviderName,
		MaxConcurrentReconciles:     options.MaxConcurrentReconciles,
		ProviderPollInterval:        options.ProviderPollInterval,
		ProviderStallAfter:          options.ProviderStallAfter,
		ProviderStalledPollInterval: options.ProviderStalledPollInterval,
		ClusterInfo:                 options.ClusterInfo,
		KubeClient:                  kubeClient,
		APIServerEndpoint:           options.APIServerEndpoint,
		CredentialSecretNamespace:   options.CredentialSecretNamespace,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineOperation controller: %w", err)
	}

	return nil
}

func validateProviders(providers []*machineops.Provider, scopedProvider string) error {
	if len(providers) == 0 {
		return fmt.Errorf("at least one provider registration is required")
	}

	names := make(map[string]struct{}, len(providers))
	for i, provider := range providers {
		if provider == nil {
			return fmt.Errorf("provider registration %d is nil", i)
		}

		name := provider.Name()
		if _, exists := names[name]; exists {
			return fmt.Errorf("provider %q is registered more than once", name)
		}

		names[name] = struct{}{}
	}

	if scopedProvider != "" {
		if _, exists := names[scopedProvider]; !exists {
			return fmt.Errorf("scoped provider %q is not registered", scopedProvider)
		}
	}

	return nil
}
