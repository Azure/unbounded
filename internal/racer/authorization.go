// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func authorizationError(err error) error {
	if apierrors.IsNotFound(err) {
		return wire.Forbidden
	}

	return wire.Unavailable
}

func authorizedNode(node *corev1.Node) bool {
	_, excluded := node.Labels[wire.ExclusionLabel]
	return node.Name != "" && wire.ValidUUID(string(node.UID)) && node.DeletionTimestamp == nil && !excluded
}

const (
	nodeUIDIndex          = "racer.authorization.nodeUID"
	authorizationPodIndex = "racer.authorization.podNode"
	// Allow rollout overlap without letting arbitrary candidate counts amplify
	// live reads. Excess candidates fail closed until the informer converges.
	maxAuthorizationPods = 4
)

func nodeUIDKeys(obj client.Object) []string {
	if obj.GetUID() == "" {
		return nil
	}

	return []string{string(obj.GetUID())}
}

func authorizationPodKeys(cfg Config) client.IndexerFunc {
	return func(obj client.Object) []string {
		pod, ok := obj.(*corev1.Pod)
		if !ok || !authorizedPodCandidate(cfg, pod) {
			return nil
		}

		return []string{pod.Spec.NodeName}
	}
}

func authorizedPodCandidate(cfg Config, pod *corev1.Pod) bool {
	if pod.Namespace != cfg.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || pod.Spec.ServiceAccountName != cfg.DataplaneServiceAccount || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}

	owner := metav1.GetControllerOf(pod)

	return owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && owner.Name == cfg.DaemonSetName && owner.UID != ""
}

// The configured namespace/name designate the managed workload. A Pod must be
// controlled by that exact current DaemonSet UID, not just carry matching labels.
func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, serviceAccountUID string) error {
	if !authorizedPodCandidate(cfg, pod) {
		return wire.Forbidden
	}

	owner := metav1.GetControllerOf(pod)

	var ds appsv1.DaemonSet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.DaemonSetName}, &ds); err != nil {
		return authorizationError(err)
	}

	if ds.UID != owner.UID || ds.DeletionTimestamp != nil {
		return wire.Forbidden
	}

	var sa corev1.ServiceAccount
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.DataplaneServiceAccount}, &sa); err != nil {
		return authorizationError(err)
	}

	if sa.UID == "" || sa.DeletionTimestamp != nil || serviceAccountUID != "" && string(sa.UID) != serviceAccountUID {
		return wire.Forbidden
	}

	return ctx.Err()
}

func authorizeNode(ctx context.Context, reader, hints client.Reader, cfg Config, id wire.NodeID) error {
	// Indexes discover names only. No positive authorization is cached, and no
	// cache miss falls back to a live list. Discovery uncertainty is retryable:
	// denying service until convergence must not terminate the client's worker.
	// Every positive security fact below still comes from live GETs.
	if hints == nil || reader == nil {
		return wire.Unavailable
	}

	var nodes corev1.NodeList
	if err := hints.List(ctx, &nodes, client.MatchingFields{nodeUIDIndex: string(id)}, client.Limit(2)); err != nil {
		return wire.Unavailable
	}

	if len(nodes.Items) != 1 || nodes.Items[0].Name == "" || wire.NodeID(nodes.Items[0].UID) != id {
		return wire.Unavailable
	}

	var node corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: nodes.Items[0].Name}, &node); err != nil {
		return authorizationError(err)
	}

	if wire.NodeID(node.UID) != id || !authorizedNode(&node) {
		return wire.Forbidden
	}

	var pods corev1.PodList
	if err := hints.List(ctx, &pods, client.InNamespace(cfg.Namespace), client.MatchingFields{authorizationPodIndex: node.Name}, client.Limit(maxAuthorizationPods+1)); err != nil {
		return wire.Unavailable
	}

	if len(pods.Items) > maxAuthorizationPods {
		return wire.Unavailable
	}

	for j := range pods.Items {
		hint := &pods.Items[j]
		if hint.Namespace != cfg.Namespace || hint.Name == "" || hint.UID == "" {
			continue
		}

		var pod corev1.Pod
		if err := reader.Get(ctx, client.ObjectKeyFromObject(hint), &pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return authorizationError(err)
		}

		if pod.UID != hint.UID || pod.Spec.NodeName != node.Name {
			continue
		}

		err := authorizePod(ctx, reader, cfg, &pod, "")
		if err == nil {
			return nil
		}

		if err != wire.Forbidden {
			return err
		}
	}

	// Even live rejection of every hinted Pod (including its workload checks)
	// cannot establish that discovery includes every replacement candidate.
	// Keep denying bytes, but allow retry with the same Node certificate. Only
	// the live Node checks above establish a terminal identity denial here;
	// bootstrap's directly bound Pod authorization retains its own live errors.
	return wire.Unavailable
}
