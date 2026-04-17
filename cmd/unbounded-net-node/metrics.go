// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/Azure/unbounded-kube/internal/net/metrics"
)

const nodeMetricsNamespace = "unbounded_cni_node"

// Reconciliation metrics.
var (
	nodeReconciliationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "reconciliation_duration_seconds",
		Help:      "Duration of a full node reconciliation loop in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	})

	nodeReconciliationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "reconciliation_total",
		Help:      "Total node reconciliations by result.",
	}, []string{metrics.LabelResult})
)

// Status push metrics.
var (
	nodeStatusPushTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "status_push_total",
		Help:      "Total node status pushes by method and result.",
	}, []string{"method", metrics.LabelResult})

	nodeStatusPushDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "status_push_duration_seconds",
		Help:      "Duration of node status push in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"method"})

	nodeStatusPushBytes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "status_push_bytes",
		Help:      "Size of node status push payloads in bytes.",
		Buckets:   metrics.DefaultSizeBuckets,
	}, []string{"method", "encoding"})
)

// CNI config metrics.
var (
	nodeCNIConfigWrites = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "cni_config_writes_total",
		Help:      "Total CNI config write attempts by result.",
	}, []string{metrics.LabelResult})

	nodeCNIConfigWriteDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: nodeMetricsNamespace,
		Name:      "cni_config_write_duration_seconds",
		Help:      "Duration of CNI config writes in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	})
)
