// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testScriptBundleDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testScriptDigest         = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRunnerImage          = "example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testAdapterImage         = "example.invalid/adapter@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testWorkflowTemplateHash = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	testResolverImage        = "example.invalid/resolver@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func TestDeriveExecutionPlanDigest(t *testing.T) {
	t.Parallel()

	baseChecks := func() []CertificationCheckSnapshot {
		minDuration := metav1.Duration{Duration: 5 * time.Minute}
		maxSampleGap := metav1.Duration{Duration: 30 * time.Second}
		coverageWindow := metav1.Duration{Duration: 8 * time.Minute}
		iterationTimeout := metav1.Duration{Duration: time.Minute}
		interval := metav1.Duration{Duration: 2 * time.Minute}
		minExecutions := int32(4)
		maxGap := metav1.Duration{Duration: 3 * time.Minute}

		return []CertificationCheckSnapshot{{
			Script: ScriptSnapshot{
				Key:    "check.sh",
				Digest: testScriptDigest,
			},
			RunnerImage: testRunnerImage,
			Backend:     ExecutionBackendPod,
			WorkloadPolicy: WorkloadPolicy{
				HostNetwork: true,
			},
			BackendActiveDeadline: metav1.Duration{Duration: 10 * time.Minute},
			MinDuration:           &minDuration,
			Continuity: &ContinuityPolicy{
				MaxSampleGap:             &maxSampleGap,
				RequireSameBootID:        true,
				RequirePerDeviceCoverage: true,
			},
			Execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModePeriodic,
				CoverageWindow:   &coverageWindow,
				IterationTimeout: &iterationTimeout,
				Interval:         &interval,
				MinExecutions:    &minExecutions,
				MaxGap:           &maxGap,
			},
		}}
	}

	baseProvenance := func() ExecutionPlanProvenance {
		return ExecutionPlanProvenance{
			AdapterImage:           testAdapterImage,
			WorkflowTemplateDigest: testWorkflowTemplateHash,
		}
	}

	want, err := DeriveExecutionPlanDigest(2, testScriptBundleDigest, baseChecks(), baseProvenance())
	if err != nil {
		t.Fatalf("derive base plan digest: %v", err)
	}

	got, err := DeriveExecutionPlanDigest(2, testScriptBundleDigest, baseChecks(), baseProvenance())
	if err != nil {
		t.Fatalf("derive equivalent plan digest: %v", err)
	}

	if got != want {
		t.Fatalf("equivalent plan digest = %q, want %q", got, want)
	}

	tests := []struct {
		name         string
		participants int32
		bundle       string
		mutate       func([]CertificationCheckSnapshot, *ExecutionPlanProvenance)
	}{
		{
			name:         "participant count",
			participants: 3,
		},
		{
			name:   "script bundle",
			bundle: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name: "script digest",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Script.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			},
		},
		{
			name: "runner image",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].RunnerImage = "example.invalid/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222"
			},
		},
		{
			name: "workload policy",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].WorkloadPolicy.HostNetwork = false
			},
		},
		{
			name: "minimum duration",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].MinDuration = &metav1.Duration{Duration: 6 * time.Minute}
			},
		},
		{
			name: "continuity",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Continuity.RequireSameBootID = false
			},
		},
		{
			name: "execution role and mode",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Execution = &CheckExecutionPolicy{
					Role: CheckStageRoleStartup,
					Mode: CheckExecutionModeRunOnce,
				}
			},
		},
		{
			name: "execution coverage window",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Execution.CoverageWindow = &metav1.Duration{Duration: 9 * time.Minute}
			},
		},
		{
			name: "execution iteration timeout",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Execution.IterationTimeout = &metav1.Duration{Duration: 90 * time.Second}
			},
		},
		{
			name: "execution interval",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Execution.Interval = &metav1.Duration{Duration: 90 * time.Second}
			},
		},
		{
			name: "execution minimum",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				minExecutions := int32(3)
				checks[0].Execution.MinExecutions = &minExecutions
			},
		},
		{
			name: "execution max gap",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Execution.MaxGap = &metav1.Duration{Duration: 4 * time.Minute}
			},
		},
		{
			name: "adapter image",
			mutate: func(_ []CertificationCheckSnapshot, provenance *ExecutionPlanProvenance) {
				provenance.AdapterImage = "example.invalid/adapter@sha256:3333333333333333333333333333333333333333333333333333333333333333"
			},
		},
		{
			name: "workflow template",
			mutate: func(_ []CertificationCheckSnapshot, provenance *ExecutionPlanProvenance) {
				provenance.WorkflowTemplateDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			participants := tt.participants
			if participants == 0 {
				participants = 2
			}

			bundle := tt.bundle
			if bundle == "" {
				bundle = testScriptBundleDigest
			}

			checks := baseChecks()

			provenance := baseProvenance()
			if tt.mutate != nil {
				tt.mutate(checks, &provenance)
			}

			digest, err := DeriveExecutionPlanDigest(participants, bundle, checks, provenance)
			if err != nil {
				t.Fatalf("derive changed plan digest: %v", err)
			}

			if digest == want {
				t.Fatalf("changed execution semantics retained digest %q", digest)
			}
		})
	}
}

