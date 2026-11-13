// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	failedAllocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "unbounded_karpenter",
		Name:      "failed_allocations_total",
		Help:      "NodeClaims deleted because their MachineAllocation could not become a Node, by reason.",
	}, []string{"reason"})

	orphans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "unbounded_karpenter",
		Name:      "orphaned_allocation_reconciles_total",
		Help:      "Reconciles of a fenced MachineAllocation whose NodeClaim is gone, by outcome.",
	}, []string{"outcome"})
)

func init() {
	metrics.Registry.MustRegister(failedAllocations, orphans)
}
