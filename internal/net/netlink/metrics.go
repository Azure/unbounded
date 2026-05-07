// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/Azure/unbounded/internal/net/metrics"
)

const nodeNamespace = "unbounded_cni_node"

// WireGuard metrics.
var (
	WireGuardPeers = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: nodeNamespace,
		Name:      "wireguard_peers",
		Help:      "Current number of WireGuard peers by interface.",
	}, []string{"interface"})

	WireGuardConfigureDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: nodeNamespace,
		Name:      "wireguard_configure_duration_seconds",
		Help:      "Duration of WireGuard peer configuration in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"interface"})

	WireGuardConfigureErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "wireguard_configure_errors_total",
		Help:      "Total WireGuard configuration errors.",
	}, []string{"interface"})

	WireGuardPeersAdded = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "wireguard_peers_added_total",
		Help:      "Total WireGuard peers added.",
	})

	WireGuardPeersRemoved = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "wireguard_peers_removed_total",
		Help:      "Total WireGuard peers removed.",
	})
)

// Route metrics.
var (
	RoutesInstalled = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: nodeNamespace,
		Name:      "routes_installed",
		Help:      "Current number of installed routes by table.",
	}, []string{"table"})

	RouteSyncDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: nodeNamespace,
		Name:      "route_sync_duration_seconds",
		Help:      "Duration of route sync operations in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"source"})

	RouteSyncErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "route_sync_errors_total",
		Help:      "Total route sync errors.",
	}, []string{"source"})

	RoutesAdded = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "routes_added_total",
		Help:      "Total routes added.",
	}, []string{"source"})

	RoutesRemoved = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "routes_removed_total",
		Help:      "Total routes removed.",
	}, []string{"source"})

	ECMPRoutesInstalled = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: nodeNamespace,
		Name:      "ecmp_routes_installed",
		Help:      "Current number of installed ECMP routes.",
	})
)

// Masquerade metrics.
var (
	MasqueradeRules = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: nodeNamespace,
		Name:      "masquerade_rules",
		Help:      "Current number of masquerade rules by address family.",
	}, []string{"family"})

	MasqueradeSyncDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: nodeNamespace,
		Name:      "masquerade_sync_duration_seconds",
		Help:      "Duration of masquerade rule sync in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	})

	MasqueradeSyncErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "masquerade_sync_errors_total",
		Help:      "Total masquerade sync errors.",
	})

	MasqueradeChainRebuilds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "masquerade_chain_rebuilds_total",
		Help:      "Total masquerade chain full rebuilds.",
	})
)

// Link/interface metrics.
var (
	Interfaces = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: nodeNamespace,
		Name:      "interfaces",
		Help:      "Current number of managed interfaces by type.",
	}, []string{"type"})

	InterfaceOperations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "interface_operations_total",
		Help:      "Total interface operations by type.",
	}, []string{"operation"})

	InterfaceOperationErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: nodeNamespace,
		Name:      "interface_operation_errors_total",
		Help:      "Total interface operation errors by type.",
	}, []string{"operation"})
)
