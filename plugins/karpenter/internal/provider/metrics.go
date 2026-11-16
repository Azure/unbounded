// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	unusableOfferings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "unbounded_karpenter",
		Name:      "unusable_offerings",
		Help:      "MachineClass offerings Karpenter cannot use, as of the last instance type listing.",
	}, []string{"machineclass"})

	estimatedPriceOfferings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "unbounded_karpenter",
		Name:      "estimated_price_offerings",
		Help:      "MachineClass offerings whose price is an estimate, as of the last instance type listing.",
	}, []string{"machineclass"})
)

func init() {
	metrics.Registry.MustRegister(unusableOfferings, estimatedPriceOfferings)
}
