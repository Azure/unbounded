// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"errors"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/gantry/progress"
)

const activityFreshness = 10 * time.Second

type agentActivity struct {
	identity                                   string
	at                                         time.Time
	response                                   progress.Response
	failed                                     bool
	originRate, peerRate                       *float64
	originFailures, peerBusy, peerDialFailures *float64
}

type ClusterActivity struct {
	Agents            int       `json:"agents"`
	Reporting         int       `json:"reporting"`
	RateReporting     int       `json:"rateReporting"`
	ActivityReporting int       `json:"activityReporting"`
	OriginRate        *float64  `json:"originRate"`
	PeerRate          *float64  `json:"peerRate"`
	OriginActive      *int64    `json:"originActive"`
	PeerActive        *int64    `json:"peerActive"`
	OriginFailures    *float64  `json:"originFailures"`
	PeerBusy          *float64  `json:"peerBusy"`
	PeerDialFailures  *float64  `json:"peerDialFailures"`
	ObservedAt        time.Time `json:"observedAt"`
	Complete          bool      `json:"complete"`
	Revision          uint64    `json:"-"`
}

func agentIdentity(agent *corev1.Pod) string {
	return agent.Namespace + "/" + agent.Name + "/" + string(agent.UID)
}

func (s *Server) setAgentRoster(agents map[string]*corev1.Pod) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()

	if s.agentRoster == nil {
		s.agentRoster = map[string]string{}
		s.agentActivity = map[string]agentActivity{}
	}

	changed := len(s.agentRoster) != len(agents)

	next := make(map[string]string, len(agents))
	for name, agent := range agents {
		next[name] = agentIdentity(agent)
		if s.agentRoster[name] != next[name] {
			changed = true
		}
	}

	for name, sample := range s.agentActivity {
		if next[name] != sample.identity {
			delete(s.agentActivity, name)
		}
	}

	s.agentRoster = next
	if changed {
		s.activityRevision++
	}
}

func counterRate(current, previous, seconds float64) *float64 {
	if seconds <= 0 || math.IsNaN(current) || math.IsInf(current, 0) || math.IsNaN(previous) || math.IsInf(previous, 0) || current < previous || previous < 0 {
		return nil
	}

	rate := (current - previous) / seconds
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil
	}

	return ptr.To(rate)
}

func validateAgentActivity(response progress.Response) error {
	values := []float64{response.Traffic.OriginBytes, response.Traffic.PeerBytes}
	if response.Activity != nil {
		if response.Activity.OriginActive < 0 || response.Activity.PeerActive < 0 {
			return errors.New("agent returned invalid active transfer counts")
		}

		values = append(values, response.Activity.OriginFailures, response.Activity.PeerBusy, response.Activity.PeerDialFailures)
	}

	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("agent returned invalid transfer counters")
		}
	}

	return nil
}

func (s *Server) recordActivity(agent *corev1.Pod, response progress.Response, at time.Time, failed bool) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()

	name, identity := agent.Spec.NodeName, agentIdentity(agent)
	if s.agentRoster[name] != identity {
		return
	}

	prior, exists := s.agentActivity[name]
	if exists && at.Sub(prior.at) < 250*time.Millisecond && !failed {
		return
	}

	response.Blobs = nil

	sample := agentActivity{identity: identity, at: at, response: response, failed: failed}
	if !failed && exists && !prior.failed && prior.identity == identity && at.Sub(prior.at) <= activityFreshness {
		seconds := at.Sub(prior.at).Seconds()
		sample.originRate = counterRate(response.Traffic.OriginBytes, prior.response.Traffic.OriginBytes, seconds)

		sample.peerRate = counterRate(response.Traffic.PeerBytes, prior.response.Traffic.PeerBytes, seconds)
		if response.Activity != nil && prior.response.Activity != nil {
			sample.originFailures = counterRate(response.Activity.OriginFailures, prior.response.Activity.OriginFailures, seconds)
			sample.peerBusy = counterRate(response.Activity.PeerBusy, prior.response.Activity.PeerBusy, seconds)
			sample.peerDialFailures = counterRate(response.Activity.PeerDialFailures, prior.response.Activity.PeerDialFailures, seconds)
		}
	}

	s.agentActivity[name] = sample
	s.activityRevision++
}

func (s *Server) clusterActivity(now time.Time) *ClusterActivity {
	s.activityMu.RLock()
	defer s.activityMu.RUnlock()

	result := &ClusterActivity{Agents: len(s.agentRoster), Revision: s.activityRevision}

	var (
		originRate, peerRate, failures, busy, dial float64
		originActive, peerActive                   int64
	)

	errorsReporting := 0

	for name, identity := range s.agentRoster {
		sample, exists := s.agentActivity[name]
		if !exists || sample.identity != identity || sample.failed || now.Sub(sample.at) > activityFreshness {
			continue
		}

		result.Reporting++
		if sample.at.After(result.ObservedAt) {
			result.ObservedAt = sample.at
		}

		if sample.originRate != nil && sample.peerRate != nil {
			result.RateReporting++
			originRate += *sample.originRate
			peerRate += *sample.peerRate
		}

		if sample.response.Activity != nil {
			result.ActivityReporting++
			originActive += sample.response.Activity.OriginActive
			peerActive += sample.response.Activity.PeerActive
		}

		if sample.originFailures != nil && sample.peerBusy != nil && sample.peerDialFailures != nil {
			errorsReporting++
			failures += *sample.originFailures
			busy += *sample.peerBusy
			dial += *sample.peerDialFailures
		}
	}

	if result.RateReporting > 0 {
		result.OriginRate, result.PeerRate = ptr.To(originRate), ptr.To(peerRate)
	}

	if result.ActivityReporting > 0 {
		result.OriginActive, result.PeerActive = ptr.To(originActive), ptr.To(peerActive)
	}

	if errorsReporting > 0 {
		result.OriginFailures, result.PeerBusy, result.PeerDialFailures = ptr.To(failures), ptr.To(busy), ptr.To(dial)
	}

	result.Complete = result.Agents > 0 && result.Reporting == result.Agents && result.RateReporting == result.Agents && result.ActivityReporting == result.Agents && errorsReporting == result.Agents

	return result
}