func TestDeriveExecutionPlanDigestRejectsMutableInputs(t *testing.T) {
	t.Parallel()

	checks := []CertificationCheckSnapshot{{
		Script: ScriptSnapshot{
			Key:    "check.sh",
			Digest: testScriptDigest,
		},
		RunnerImage:           "example.invalid/runner:latest",
		Backend:               ExecutionBackendPod,
		BackendActiveDeadline: metav1.Duration{Duration: time.Minute},
	}}
	provenance := ExecutionPlanProvenance{
		AdapterImage:           testAdapterImage,
		WorkflowTemplateDigest: testWorkflowTemplateHash,
	}

	if _, err := DeriveExecutionPlanDigest(1, testScriptBundleDigest, checks, provenance); err == nil {
		t.Fatal("derive plan digest accepted an unpinned runner image")
	}

	checks[0].RunnerImage = testRunnerImage

	provenance.AdapterImage = "example.invalid/adapter:latest"
	if _, err := DeriveExecutionPlanDigest(1, testScriptBundleDigest, checks, provenance); err == nil {
		t.Fatal("derive plan digest accepted an unpinned adapter image")
	}

	provenance.AdapterImage = testAdapterImage

	checks[0].Execution = &CheckExecutionPolicy{
		Role: CheckStageRoleStartup,
		Mode: CheckExecutionModeContinuous,
	}
	if _, err := DeriveExecutionPlanDigest(1, testScriptBundleDigest, checks, provenance); err == nil {
		t.Fatal("derive plan digest accepted an invalid execution policy")
	}
}

func TestDigestPinnedImageValidation(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{
		testRunnerImage,
		"registry.example.invalid:5000/path/image@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if !IsDigestPinnedImage(valid) {
			t.Fatalf("IsDigestPinnedImage(%q) = false, want true", valid)
		}
	}

	for _, invalid := range []string{
		"",
		"example.invalid/runner:latest",
		"example.invalid/runner@sha512:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"example.invalid/runner@sha256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
	} {
		if IsDigestPinnedImage(invalid) {
			t.Fatalf("IsDigestPinnedImage(%q) = true, want false", invalid)
		}
	}
}

