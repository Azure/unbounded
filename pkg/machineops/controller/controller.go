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
//
// The controller never times a long-running operation out. It retries
// ordinary Begin and Poll errors without limit, and it only reports a stalled
// operation and polls it more slowly. The provider owns deadlines: it ends an
// operation by returning a machineops.PermanentError or a Failed PollResult.
type Options struct {
	SiteName                string
	ProviderName            string
	MaxConcurrentReconciles int
	// ProviderPollInterval is the wait between Polls when a BeginResult or
	// PollResult sets no RequeueAfter, and the wait before Begin or Poll is
	// called again after an ordinary error. 0 means 15s.
	ProviderPollInterval time.Duration
	// ProviderStallAfter is how long after its handle was stored an
	// operation may stay in progress before it is stalled. A stalled
	// operation gets the ProviderOperationStalled condition on its target
	// and is polled at ProviderStalledPollInterval, whatever RequeueAfter
	// the provider returns. It is never ended. 0 means 75m.
	ProviderStallAfter time.Duration
	// ProviderStalledPollInterval is the wait between Polls of a stalled
	// operation. 0 means 5m.
	ProviderStalledPollInterval time.Duration
	APIServerEndpoint           string
	CredentialSecretNamespace   string
	ClusterInfo                 *ClusterInfo
	// ExperimentalContainerIsolation enables the experimental container
	// isolation mode for HostReplace. See
	// internal/machineops.MachineOperationReconciler.
	ExperimentalContainerIsolation bool
}

// AddToManager validates provider registrations and adds one MachineOperation
// reconciler to mgr. When a selected registration declares capacity, the
// reconciler also runs that registration's HostAllocate and HostRelease
// operations for the MachineAllocation controller.
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

		ExperimentalContainerIsolation: options.ExperimentalContainerIsolation,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineOperation controller: %w", err)
	}

	if !allocationPathEnabled(providers, options.ProviderName) {
		return nil
	}

	if err := reconciler.SetupAllocationWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineOperation allocation controller: %w", err)
	}

	return nil
}

// allocationPathEnabled reports whether a selected registration declares
// capacity. With scopedProvider set, only that registration is selected.
func allocationPathEnabled(providers []*machineops.Provider, scopedProvider string) bool {
	for _, provider := range providers {
		if scopedProvider != "" && provider.Name() != scopedProvider {
			continue
		}

		if _, ok := provider.Capacity(); ok {
			return true
		}
	}

	return false
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
