// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestApplyQualificationEvidenceAttributesPodParticipants(t *testing.T) {
	t.Parallel()

	config := qualificationConfig(signalv1.ExecutionBackendPod)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	evidence := qualificationEvidenceSkeleton(2)
	addHealthyMPIWorkers(&evidence)

	results := []Result{
		qualificationResult(0, "Pass", "participant-zero", started),
		qualificationResult(1, "Fail", "participant-one", started),
	}
	results[0].Measurements = `{"qualification":{"driverVersion":"580.1","gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]}}`

	applyQualificationEvidence(
		&evidence,
		config,
		results,
		qualificationFingerprints("boot-a", "boot-a"),
		qualificationFingerprints("boot-a", "boot-a"),
	)

	if evidence.GroupResult != nil {
		t.Fatal("Pod evidence unexpectedly contains a group verdict")
	}

	if evidence.Participants[0].Verdict.Verdict != "Pass" ||
		evidence.Participants[1].Verdict.Verdict != "Fail" ||
		evidence.Participants[0].Verdict.Attribution != "participant" {
		t.Fatalf("participant verdict attribution = %#v", evidence.Participants)
	}
}

func TestApplyQualificationEvidenceSeparatesMPIGroupAndParticipantObservation(t *testing.T) {
	t.Parallel()

	config := qualificationConfig(signalv1.ExecutionBackendMPIJob)
	config.Continuity = &signalv1.ContinuityPolicy{RequirePerDeviceCoverage: true}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	evidence := qualificationEvidenceSkeleton(2)
	measurements := `{"qualification":{"participants":[` +
		`{"participantIndex":0,"driverVersion":"1.0","gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-2","gpu-index-0","gpu-index-1"],"coveredDevices":["gpu-index-0","gpu-index-1","gpu-index-2"]},` +
		`{"participantIndex":1,"driverVersion":"1.0","gpuInventoryDigest":"sha256:27c73c2f1eb444c0c09847b6d533641e3c9e56327456793bb2e0d8c2ff78b9a0","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]}]}}`
	results := []Result{
		qualificationResult(0, "Pass", "group-passed", started),
		qualificationResult(1, "Pass", "group-passed", started),
	}
	results[0].Measurements = measurements
	results[1].Measurements = measurements

	applyQualificationEvidence(
		&evidence,
		config,
		results,
		qualificationFingerprints("boot-a", "boot-a"),
		qualificationFingerprints("boot-a", "boot-a"),
	)

	if evidence.GroupResult == nil || evidence.GroupResult.Verdict != "Pass" ||
		evidence.GroupResult.Attribution != "group" {
		t.Fatalf("MPI group result = %#v", evidence.GroupResult)
	}

	for index := range evidence.Participants {
		if evidence.Participants[index].Verdict.Attribution != "group-with-participant-observation" {
			t.Fatalf("participant %d attribution = %#v", index, evidence.Participants[index].Verdict)
		}
	}

	if evidence.Participants[0].Start.DriverVersion != "1.0" ||
		evidence.Participants[1].Start.DriverVersion != "1.0" ||
		evidence.Participants[0].Start.GPUInventoryDigest !=
			"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8" ||
		evidence.Participants[1].Start.GPUInventoryDigest !=
			"sha256:27c73c2f1eb444c0c09847b6d533641e3c9e56327456793bb2e0d8c2ff78b9a0" {
		t.Fatalf("per-participant fingerprint provenance = %#v", evidence.Participants)
	}

	evidence.Participants[1].CaptureError = "worker identity was not observed"
	applyQualificationEvidence(
		&evidence,
		config,
		results,
		qualificationFingerprints("boot-a", "boot-a"),
		qualificationFingerprints("boot-a", "boot-a"),
	)

	if evidence.Participants[1].Verdict.Verdict != "Inconclusive" {
		t.Fatalf("incomplete MPI participant verdict = %#v", evidence.Participants[1].Verdict)
	}

	evidence = qualificationEvidenceSkeleton(2)
	addHealthyMPIWorkers(&evidence)

	results[0].Measurements = `{"qualification":{"participants":[{"participantIndex":0,"driverVersion":"1.0","gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]}]}}`
	results[1].Measurements = results[0].Measurements
	applyQualificationEvidence(
		&evidence,
		config,
		results,
		qualificationFingerprints("boot-a", "boot-a"),
		qualificationFingerprints("boot-a", "boot-a"),
	)

	if evidence.Participants[1].Verdict.Reason != "ParticipantFingerprintNotProven" {
		t.Fatalf("missing MPI participant evidence = %#v", evidence.Participants[1].Verdict)
	}
}

