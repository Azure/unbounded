// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	internalmachineops "github.com/Azure/unbounded/internal/machineops"
	"github.com/Azure/unbounded/pkg/machineops"
)

// CapacityOptions scopes and tunes the capacity controllers. Later capacity
// controllers add fields to this struct, and the zero value of each new field
// keeps the earlier behavior.
//
// EnableAllocationController adds the MachineAllocation controller. It needs
// the reusable roles in deploy/machine-ops/05-machineallocation-rbac.yaml.tmpl
// and the two ValidatingAdmissionPolicies in
// deploy/machine-ops/06-machine-allocationref-policy.yaml.tmpl and
// deploy/machine-ops/07-machineallocation-bootstrap-token-policy.yaml.tmpl.
// The same manager must also call AddToManager with the same providers,
// because that registers the path that runs HostAllocate and HostRelease.
type CapacityOptions struct {
	// ProviderName, when set, selects only this registration, which must
	// declare capacity.
	ProviderName string

	// SiteName, when set, selects only classes whose spec.siteRef.name equals
	// it.
	SiteName string

	// CredentialSecretNamespace is the only namespace credential Secrets are
	// read from. It is required.
	CredentialSecretNamespace string

	// ProviderConfigNamespace is the only namespace provider-config and
	// bootstrap files ConfigMaps are read from. It is required.
	ProviderConfigNamespace string

	// ClassRefreshInterval is how often a class is resolved again when no
	// offering expires sooner. 0 means 5m. Otherwise it must be at least the
	// effective MinRequeueInterval.
	ClassRefreshInterval time.Duration

	// MinRequeueInterval is the shortest requeue after a class resolved. 0
	// means 10s. Otherwise it must be at least 1s and at most the effective
	// ClassRefreshInterval.
	MinRequeueInterval time.Duration

	// EnableAllocationController registers the MachineAllocation controller
	// next to the MachineClass controller. false keeps the earlier behavior:
	// only the MachineClass controller runs.
	EnableAllocationController bool

	// AllocationConsumerKinds are the cluster-scoped consumer kinds whose
	// termination and loss the MachineAllocation controller observes. Empty
	// means consumer loss is never observed.
	AllocationConsumerKinds []schema.GroupVersionKind
}

const (
	machineClassControllerName      = "machineclass"
	machineAllocationControllerName = "machineallocation"
)

// AddCapacityToManager registers the MachineClass controller for the provider
// registrations that declare capacity. The controller runs only while mgr
// holds its leader-election lease, so the caller must enable leader election
// on mgr with a lease ID unique to the scope (provider and Site). Two
// deployments must not have overlapping scopes: for example, an unscoped
// deployment next to one scoped to a Site would both write the same classes.
//
// With options.EnableAllocationController it also registers the
// MachineAllocation controller, which needs the roles in
// deploy/machine-ops/05-machineallocation-rbac.yaml.tmpl. Install the two
// ValidatingAdmissionPolicies in
// deploy/machine-ops/06-machine-allocationref-policy.yaml.tmpl and
// deploy/machine-ops/07-machineallocation-bootstrap-token-policy.yaml.tmpl
// with it. The same manager must also call AddToManager with the same
// providers, because that registers the path that runs HostAllocate and
// HostRelease.
func AddCapacityToManager(mgr ctrl.Manager, providers []*machineops.Provider, options CapacityOptions) error {
	if mgr == nil {
		return fmt.Errorf("controller-runtime manager is required")
	}

	selected, err := selectCapacityProviders(providers, options)
	if err != nil {
		return err
	}

	configMaps, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme:            mgr.GetScheme(),
		Mapper:            mgr.GetRESTMapper(),
		DefaultNamespaces: map[string]cache.Config{options.ProviderConfigNamespace: {}},
	})
	if err != nil {
		return fmt.Errorf("create provider-config ConfigMap cache: %w", err)
	}

	if err := mgr.Add(configMaps); err != nil {
		return fmt.Errorf("add provider-config ConfigMap cache: %w", err)
	}

	reconciler := &internalmachineops.MachineClassReconciler{
		Client:                    mgr.GetClient(),
		ConfigMapReader:           configMaps,
		SecretReader:              mgr.GetAPIReader(),
		Providers:                 selected,
		SiteName:                  options.SiteName,
		CredentialSecretNamespace: options.CredentialSecretNamespace,
		ProviderConfigNamespace:   options.ProviderConfigNamespace,
		ClassRefreshInterval:      options.ClassRefreshInterval,
		MinRequeueInterval:        options.MinRequeueInterval,
	}

	if err := reconciler.SetupWithManager(mgr, configMaps); err != nil {
		return fmt.Errorf("setup MachineClass controller: %w", err)
	}

	if !options.EnableAllocationController {
		return nil
	}

	allocations := &internalmachineops.MachineAllocationReconciler{
		Client:          mgr.GetClient(),
		ConfigMapReader: configMaps,
		APIReader:       mgr.GetAPIReader(),
		Providers:       selected,
		SiteName:        options.SiteName,
		ConsumerKinds:   slices.Clone(options.AllocationConsumerKinds),
	}

	if err := allocations.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineAllocation controller: %w", err)
	}

	return nil
}

