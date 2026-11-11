// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"slices"
	"time"
)

type RunPoint struct {
	At          time.Time `json:"at"`
	Origin      *float64  `json:"origin"`
	Peer        *float64  `json:"peer"`
	OriginBytes float64   `json:"originBytes"`
	PeerBytes   float64   `json:"peerBytes"`
	Ready       int       `json:"ready"`
	Total       int       `json:"total"`
}

type RunHistory struct {
	Start        time.Time  `json:"start"`
	ObservedFrom time.Time  `json:"observedFrom"`
	Points       []RunPoint `json:"points"`
	OriginBytes  float64    `json:"originBytes"`
	PeerBytes    float64    `json:"peerBytes"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
}

type trafficBaseline struct {
	identity string
	at       time.Time
	origin   float64
	peer     float64
}

// Each run has its own counter baselines. Absolute process counters must not
// count traffic from before the run or invent traffic across agent restarts.
func (s *Server) recordRunHistory(cached *snapshotCache, now time.Time) *RunHistory {
	s.activityMu.RLock()
	defer s.activityMu.RUnlock()

	cached.mu.Lock()
	defer cached.mu.Unlock()

	snapshot := cached.snapshot
	if snapshot.Deployment.UID == "" {
		return nil
	}

	if cached.historyUID != snapshot.Deployment.UID {
		cached.historyUID = snapshot.Deployment.UID
		cached.history = RunHistory{Start: snapshot.Deployment.Created, ObservedFrom: now}
		cached.baselines = map[string]trafficBaseline{}
	}

	history := &cached.history
	if history.CompletedAt != nil {
		copy := *history
		return &copy
	}

	if len(history.Points) > 0 && now.Sub(history.Points[len(history.Points)-1].At) < time.Second {
		copy := *history
		return &copy
	}

	for name, identity := range s.agentRoster {
		sample, exists := s.agentActivity[name]
		if !exists || sample.identity != identity || sample.failed || now.Sub(sample.at) > activityFreshness {
			continue
		}

		prior, known := cached.baselines[name]

		current := trafficBaseline{identity: identity, at: sample.at, origin: sample.response.Traffic.OriginBytes, peer: sample.response.Traffic.PeerBytes}
		if known && prior.identity == identity && current.at.After(prior.at) {
			if current.origin >= prior.origin {
				history.OriginBytes += current.origin - prior.origin
			}

			if current.peer >= prior.peer {
				history.PeerBytes += current.peer - prior.peer
			}
		}

		cached.baselines[name] = current
	}

	point := RunPoint{At: now, OriginBytes: history.OriginBytes, PeerBytes: history.PeerBytes, Total: len(snapshot.Nodes)}
	// Rates need fresh observations; cumulative counters retain observed deltas
	// even when instantaneous bandwidth is temporarily unavailable.
	var origin, peer float64

	reporting := false

	for name, identity := range s.agentRoster {
		sample := s.agentActivity[name]
		if sample.identity == identity && !sample.failed && now.Sub(sample.at) <= activityFreshness &&
			sample.originRate != nil && sample.peerRate != nil {
			origin += *sample.originRate
			peer += *sample.peerRate
			reporting = true
		}
	}

	if reporting {
		point.Origin, point.Peer = &origin, &peer
	}

	finished := 0

	for _, node := range snapshot.Nodes {
		if node.Phase == "ready" {
			point.Ready++
		}

		if node.Phase == "ready" || node.Phase == "completed" {
			finished++
		}
	}

	if point.Total > 0 && finished == point.Total {
		at := now
		history.CompletedAt = &at
	}

	history.Points = append(history.Points, point)
	copy := *history

	return &copy
}

func cloneRunHistory(history *RunHistory, from int) *RunHistory {
	if history == nil {
		return nil
	}

	copy := *history
	copy.Points = slices.Clone(history.Points[min(from, len(history.Points)):])

	return &copy
}
