// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// machineOperationSelectors returns the field selectors that together select
// the MachineOperations the daemon for machineName can act on: those whose
// spec.machineRef is machineName, and those with no machineRef, which select
// their Machines by label. Every other MachineOperation names another Machine.
func machineOperationSelectors(machineName string) []fields.Selector {
	return []fields.Selector{
		fields.OneTermEqualSelector(machinav1alpha3.MachineOperationMachineRefField, machineName),
		fields.OneTermEqualSelector(machinav1alpha3.MachineOperationMachineRefField, ""),
	}
}

// MachineRefSelectable reports whether the API server can select
// MachineOperations by spec.machineRef. The MachineOperation CRD declares the
// field selectable, which the API server honors from Kubernetes 1.31. An older
// API server, or one serving a CRD that predates the declaration, rejects the
// selector as a bad request; a daemon there has to watch every
// MachineOperation instead.
func MachineRefSelectable(ctx context.Context, reader client.Reader, machineName string) (bool, error) {
	err := reader.List(
		ctx,
		&machinav1alpha3.MachineOperationList{},
		client.MatchingFieldsSelector{Selector: machineOperationSelectors(machineName)[0]},
		client.Limit(1),
	)

	switch {
	case err == nil:
		return true, nil
	case apierrors.IsBadRequest(err):
		return false, nil
	default:
		return false, fmt.Errorf("list MachineOperations by %s: %w", machinav1alpha3.MachineOperationMachineRefField, err)
	}
}

// AddMachineOperationCaches adds to mgr, and returns, two caches that hold
// only the MachineOperations the daemon for machineName can act on: one those
// whose spec.machineRef is machineName, and one those with no machineRef. The
// API server filters them, so the daemon neither downloads nor hears about
// MachineOperations for other Machines. Pass them to WatchMachineOperationsIn.
//
// The API server must be able to select MachineOperations by
// spec.machineRef; see MachineRefSelectable.
func AddMachineOperationCaches(mgr manager.Manager, machineName string) ([]cache.Cache, error) {
	if machineName == "" {
		return nil, fmt.Errorf("machine name is required")
	}

	selectors := machineOperationSelectors(machineName)
	caches := make([]cache.Cache, 0, len(selectors))

	for _, selector := range selectors {
		c, err := cache.New(mgr.GetConfig(), cache.Options{
			HTTPClient: mgr.GetHTTPClient(),
			Scheme:     mgr.GetScheme(),
			Mapper:     mgr.GetRESTMapper(),
			ByObject: map[client.Object]cache.ByObject{
				&machinav1alpha3.MachineOperation{}: {Field: selector},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("create MachineOperation cache for %s: %w", selector, err)
		}

		if err := mgr.Add(c); err != nil {
			return nil, fmt.Errorf("add MachineOperation cache for %s: %w", selector, err)
		}

		caches = append(caches, c)
	}

	return caches, nil
}

// ListMachineOperations returns the MachineOperations the daemon for
// machineName can act on: those whose spec.machineRef is machineName, and
// those with no machineRef. If selectable, as MachineRefSelectable reports,
// the API server selects them. Otherwise every MachineOperation is listed and
// the others are dropped here.
func ListMachineOperations(
	ctx context.Context,
	reader client.Reader,
	machineName string,
	selectable bool,
) ([]machinav1alpha3.MachineOperation, error) {
	if machineName == "" {
		return nil, fmt.Errorf("machine name is required")
	}

	if !selectable {
		var list machinav1alpha3.MachineOperationList
		if err := reader.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("list MachineOperations: %w", err)
		}

		return slices.DeleteFunc(list.Items, func(op machinav1alpha3.MachineOperation) bool {
			return op.Spec.MachineRef != "" && op.Spec.MachineRef != machineName
		}), nil
	}

	var ops []machinav1alpha3.MachineOperation

	for _, selector := range machineOperationSelectors(machineName) {
		var list machinav1alpha3.MachineOperationList
		if err := reader.List(ctx, &list, client.MatchingFieldsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("list MachineOperations with %s: %w", selector, err)
		}

		ops = append(ops, list.Items...)
	}

	return ops, nil
}
