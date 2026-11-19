// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The scheduler sets these on every pass.
var (
	nodesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "centerpivot_nodes",
		Help: "Managed nodes, by Site, app, and the version their agent reports.",
	}, []string{"site", "app", "version"})
	updatingGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "centerpivot_updating_nodes",
		Help: "Nodes with an update under way, by Site and phase.",
	}, []string{"site", "phase"})
	heldGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "centerpivot_held_nodes",
		Help: "Nodes held after a failed update, until a person clears them.",
	})
	blockedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "centerpivot_updates_blocked",
		Help: "1 when a Site's due nodes can't start updating, by Site and reason: held, while a hold stops their app; " +
			"unavailable, while nodes that aren't updating use every slot; invalid_settings, while the Site's settings " +
			"don't fit Nebraska's update timeout.",
	}, []string{"site", "reason"})
)

// The reconciler sets this as each node leaves a phase.
var phaseSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "centerpivot_phase_seconds",
	Help: "How long nodes spent in each phase of an update, by Site, app, the phase, and the phase they went to next.",
	// 5s to about 40m: every phase ends at a deadline within that.
	Buckets: prometheus.ExponentialBuckets(5, 2, 10),
}, []string{"site", "app", "phase", "next"})

func init() {
	ctrlmetrics.Registry.MustRegister(nodesGauge, updatingGauge, heldGauge, blockedGauge, phaseSeconds)
}

// phaseTime returns how long a node spent in the phase of an update it left,
// going from before to next at now, if it left one.
func phaseTime(before, next State, now time.Time) (time.Duration, bool) {
	if !before.Phase.Updating() || next.Phase == before.Phase || before.Since.IsZero() {
		return 0, false
	}

	return now.Sub(before.Since), true
}
