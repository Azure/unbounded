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

// The configured namespace/name designate the managed workload. A Pod must be
// controlled by that exact current DaemonSet UID, not just carry matching labels.
func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, serviceAccountUID string) error {
	if pod.Namespace != cfg.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || pod.Spec.ServiceAccountName != cfg.DataplaneServiceAccount || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return wire.Forbidden
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.Name != cfg.DaemonSetName || owner.UID == "" {
		return wire.Forbidden
	}

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

const nodeUIDIndex = "racer.nodeUID"

func nodeUIDKeys(obj client.Object) []string {
	if obj.GetUID() == "" {
		return nil
	}

	return []string{string(obj.GetUID())}
}

func authorizeNode(ctx context.Context, reader, hints client.Reader, cfg Config, id wire.NodeID) error {
	// The informer is only a UID-to-name hint. Never authorize from its labels,
	// deletion state, or UID: a live GET must still match the certificate UID.
	if hints == nil {
		return wire.Unavailable
	}

	var nodes corev1.NodeList
	if err := hints.List(ctx, &nodes, client.MatchingFields{nodeUIDIndex: string(id)}); err != nil {
		return wire.Unavailable
	}

	// A missing hint may be informer lag, so let the client retry. Never fall
	// back to a full API list during cold startup or an informer outage.
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
	if err := reader.List(ctx, &pods, client.InNamespace(cfg.Namespace), client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return authorizationError(err)
	}

	for j := range pods.Items {
		pod := &pods.Items[j]
		if pod.Spec.NodeName != node.Name {
			continue
		}

		err := authorizePod(ctx, reader, cfg, pod, "")
		if err == nil {
			return nil
		}

		if err != wire.Forbidden {
			return err
		}
	}

	return wire.Forbidden
}
