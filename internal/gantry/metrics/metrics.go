// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metrics owns the Prometheus registry shared across Gantry
// subsystems and provides constructor helpers that record metric ownership
// so Phase 6's final audit can verify the §7.6 metric set is complete.
//
// Subsystems do NOT call prometheus.MustRegister directly; they go through
// (*Registry).NewCounter / NewGauge / NewHistogram so the ownership map is
// populated automatically.
package metrics

import (
	"net/http"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry wraps a *prometheus.Registry with an ownership map of metric
// name → subsystem.
type Registry struct {
	reg   *prometheus.Registry
	mu    sync.Mutex
	owner map[string]string
}

// New returns a Registry with an empty Prometheus registry. New() does not
// install any default Go/process collectors so tests don't see runtime
// metric noise; cmd/gantry wires those collectors explicitly via
// reg.RegisterDefaultCollectors().
func New() *Registry {
	return &Registry{
		reg:   prometheus.NewRegistry(),
		owner: map[string]string{},
	}
}

// RegisterDefaultCollectors adds the standard process and Go runtime
// collectors. Call from cmd/gantry, not from tests.
func (r *Registry) RegisterDefaultCollectors() {
	r.reg.MustRegister(collectors.NewGoCollector())
	r.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Handler returns an http.Handler serving /metrics.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// PrometheusRegistry exposes the underlying *prometheus.Registry for tests
// or for code that must register a third-party collector directly.
func (r *Registry) PrometheusRegistry() *prometheus.Registry { return r.reg }

// Owners returns the metric-name → subsystem map, sorted by name. Used by
// Phase 6's audit step to compare against the §7.6 inventory.
func (r *Registry) Owners() []NameOwner {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NameOwner, 0, len(r.owner))
	for n, s := range r.owner {
		out = append(out, NameOwner{Name: n, Subsystem: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NameOwner is one row of the ownership audit.
type NameOwner struct {
	Name      string
	Subsystem string
}

// NewCounter registers a Counter and records its ownership.
func (r *Registry) NewCounter(subsystem string, opts prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(opts)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(c)
	return c
}

// NewCounterVec registers a CounterVec and records its ownership.
func (r *Registry) NewCounterVec(subsystem string, opts prometheus.CounterOpts, labels []string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(opts, labels)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(c)
	return c
}

// NewGauge registers a Gauge and records its ownership.
func (r *Registry) NewGauge(subsystem string, opts prometheus.GaugeOpts) prometheus.Gauge {
	g := prometheus.NewGauge(opts)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(g)
	return g
}

// NewGaugeFunc registers a GaugeFunc and records its ownership.
func (r *Registry) NewGaugeFunc(subsystem string, opts prometheus.GaugeOpts, f func() float64) prometheus.GaugeFunc {
	g := prometheus.NewGaugeFunc(opts, f)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(g)
	return g
}

// NewGaugeVec registers a GaugeVec and records its ownership.
func (r *Registry) NewGaugeVec(subsystem string, opts prometheus.GaugeOpts, labels []string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(opts, labels)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(g)
	return g
}

// NewHistogram registers a Histogram and records its ownership.
func (r *Registry) NewHistogram(subsystem string, opts prometheus.HistogramOpts) prometheus.Histogram {
	h := prometheus.NewHistogram(opts)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(h)
	return h
}

// NewHistogramVec registers a HistogramVec and records its ownership.
func (r *Registry) NewHistogramVec(subsystem string, opts prometheus.HistogramOpts, labels []string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(opts, labels)
	r.record(subsystem, opts.Name)
	r.reg.MustRegister(h)
	return h
}

func (r *Registry) record(subsystem, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.owner[name]; ok && existing != subsystem {
		panic("metrics: " + name + " already owned by " + existing + ", cannot reassign to " + subsystem)
	}
	r.owner[name] = subsystem
}
