// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestCertificationMetricsCollectorPublishesActiveCompletedStageMinimum(t *testing.T) {
	t.Parallel()

	first := "812.5"
	second := "799.25"
	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "active-run",
			Namespace: "project-signal",
			UID:       types.UID("run-uid"),
		},
		Spec: signalv1.CertificationRunSpec{
			Participants: []signalv1.Participant{{
				NodeName: "participant-0",
				NodeUID:  types.UID("node-uid"),
			}},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseRunning,
			Evidence: signalv1.EvidenceStatus{
				Identity: &signalv1.QualificationEvidenceIdentity{
					Stages: []signalv1.StageEvidenceSummary{{
						Name: "r07-gpu-gemm-bf16",
						Measurements: []signalv1.MetricMeasurement{
							{
								Name: "gpu0MinTflops",
								Value: signalv1.TypedValue{
									Type:         signalv1.TypedValueTypeDecimal,
									DecimalValue: &first,
								},
								Unit: "TFLOP/s",
							},
							{
								Name: "gpu1MinTflops",
								Value: signalv1.TypedValue{
									Type:         signalv1.TypedValueTypeDecimal,
									DecimalValue: &second,
								},
								Unit: "TFLOP/s",
							},
						},
					}},
				},
			},
		},
	}

	families := gatherCertificationMetrics(t, run)

	completed := families["project_signal_fleet_certification_measurement"]
	if completed == nil || len(completed.Metric) != 2 {
		t.Fatalf("completed measurements = %#v, want 2", completed)
	}

	assertMetricLabels(t, completed.Metric[0], map[string]string{
		"namespace": "project-signal",
		"run":       "active-run",
		"run_uid":   "run-uid",
		"stage":     "r07-gpu-gemm-bf16",
		"metric_id": "gpu0MinTflops",
		"unit":      "TFLOP/s",
	})

	minimum := families["project_signal_fleet_certification_r07_assertion_decimal"]
	if minimum == nil || len(minimum.Metric) != 1 ||
		minimum.Metric[0].Gauge == nil ||
		minimum.Metric[0].Gauge.GetValue() != 799.25 {
		t.Fatalf("minimum metric = %#v, want 799.25", minimum)
	}

	assertMetricLabels(t, minimum.Metric[0], map[string]string{
		"namespace": "project-signal",
		"run":       "active-run",
		"run_uid":   "run-uid",
		"assertion": "bf16-throughput",
	})
}

func TestCertificationMetricsCollectorOmitsMissingMeasurements(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{Name: "active-run", Namespace: "project-signal"},
		Spec: signalv1.CertificationRunSpec{
			Participants: []signalv1.Participant{{NodeName: "participant-0", NodeUID: "node-uid"}},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseRunning,
			Evidence: signalv1.EvidenceStatus{
				Identity: &signalv1.QualificationEvidenceIdentity{
					Stages: []signalv1.StageEvidenceSummary{{Name: "r07-gpu-gemm-bf16"}},
				},
			},
		},
	}

	families := gatherCertificationMetrics(t, run)
	if families["project_signal_fleet_certification_measurement"] != nil {
		t.Fatalf("unexpected completed metrics without measurements: %#v", families)
	}

	if families["project_signal_fleet_certification_r07_assertion_decimal"] != nil {
		t.Fatalf("unexpected metrics without measurements: %#v", families)
	}
}

func TestCertificationMetricsCollectorPublishesLatestNodeStatusAndStage(t *testing.T) {
	t.Parallel()

	oldRun := certificationStatusRun(
		"old-run",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseFailed,
		[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseFailed},
	)
	oldRun.Spec.Participants[0].NodeUID = "old-node-uid"
	newRun := certificationStatusRun(
		"new-run",
		"2026-01-02T00:00:00Z",
		signalv1.RunPhaseRunning,
		[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseRunning},
	)

	families := gatherCertificationMetrics(t, oldRun, newRun)

	status := families["project_signal_node_certification_status"]
	if status == nil || len(status.Metric) != 1 {
		t.Fatalf("node status metrics = %#v, want one latest metric", status)
	}

	assertMetricLabels(t, status.Metric[0], map[string]string{
		"namespace":   "project-signal",
		"node":        "participant-0",
		"node_uid":    "node-uid",
		"profile":     "profile-v1",
		"suite":       "suite-v1",
		"status":      "certifying",
		"phase":       "Running",
		"stage":       "r02-second",
		"stage_index": "2",
		"stage_total": "2",
	})
}