func TestDeriveResolvedExecutionPlanDigest(t *testing.T) {
	t.Parallel()

	stringValue := func(value string) TypedValue {
		return TypedValue{
			Type:        TypedValueTypeString,
			StringValue: &value,
		}
	}

	basePlan := func() ResolvedCertificationPlan {
		return ResolvedCertificationPlan{
			Phases: []ResolvedPhasePlan{{
				Name: "validation",
				Role: CheckStageRoleValidation,
				Stages: []ResolvedStagePlan{{
					Name: "load",
					Test: ResolvedCertificationTest{
						Identity: TestIdentitySnapshot{
							Name:          "load-test",
							UID:           types.UID("test-uid"),
							Generation:    3,
							ContentDigest: testScriptDigest,
							Trust:         CertificationTestTrustApproved,
						},
						ParameterSchemaDigest:      testScriptBundleDigest,
						MetricSchemaDigest:         testScriptDigest,
						SecurityRequirementsDigest: testWorkflowTemplateHash,
						Metrics: []TestMetricDefinition{{
							Name:        "throughput",
							DisplayName: "Throughput",
							Type:        MetricValueTypeThroughput,
							Required:    true,
						}},
						Topology: TestTopology{
							MinParticipants: 2,
							MaxParticipants: 2,
							Roles: []TestParticipantRole{
								{
									Name:        "requester",
									DisplayName: "Requester",
									MinCount:    1,
									MaxCount:    1,
								},
								{
									Name:        "worker",
									DisplayName: "Worker",
									MinCount:    1,
									MaxCount:    1,
								},
							},
						},
						Driver: CertificationTestDriver{
							Type: TestDriverTypeContainer,
							Container: &ContainerTestDriver{
								Image: testRunnerImage,
							},
						},
					},
					Parameters: []StageParameterBinding{{
						Name:  "model",
						Value: stringValue("requested-model"),
					}},
					ParticipantRoles: []ResolvedStageParticipantRoleBinding{
						{
							Role:               "requester",
							ParticipantIndexes: []int32{0},
						},
						{
							Role:               "worker",
							ParticipantIndexes: []int32{1},
						},
					},
					Schedule: StageSchedule{
						Mode:              CheckExecutionModeRunOnce,
						InvocationTimeout: metav1.Duration{Duration: 5 * time.Minute},
					},
					Assertions: []TestAssertion{{
						Name:     "throughput-floor",
						Metric:   "throughput",
						Operator: AssertionOperatorMinimum,
						Expected: func() *TypedValue {
							value := stringValue("100")

							return &value
						}(),
					}},
					ResolvedInputs: []ResolvedInputIdentity{{
						Name:      "model",
						Kind:      ResolvedInputKindModel,
						Requested: "latest",
						State:     ResolvedInputStateComplete,
						Resolved:  "revision-1",
						Digest:    testScriptBundleDigest,
					}},
					QualificationEligible: true,
				}},
			}},
		}
	}

	baseEnvironment := func() *CertificationEnvironmentBindings {
		return &CertificationEnvironmentBindings{
			Namespace: "signal",
		}
	}

	baseProvenance := func() ExecutionPlanProvenance {
		return ExecutionPlanProvenance{
			ControllerImage:        testRunnerImage,
			AdapterImage:           testAdapterImage,
			WorkflowTemplateDigest: testWorkflowTemplateHash,
			ResolverImages:         []string{testRunnerImage},
		}
	}

	want, err := DeriveResolvedExecutionPlanDigest(
		2,
		baseEnvironment(),
		basePlan(),
		baseProvenance(),
	)
	if err != nil {
		t.Fatalf("derive resolved plan digest: %v", err)
	}

	got, err := DeriveResolvedExecutionPlanDigest(
		2,
		baseEnvironment(),
		basePlan(),
		baseProvenance(),
	)
	if err != nil {
		t.Fatalf("derive equivalent resolved plan digest: %v", err)
	}

	if got != want {
		t.Fatalf("equivalent resolved plan digest = %q, want %q", got, want)
	}

	tests := []struct {
		name   string
		mutate func(*CertificationEnvironmentBindings, *ResolvedCertificationPlan, *ExecutionPlanProvenance)
	}{
		{
			name: "environment",
			mutate: func(environment *CertificationEnvironmentBindings, _ *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				environment.Namespace = "other"
			},
		},
		{
			name: "test identity",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].Test.Identity.ContentDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			},
		},
		{
			name: "driver",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].Test.Driver.Container.Image = "example.invalid/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222"
			},
		},
		{
			name: "schedule",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].Schedule.InvocationTimeout = metav1.Duration{Duration: 6 * time.Minute}
			},
		},
		{
			name: "parameter",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].Parameters[0].Value = stringValue("other-model")
			},
		},
		{
			name: "participant role assignment",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].ParticipantRoles[0].ParticipantIndexes = []int32{1}
				plan.Phases[0].Stages[0].ParticipantRoles[1].ParticipantIndexes = []int32{0}
			},
		},
		{
			name: "assertion",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				value := stringValue("200")
				plan.Phases[0].Stages[0].Assertions[0].Expected = &value
			},
		},
		{
			name: "resolved input",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				plan.Phases[0].Stages[0].ResolvedInputs[0].Digest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
			},
		},
		{
			name: "trust decision",
			mutate: func(_ *CertificationEnvironmentBindings, plan *ResolvedCertificationPlan, _ *ExecutionPlanProvenance) {
				stage := &plan.Phases[0].Stages[0]
				stage.Test.Identity.Trust = CertificationTestTrustDiagnostic
				stage.QualificationEligible = false
				stage.DiagnosticOnly = true
			},
		},
		{
			name: "controller provenance",
			mutate: func(_ *CertificationEnvironmentBindings, _ *ResolvedCertificationPlan, provenance *ExecutionPlanProvenance) {
				provenance.ControllerImage = "example.invalid/controller@sha256:4444444444444444444444444444444444444444444444444444444444444444"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			environment := baseEnvironment()
			plan := basePlan()
			provenance := baseProvenance()
			tt.mutate(environment, &plan, &provenance)

			digest, err := DeriveResolvedExecutionPlanDigest(2, environment, plan, provenance)
			if err != nil {
				t.Fatalf("derive changed resolved plan digest: %v", err)
			}

			if digest == want {
				t.Fatalf("changed resolved execution semantics retained digest %q", digest)
			}
		})
	}
}

