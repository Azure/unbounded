// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DataplaneDaemonSetName  = "racer-dataplane"
	PodNetworkDaemonSetName = "racer-dataplane-podnet"
)

// DataplaneWorkloadIdentities is a bounded snapshot, never a label-derived or
// caller-configurable owner allowlist. Refresh it for each authorization pass.
type DataplaneWorkloadIdentities struct {
	namespace string
	hostUID   types.UID
	podUID    types.UID
}

// ReadDataplaneWorkloadIdentities requires at most two exact-name reads. Missing
// or deleting workloads authorize no Pods; any other read error fails closed.
func ReadDataplaneWorkloadIdentities(ctx context.Context, reader client.Reader, namespace string) (DataplaneWorkloadIdentities, error) {
	ids := DataplaneWorkloadIdentities{namespace: namespace}
	for _, group := range []struct {
		name string
		uid  *types.UID
	}{{DataplaneDaemonSetName, &ids.hostUID}, {PodNetworkDaemonSetName, &ids.podUID}} {
		var ds appsv1.DaemonSet
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group.name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return DataplaneWorkloadIdentities{}, err
		}

		if ds.DeletionTimestamp == nil {
			*group.uid = ds.UID
		}
	}

	return ids, nil
}

// Owns checks ownership only. Callers must retain their existing Pod, Node,
// service-account, token and readiness-independent membership checks.
func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool {
	if pod == nil || pod.Namespace != ids.namespace {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return false
	}

	switch owner.Name {
	case DataplaneDaemonSetName:
		return owner.UID == ids.hostUID
	case PodNetworkDaemonSetName:
		return owner.UID == ids.podUID
	default:
		return false
	}
}
