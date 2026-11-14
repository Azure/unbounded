// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const metricsCollectionTimeout = 5 * time.Second

var gpuTflopsMetricPattern = regexp.MustCompile(`^gpu([0-7])MinTflops$`)

type CertificationMetricsCollector struct {
	reader               client.Reader
	completedMeasurement *prometheus.Desc
	r07MinimumAssertion  *prometheus.Desc
	nodeStatus           *prometheus.Desc
}

func NewCertificationMetricsCollector(reader client.Reader) *CertificationMetricsCollector {
	return &CertificationMetricsCollector{
		reader: reader,
		completedMeasurement: prometheus.NewDesc(
			"project_signal_fleet_certification_measurement",
			"Numeric measurement projected from immutable completed stage evidence.",
			[]string{"namespace", "run", "run_uid", "stage", "metric_id", "unit"},
			nil,
		),
		r07MinimumAssertion: prometheus.NewDesc(
			"project_signal_fleet_certification_r07_assertion_decimal",
			"Minimum BF16 throughput projected from immutable completed R07 stage evidence.",
			[]string{"namespace", "run", "run_uid", "assertion"},
			nil,
		),
		nodeStatus: prometheus.NewDesc(
			"project_signal_node_certification_status",
			"Current certification state and stage from the newest Run for each participant.",
			[]string{
				"namespace",
				"node",
				"node_uid",
				"profile",
				"suite",
				"status",
				"phase",
				"stage",
				"stage_index",
				"stage_total",
			},
			nil,
		),
	}
}

func (c *CertificationMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.completedMeasurement

	ch <- c.r07MinimumAssertion

	ch <- c.nodeStatus
}

func (c *CertificationMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectionTimeout)
	defer cancel()

	var runs signalv1.CertificationRunList
	if err := c.reader.List(ctx, &runs); err != nil {
		ch <- prometheus.NewInvalidMetric(
			c.r07MinimumAssertion,
			fmt.Errorf("list CertificationRuns: %w", err),
		)

		return
	}

	latestRuns := make(map[string]nodeRunStatus)

	for runIndex := range runs.Items {
		run := &runs.Items[runIndex]
		c.collectRun(run, ch)

		for participantIndex := range run.Spec.Participants {
			participant := &run.Spec.Participants[participantIndex]
			key := run.Namespace + "\x00" + participant.NodeName

			current, exists := latestRuns[key]
			if exists && !runIsNewer(run, current.run) {
				continue
			}

			latestRuns[key] = nodeRunStatus{
				run:              run,
				participantIndex: int32(participantIndex),
			}
		}
	}

	for _, status := range latestRuns {
		c.collectNodeStatus(status.run, status.participantIndex, ch)
	}
}

type nodeRunStatus struct {
	run              *signalv1.CertificationRun
	participantIndex int32
}

func runIsNewer(candidate, current *signalv1.CertificationRun) bool {
	if candidate.CreationTimestamp.Equal(&current.CreationTimestamp) {
		return candidate.Name > current.Name
	}

	return candidate.CreationTimestamp.After(current.CreationTimestamp.Time)
}

func (c *CertificationMetricsCollector) collectNodeStatus(
	run *signalv1.CertificationRun,
	participantIndex int32,
	ch chan<- prometheus.Metric,
) {
	if participantIndex < 0 || int(participantIndex) >= len(run.Spec.Participants) {
		return
	}

	participant := &run.Spec.Participants[participantIndex]
	stage, stageIndex, stageTotal := currentParticipantStage(run, participantIndex)

	ch <- prometheus.MustNewConstMetric(
		c.nodeStatus,
		prometheus.GaugeValue,
		1,
		run.Namespace,
		participant.NodeName,
		string(participant.NodeUID),
		run.Spec.Profile.Name,
		run.Spec.Suite.Name,
		dashboardStatus(run.Status.Phase),
		string(run.Status.Phase),
		stage,
		strconv.Itoa(stageIndex),
		strconv.Itoa(stageTotal),
	)
}

func dashboardStatus(phase signalv1.RunPhase) string {
	switch phase {
	case signalv1.RunPhaseSucceeded:
		return "passed"
	case signalv1.RunPhaseFailed, signalv1.RunPhaseInconclusive, signalv1.RunPhaseCancelled:
		return "failed"
	default:
		return "certifying"
	}
}