func TestDeriveResolvedExecutionPlanDigestRejectsUntrustedOrMutableInputs(t *testing.T) {
	t.Parallel()

	plan := ResolvedCertificationPlan{
		Phases: []ResolvedPhasePlan{{
			Name: "validation",
			Role: CheckStageRoleValidation,
			Stages: []ResolvedStagePlan{{
				Name: "test",
				Test: ResolvedCertificationTest{
					Identity: TestIdentitySnapshot{
						Name:          "test",
						UID:           types.UID("test-uid"),
						Generation:    1,
						ContentDigest: testScriptDigest,
						Trust:         CertificationTestTrustApproved,
					},
					ParameterSchemaDigest:      testScriptBundleDigest,
					MetricSchemaDigest:         testScriptDigest,
					SecurityRequirementsDigest: testWorkflowTemplateHash,
					Metrics: []TestMetricDefinition{{
						Name:        "status",
						DisplayName: "Status",
						Type:        MetricValueTypeStatus,
					}},
					Topology: TestTopology{MinParticipants: 1, MaxParticipants: 1},
					Driver: CertificationTestDriver{
						Type: TestDriverTypeContainer,
						Container: &ContainerTestDriver{
							Image: "example.invalid/test:latest",
						},
					},
				},
				Schedule: StageSchedule{
					Mode:              CheckExecutionModeRunOnce,
					InvocationTimeout: metav1.Duration{Duration: time.Minute},
				},
				QualificationEligible: true,
			}},
		}},
	}
	provenance := ExecutionPlanProvenance{
		ControllerImage:        testRunnerImage,
		AdapterImage:           testAdapterImage,
		WorkflowTemplateDigest: testWorkflowTemplateHash,
	}

	if _, err := DeriveResolvedExecutionPlanDigest(1, nil, plan, provenance); err == nil {
		t.Fatal("derive resolved plan digest accepted a mutable container image")
	}

	plan.Phases[0].Stages[0].Test.Driver.Container.Image = testRunnerImage

	plan.Phases[0].Stages[0].Test.Identity.Trust = CertificationTestTrustDiagnostic
	if _, err := DeriveResolvedExecutionPlanDigest(1, nil, plan, provenance); err == nil {
		t.Fatal("derive resolved plan digest accepted an unapproved qualification stage")
	}
}

