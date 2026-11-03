// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerobject"
)

// The Racer dataplane reads ClusterCache spec.origin itself. Earlier operator
// versions ran one racer-object origin DaemonSet and ServiceAccount per cache,
// named racer-origin-<cache> and labeled with the component below; they are
// only pruned now.
const (
	originPrefix      = "racer-origin-"
	originAppName     = "racer-origin"
	originMaxRequests = 65536
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
// status, and prunes every origin DaemonSet and ServiceAccount left by
// earlier operator versions.
func planOrigins(ctx context.Context, env *component.Env, plan *component.Plan) (invalid []string, err error) {
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

	if err := pruneOrigins(ctx, env, plan); err != nil {
		return nil, err
	}

	return invalid, nil
}

// pruneOrigins deletes the origin DaemonSets and ServiceAccounts that this
// operator created before the dataplane read origins in-process. Objects it
// does not manage are left alone.
func pruneOrigins(ctx context.Context, env *component.Env, plan *component.Plan) error {
	opts := []client.ListOption{client.InNamespace(env.Namespace), client.MatchingLabels{"app.kubernetes.io/component": originAppName}}

	daemonSets := &appsv1.DaemonSetList{}
	if err := env.LiveReader().List(ctx, daemonSets, opts...); err != nil {
		return err
	}

	accounts := &corev1.ServiceAccountList{}
	if err := env.LiveReader().List(ctx, accounts, opts...); err != nil {
		return err
	}

	var stale []client.Object

	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		ds.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}
		stale = append(stale, ds)
	}

	for i := range accounts.Items {
		sa := &accounts.Items[i]
		sa.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
		stale = append(stale, sa)
	}

	for _, obj := range stale {
		if !strings.HasPrefix(obj.GetName(), originPrefix) {
			continue
		}

		ok, err := adoptable(ctx, env, obj)
		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		op := component.DeleteOperation(obj, name, "")
		op.FailureDomain = "racer-origin/" + strings.TrimPrefix(obj.GetName(), originPrefix)
		plan.Add(op)
	}

	return nil
}