func currentParticipantStage(
	run *signalv1.CertificationRun,
	participantIndex int32,
) (string, int, int) {
	if run.Spec.ResolvedPlan == nil {
		return currentLegacyCheck(run)
	}

	type stageState struct {
		name  string
		index int
	}

	total := 0
	statusIndex := 0

	var (
		firstPending  *stageState
		firstFailed   *stageState
		current       *stageState
		lastCompleted *stageState
	)

	for phaseIndex := range run.Spec.ResolvedPlan.Phases {
		phase := &run.Spec.ResolvedPlan.Phases[phaseIndex]
		for stageIndex := range phase.Stages {
			stage := &phase.Stages[stageIndex]

			stagePhase := signalv1.CheckPhasePending
			if statusIndex < len(run.Status.Checks) {
				stagePhase = run.Status.Checks[statusIndex].Phase
			}

			statusIndex++

			if !stageIncludesParticipant(stage, participantIndex) {
				continue
			}

			total++
			state := &stageState{name: stage.Name, index: total}

			switch stagePhase {
			case signalv1.CheckPhaseRunning:
				if current == nil {
					current = state
				}
			case signalv1.CheckPhasePending:
				if firstPending == nil {
					firstPending = state
				}
			case signalv1.CheckPhaseFailed, signalv1.CheckPhaseInconclusive:
				if firstFailed == nil {
					firstFailed = state
				}

				lastCompleted = state
			default:
				lastCompleted = state
			}
		}
	}

	if current != nil {
		return current.name, current.index, total
	}

	if firstFailed != nil && run.Status.Phase.Terminal() {
		return firstFailed.name, firstFailed.index, total
	}

	if firstPending != nil && !run.Status.Phase.Terminal() {
		return firstPending.name, firstPending.index, total
	}

	if run.Status.Phase == signalv1.RunPhaseSucceeded {
		return "complete", total, total
	}

	if lastCompleted != nil {
		return lastCompleted.name, lastCompleted.index, total
	}

	return "pending", 0, total
}

func stageIncludesParticipant(stage *signalv1.ResolvedStagePlan, participantIndex int32) bool {
	if len(stage.ParticipantRoles) == 0 {
		return true
	}

	for roleIndex := range stage.ParticipantRoles {
		for index := range stage.ParticipantRoles[roleIndex].ParticipantIndexes {
			if stage.ParticipantRoles[roleIndex].ParticipantIndexes[index] == participantIndex {
				return true
			}
		}
	}

	return false
}

func currentLegacyCheck(run *signalv1.CertificationRun) (string, int, int) {
	total := len(run.Spec.Checks)
	for index := range run.Status.Checks {
		if run.Status.Checks[index].Phase == signalv1.CheckPhaseRunning {
			return fmt.Sprintf("check-%03d", index+1), index + 1, total
		}
	}

	if run.Status.Phase.Terminal() {
		for index := range run.Status.Checks {
			if run.Status.Checks[index].Phase == signalv1.CheckPhaseFailed ||
				run.Status.Checks[index].Phase == signalv1.CheckPhaseInconclusive {
				return fmt.Sprintf("check-%03d", index+1), index + 1, total
			}
		}

		if run.Status.Phase == signalv1.RunPhaseSucceeded {
			return "complete", total, total
		}
	} else {
		for index := range run.Status.Checks {
			if run.Status.Checks[index].Phase == signalv1.CheckPhasePending {
				return fmt.Sprintf("check-%03d", index+1), index + 1, total
			}
		}
	}

	return "pending", 0, total
}

func (c *CertificationMetricsCollector) collectRun(
	run *signalv1.CertificationRun,
	ch chan<- prometheus.Metric,
) {
	if run.Status.Evidence.Identity == nil || len(run.Spec.Participants) == 0 {
		return
	}

	for stageIndex := range run.Status.Evidence.Identity.Stages {
		stage := &run.Status.Evidence.Identity.Stages[stageIndex]
		for measurementIndex := range stage.Measurements {
			measurement := &stage.Measurements[measurementIndex]

			value, ok := numericMetricValue(measurement.Value)
			if !ok {
				continue
			}

			ch <- prometheus.MustNewConstMetric(
				c.completedMeasurement,
				prometheus.GaugeValue,
				value,
				run.Namespace,
				run.Name,
				string(run.UID),
				stage.Name,
				measurement.Name,
				measurement.Unit,
			)
		}

		if stage.Name != "r07-gpu-gemm-bf16" {
			continue
		}

		minimum := 0.0
		hasMinimum := false

		for measurementIndex := range stage.Measurements {
			measurement := &stage.Measurements[measurementIndex]

			matches := gpuTflopsMetricPattern.FindStringSubmatch(measurement.Name)
			if len(matches) != 2 || measurement.Unit != "TFLOP/s" {
				continue
			}

			value, ok := numericMetricValue(measurement.Value)
			if !ok {
				continue
			}

			if !hasMinimum || value < minimum {
				minimum = value
				hasMinimum = true
			}
		}

		if hasMinimum {
			ch <- prometheus.MustNewConstMetric(
				c.r07MinimumAssertion,
				prometheus.GaugeValue,
				minimum,
				run.Namespace,
				run.Name,
				string(run.UID),
				"bf16-throughput",
			)
		}
	}
}

func numericMetricValue(value signalv1.TypedValue) (float64, bool) {
	switch value.Type {
	case signalv1.TypedValueTypeInteger:
		if value.IntegerValue == nil {
			return 0, false
		}

		return float64(*value.IntegerValue), true
	case signalv1.TypedValueTypeDecimal:
		if value.DecimalValue == nil {
			return 0, false
		}

		parsed, err := strconv.ParseFloat(*value.DecimalValue, 64)

		return parsed, err == nil
	default:
		return 0, false
	}
}
