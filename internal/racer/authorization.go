// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"slices"

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

func authorizedPodCandidate(cfg Config, pod *corev1.Pod) bool {
	if pod.Namespace != cfg.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || pod.Spec.ServiceAccountName != cfg.DataplaneServiceAccount || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}

	owner := metav1.GetControllerOf(pod)

	return owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && slices.Contains(managedWorkloadNames(cfg), owner.Name) && owner.UID != ""
}

// The configured namespace/name designate the managed workload. A Pod must be
// controlled by that exact current DaemonSet UID, not just carry matching labels.
func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, serviceAccountUID string) error {
	if !authorizedPodCandidate(cfg, pod) {
		return wire.Forbidden
	}

	owns, err := readManagedWorkloadOwnership(ctx, reader, cfg)
	if err != nil {
		return authorizationError(err)
	}

	if !owns(pod) {
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
