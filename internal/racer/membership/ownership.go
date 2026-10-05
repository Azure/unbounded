// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package membership

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// WorkloadIdentity is an observed live DaemonSet identity, not a label selector.
// An absent or terminating workload must be supplied with an empty UID.
type WorkloadIdentity struct {
	Name string
	UID  types.UID
}

// WorkloadIdentities is a bounded value snapshot supplied by the caller, which
// remains responsible for refreshing live identities on every topology or
// authorization pass. This package performs no API reads.
type WorkloadIdentities struct {
	Namespace string
	Workloads [2]WorkloadIdentity
}

// Owns checks ownership only. Callers retain their Pod, Node, service-account,
// token and readiness-independent membership checks.
func (ids WorkloadIdentities) Owns(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return false
	}

	if pod.Namespace != ids.Namespace {
		return false
	}

	for _, workload := range ids.Workloads {
		if owner.Name == workload.Name && owner.UID == workload.UID {
			return true
		}
	}

	return false
}
