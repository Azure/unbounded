// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	fixtureDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixtureDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixtureDigestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	fixtureDigestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	fixtureDigestE = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	fixtureImageA  = "example.invalid/vllm@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestGenericVLLMContractFixture(t *testing.T) {
	t.Parallel()

	test, suite, run := genericVLLMContractFixture(t)
	if test.Spec.Driver.Type != TestDriverTypeWorkflowTemplate ||
		test.Status.Trust == nil ||
		test.Status.Trust.State != CertificationTestTrustApproved {
		t.Fatal("vLLM fixture is not an approved generic WorkflowTemplate test")
	}

	stage := suite.Spec.Phases[0].Stages[0]
	if stage.Name != "vllm-smoke" || stage.Schedule.Mode != CheckExecutionModeRunOnce {
		t.Fatalf("vLLM Suite stage = %q/%q, want vllm-smoke/RunOnce", stage.Name, stage.Schedule.Mode)
	}

	if err := ValidateStageSchedule(stage.Schedule); err != nil {
		t.Fatalf("validate vLLM RunOnce stage: %v", err)
	}

	for name, schedule := range map[string]StageSchedule{
		"continuous": {
			Mode:              CheckExecutionModeContinuous,
			InvocationTimeout: metav1.Duration{Duration: 5 * time.Minute},
			CoverageWindow:    durationPointer(time.Hour),
			MinExecutions:     int32Pointer(12),
			MaxGap:            durationPointer(6 * time.Minute),
		},
		"periodic": {
			Mode:              CheckExecutionModePeriodic,
			InvocationTimeout: metav1.Duration{Duration: 5 * time.Minute},
			CoverageWindow:    durationPointer(time.Hour),
			Interval:          durationPointer(10 * time.Minute),
			MinExecutions:     int32Pointer(6),
			MaxGap:            durationPointer(12 * time.Minute),
		},
	} {
		if err := ValidateStageSchedule(schedule); err != nil {
			t.Fatalf("validate vLLM %s schedule: %v", name, err)
		}
	}

	if run.Spec.PlanDigest == "" {
		t.Fatal("vLLM fixture Run has no derived plan digest")
	}
}

func TestGenericVLLMFixtureDigestSensitivity(t *testing.T) {
	t.Parallel()

	_, _, base := genericVLLMContractFixture(t)
	want := base.Spec.PlanDigest

	tests := []struct {
		name   string
		mutate func(*CertificationRun)
	}{
		{
			name: "resolved image",
			mutate: func(run *CertificationRun) {
				input := &run.Spec.ResolvedPlan.Phases[0].Stages[0].ResolvedInputs[0]
				input.Resolved = "example.invalid/vllm@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				input.Digest = fixtureDigestB
			},
		},
		{
			name: "resolved model",
			mutate: func(run *CertificationRun) {
				input := &run.Spec.ResolvedPlan.Phases[0].Stages[0].ResolvedInputs[1]
				input.Resolved = "model-revision-2"
				input.Digest = fixtureDigestC
			},
		},
		{
			name: "workload shape",
			mutate: func(run *CertificationRun) {
				value := int64(512)
				run.Spec.ResolvedPlan.Phases[0].Stages[0].Parameters[3].Value.IntegerValue = &value
			},
		},
		{
			name: "threshold",
			mutate: func(run *CertificationRun) {
				value := "25.0"
				run.Spec.ResolvedPlan.Phases[0].Stages[0].Assertions[0].Expected.DecimalValue = &value
			},
		},
		{
			name: "duration",
			mutate: func(run *CertificationRun) {
				run.Spec.ResolvedPlan.Phases[0].Stages[0].Schedule.InvocationTimeout = metav1.Duration{Duration: 15 * time.Minute}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			run := base.DeepCopy()
			tt.mutate(run)

			got, err := deriveFixturePlanDigest(run)
			if err != nil {
				t.Fatalf("derive mutated fixture digest: %v", err)
			}

			if got == want {
				t.Fatalf("%s change retained plan digest %q", tt.name, got)
			}
		})
	}
}