func TestCertificationMetricsCollectorPublishesTerminalNodeStatus(t *testing.T) {
	t.Parallel()

	succeeded := certificationStatusRun(
		"succeeded-run",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseSucceeded,
		[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseSucceeded},
	)
	succeeded.Spec.Participants[0].NodeName = "passed-node"
	succeeded.Spec.Participants[0].NodeUID = "passed-node-uid"

	failed := certificationStatusRun(
		"failed-run",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseInconclusive,
		[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseInconclusive},
	)
	failed.Spec.Participants[0].NodeName = "failed-node"
	failed.Spec.Participants[0].NodeUID = "failed-node-uid"

	families := gatherCertificationMetrics(t, succeeded, failed)

	status := families["project_signal_node_certification_status"]
	if status == nil || len(status.Metric) != 2 {
		t.Fatalf("node status metrics = %#v, want two metrics", status)
	}

	labelsByNode := make(map[string]map[string]string, len(status.Metric))
	for _, metric := range status.Metric {
		labels := metricLabels(metric)
		labelsByNode[labels["node"]] = labels
	}

	if labelsByNode["passed-node"]["status"] != "passed" ||
		labelsByNode["passed-node"]["stage"] != "complete" {
		t.Fatalf("passed labels = %#v", labelsByNode["passed-node"])
	}

	if labelsByNode["failed-node"]["status"] != "failed" ||
		labelsByNode["failed-node"]["stage"] != "r02-second" {
		t.Fatalf("failed labels = %#v", labelsByNode["failed-node"])
	}
}

func TestCertificationMetricsCollectorPublishesCanceledResolvedStage(t *testing.T) {
	t.Parallel()

	run := certificationStatusRun(
		"canceled-run",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseCancelled,
		[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseInconclusive},
	)

	families := gatherCertificationMetrics(t, run)

	status := families["project_signal_node_certification_status"]
	if status == nil || len(status.Metric) != 1 {
		t.Fatalf("node status metrics = %#v, want one metric", status)
	}

	assertMetricLabels(t, status.Metric[0], map[string]string{
		"status":      "failed",
		"phase":       "Canceled",
		"stage":       "r02-second",
		"stage_index": "2",
		"stage_total": "2",
	})
}

func TestCurrentParticipantStageReportsCanceledBeforeStart(t *testing.T) {
	t.Parallel()

	run := certificationStatusRun(
		"canceled-before-start",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseCancelled,
		nil,
	)

	stage, index, total := currentParticipantStage(run, 0)
	if stage != "canceled" || index != 0 || total != 2 {
		t.Fatalf("stage = %q %d/%d, want canceled 0/2", stage, index, total)
	}
}

func TestCertificationMetricsCollectorUsesParticipantSpecificStage(t *testing.T) {
	t.Parallel()

	run := certificationStatusRun(
		"role-run",
		"2026-01-01T00:00:00Z",
		signalv1.RunPhaseRunning,
		[]signalv1.CheckPhase{signalv1.CheckPhaseRunning, signalv1.CheckPhasePending},
	)
	run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{
		NodeName: "participant-1",
		NodeUID:  "node-uid-1",
	})
	run.Spec.ResolvedPlan.Phases[0].Stages[0].ParticipantRoles = []signalv1.ResolvedStageParticipantRoleBinding{{
		Role:               "primary",
		ParticipantIndexes: []int32{0},
	}}
	run.Spec.ResolvedPlan.Phases[0].Stages[1].ParticipantRoles = []signalv1.ResolvedStageParticipantRoleBinding{{
		Role:               "secondary",
		ParticipantIndexes: []int32{1},
	}}

	families := gatherCertificationMetrics(t, run)

	status := families["project_signal_node_certification_status"]
	if status == nil || len(status.Metric) != 2 {
		t.Fatalf("node status metrics = %#v, want two metrics", status)
	}

	labelsByNode := make(map[string]map[string]string, len(status.Metric))
	for _, metric := range status.Metric {
		labels := metricLabels(metric)
		labelsByNode[labels["node"]] = labels
	}

	if labelsByNode["participant-0"]["stage"] != "r01-first" ||
		labelsByNode["participant-1"]["stage"] != "r02-second" {
		t.Fatalf("participant stages = %#v", labelsByNode)
	}
}

