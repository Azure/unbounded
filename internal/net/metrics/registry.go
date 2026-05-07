// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metrics provides shared Prometheus metrics helpers for the
// unbounded-net controller and node agent.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the default Prometheus HTTP handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	)
}

// Register adds the /metrics endpoint to the given ServeMux.
func Register(mux *http.ServeMux) {
	mux.Handle("/metrics", Handler())
}
