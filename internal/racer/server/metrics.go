// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var servingTransportMetrics = newTransportMetrics(metrics.Registry)

// Collectors aggregate all Racer listeners in this process. Tests use their own
// registry and collectors, without replacing or resetting the shared registry.
type transportMetrics struct {
	connections        prometheus.Gauge
	handshakes         prometheus.Gauge
	connectionRejected prometheus.Counter
	handshakeRejected  prometheus.Counter
	handshakeTimeouts  prometheus.Counter
}

func newTransportMetrics(reg prometheus.Registerer) *transportMetrics {
	connections := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "racer_server_connections",
		Help: "Admitted sockets from connection admission until raw close, including TLS handshakes, HTTP requests, and idle connections.",
	})
	handshakes := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "racer_server_tls_handshakes",
		Help: "Full TLS handshake slots held from admission before reading TLS bytes until HandshakeContext returns, not just TLS configuration callbacks.",
	})
	rejected := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "racer_server_admission_rejections_total",
		Help: "Sockets rejected by the first exhausted admission stage: connection or full_handshake. Excludes shutdown and TLS errors.",
	}, []string{"stage"})
	timeouts := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "racer_server_tls_handshake_timeouts_total",
		Help: "Failed full TLS handshakes returning a context deadline or transport timeout error, counted once. Excludes HTTP timeouts and cancellation.",
	})
	reg.MustRegister(connections, handshakes, rejected, timeouts)

	return &transportMetrics{
		connections: connections, handshakes: handshakes,
		connectionRejected: rejected.WithLabelValues("connection"),
		handshakeRejected:  rejected.WithLabelValues("full_handshake"),
		handshakeTimeouts:  timeouts,
	}
}
