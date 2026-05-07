// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/Azure/unbounded/internal/net/metrics"
)

const controllerNamespace = "unbounded_cni_controller"

// Reconciliation metrics -- instrumented in each controller's processNextWorkItem.
var (
	reconciliationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: controllerNamespace,
		Name:      "reconciliation_duration_seconds",
		Help:      "Duration of a single reconciliation in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{metrics.LabelController})

	reconciliationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "reconciliation_total",
		Help:      "Total number of reconciliations by controller and result.",
	}, []string{metrics.LabelController, metrics.LabelResult})

	reconciliationErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "reconciliation_errors_total",
		Help:      "Total reconciliation errors by controller.",
	}, []string{metrics.LabelController})

	workqueueRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "workqueue_retries_total",
		Help:      "Total number of workqueue requeues by controller.",
	}, []string{metrics.LabelController})
)

// Resource state metrics -- set after each successful reconciliation.
var (
	SiteNodesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: controllerNamespace,
		Name:      "site_nodes_total",
		Help:      "Number of nodes in each site.",
	}, []string{"site"})

	SiteNodeSlicesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: controllerNamespace,
		Name:      "site_node_slices_total",
		Help:      "Number of SiteNodeSlice objects per site.",
	}, []string{"site"})

	PodCIDRAllocations = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "pod_cidr_allocations_total",
		Help:      "Total pod CIDR allocations.",
	})

	PodCIDRReleases = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "pod_cidr_releases_total",
		Help:      "Total pod CIDR releases.",
	})

	PodCIDRExhaustion = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "pod_cidr_exhaustion_total",
		Help:      "Total pod CIDR pool exhaustion events.",
	})

	GatewayPoolNodesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: controllerNamespace,
		Name:      "gateway_pool_nodes_total",
		Help:      "Number of nodes in each gateway pool.",
	}, []string{"pool"})

	GatewayPoolReachableSitesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: controllerNamespace,
		Name:      "gateway_pool_reachable_sites_total",
		Help:      "Number of reachable sites per gateway pool.",
	}, []string{"pool"})

	GatewayPoolRoutesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: controllerNamespace,
		Name:      "gateway_pool_routes_total",
		Help:      "Number of routes per gateway pool.",
	}, []string{"pool"})
)

// CRD management metrics.
var (
	CRDEnsureDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: controllerNamespace,
		Name:      "crd_ensure_duration_seconds",
		Help:      "Duration of CRD ensure operations in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"crd"})

	CRDEnsureErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerNamespace,
		Name:      "crd_ensure_errors_total",
		Help:      "Total CRD ensure errors by CRD name.",
	}, []string{"crd"})
)
