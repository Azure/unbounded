// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metrics

import (
	"context"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	clientmetrics "k8s.io/client-go/tools/metrics"
	"k8s.io/client-go/util/workqueue"
)

// RegisterClientGoMetrics wires up the Kubernetes client-go REST client and
// workqueue metrics so they are exposed via the Prometheus default registry.
// Call this before creating any Kubernetes clients or workqueues.
func RegisterClientGoMetrics() {
	registerRESTClientMetrics()
	registerWorkqueueMetrics()
}

// --- REST client adapter ---

var (
	requestLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_request_duration_seconds",
		Help:    "Request latency in seconds by verb and URL.",
		Buckets: prometheus.DefBuckets,
	}, []string{"verb", "url"})

	requestResult = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rest_client_requests_total",
		Help: "Number of HTTP requests partitioned by status code, method, and host.",
	}, []string{"code", "method", "host"})
)

func registerRESTClientMetrics() {
	clientmetrics.Register(clientmetrics.RegisterOpts{
		RequestLatency: &restLatencyAdapter{},
		RequestResult:  &restResultAdapter{},
	})
}

type restLatencyAdapter struct{}

func (r *restLatencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	requestLatency.WithLabelValues(verb, u.String()).Observe(latency.Seconds())
}

type restResultAdapter struct{}

func (r *restResultAdapter) Increment(_ context.Context, code, method, host string) {
	requestResult.WithLabelValues(code, method, host).Inc()
}

// --- Workqueue adapter ---

var (
	wqDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "workqueue",
		Name:      "depth",
		Help:      "Current depth of workqueue.",
	}, []string{"name"})

	wqAdds = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "workqueue",
		Name:      "adds_total",
		Help:      "Total number of adds handled by workqueue.",
	}, []string{"name"})

	wqLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "workqueue",
		Name:      "queue_duration_seconds",
		Help:      "How long in seconds an item stays in workqueue before being requested.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"name"})

	wqWorkDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "workqueue",
		Name:      "work_duration_seconds",
		Help:      "How long in seconds processing an item from workqueue takes.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"name"})

	wqUnfinished = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "workqueue",
		Name:      "unfinished_work_seconds",
		Help:      "How many seconds of work has done that is in progress and hasn't been observed by work_duration.",
	}, []string{"name"})

	wqLongestRunning = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "workqueue",
		Name:      "longest_running_processor_seconds",
		Help:      "How many seconds has the longest running processor for workqueue been running.",
	}, []string{"name"})

	wqRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "workqueue",
		Name:      "retries_total",
		Help:      "Total number of retries handled by workqueue.",
	}, []string{"name"})
)

func registerWorkqueueMetrics() {
	workqueue.SetProvider(&wqProvider{})
}

type wqProvider struct{}

func (p *wqProvider) NewDepthMetric(name string) workqueue.GaugeMetric {
	return wqDepth.WithLabelValues(name)
}

func (p *wqProvider) NewAddsMetric(name string) workqueue.CounterMetric {
	return wqAdds.WithLabelValues(name)
}

func (p *wqProvider) NewLatencyMetric(name string) workqueue.HistogramMetric {
	return wqLatency.WithLabelValues(name)
}

func (p *wqProvider) NewWorkDurationMetric(name string) workqueue.HistogramMetric {
	return wqWorkDuration.WithLabelValues(name)
}

func (p *wqProvider) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return wqUnfinished.WithLabelValues(name)
}

func (p *wqProvider) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return wqLongestRunning.WithLabelValues(name)
}

func (p *wqProvider) NewRetriesMetric(name string) workqueue.CounterMetric {
	return wqRetries.WithLabelValues(name)
}