func TestDeriveResolvedExecutionPlanDigestCanonicalizesMapAndSetLists(t *testing.T) {
	t.Parallel()

	plan := ResolvedCertificationPlan{Phases: []ResolvedPhasePlan{{
		Name: "validation",
		Role: CheckStageRoleValidation,
		Stages: []ResolvedStagePlan{{
			Name: "workload",
			Test: ResolvedCertificationTest{
				Identity: TestIdentitySnapshot{
					Name:          "workload",
					UID:           types.UID("test-uid"),
					Generation:    1,
					ContentDigest: testScriptDigest,
					Trust:         CertificationTestTrustApproved,
				},
				ParameterSchemaDigest:      testScriptBundleDigest,
				MetricSchemaDigest:         testScriptDigest,
				SecurityRequirementsDigest: testWorkflowTemplateHash,
				Metrics: []TestMetricDefinition{
					{Name: "latency", DisplayName: "Latency", Type: MetricValueTypeLatency},
					{Name: "throughput", DisplayName: "Throughput", Type: MetricValueTypeThroughput},
				},
				Topology: TestTopology{
					MinParticipants: 2,
					MaxParticipants: 2,
					Roles: []TestParticipantRole{
						{Name: "worker", DisplayName: "Worker", MinCount: 1, MaxCount: 1},
						{Name: "requester", DisplayName: "Requester", MinCount: 1, MaxCount: 1},
					},
				},
				Driver: CertificationTestDriver{
					Type:      TestDriverTypeContainer,
					Container: &ContainerTestDriver{Image: testRunnerImage},
				},
				Security: ResolvedSecurityRequirements{
					Capabilities: []string{"SYS_NICE", "IPC_LOCK"},
					HostPaths: []WorkloadHostPath{
						{MountPath: "/host/sys", Path: "/sys", ReadOnly: true},
						{MountPath: "/host/meminfo", Path: "/proc/meminfo", ReadOnly: true},
					},
				},
			},
			Parameters: []StageParameterBinding{
				{Name: "model", Value: testStringValue("model-a")},
				{Name: "revision", Value: testStringValue("revision-a")},
			},
			ParticipantRoles: []ResolvedStageParticipantRoleBinding{
				{Role: "worker", ParticipantIndexes: []int32{1}},
				{Role: "requester", ParticipantIndexes: []int32{0}},
			},
			Schedule: StageSchedule{
				Mode:              CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
			},
			Assertions: []TestAssertion{
				{Name: "throughput", Metric: "throughput", Operator: AssertionOperatorMinimum, Expected: testTypedValuePointer(testStringValue("1"))},
				{Name: "latency", Metric: "latency", Operator: AssertionOperatorMaximum, Expected: testTypedValuePointer(testStringValue("10"))},
			},
			ResolvedInputs: []ResolvedInputIdentity{
				{Name: "model", Kind: ResolvedInputKindModel, Requested: "latest", State: ResolvedInputStateComplete, Resolved: "model-a", Digest: testScriptDigest},
				{Name: "image", Kind: ResolvedInputKindImage, Requested: "stable", State: ResolvedInputStateComplete, Resolved: testRunnerImage, Digest: testScriptBundleDigest},
			},
			QualificationEligible: true,
		}},
	}}}
	environment := &CertificationEnvironmentBindings{
		Namespace: "signal",
		Resolvers: []NamedEnvironmentReference{
			{Name: "model", Reference: "resolver-b"},
			{Name: "image", Reference: "resolver-a"},
		},
	}
	provenance := ExecutionPlanProvenance{
		ControllerImage:        testRunnerImage,
		AdapterImage:           testAdapterImage,
		WorkflowTemplateDigest: testWorkflowTemplateHash,
		ResolverImages:         []string{testResolverImage, testRunnerImage},
	}

	want, err := DeriveResolvedExecutionPlanDigest(2, environment, plan, provenance)
	if err != nil {
		t.Fatalf("derive canonical digest: %v", err)
	}

	reverseSlice(plan.Phases[0].Stages[0].Test.Metrics)
	reverseSlice(plan.Phases[0].Stages[0].Test.Topology.Roles)
	reverseSlice(plan.Phases[0].Stages[0].Test.Security.Capabilities)
	reverseSlice(plan.Phases[0].Stages[0].Test.Security.HostPaths)
	reverseSlice(plan.Phases[0].Stages[0].Parameters)
	reverseSlice(plan.Phases[0].Stages[0].ParticipantRoles)
	reverseSlice(plan.Phases[0].Stages[0].Assertions)
	reverseSlice(plan.Phases[0].Stages[0].ResolvedInputs)
	reverseSlice(environment.Resolvers)
	reverseSlice(provenance.ResolverImages)

	got, err := DeriveResolvedExecutionPlanDigest(2, environment, plan, provenance)
	if err != nil {
		t.Fatalf("derive permuted digest: %v", err)
	}

	if got != want {
		t.Fatalf("permuted map/set digest = %q, want %q", got, want)
	}
}

func reverseSlice[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func testTypedValuePointer(value TypedValue) *TypedValue {
	return &value
}

func testStringValue(value string) TypedValue {
	return TypedValue{
		Type:        TypedValueTypeString,
		StringValue: &value,
	}
}