func TestIncompleteMutableVLLMInputCannotQualify(t *testing.T) {
	t.Parallel()

	_, _, run := genericVLLMContractFixture(t)
	stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
	model := &stage.ResolvedInputs[1]
	model.State = ResolvedInputStateIncomplete
	model.Resolved = ""
	model.Digest = ""
	model.IncompleteReason = "registry did not provide an immutable manifest digest"

	if _, err := deriveFixturePlanDigest(run); err == nil {
		t.Fatal("qualification-eligible vLLM stage accepted an incomplete mutable model input")
	}

	stage.QualificationEligible = false

	if _, err := deriveFixturePlanDigest(run); err == nil {
		t.Fatal("non-diagnostic vLLM stage accepted an incomplete mutable model input")
	}

	stage.DiagnosticOnly = true

	if _, err := deriveFixturePlanDigest(run); err != nil {
		t.Fatalf("diagnostic-only vLLM stage rejected explicit incomplete input: %v", err)
	}
}

func TestResolvedStageRejectsUnsafeSecurityAndTopology(t *testing.T) {
	t.Parallel()

	_, _, base := genericVLLMContractFixture(t)

	t.Run("unsafe host path", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].Test.Security.HostPaths = []WorkloadHostPath{{
			MountPath: "/host",
			Path:      "/",
		}}

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a host path outside approved roots")
		}
	})

	t.Run("proc magic path", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].Test.Security.HostPaths = []WorkloadHostPath{{
			MountPath: "/host-root",
			Path:      "/proc/1/root",
			ReadOnly:  true,
		}}

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a proc path that can escape the approved root")
		}
	})

	t.Run("reserved mount descendant", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].Test.Security.HostPaths = []WorkloadHostPath{{
			MountPath: "/scripts/override",
			Path:      "/sys/class",
			ReadOnly:  true,
		}}

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a mount below a reserved path")
		}
	})

	t.Run("writable sys path", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].Test.Security.HostPaths = []WorkloadHostPath{{
			MountPath: "/host-sys",
			Path:      "/sys/class",
		}}

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a writable sys host path")
		}
	})

	t.Run("per-device coverage on RunOnce", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].PerDeviceCoverageInterval = durationPointer(time.Minute)

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted per-device interval coverage on RunOnce")
		}
	})

	t.Run("duplicate role assignment", func(t *testing.T) {
		run := base.DeepCopy()
		stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
		stage.Test.Topology = TestTopology{
			MinParticipants: 1,
			MaxParticipants: 1,
			Roles: []TestParticipantRole{
				{Name: "client", DisplayName: "Client", MinCount: 1, MaxCount: 1},
				{Name: "server", DisplayName: "Server", MinCount: 1, MaxCount: 1},
			},
		}
		stage.ParticipantRoles = []ResolvedStageParticipantRoleBinding{
			{Role: "client", ParticipantIndexes: []int32{0}},
			{Role: "server", ParticipantIndexes: []int32{0}},
		}

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted one participant in multiple roles")
		}
	})

	t.Run("rejected diagnostic test", func(t *testing.T) {
		run := base.DeepCopy()
		stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
		stage.Test.Identity.Trust = CertificationTestTrustRejected
		stage.QualificationEligible = false
		stage.DiagnosticOnly = true

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a rejected diagnostic test")
		}
	})

	t.Run("invalid incomplete marker", func(t *testing.T) {
		run := base.DeepCopy()
		stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
		stage.QualificationEligible = false
		stage.DiagnosticOnly = true
		input := &stage.ResolvedInputs[0]
		input.State = ResolvedInputStateIncomplete
		input.IncompleteReason = "digest unavailable"

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted an incomplete input that still claimed a digest")
		}
	})

	t.Run("complete input with incomplete reason", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].ResolvedInputs[0].IncompleteReason = "not complete"

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a complete input with incompleteReason")
		}
	})

	t.Run("whitespace complete identity", func(t *testing.T) {
		run := base.DeepCopy()
		run.Spec.ResolvedPlan.Phases[0].Stages[0].ResolvedInputs[0].Resolved = "   "

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a whitespace-only complete identity")
		}
	})

	t.Run("whitespace incomplete reason", func(t *testing.T) {
		run := base.DeepCopy()
		stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
		stage.QualificationEligible = false
		stage.DiagnosticOnly = true
		input := &stage.ResolvedInputs[0]
		input.State = ResolvedInputStateIncomplete
		input.Resolved = ""
		input.Digest = ""
		input.IncompleteReason = "   "

		if _, err := deriveFixturePlanDigest(run); err == nil {
			t.Fatal("resolved plan accepted a whitespace-only incomplete reason")
		}
	})
}

