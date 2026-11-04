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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// nativeOriginAnnotation marks a dataplane Pod template that reads
	// ClusterCache origins in-process. Legacy origin DaemonSets are pruned
	// only once every dataplane Pod runs such a template.
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
// status, and prunes every origin DaemonSet and ServiceAccount left by
// earlier operator versions. Until the dataplane has rolled out its
// native-origin revision, the leftovers are kept, because nodes still on the
// old dataplane read through their origin sockets; waiting is then true.
func planOrigins(ctx context.Context, env *component.Env, plan *component.Plan) (invalid []string, waiting bool, err error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches); err != nil {
		return nil, false, err
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

	waiting, err = pruneOrigins(ctx, env, plan)
	if err != nil {
		return nil, false, err
	}

	return invalid, waiting, nil
}

// nativeOriginRolledOut reports whether the live dataplane DaemonSet runs the
// native-origin template on every scheduled node. A missing DaemonSet is not
// rolled out: it is created in this pass, and a pre-rename dataplane may still
// read through origin sockets until it is replaced.
func nativeOriginRolledOut(ctx context.Context, env *component.Env) (bool, error) {
	ds := &appsv1.DaemonSet{}

	err := env.LiveReader().Get(ctx, objectKey(env, dataplaneName), ds)
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if ds.DeletionTimestamp != nil || ds.Spec.Template.Annotations[nativeOriginAnnotation] != "true" {
		return false, nil
	}

	st := ds.Status
	if st.ObservedGeneration < ds.Generation ||
		st.UpdatedNumberScheduled != st.DesiredNumberScheduled ||
		st.NumberAvailable != st.DesiredNumberScheduled ||
		st.NumberUnavailable != 0 {
		return false, nil
	}

	// The counters alone are not enough: during a surge rollout a ready old
	// Pod and an unready replacement on the same node can satisfy all of
	// them. Require every live Pod the DaemonSet owns to run the
	// native-origin template and be Ready.
	return dataplanePodsNative(ctx, env, ds)
}

// dataplanePodsNative reports whether every non-terminating Pod owned by ds
// carries the native-origin annotation and is Ready.
func dataplanePodsNative(ctx context.Context, env *component.Env, ds *appsv1.DaemonSet) (bool, error) {
	if ds.Spec.Selector == nil {
		return false, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return false, err
	}

	pods := &corev1.PodList{}
	if err := env.LiveReader().List(ctx, pods, client.InNamespace(ds.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, err
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedBy(pod, ds) {
			continue
		}

		if pod.Annotations[nativeOriginAnnotation] != "true" {
			// An old-revision Pod may still serve origin reads, even while
			// terminating.
			return false, nil
		}

		if pod.DeletionTimestamp != nil {
			continue
		}

		if !podReady(pod) {
			return false, nil
		}
	}

	return true, nil
}

func ownedBy(pod *corev1.Pod, ds *appsv1.DaemonSet) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.UID == ds.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}

	return false
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// pruneOrigins deletes the origin DaemonSets and ServiceAccounts that this
// operator created before the dataplane read origins in-process. Objects it
// does not manage are left alone. Pruning waits for the native-origin
// dataplane rollout; waiting reports that leftovers remain for that reason.
func pruneOrigins(ctx context.Context, env *component.Env, plan *component.Plan) (waiting bool, err error) {
	opts := []client.ListOption{client.InNamespace(env.Namespace), client.MatchingLabels{"app.kubernetes.io/component": originAppName}}

	daemonSets := &appsv1.DaemonSetList{}
	if err := env.LiveReader().List(ctx, daemonSets, opts...); err != nil {
		return false, err
	}

	accounts := &corev1.ServiceAccountList{}
	if err := env.LiveReader().List(ctx, accounts, opts...); err != nil {
		return false, err
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

	var prunable []client.Object

	for _, obj := range stale {
		if !strings.HasPrefix(obj.GetName(), originPrefix) {
			continue
		}

		ok, err := adoptable(ctx, env, obj)
		if err != nil {
			return false, err
		}

		if ok {
			prunable = append(prunable, obj)
		}
	}

	if len(prunable) == 0 {
		return false, nil
	}

	// Leftovers for deleted caches wait too: an old dataplane Pod may still
	// hold the cache open until it is replaced.
	done, err := nativeOriginRolledOut(ctx, env)
	if err != nil {
		return false, err
	}

	if !done {
		return true, nil
	}

	for _, obj := range prunable {
		op := component.DeleteOperation(obj, name, "")
		op.FailureDomain = "racer-origin/" + strings.TrimPrefix(obj.GetName(), originPrefix)
		plan.Add(op)
	}

	return false, nil
}
