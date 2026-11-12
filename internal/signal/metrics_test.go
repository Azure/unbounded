// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"testing"

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
	if families["project_signal_fleet_certification_r07_assertion_decimal"] != nil {
		t.Fatalf("unexpected metrics without measurements: %#v", families)
	}
}

func gatherCertificationMetrics(
	t *testing.T,
	run *signalv1.CertificationRun,
) map[string]*dto.MetricFamily {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
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

	actual := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		actual[label.GetName()] = label.GetValue()
	}

	for name, value := range expected {
		if actual[name] != value {
			t.Fatalf("label %s = %q, want %q; labels = %#v", name, actual[name], value, actual)
		}
	}
}