func TestCertificationMetricsCollectorPublishes5000NodeFleet(t *testing.T) {
	t.Parallel()

	const fleetSize = 5000

	runs := make([]*signalv1.CertificationRun, fleetSize)
	for index := range fleetSize {
		run := certificationStatusRun(
			fmt.Sprintf("fleet-run-%04d", index),
			"2026-01-01T00:00:00Z",
			signalv1.RunPhaseRunning,
			[]signalv1.CheckPhase{signalv1.CheckPhaseSucceeded, signalv1.CheckPhaseRunning},
		)
		run.Spec.Participants[0].NodeName = fmt.Sprintf("participant-%04d", index)
		run.Spec.Participants[0].NodeUID = types.UID(fmt.Sprintf("node-uid-%04d", index))
		runs[index] = run
	}

	families := gatherCertificationMetrics(t, runs...)

	status := families["project_signal_node_certification_status"]
	if status == nil || len(status.Metric) != fleetSize {
		t.Fatalf("node status metric count = %d, want %d", len(status.GetMetric()), fleetSize)
	}
}

func TestCurrentLegacyCheckReportsTerminalFailureBeforePendingCheck(t *testing.T) {
	t.Parallel()

	run := &signalv1.CertificationRun{
		Spec: signalv1.CertificationRunSpec{
			Checks: make([]signalv1.CertificationCheckSnapshot, 3),
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseFailed,
			Checks: []signalv1.CheckStatus{
				{Phase: signalv1.CheckPhaseSucceeded},
				{Phase: signalv1.CheckPhaseFailed},
				{Phase: signalv1.CheckPhasePending},
			},
		},
	}

	stage, index, total := currentLegacyCheck(run)
	if stage != "check-002" || index != 2 || total != 3 {
		t.Fatalf("legacy stage = %q %d/%d, want check-002 2/3", stage, index, total)
	}
}

func certificationStatusRun(
	name string,
	createdAt string,
	phase signalv1.RunPhase,
	checks []signalv1.CheckPhase,
) *signalv1.CertificationRun {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		panic(err)
	}

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "project-signal",
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{Name: "profile-v1"},
			Suite:   signalv1.PinnedSuite{Name: "suite-v1"},
			Participants: []signalv1.Participant{{
				NodeName: "participant-0",
				NodeUID:  "node-uid",
			}},
			ResolvedPlan: &signalv1.ResolvedCertificationPlan{
				Phases: []signalv1.ResolvedPhasePlan{{
					Name: "certification",
					Stages: []signalv1.ResolvedStagePlan{
						{Name: "r01-first"},
						{Name: "r02-second"},
					},
				}},
			},
		},
		Status: signalv1.CertificationRunStatus{Phase: phase},
	}
	for _, checkPhase := range checks {
		run.Status.Checks = append(run.Status.Checks, signalv1.CheckStatus{Phase: checkPhase})
	}

	return run
}

func gatherCertificationMetrics(
	t *testing.T,
	runs ...*signalv1.CertificationRun,
) map[string]*dto.MetricFamily {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	objects := make([]runtime.Object, len(runs))
	for index := range runs {
		objects[index] = runs[index]
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewCertificationMetricsCollector(reader))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	result := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		result[family.GetName()] = family
	}

	return result
}

func assertMetricLabels(
	t *testing.T,
	metric *dto.Metric,
	expected map[string]string,
) {
	t.Helper()

	actual := metricLabels(metric)

	for name, value := range expected {
		if actual[name] != value {
			t.Fatalf("label %s = %q, want %q; labels = %#v", name, actual[name], value, actual)
		}
	}
}

func metricLabels(metric *dto.Metric) map[string]string {
	actual := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		actual[label.GetName()] = label.GetValue()
	}

	return actual
}