// capacityControllers returns the names of the controllers options register.
func capacityControllers(options CapacityOptions) []string {
	if options.EnableAllocationController {
		return []string{machineClassControllerName, machineAllocationControllerName}
	}

	return []string{machineClassControllerName}
}

// selectCapacityProviders validates the registrations and options and returns
// the registrations the capacity controllers serve: every registration that
// declares capacity, or only options.ProviderName's.
func selectCapacityProviders(providers []*machineops.Provider, options CapacityOptions) ([]*machineops.Provider, error) {
	if err := validateProviders(providers, options.ProviderName); err != nil {
		return nil, err
	}

	if err := validateCapacityOptions(options); err != nil {
		return nil, err
	}

	var selected []*machineops.Provider

	for _, provider := range providers {
		if options.ProviderName != "" && provider.Name() != options.ProviderName {
			continue
		}

		if _, ok := provider.Capacity(); !ok {
			if options.ProviderName != "" {
				return nil, fmt.Errorf("scoped provider %q does not declare capacity", options.ProviderName)
			}

			continue
		}

		selected = append(selected, provider)
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("no provider registration declares capacity")
	}

	return selected, nil
}

func validateCapacityOptions(options CapacityOptions) error {
	if options.ProviderConfigNamespace == "" {
		return fmt.Errorf("CapacityOptions.ProviderConfigNamespace is required")
	}

	if options.CredentialSecretNamespace == "" {
		return fmt.Errorf("CapacityOptions.CredentialSecretNamespace is required")
	}

	if options.ClassRefreshInterval < 0 {
		return fmt.Errorf("CapacityOptions.ClassRefreshInterval %s must not be negative", options.ClassRefreshInterval)
	}

	if err := validateAllocationOptions(options); err != nil {
		return err
	}

	if options.MinRequeueInterval < 0 {
		return fmt.Errorf("CapacityOptions.MinRequeueInterval %s must not be negative", options.MinRequeueInterval)
	}

	if options.MinRequeueInterval > 0 && options.MinRequeueInterval < internalmachineops.ClassMinRequeueIntervalLowerBound {
		return fmt.Errorf("CapacityOptions.MinRequeueInterval %s must be at least %s",
			options.MinRequeueInterval, internalmachineops.ClassMinRequeueIntervalLowerBound)
	}

	refresh := options.ClassRefreshInterval
	if refresh == 0 {
		refresh = internalmachineops.DefaultClassRefreshInterval
	}

	floor := options.MinRequeueInterval
	if floor == 0 {
		floor = internalmachineops.DefaultClassMinRequeueInterval
	}

	if refresh < floor {
		return fmt.Errorf("effective CapacityOptions.ClassRefreshInterval %s must be at least the effective MinRequeueInterval %s", refresh, floor)
	}

	return nil
}

func validateAllocationOptions(options CapacityOptions) error {
	if len(options.AllocationConsumerKinds) > 0 && !options.EnableAllocationController {
		return fmt.Errorf("CapacityOptions.AllocationConsumerKinds requires EnableAllocationController")
	}

	seen := make(map[schema.GroupKind]struct{}, len(options.AllocationConsumerKinds))

	for i, gvk := range options.AllocationConsumerKinds {
		if gvk.Kind == "" || gvk.Version == "" {
			return fmt.Errorf("CapacityOptions.AllocationConsumerKinds[%d] %q must set Kind and Version", i, gvk)
		}

		if _, exists := seen[gvk.GroupKind()]; exists {
			return fmt.Errorf("CapacityOptions.AllocationConsumerKinds[%d] repeats %s", i, gvk.GroupKind())
		}

		seen[gvk.GroupKind()] = struct{}{}
	}

	return nil
}
