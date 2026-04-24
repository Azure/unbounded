// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import "github.com/prometheus/client_golang/prometheus"

var (
	metricPeers = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "healthcheck",
			Name:      "peers",
			Help:      "Number of health check peers by state.",
		},
		[]string{"state"},
	)

	metricSessionFlaps = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "healthcheck",
			Name:      "session_flaps_total",
			Help:      "Total number of health check state transitions.",
		},
	)

	metricRTT = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "healthcheck",
			Name:      "rtt_seconds",
			Help:      "Round-trip time distribution for health check packets.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		},
	)

	metricPacketsSent = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "healthcheck",
			Name:      "packets_sent_total",
			Help:      "Total number of health check packets sent.",
		},
	)

	metricPacketsReceived = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "healthcheck",
			Name:      "packets_received_total",
			Help:      "Total number of health check packets received.",
		},
	)

	metricPacketsTimeout = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "healthcheck",
			Name:      "packets_timeout_total",
			Help:      "Total number of health check packet timeouts.",
		},
	)
)

func init() {
	prometheus.MustRegister(
		metricPeers,
		metricSessionFlaps,
		metricRTT,
		metricPacketsSent,
		metricPacketsReceived,
		metricPacketsTimeout,
	)
}

// updatePeerGauges recalculates the peer gauge from a snapshot of session states.
func updatePeerGauges(sessions map[string]*session) {
	counts := map[SessionState]float64{
		StateDown:      0,
		StateUp:        0,
		StateAdminDown: 0,
	}

	for _, s := range sessions {
		s.mu.Lock()
		counts[s.state]++
		s.mu.Unlock()
	}

	for state, count := range counts {
		metricPeers.WithLabelValues(state.String()).Set(count)
	}
}
