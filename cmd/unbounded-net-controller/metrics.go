// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/Azure/unbounded/internal/net/metrics"
)

const controllerMetricsNamespace = "unbounded_cni_controller"

// Status/push metrics.
var (
	nodeStatusPushesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: controllerMetricsNamespace,
		Name:      "node_status_pushes_total",
		Help:      "Total node status pushes by method and result.",
	}, []string{"method", metrics.LabelResult})

	nodeStatusPushDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: controllerMetricsNamespace,
		Name:      "node_status_push_duration_seconds",
		Help:      "Duration of node status push processing in seconds.",
		Buckets:   metrics.DefaultDurationBuckets,
	}, []string{"method"})

	websocketConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: controllerMetricsNamespace,
		Name:      "websocket_connections",
		Help:      "Current number of active node WebSocket connections.",
	})

	leaderElectionTransitions = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: controllerMetricsNamespace,
		Name:      "leader_election_transitions_total",
		Help:      "Total number of leader election transitions.",
	})

	leaderIsLeader = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: controllerMetricsNamespace,
		Name:      "leader_is_leader",
		Help:      "Whether this instance is the current leader (1=leader, 0=not leader).",
	})
)