func TestFifteenScriptBaselineMapsToIndependentTests(t *testing.T) {
	t.Parallel()

	tests := make([]CertificationTest, 15)
	stages := make([]CertificationStage, 15)

	for index := range tests {
		name := fmt.Sprintf("baseline-%02d", index+1)
		digest := fmt.Sprintf("sha256:%064x", index+1)
		tests[index] = CertificationTest{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				UID:        types.UID(name + "-uid"),
				Generation: 1,
			},
			Spec: CertificationTestSpec{
				DisplayName: name,
				Owner:       "project-signal",
				Version:     "1",
				Metrics: []TestMetricDefinition{{
					Name:        "passed",
					DisplayName: "Passed",
					Type:        MetricValueTypeBoolean,
					Required:    true,
				}},
				Topology: TestTopology{MinParticipants: 1, MaxParticipants: 1},
				Driver: CertificationTestDriver{
					Type: TestDriverTypeScript,
					Script: &ScriptTestDriver{
						Language: ScriptLanguageShell,
						Source: ImmutableArtifactSource{
							Type: ArtifactSourceTypeConfigMap,
							ConfigMap: &ConfigMapArtifactSource{
								Name:   "dapple-baseline-scripts",
								Key:    name + ".sh",
								Digest: digest,
							},
						},
						RunnerImage: fixtureImageA,
					},
				},
			},
			Status: CertificationTestStatus{
				Trust: &CertificationTestTrustStatus{
					State:         CertificationTestTrustApproved,
					ContentDigest: digest,
				},
			},
		}
		stages[index] = CertificationStage{
			Name: name,
			Test: CertificationTestReference{
				Name:          name,
				UID:           tests[index].UID,
				Generation:    1,
				ContentDigest: digest,
			},
			Schedule: StageSchedule{
				Mode:              CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: 10 * time.Minute},
			},
			Assertions: []TestAssertion{{
				Name:     "passed",
				Metric:   "passed",
				Operator: AssertionOperatorBooleanEquals,
				Expected: booleanValuePointer(true),
			}},
		}
	}

	suite := CertificationSuite{
		Spec: CertificationSuiteSpec{
			Phases: []CertificationPhase{{
				Name:   "validation",
				Role:   CheckStageRoleValidation,
				Stages: stages,
			}},
		},
	}

	content, err := json.Marshal(suite)
	if err != nil {
		t.Fatalf("marshal baseline Suite: %v", err)
	}

	var decoded CertificationSuite
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("decode baseline Suite: %v", err)
	}

	if len(decoded.Spec.Phases[0].Stages) != 15 {
		t.Fatalf("baseline stage count = %d, want 15", len(decoded.Spec.Phases[0].Stages))
	}

	for index, stage := range decoded.Spec.Phases[0].Stages {
		want := fmt.Sprintf("baseline-%02d", index+1)
		if stage.Name != want || stage.Test.Name != want {
			t.Fatalf("baseline stage %d identity = %q/%q, want %q", index, stage.Name, stage.Test.Name, want)
		}
	}
}

