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
	runFinishedTimestamp *prometheus.Desc
	runParticipantInfo   *prometheus.Desc
	r07MinimumAssertion  *prometheus.Desc
	liveStageVerdict     *prometheus.Desc
	nodeStatus           *prometheus.Desc
	campaignInfo         *prometheus.Desc
	campaignRuns         *prometheus.Desc
	campaignDeadline     *prometheus.Desc
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
		runFinishedTimestamp: prometheus.NewDesc(
			"project_signal_fleet_certification_run_finished_timestamp_seconds",
			"Unix timestamp when a retained CertificationRun finished.",
			[]string{"namespace", "run", "run_uid", "profile", "suite", "phase"},
			nil,
		),
		runParticipantInfo: prometheus.NewDesc(
			"project_signal_fleet_certification_run_participant_info",
			"Participant identity for a retained CertificationRun.",
			[]string{
				"namespace",
				"run",
				"run_uid",
				"node",
				"node_uid",
				"profile",
				"suite",
				"campaign",
			},
			nil,
		),
		r07MinimumAssertion: prometheus.NewDesc(
			"project_signal_fleet_certification_r07_assertion_decimal",
			"Minimum BF16 throughput projected from immutable completed R07 stage evidence.",
			[]string{"namespace", "run", "run_uid", "assertion"},
			nil,
		),
		liveStageVerdict: prometheus.NewDesc(
			"project_signal_fleet_certification_live_stage_verdict",
			"Validated stage verdict published while a CertificationRun is active.",
			[]string{"namespace", "run", "run_uid", "stage", "verdict"},
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
		campaignInfo: prometheus.NewDesc(
			"project_signal_certification_campaign_info",
			"Current state of a bounded CertificationCampaign.",
			[]string{"namespace", "campaign", "node", "node_uid", "profile", "suite", "phase"},
			nil,
		),
		campaignRuns: prometheus.NewDesc(
			"project_signal_certification_campaign_runs",
			"Persisted Run counts for a bounded CertificationCampaign.",
			[]string{"namespace", "campaign", "outcome"},
			nil,
		),
		campaignDeadline: prometheus.NewDesc(
			"project_signal_certification_campaign_deadline_timestamp_seconds",
			"Unix timestamp after which a CertificationCampaign stops admitting new Runs.",
			[]string{"namespace", "campaign"},
			nil,
		),
	}
}

func (c *CertificationMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.completedMeasurement

	ch <- c.runFinishedTimestamp

	ch <- c.runParticipantInfo

	ch <- c.r07MinimumAssertion

	ch <- c.liveStageVerdict

	ch <- c.nodeStatus

	ch <- c.campaignInfo

	ch <- c.campaignRuns

	ch <- c.campaignDeadline
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
		c.collectRunFinishedTimestamp(run, ch)
		c.collectRunParticipants(run, ch)
		c.collectLiveStageVerdicts(run, ch)

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

	var campaigns signalv1.CertificationCampaignList
	if err := c.reader.List(ctx, &campaigns); err != nil {
		ch <- prometheus.NewInvalidMetric(
			c.campaignInfo,
			fmt.Errorf("list CertificationCampaigns: %w", err),
		)

		return
	}

	for index := range campaigns.Items {
		c.collectCampaign(&campaigns.Items[index], ch)
	}
}

func (c *CertificationMetricsCollector) collectRunParticipants(
	run *signalv1.CertificationRun,
	ch chan<- prometheus.Metric,
) {
	campaign := ""
	if run.Spec.Campaign != nil {
		campaign = run.Spec.Campaign.Name
	}

	for index := range run.Spec.Participants {
		participant := &run.Spec.Participants[index]
		ch <- prometheus.MustNewConstMetric(
			c.runParticipantInfo,
			prometheus.GaugeValue,
			1,
			run.Namespace,
			run.Name,
			string(run.UID),
			participant.NodeName,
			string(participant.NodeUID),
			run.Spec.Profile.Name,
			run.Spec.Suite.Name,
			campaign,
		)
	}
}

func (c *CertificationMetricsCollector) collectCampaign(
	campaign *signalv1.CertificationCampaign,
	ch chan<- prometheus.Metric,
) {
	ch <- prometheus.MustNewConstMetric(
		c.campaignInfo,
		prometheus.GaugeValue,
		1,
		campaign.Namespace,
		campaign.Name,
		campaign.Spec.NodeName,
		string(campaign.Spec.NodeUID),
		campaign.Spec.Profile.Name,
		campaign.Spec.Suite.Name,
		string(campaign.Status.Phase),
	)

	counts := []struct {
		outcome string
		value   int32
	}{
		{outcome: "started", value: campaign.Status.StartedRuns},
		{outcome: "succeeded", value: campaign.Status.SucceededRuns},
		{outcome: "failed", value: campaign.Status.FailedRuns},
		{outcome: "inconclusive", value: campaign.Status.InconclusiveRuns},
		{outcome: "canceled", value: campaign.Status.CanceledRuns},
	}
	for _, count := range counts {
		ch <- prometheus.MustNewConstMetric(
			c.campaignRuns,
			prometheus.GaugeValue,
			float64(count.value),
			campaign.Namespace,
			campaign.Name,
			count.outcome,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		c.campaignDeadline,
		prometheus.GaugeValue,
		float64(campaign.Spec.Deadline.Unix()),
		campaign.Namespace,
		campaign.Name,
	)
}

func (c *CertificationMetricsCollector) collectRunFinishedTimestamp(
	run *signalv1.CertificationRun,
	ch chan<- prometheus.Metric,
) {
	if !run.Status.Phase.Terminal() || run.Status.Evidence.Identity == nil {
		return
	}

	finishedAt := identityFinishedAt(run.Status.Evidence.Identity)
	if finishedAt.IsZero() {
		return
	}

	ch <- prometheus.MustNewConstMetric(
		c.runFinishedTimestamp,
		prometheus.GaugeValue,
		float64(finishedAt.Unix()),
		run.Namespace,
		run.Name,
		string(run.UID),
		run.Spec.Profile.Name,
		run.Spec.Suite.Name,
		string(run.Status.Phase),
	)
}

func (c *CertificationMetricsCollector) collectLiveStageVerdicts(
	run *signalv1.CertificationRun,
	ch chan<- prometheus.Metric,
) {
	if run.Spec.ResolvedPlan == nil {
		return
	}

	statusIndex := 0

	for phaseIndex := range run.Spec.ResolvedPlan.Phases {
		phase := &run.Spec.ResolvedPlan.Phases[phaseIndex]
		for stageIndex := range phase.Stages {
			if statusIndex >= len(run.Status.Checks) {
				return
			}

			verdict := signalv1.StageVerdict(run.Status.Checks[statusIndex].Verdict)
			statusIndex++

			switch verdict {
			case signalv1.StageVerdictPassed,
				signalv1.StageVerdictFailed,
				signalv1.StageVerdictInconclusive,
				signalv1.StageVerdictCanceled,
				signalv1.StageVerdictSkipped:
			default:
				continue
			}

			ch <- prometheus.MustNewConstMetric(
				c.liveStageVerdict,
				prometheus.GaugeValue,
				1,
				run.Namespace,
				run.Name,
				string(run.UID),
				phase.Stages[stageIndex].Name,
				string(verdict),
			)
		}
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
			case signalv1.CheckPhaseSkipped:
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

	if run.Status.Phase == signalv1.RunPhaseCancelled {
		return "canceled", 0, total
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