func TestApplyQualificationEvidenceEnforcesContinuity(t *testing.T) {
	t.Parallel()

	maxGap := metav1.Duration{Duration: 10 * time.Second}
	config := qualificationConfig(signalv1.ExecutionBackendPod)
	config.MinDuration = &metav1.Duration{Duration: 20 * time.Second}
	config.Continuity = &signalv1.ContinuityPolicy{
		MaxSampleGap:             &maxGap,
		RequireSameBootID:        true,
		RequirePerDeviceCoverage: true,
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	result := qualificationResult(0, "Pass", "passed", started)
	result.FinishedAt = started.Add(30 * time.Second)
	result.SampleTimes = []time.Time{
		started.Add(5 * time.Second),
		started.Add(10 * time.Second),
		started.Add(20 * time.Second),
	}
	result.Measurements = `{"throughput":42,"qualification":{"driverVersion":"1.2.3","gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-1","gpu-index-0"],"coveredDevices":["gpu-index-0","gpu-index-1"]}}`

	evidence := qualificationEvidenceSkeleton(1)
	applyQualificationEvidence(
		&evidence,
		config,
		[]Result{result},
		qualificationFingerprints("boot-a"),
		qualificationFingerprints("boot-a"),
	)

	participant := evidence.Participants[0]
	if participant.Verdict.Verdict != "Pass" || !participant.PerDeviceCoverage ||
		participant.MaxObservedGap == nil || participant.MaxObservedGap.Duration > maxGap.Duration {
		t.Fatalf("complete continuity evidence = %#v", participant)
	}

	if participant.Start.DriverVersion != "1.2.3" ||
		participant.Start.GPUInventoryDigest == "" ||
		participant.Start.GPUInventoryDigest != participant.End.GPUInventoryDigest {
		t.Fatalf("fingerprint provenance = start %#v end %#v", participant.Start, participant.End)
	}

	cases := []struct {
		name       string
		mutate     func(*Result, []signalv1.NodeFingerprint)
		wantReason string
	}{
		{
			name: "missing samples",
			mutate: func(result *Result, _ []signalv1.NodeFingerprint) {
				result.SampleTimes = nil
			},
			wantReason: "SampleContinuityNotProven",
		},
		{
			name: "same boot mismatch",
			mutate: func(_ *Result, ends []signalv1.NodeFingerprint) {
				ends[0].BootID = "boot-b"
			},
			wantReason: "SameBootNotProven",
		},
		{
			name: "incomplete devices",
			mutate: func(result *Result, _ []signalv1.NodeFingerprint) {
				result.Measurements = `{"qualification":{"driverVersion":"1.2.3","gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-0","gpu-index-1"],"coveredDevices":["gpu-index-0"]}}`
			},
			wantReason: "PerDeviceCoverageNotProven",
		},
		{
			name: "malformed devices",
			mutate: func(result *Result, _ []signalv1.NodeFingerprint) {
				result.Measurements = `{"qualification":`
			},
			wantReason: "ParticipantFingerprintNotProven",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testResult := result
			testResult.SampleTimes = append([]time.Time(nil), result.SampleTimes...)
			ends := qualificationFingerprints("boot-a")
			testCase.mutate(&testResult, ends)

			testEvidence := qualificationEvidenceSkeleton(1)

			applyQualificationEvidence(
				&testEvidence,
				config,
				[]Result{testResult},
				qualificationFingerprints("boot-a"),
				ends,
			)

			if testEvidence.Participants[0].Verdict.Verdict != "Inconclusive" ||
				testEvidence.Participants[0].Verdict.Reason != testCase.wantReason {
				t.Fatalf("verdict = %#v", testEvidence.Participants[0].Verdict)
			}
		})
	}
}

