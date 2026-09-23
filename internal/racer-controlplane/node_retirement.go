// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

// Multiple authenticated admissions for one Pod cannot tell us which boot is
// dead: the boot nonce has no authoritative binding to kubelet container status.
// Replace the entire managed Pod instead. This also stops concurrent live boots,
// and the DaemonSet supplies a new Pod UID. Keep every admission until a later
// authoritative list observes the old UID absent, including throughout graceful
// termination. Neither deletion success nor a termination timestamp retires it.
func (s *tlsControl) replaceRestartedNode(ctx context.Context, members []pki.Identity, live map[string]*corev1.Pod) error {
	boots := make(map[string]int)

	for _, member := range members {
		if member.Kind != pki.Node {
			continue
		}

		boots[member.PodUID]++
		if boots[member.PodUID] != 2 {
			continue
		}

		pod := live[member.PodUID]
		if pod == nil || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != "racer-dataplane" {
			continue
		}

		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
			continue
		}

		var daemon appsv1.DaemonSet
		if err := s.kube.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &daemon); err != nil {
			return err
		}

		if daemon.UID != owner.UID || daemon.DeletionTimestamp != nil || daemon.Labels[racer.MetadataPrefix+"component"] != "racer-dataplane" || daemon.Spec.Template.Spec.ServiceAccountName != "racer-dataplane" {
			continue
		}

		// Preserve Kubernetes' graceful deletion default. Preconditions protect
		// a same-name replacement and ownership changes after the direct list.
		if err := s.kube.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); err != nil {
			return err
		}

		log.Printf("replacing dataplane Pod %s/%s UID %s with multiple enrolled boots", pod.Namespace, pod.Name, pod.UID)

		// Initiate at most one replacement per reconciliation.
		return nil
	}

	return nil
}
