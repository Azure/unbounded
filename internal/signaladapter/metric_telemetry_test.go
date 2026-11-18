// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"strings"
	"testing"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestProcessMetricTelemetryExportsCurrentSamplesAndFinalMinimum(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGaugeAndSamples, 1, 2)
	content := []byte(
		`{"metric":"gpu0MinTflops","value":120.5,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n" +
			`{"metric":"gpu0MinTflops","value":110.25,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n",
	)

	result, err := processMetricTelemetry(contract, content, true, testMetricProxyIdentity())
	if err != nil {
		t.Fatalf("processMetricTelemetry() error = %v", err)
	}

	if result.Final["gpu0MinTflops"] != 110.25 {
		t.Fatalf("final minimum = %v, want 110.25", result.Final)
	}

	if !strings.Contains(
		result.Prometheus,
		`project_signal_certification_metric{dtype="bf16",gpu="gpu-index-0",`+
			`invocation="invocation",metric_id="gpu0MinTflops",namespace="signal-system",`+
			`node="worker",participant_role="participant",run="run",run_uid="run-uid",`+
			`stage="r07-gpu-gemm-bf16",unit="TFLOP/s"} 110.25`,
	) {
		t.Fatalf("current metric is absent:\n%s", result.Prometheus)
	}

	if strings.Count(result.Prometheus, metricTelemetrySampleName+"{") != 2 {
		t.Fatalf("sample metrics =\n%s", result.Prometheus)
	}
}

func TestProcessMetricTelemetryUsesLatestRecordAcrossSeries(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContractWithEvidence(
		t,
		signalv1.MetricLiveModeGauge,
		2,
		2,
		signalv1.MetricEvidenceLatest,
	)
	content := []byte(
		`{"metric":"gpu0MinTflops","value":100,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n" +
			`{"metric":"gpu0MinTflops","value":200,"dimensions":{"dtype":"bf16","gpu":"gpu-index-1"}}` + "\n" +
			`{"metric":"gpu0MinTflops","value":150,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n",
	)

	result, err := processMetricTelemetry(contract, content, true, testMetricProxyIdentity())
	if err != nil {
		t.Fatalf("processMetricTelemetry() error = %v", err)
	}

	if result.Final["gpu0MinTflops"] != 150 {
		t.Fatalf("final latest = %v, want 150", result.Final)
	}
}

func TestProcessMetricTelemetryRejectsUndeclaredMetric(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 1)

	_, err := processMetricTelemetry(
		contract,
		[]byte(`{"metric":"other","value":1,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}`+"\n"),
		true,
		testMetricProxyIdentity(),
	)
	if err == nil || !strings.Contains(err.Error(), `unknown metric "other"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestProcessMetricTelemetryRejectsUndeclaredDimension(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 1)

	_, err := processMetricTelemetry(
		contract,
		[]byte(
			`{"metric":"gpu0MinTflops","value":1,"dimensions":`+
				`{"dtype":"bf16","gpu":"gpu-index-0","run_uid":"forged"}}`+"\n",
		),
		true,
		testMetricProxyIdentity(),
	)
	if err == nil || !strings.Contains(err.Error(), `dimensions do not match the declaration`) {
		t.Fatalf("error = %v", err)
	}
}

func TestProcessMetricTelemetryDuplicateSeriesUpdatesCurrentGauge(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 2)
	content := []byte(
		`{"metric":"gpu0MinTflops","value":1,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n" +
			`{"metric":"gpu0MinTflops","value":2,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n",
	)

	result, err := processMetricTelemetry(contract, content, true, testMetricProxyIdentity())
	if err != nil {
		t.Fatalf("processMetricTelemetry() error = %v", err)
	}

	if strings.Contains(result.Prometheus, metricTelemetrySampleName+"{") {
		t.Fatalf("Gauge mode exported samples:\n%s", result.Prometheus)
	}

	if !strings.Contains(result.Prometheus, `unit="TFLOP/s"} 2`) {
		t.Fatalf("current metric did not use the latest value:\n%s", result.Prometheus)
	}
}

func TestProcessMetricTelemetryRejectsSeriesAndSampleOverflow(t *testing.T) {
	t.Parallel()

	t.Run("series", func(t *testing.T) {
		t.Parallel()

		contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 2)
		content := []byte(
			`{"metric":"gpu0MinTflops","value":1,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n" +
				`{"metric":"gpu0MinTflops","value":2,"dimensions":{"dtype":"bf16","gpu":"gpu-index-1"}}` + "\n",
		)

		_, err := processMetricTelemetry(contract, content, true, testMetricProxyIdentity())
		if err == nil || !strings.Contains(err.Error(), "exceeds series limit 1") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("samples", func(t *testing.T) {
		t.Parallel()

		contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 1)
		content := []byte(
			`{"metric":"gpu0MinTflops","value":1,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n" +
				`{"metric":"gpu0MinTflops","value":2,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}` + "\n",
		)

		_, err := processMetricTelemetry(contract, content, true, testMetricProxyIdentity())
		if err == nil || !strings.Contains(err.Error(), "exceeds sample limit 1") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestProcessMetricTelemetryRejectsMalformedAndNonFiniteValues(t *testing.T) {
	t.Parallel()

	contract := testMetricTelemetryContract(t, signalv1.MetricLiveModeGauge, 1, 1)
	for name, content := range map[string]string{
		"malformed":  `{"metric":`,
		"non-finite": `{"metric":"gpu0MinTflops","value":1e999,"dimensions":{"dtype":"bf16","gpu":"gpu-index-0"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := processMetricTelemetry(
				contract,
				[]byte(content+"\n"),
				true,
				testMetricProxyIdentity(),
			)
			if err == nil {
				t.Fatal("processMetricTelemetry() unexpectedly succeeded")
			}
		})
	}
}

func testMetricTelemetryContract(
	t *testing.T,
	live signalv1.MetricLiveMode,
	seriesLimit int32,
	sampleLimit int32,
) string {
	t.Helper()

	return testMetricTelemetryContractWithEvidence(
		t,
		live,
		seriesLimit,
		sampleLimit,
		signalv1.MetricEvidenceMinimum,
	)
}

func testMetricTelemetryContractWithEvidence(
	t *testing.T,
	live signalv1.MetricLiveMode,
	seriesLimit int32,
	sampleLimit int32,
	evidence signalv1.MetricEvidenceAggregation,
) string {
	t.Helper()

	content, enabled, err := metricTelemetryContract([]signalv1.TestMetricDefinition{{
		Name:        "gpu0MinTflops",
		DisplayName: "GPU 0 minimum throughput",
		Type:        signalv1.MetricValueTypeThroughput,
		Unit:        "TFLOP/s",
		Required:    true,
		Telemetry: &signalv1.TestMetricTelemetry{
			Live:        live,
			Dimensions:  []signalv1.MetricDimensionName{"dtype", "gpu"},
			SeriesLimit: seriesLimit,
			SampleLimit: sampleLimit,
			Evidence:    evidence,
		},
	}})
	if err != nil {
		t.Fatalf("metricTelemetryContract() error = %v", err)
	}

	if !enabled {
		t.Fatal("metric telemetry was not enabled")
	}

	return content
}

func testMetricProxyIdentity() MetricProxyIdentity {
	return MetricProxyIdentity{
		Namespace:       "signal-system",
		Run:             "run",
		RunUID:          "run-uid",
		Stage:           "r07-gpu-gemm-bf16",
		Invocation:      "invocation",
		ParticipantRole: "participant",
		Node:            "worker",
	}
}