func TestCanonicalGPUInventoryUsesNumericIndexOrder(t *testing.T) {
	t.Parallel()

	canonical, err := canonicalGPUInventory([]string{
		"gpu-index-2",
		"gpu-index-0",
		"gpu-index-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(canonical, "\x00") != "gpu-index-0\x00gpu-index-1\x00gpu-index-2" {
		t.Fatalf("canonical inventory = %#v", canonical)
	}

	for _, inventory := range [][]string{
		nil,
		{"gpu-0"},
		{"gpu-index-02"},
		{"gpu-index--1"},
		{"gpu-index-"},
		{"gpu-index-+1"},
		{"gpu-index- 1"},
		{"other-index-1"},
		{"gpu-index-2147483648"},
		{"gpu-index-0", "gpu-index-0"},
		{"gpu-index-0", "gpu-index-2"},
	} {
		if _, err := canonicalGPUInventory(inventory); err == nil {
			t.Fatalf("canonicalGPUInventory(%#v) unexpectedly succeeded", inventory)
		}
	}
}

func TestQualificationDigestIsIndependentOfRedactedDeviceOrder(t *testing.T) {
	t.Parallel()

	const digest = "sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8"
	for _, visible := range []string{
		`["gpu-index-2","gpu-index-0","gpu-index-1"]`,
		`["gpu-index-0","gpu-index-1","gpu-index-2"]`,
	} {
		measurements, err := decodeQualificationMeasurements(
			`{"qualification":{"driverVersion":"580.1","gpuInventoryDigest":"`+
				digest+`","visibleDevices":`+visible+`,"coveredDevices":`+visible+`}}`,
			0,
			string(signalv1.ExecutionBackendPod),
		)
		if err != nil {
			t.Fatal(err)
		}

		if measurements.GPUInventoryDigest != digest {
			t.Fatalf("explicit digest = %q, want %q", measurements.GPUInventoryDigest, digest)
		}
	}
}

func TestApplyQualificationEvidenceRequiresExplicitFingerprint(t *testing.T) {
	t.Parallel()

	config := qualificationConfig(signalv1.ExecutionBackendPod)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	for _, testCase := range []struct {
		name          string
		qualification string
	}{
		{
			name:          "missing digest",
			qualification: `"driverVersion":"580.1","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]`,
		},
		{
			name:          "invalid digest",
			qualification: `"driverVersion":"580.1","gpuInventoryDigest":"sha256:invalid","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]`,
		},
		{
			name:          "missing driver",
			qualification: `"gpuInventoryDigest":"sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8","visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := qualificationResult(0, "Pass", "passed", started)
			result.Measurements = `{"qualification":{` + testCase.qualification + `}}`
			evidence := qualificationEvidenceSkeleton(1)

			applyQualificationEvidence(
				&evidence,
				config,
				[]Result{result},
				qualificationFingerprints("boot-a"),
				qualificationFingerprints("boot-a"),
			)

			if evidence.Participants[0].Verdict.Verdict != "Inconclusive" ||
				evidence.Participants[0].Verdict.Reason != "ParticipantFingerprintNotProven" {
				t.Fatalf("verdict = %#v", evidence.Participants[0].Verdict)
			}
		})
	}
}

func TestBackendEvidenceBindingAndCompatibility(t *testing.T) {
	t.Parallel()

	config := qualificationConfig(signalv1.ExecutionBackendPod)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	evidence := qualificationEvidenceSkeleton(1)
	evidence.Identity = EvidenceIdentity{
		RunUID:     config.RunUID,
		CheckID:    config.CheckID,
		CheckOrder: config.CheckOrder,
		PlanDigest: config.PlanDigest,
	}
	evidence.Provenance = ExecutionProvenanceEvidence{
		AdapterImage: config.AdapterImage,
		RunnerImage:  config.RunnerImage,
	}
	applyQualificationEvidence(
		&evidence,
		config,
		[]Result{qualificationResult(0, "Pass", "passed", started)},
		qualificationFingerprints("boot-a"),
		qualificationFingerprints("boot-a"),
	)

	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidateBackendEvidence(string(content)); err != nil {
		t.Fatalf("ValidateBackendEvidence() error = %v", err)
	}

	if err := ValidateBackendEvidenceBinding(
		string(content),
		config.RunUID,
		config.CheckID,
		config.CheckOrder,
		config.PlanDigest,
	); err != nil {
		t.Fatalf("ValidateBackendEvidenceBinding() error = %v", err)
	}

	if err := ValidateBackendEvidenceBinding(
		string(content),
		"other-run",
		config.CheckID,
		config.CheckOrder,
		config.PlanDigest,
	); err == nil {
		t.Fatal("binding validation accepted evidence replayed to another Run")
	}

	legacyConfig := config
	legacyConfig.PlanDigest = ""
	legacyConfig.AdapterImage = ""
	legacyEvidence := qualificationEvidenceSkeleton(1)
	applyQualificationEvidence(
		&legacyEvidence,
		legacyConfig,
		[]Result{qualificationResult(0, "Pass", "passed", started)},
		qualificationFingerprints("boot-a"),
		qualificationFingerprints("boot-a"),
	)

	if legacyEvidence.Participants[0].Verdict.Reason != "QualificationIdentityUnavailable" {
		t.Fatalf("legacy evidence verdict = %#v", legacyEvidence.Participants[0].Verdict)
	}
}

func TestBackendEvidenceRemainsBounded(t *testing.T) {
	t.Parallel()

	evidence := qualificationEvidenceSkeleton(1)
	evidence.CaptureError = strings.Repeat("x", MaxBackendEvidenceBytes)
	evidence.Participants[0].CaptureError = strings.Repeat("y", MaxBackendEvidenceBytes)
	evidence.Participants[0].Pods = []PodEvidence{{
		Name:     "pod",
		UID:      "pod-uid",
		Role:     "participant",
		NodeName: "node-0",
		NodeUID:  "node-uid-0",
		Containers: []ContainerEvidence{{
			Name: "runner",
			CurrentLog: CapturedLogEvidence{
				Status:  "captured",
				Content: strings.Repeat("z", MaxBackendEvidenceBytes),
			},
		}},
	}}

	content, complete, err := marshalBackendEvidence(evidence, MaxBackendEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}

	if complete || len(content) > MaxBackendEvidenceBytes {
		t.Fatalf("bounded evidence complete=%t bytes=%d", complete, len(content))
	}
}

func qualificationConfig(backend signalv1.ExecutionBackend) Config {
	return Config{
		RunName:      "run",
		RunUID:       "run-uid",
		CheckID:      "R01",
		CheckOrder:   0,
		PlanDigest:   "sha256:" + strings.Repeat("a", 64),
		AdapterImage: "example.invalid/adapter@sha256:" + strings.Repeat("b", 64),
		RunnerImage:  "example.invalid/runner@sha256:" + strings.Repeat("c", 64),
		BackendKind:  string(backend),
	}
}

func qualificationEvidenceSkeleton(participantCount int) BackendEvidence {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	participants := make([]ParticipantBackendEvidence, participantCount)
	for index := range participants {
		participants[index] = ParticipantBackendEvidence{
			ParticipantIndex: index,
			Resource: BackendObjectEvidence{
				APIVersion: "v1",
				Kind:       "Pod",
				Namespace:  "signal",
				Name:       "backend",
				UID:        "backend-uid",
			},
		}
	}

	return BackendEvidence{
		Version: backendEvidenceVersion,
		Timing: CheckTimingEvidence{
			StartedAt:        metav1.NewTime(started),
			FinishedAt:       metav1.NewTime(started.Add(time.Minute)),
			ObservedDuration: metav1.Duration{Duration: time.Minute},
			SampleCount:      2,
		},
		Complete:     true,
		Participants: participants,
	}
}

func qualificationResult(index int, verdict, reason string, started time.Time) Result {
	return Result{
		ParticipantIndex: index,
		ExecutionState:   "Succeeded",
		Verdict:          verdict,
		Reason:           reason,
		StartedAt:        started,
		FinishedAt:       started.Add(time.Minute),
		SampleTimes:      []time.Time{started.Add(10 * time.Second), started.Add(20 * time.Second)},
	}
}

func addHealthyMPIWorkers(evidence *BackendEvidence) {
	for index := range evidence.Participants {
		evidence.Participants[index].Pods = []PodEvidence{{
			Name:     "worker",
			UID:      types.UID("worker-uid-" + string(rune('0'+index))),
			Role:     "worker",
			NodeName: "node",
			NodeUID:  types.UID("node-uid-" + string(rune('0'+index))),
			Phase:    corev1.PodSucceeded,
			Containers: []ContainerEvidence{{
				Name:  "runner",
				State: ContainerStateEvidence{State: "terminated", ExitCode: 0},
			}},
		}}
	}
}

func qualificationFingerprints(bootIDs ...string) []signalv1.NodeFingerprint {
	fingerprints := make([]signalv1.NodeFingerprint, len(bootIDs))
	for index := range bootIDs {
		fingerprints[index] = signalv1.NodeFingerprint{
			NodeUID:        types.UID("node-uid-" + string(rune('0'+index))),
			BootID:         bootIDs[index],
			KernelVersion:  "kernel",
			OSImage:        "os",
			KubeletVersion: "kubelet",
			SystemUUID:     "system",
		}
	}

	return fingerprints
}
