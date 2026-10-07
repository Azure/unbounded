// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestTransportMetricsRegistryIsolation(t *testing.T) {
	first, second := prometheus.NewPedanticRegistry(), prometheus.NewPedanticRegistry()
	m := newTransportMetrics(first)
	newTransportMetrics(second)
	m.connections.Inc()
	m.handshakes.Inc()
	m.connectionRejected.Inc()
	m.handshakeRejected.Add(2)
	m.handshakeTimeouts.Inc()

	require.NoError(t, testutil.GatherAndCompare(first, strings.NewReader(`
# HELP racer_server_admission_rejections_total Sockets rejected by the first exhausted admission stage: connection or full_handshake. Excludes shutdown and TLS errors.
# TYPE racer_server_admission_rejections_total counter
racer_server_admission_rejections_total{stage="connection"} 1
racer_server_admission_rejections_total{stage="full_handshake"} 2
# HELP racer_server_connections Admitted sockets from connection admission until raw close, including TLS handshakes, HTTP requests, and idle connections.
# TYPE racer_server_connections gauge
racer_server_connections 1
# HELP racer_server_tls_handshake_timeouts_total Failed full TLS handshakes returning a context deadline or transport timeout error, counted once. Excludes HTTP timeouts and cancellation.
# TYPE racer_server_tls_handshake_timeouts_total counter
racer_server_tls_handshake_timeouts_total 1
# HELP racer_server_tls_handshakes Full TLS handshake slots held from admission before reading TLS bytes until HandshakeContext returns, not just TLS configuration callbacks.
# TYPE racer_server_tls_handshakes gauge
racer_server_tls_handshakes 1
`)))
	// Both rejection stages are present at zero before any traffic. The other
	// collectors have no labels; no peer, identity, path, or error text is used.
	families, err := second.Gather()
	require.NoError(t, err)
	require.Len(t, families, 4)

	for _, family := range families {
		for _, sample := range family.GetMetric() {
			require.Zero(t, sample.GetGauge().GetValue())
			require.Zero(t, sample.GetCounter().GetValue())
		}
	}

	// Production registers once with the controller-runtime metrics endpoint.
	count, err := testutil.GatherAndCount(metrics.Registry,
		"racer_server_connections", "racer_server_tls_handshakes",
		"racer_server_admission_rejections_total", "racer_server_tls_handshake_timeouts_total")
	require.NoError(t, err)
	require.Equal(t, 5, count)
}

func TestTransportMetricsShutdownAcceptance(t *testing.T) {
	for _, capacity := range []int{0, 1} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			accepted, peer := net.Pipe()
			defer accepted.Close()
			defer peer.Close()

			listener := teardownListener{close: func() error { return nil }, accept: func() (net.Conn, error) {
				// Accept can return a socket while shutdown is already in progress.
				cancel()
				return accepted, nil
			}}

			l := newTransportListenerWithMetrics(ctx, listener, &tls.Config{}, Limits{MaxConnections: capacity}, newTransportMetrics(prometheus.NewPedanticRegistry()))
			defer l.Close()

			<-l.acceptDone
			synctest.Wait()

			assertTransportMetrics(t, l, 0, 0, 0, 0, 0)
			require.Empty(t, l.connections)
			require.Empty(t, l.handshakes)
		})
	}
}

func assertTransportMetrics(t *testing.T, l *transportListener, connections, handshakes, connectionRejected, handshakeRejected, timeouts float64) {
	t.Helper()

	// Slot changes and gauge changes are separate atomic operations. Wait for
	// both before asserting the counters at this lifecycle boundary.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(l.metrics.connections) == connections && testutil.ToFloat64(l.metrics.handshakes) == handshakes
	}, time.Second, time.Millisecond)
	require.Equal(t, connectionRejected, testutil.ToFloat64(l.metrics.connectionRejected))
	require.Equal(t, handshakeRejected, testutil.ToFloat64(l.metrics.handshakeRejected))
	require.Equal(t, timeouts, testutil.ToFloat64(l.metrics.handshakeTimeouts))
}