func genericVLLMContractFixture(t *testing.T) (*CertificationTest, *CertificationSuite, *CertificationRun) {
	t.Helper()

	parameters := []StageParameterBinding{
		{Name: "imageChannel", Value: stringValue("latest")},
		{Name: "model", Value: stringValue("example/model")},
		{Name: "modelRevision", Value: stringValue("latest")},
		{Name: "promptTokens", Value: integerValue(256)},
		{Name: "outputTokens", Value: integerValue(128)},
		{Name: "concurrency", Value: integerValue(16)},
		{Name: "duration", Value: durationValue(5 * time.Minute)},
	}
	metrics := []TestMetricDefinition{
		{Name: "requestThroughput", DisplayName: "Request throughput", Type: MetricValueTypeThroughput, Required: true},
		{Name: "tokenThroughput", DisplayName: "Token throughput", Type: MetricValueTypeThroughput, Required: true},
		{Name: "ttftP95", DisplayName: "TTFT p95", Type: MetricValueTypeLatency, Unit: "ms", Required: true},
		{Name: "tpotP95", DisplayName: "TPOT p95", Type: MetricValueTypeLatency, Unit: "ms", Required: true},
		{Name: "latencyP99", DisplayName: "Latency p99", Type: MetricValueTypeLatency, Unit: "ms", Required: true},
		{Name: "requestCount", DisplayName: "Request count", Type: MetricValueTypeCount, Required: true},
		{Name: "errorCount", DisplayName: "Error count", Type: MetricValueTypeCount, Required: true},
		{Name: "modelIdentity", DisplayName: "Model identity", Type: MetricValueTypeStatus, Required: true},
		{Name: "gpuCoverage", DisplayName: "GPU coverage", Type: MetricValueTypeRatio, Required: true},
	}
	assertions := []TestAssertion{
		{Name: "request-throughput", Metric: "requestThroughput", Operator: AssertionOperatorMinimum, Expected: decimalValuePointer("20.0")},
		{Name: "token-throughput", Metric: "tokenThroughput", Operator: AssertionOperatorMinimum, Expected: decimalValuePointer("1000.0")},
		{Name: "ttft-p95", Metric: "ttftP95", Operator: AssertionOperatorMaximum, Expected: durationValuePointer(2 * time.Second)},
		{Name: "error-rate", Metric: "errorCount", Operator: AssertionOperatorErrorRateMaximum, Expected: decimalValuePointer("0.01")},
	}

	test := &CertificationTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "vllm-serving",
			UID:        types.UID("vllm-serving-uid"),
			Generation: 1,
		},
		Spec: CertificationTestSpec{
			DisplayName: "vLLM serving qualification",
			Description: "Generic approved WorkflowTemplate contract fixture for serving qualification.",
			Owner:       "project-signal",
			Version:     "1",
			Parameters: []TestParameterDefinition{
				{Name: "imageChannel", DisplayName: "Image channel", Type: TypedValueTypeString, Required: true},
				{Name: "model", DisplayName: "Model", Type: TypedValueTypeString, Required: true},
				{Name: "modelRevision", DisplayName: "Model revision", Type: TypedValueTypeString, Required: true},
				{Name: "promptTokens", DisplayName: "Prompt tokens", Type: TypedValueTypeInteger, Required: true},
				{Name: "outputTokens", DisplayName: "Output tokens", Type: TypedValueTypeInteger, Required: true},
				{Name: "concurrency", DisplayName: "Concurrency", Type: TypedValueTypeInteger, Required: true},
				{Name: "duration", DisplayName: "Duration", Type: TypedValueTypeDuration, Required: true},
			},
			Metrics:  metrics,
			Topology: TestTopology{MinParticipants: 1, MaxParticipants: 1},
			Driver: CertificationTestDriver{
				Type: TestDriverTypeWorkflowTemplate,
				WorkflowTemplate: &WorkflowTemplateTestDriver{
					Name:   "vllm-qualification",
					Digest: fixtureDigestA,
				},
			},
		},
		Status: CertificationTestStatus{
			ObservedGeneration: 1,
			Trust: &CertificationTestTrustStatus{
				State:         CertificationTestTrustApproved,
				ContentDigest: fixtureDigestB,
			},
		},
	}

	testReference := CertificationTestReference{
		Name:          test.Name,
		UID:           test.UID,
		Generation:    test.Generation,
		ContentDigest: fixtureDigestB,
	}
	stage := CertificationStage{
		Name:        "vllm-smoke",
		DisplayName: "vLLM smoke",
		Test:        testReference,
		Parameters:  parameters,
		Schedule: StageSchedule{
			Mode:              CheckExecutionModeRunOnce,
			InvocationTimeout: metav1.Duration{Duration: 10 * time.Minute},
		},
		Assertions: assertions,
	}
	suite := &CertificationSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "vllm-smoke",
			UID:        types.UID("vllm-smoke-suite-uid"),
			Generation: 1,
		},
		Spec: CertificationSuiteSpec{
			Phases: []CertificationPhase{{
				Name:        "validation",
				DisplayName: "Validation",
				Role:        CheckStageRoleValidation,
				Stages:      []CertificationStage{stage},
			}},
		},
	}

	run := &CertificationRun{
		Spec: CertificationRunSpec{
			Profile: PinnedProfile{Name: "vllm", UID: types.UID("vllm-profile-uid"), Generation: 1},
			Suite:   PinnedSuite{Name: suite.Name, UID: suite.UID, Generation: suite.Generation},
			Participants: []Participant{{
				NodeName: "worker-0",
				NodeUID:  types.UID("worker-0-uid"),
			}},
			Environment: &CertificationEnvironmentBindings{Namespace: "signal-qualification"},
			ResolvedPlan: &ResolvedCertificationPlan{
				Phases: []ResolvedPhasePlan{{
					Name:        "validation",
					DisplayName: "Validation",
					Role:        CheckStageRoleValidation,
					Stages: []ResolvedStagePlan{{
						Name:        stage.Name,
						DisplayName: stage.DisplayName,
						Test: ResolvedCertificationTest{
							Identity: TestIdentitySnapshot{
								Name:          test.Name,
								UID:           test.UID,
								Generation:    test.Generation,
								ContentDigest: fixtureDigestB,
								Trust:         CertificationTestTrustApproved,
							},
							ParameterSchemaDigest:      fixtureDigestC,
							MetricSchemaDigest:         fixtureDigestD,
							SecurityRequirementsDigest: fixtureDigestE,
							Metrics:                    metrics,
							Topology:                   test.Spec.Topology,
							Driver:                     test.Spec.Driver,
						},
						Parameters: parameters,
						Schedule:   stage.Schedule,
						Assertions: assertions,
						ResolvedInputs: []ResolvedInputIdentity{
							{Name: "image", Kind: ResolvedInputKindImage, Requested: "latest", State: ResolvedInputStateComplete, Resolved: fixtureImageA, Digest: fixtureDigestA},
							{Name: "model", Kind: ResolvedInputKindModel, Requested: "example/model@latest", State: ResolvedInputStateComplete, Resolved: "model-revision-1", Digest: fixtureDigestB},
							{Name: "tokenizer", Kind: ResolvedInputKindTokenizer, Requested: "example/model@latest", State: ResolvedInputStateComplete, Resolved: "tokenizer-revision-1", Digest: fixtureDigestC},
							{Name: "workflow", Kind: ResolvedInputKindWorkflowTemplate, Requested: "vllm-qualification", State: ResolvedInputStateComplete, Resolved: "vllm-qualification@1", Digest: fixtureDigestA},
						},
						QualificationEligible: true,
					}},
				}},
			},
			PlanProvenance: &ExecutionPlanProvenance{
				ControllerImage:        "example.invalid/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				AdapterImage:           "example.invalid/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				WorkflowTemplateDigest: fixtureDigestA,
				ResolverImages: []string{
					"example.invalid/resolver@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				},
			},
		},
	}

	digest, err := deriveFixturePlanDigest(run)
	if err != nil {
		t.Fatalf("derive vLLM fixture plan digest: %v", err)
	}

	run.Spec.PlanDigest = digest

	return test, suite, run
}

func deriveFixturePlanDigest(run *CertificationRun) (string, error) {
	return DeriveResolvedExecutionPlanDigest(
		int32(len(run.Spec.Participants)),
		run.Spec.Environment,
		*run.Spec.ResolvedPlan,
		*run.Spec.PlanProvenance,
	)
}

func stringValue(value string) TypedValue {
	return TypedValue{Type: TypedValueTypeString, StringValue: &value}
}

func integerValue(value int64) TypedValue {
	return TypedValue{Type: TypedValueTypeInteger, IntegerValue: &value}
}

func durationValue(value time.Duration) TypedValue {
	return TypedValue{Type: TypedValueTypeDuration, DurationValue: durationPointer(value)}
}

func decimalValuePointer(value string) *TypedValue {
	return &TypedValue{Type: TypedValueTypeDecimal, DecimalValue: &value}
}

func booleanValuePointer(value bool) *TypedValue {
	return &TypedValue{Type: TypedValueTypeBoolean, BooleanValue: &value}
}

func durationValuePointer(value time.Duration) *TypedValue {
	return &TypedValue{Type: TypedValueTypeDuration, DurationValue: durationPointer(value)}
}

func durationPointer(value time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: value}
}

func int32Pointer(value int32) *int32 {
	return &value
}
