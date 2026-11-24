// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerobject"
)

const (
	originMaxRequests = 65536

	// nativeOriginAnnotation marks a dataplane Pod template that reads
	// ClusterCache origins in-process. It is written to keep the DaemonSet
	// template stable across releases.
	nativeOriginAnnotation = "unbounded-cloud.io/racer-native-origin"
)

// validateOrigin re-checks the CRD rules for spec.origin so a cache that
// bypassed admission is reported instead of being silently served.
func validateOrigin(cache *racerv1.ClusterCache) error {
	origin := cache.Spec.Origin
	if origin.Azure == nil {
		return errors.New("spec.origin.azure is required")
	}

	credential := "anonymous"

	if c := origin.Azure.Credential; c != nil {
		credential = "default"

		switch {
		case (c.Default == nil) == (c.WorkloadIdentity == nil):
			return errors.New("spec.origin.azure.credential must set exactly one of default or workloadIdentity")
		case c.WorkloadIdentity != nil && c.WorkloadIdentity.ClientID == "":
			return errors.New("spec.origin.azure.credential.workloadIdentity.clientID is required")
		case c.Default != nil && c.Default.SecretRef != nil:
			return errors.New("spec.origin.azure.credential.default.secretRef is no longer supported; use clientID or workloadIdentity")
		}
	}

	if _, _, err := racerobject.ParseAzureContainerURL(origin.Azure.ContainerURL, credential); err != nil {
		return fmt.Errorf("spec.origin.azure.containerURL: %w", err)
	}

	if origin.MetadataTTL != nil && origin.MetadataTTL.Duration < 0 {
		return errors.New("spec.origin.metadataTTL must not be negative")
	}

	if origin.RequestTimeout != nil && origin.RequestTimeout.Duration <= 0 {
		return errors.New("spec.origin.requestTimeout must be positive")
	}

	if origin.MaxConcurrentRequests != nil {
		if n := *origin.MaxConcurrentRequests; n < 1 || n > originMaxRequests {
			return fmt.Errorf("spec.origin.maxConcurrentRequests must be between 1 and %d", originMaxRequests)
		}
	}

	return nil
}

// planOrigins returns the ClusterCaches whose spec.origin is invalid, for
// status.
func planOrigins(ctx context.Context, env *component.Env) (invalid []string, err error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches); err != nil {
		return nil, err
	}

	slices.SortFunc(caches.Items, func(a, b racerv1.ClusterCache) int { return strings.Compare(a.Name, b.Name) })

	for i := range caches.Items {
		cache := &caches.Items[i]
		if cache.Spec.Origin == nil || cache.DeletionTimestamp != nil {
			continue
		}

		if err := validateOrigin(cache); err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (%v)", cache.Name, err))
		}
	}

	return invalid, nil
}
