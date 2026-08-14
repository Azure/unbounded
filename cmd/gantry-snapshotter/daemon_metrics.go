// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/clean"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
)

// metricsSubsystem is the ownership key every metric here is registered under.
// See internal/gantry/metrics.
const metricsSubsystem = "snapshotter"

// ingestFailed is the outcome label for an ingest that returned an error.
// ingest.Outcome has no such value because it describes what an attempt did,
// and a failed attempt did nothing.
const ingestFailed = "failed"

// daemonMetrics is the snapshotter's metric set.
//
// It lives in cmd rather than beside the code it measures because the packages
// under internal/gantry/snapshotter are usable without Prometheus and are
// tested without it. Each of them already exposes the hook this needs, so the
// wiring is here and the instrumentation is nowhere.
type daemonMetrics struct {
	// adopt counts what happened to each layer containerd asked about. The
	// hit rate over this is what the whole daemon exists to raise.
	adopt *prometheus.CounterVec

	// ingest counts terminal ingest outcomes, including failures. A rising
	// failure count with a flat adopt hit rate is a node publishing
	// nothing, which no other signal here would show.
	ingest *prometheus.CounterVec

	// ingestBytes is the volume this node has written into the cluster's
	// segments, which is what fills them.
	ingestBytes prometheus.Counter

	// rolls counts segment rollovers. A sealed segment does not come back
	// until the cleaner reclaims it, so this is the capacity alarm.
	rolls prometheus.Counter

	// cycles counts cleaner steps by the phase they reached. Reclamation is
	// a ladder rather than one operation, so a cluster stuck on any rung is
	// only visible if each rung is counted separately.
	cycles *prometheus.CounterVec

	// copied counts the bytes the cleaner has moved out of victim segments.
	// It is the price of reclamation and the number that says whether the
	// low-water mark is set too high.
	copied prometheus.Counter
}

// newDaemonMetrics registers the daemon's metrics on reg.
//
// Gauges that read live state are registered as GaugeFuncs against the running
// stack rather than being set from the code paths that change them: the values
// they report are already tracked, and a callback cannot drift from them the
// way a mirrored counter can.
func newDaemonMetrics(reg *metrics.Registry, cat *holder) *daemonMetrics {
	m := &daemonMetrics{
		adopt: reg.NewCounterVec(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_adopt_total",
			Help: "Layers containerd offered, by what the cluster catalog could do with them.",
		}, []string{"outcome"}),
		ingest: reg.NewCounterVec(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_ingest_total",
			Help: "Terminal ingest attempts, by outcome.",
		}, []string{"outcome"}),
		ingestBytes: reg.NewCounter(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_ingest_bytes_total",
			Help: "Bytes this node has written into cluster segments.",
		}),
		rolls: reg.NewCounter(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_segment_rolls_total",
			Help: "Segments sealed because ingest could not fit in them.",
		}),
		cycles: reg.NewCounterVec(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_clean_cycles_total",
			Help: "Cleaner steps, by the phase the step reached.",
		}, []string{"phase"}),
		copied: reg.NewCounter(metricsSubsystem, prometheus.CounterOpts{
			Name: "gantry_snapshotter_clean_copied_bytes_total",
			Help: "Bytes copied out of segments being reclaimed.",
		}),
	}

	// Pre-create every label value so a scrape of a healthy node shows a
	// zero rather than a missing series. Rate over an absent series is not
	// a number an alert can use.
	for _, o := range []snapshotter.AdoptOutcome{
		snapshotter.AdoptHit,
		snapshotter.AdoptMiss,
		snapshotter.AdoptExists,
		snapshotter.AdoptFailed,
	} {
		m.adopt.WithLabelValues(o.String())
	}

	for _, o := range []ingest.Outcome{ingest.OutcomePresent, ingest.OutcomeLinked, ingest.OutcomeIngested} {
		m.ingest.WithLabelValues(o.String())
	}

	m.ingest.WithLabelValues(ingestFailed)

	reg.NewGaugeFunc(metricsSubsystem, prometheus.GaugeOpts{
		Name: "gantry_snapshotter_catalog_records",
		Help: "Keys the attached catalog resolves. Zero means no catalog is attached.",
	}, func() float64 { return float64(cat.Len()) })

	// A hole stops every node reading past it, so its age is the number
	// that matters: a hole seconds old is an ingest in flight, and a hole
	// older than the repair grace is a catalog that stopped advancing.
	reg.NewGaugeFunc(metricsSubsystem, prometheus.GaugeOpts{
		Name: "gantry_snapshotter_catalog_hole_seconds",
		Help: "How long the catalog has been stopped at an unwritten record slot. Zero means no hole.",
	}, func() float64 {
		_, age := cat.Hole()

		return age.Seconds()
	})

	return m
}

// trackQueue registers the queue depth gauge.
//
// It is separate from newDaemonMetrics because the queue is built with this
// metric set's ingest observer, so one of the two has to exist first.
func (m *daemonMetrics) trackQueue(reg *metrics.Registry, queue *ingest.Queue) {
	reg.NewGaugeFunc(metricsSubsystem, prometheus.GaugeOpts{
		Name: "gantry_snapshotter_ingest_pending",
		Help: "Layers accepted for ingest and not finished.",
	}, func() float64 { return float64(queue.Pending()) })
}

// chainObserver runs several ingest observers in order, so logging and
// counting do not have to be the same function.
func chainObserver(fns ...func(ingest.Request, ingest.Result, error)) func(ingest.Request, ingest.Result, error) {
	return func(req ingest.Request, res ingest.Result, err error) {
		for _, fn := range fns {
			fn(req, res, err)
		}
	}
}

// observeAdopt reports one Prepare outcome. It runs on containerd's goroutine.
func (m *daemonMetrics) observeAdopt(outcome snapshotter.AdoptOutcome) {
	m.adopt.WithLabelValues(outcome.String()).Inc()
}

// observeIngest reports one terminal ingest outcome. Its signature matches
// ingest.QueueOptions.Observe; the request itself carries digests, which have
// no place in a label.
func (m *daemonMetrics) observeIngest(_ ingest.Request, res ingest.Result, err error) {
	if err != nil {
		m.ingest.WithLabelValues(ingestFailed).Inc()
		return
	}

	m.ingest.WithLabelValues(res.Outcome.String()).Inc()

	// Only a real ingest moved bytes. Present and linked resolved against
	// what somebody else already wrote.
	if res.Outcome == ingest.OutcomeIngested {
		m.ingestBytes.Add(float64(res.Blob.Address.ByteLength))
	}
}

// observeClean reports one step of the reclamation ladder.
func (m *daemonMetrics) observeClean(res clean.Result) {
	m.cycles.WithLabelValues(string(res.Phase)).Inc()
	m.copied.Add(float64(res.Bytes))
}

// observeRoll reports one segment rollover.
func (m *daemonMetrics) observeRoll(catalog.Roll) {
	m.rolls.Inc()
}
