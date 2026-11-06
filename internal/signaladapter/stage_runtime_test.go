// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

type fakeStageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeStageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeStageClock) Sleep(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()

	return nil
}

type fakeStageExecutor struct {
	clock    *fakeStageClock
	outcomes []signalv1.InvocationOutcome
	reasons  []signalv1.ResultReasonCode
	calls    []StageInvocation
	starts   []time.Time
	canceled []string
}

func (e *fakeStageExecutor) ExecuteInvocation(
	_ context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	e.calls = append(e.calls, invocation)
	index := len(e.calls) - 1

	outcome := signalv1.InvocationOutcomePassed
	if index < len(e.outcomes) {
		outcome = e.outcomes[index]
	}

	reason := signalv1.ResultReasonCompleted
	if index < len(e.reasons) {
		reason = e.reasons[index]
	}

	started := e.clock.Now()
	e.starts = append(e.starts, started)
	e.clock.mu.Lock()
	e.clock.now = e.clock.now.Add(time.Second)
	e.clock.mu.Unlock()
	finished := e.clock.Now()

	participants := make([]signalv1.InvocationParticipantResult, 0, len(invocation.Participants))
	for participantIndex := range invocation.Participants {
		participant := invocation.Participants[participantIndex]
		participants = append(participants, signalv1.InvocationParticipantResult{
			Index:          int32(participantIndex),
			Role:           invocation.ParticipantRole[int32(participantIndex)],
			Outcome:        outcome,
			Start:          signalv1.NodeFingerprint{NodeUID: participant.NodeUID, BootID: "boot"},
			End:            signalv1.NodeFingerprint{NodeUID: participant.NodeUID, BootID: "boot"},
			VisibleDevices: []string{"gpu-0"},
			CoveredDevices: []string{"gpu-0"},
		})
	}

	passed := true

	return signalv1.InvocationResultEnvelope{
		SchemaVersion:     signalv1.InvocationResultSchemaV1,
		InvocationID:      invocation.ID,
		RunUID:            invocation.RunUID,
		StageID:           invocation.Stage.Name,
		StageOrder:        invocation.StageOrder,
		PlanDigest:        invocation.PlanDigest,
		Test:              invocation.Stage.Test.Identity,
		ResolvedInputs:    invocation.Stage.ResolvedInputs,
		StartedAt:         metav1.NewTime(started),
		FinishedAt:        metav1.NewTime(finished),
		MonotonicDuration: metav1.Duration{Duration: time.Second},
		Outcome:           outcome,
		ReasonCode:        reason,
		Measurements: []signalv1.MetricMeasurement{{
			Name: "passed",
			Value: signalv1.TypedValue{
				Type:         signalv1.TypedValueTypeBoolean,
				BooleanValue: &passed,
			},
		}},
		Participants: participants,
		Activity: signalv1.WorkloadActivitySummary{
			SampleCount:          1,
			SampleSequenceDigest: testStageDigest("activity"),
		},
		Cleanup: signalv1.InvocationCleanupResult{Outcome: signalv1.CleanupOutcomeSucceeded},
	}, nil
}

func (e *fakeStageExecutor) CancelInvocation(_ context.Context, invocation StageInvocation) error {
	e.canceled = append(e.canceled, invocation.ID)

	return nil
}

func TestStageRunnerPeriodicUsesAnchoredCadence(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeStageClock{now: start}
	executor := &fakeStageExecutor{clock: clock}
	minimum := int32(3)
	expected := true
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModePeriodic,
		InvocationTimeout: metav1.Duration{Duration: 2 * time.Second},
		CoverageWindow:    &metav1.Duration{Duration: 10 * time.Second},
		Interval:          &metav1.Duration{Duration: 3 * time.Second},
		MinExecutions:     &minimum,
		MaxGap:            &metav1.Duration{Duration: 4 * time.Second},
	})
	request.Stage.Assertions = []signalv1.TestAssertion{{
		Name:     "passed",
		Metric:   "passed",
		Operator: signalv1.AssertionOperatorBooleanEquals,
		Expected: &signalv1.TypedValue{
			Type:         signalv1.TypedValueTypeBoolean,
			BooleanValue: &expected,
		},
	}}

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictPassed ||
		summary.InvocationCount != 4 ||
		summary.MaxStartGap == nil ||
		summary.MaxStartGap.Duration != 3*time.Second {
		t.Fatalf("periodic summary = %#v", summary)
	}

	for index := range executor.calls {
		want := start.Add(time.Duration(index) * 3 * time.Second)
		if got := executor.starts[index]; !got.Equal(want) {
			t.Fatalf("invocation %d start = %s, want %s", index, got, want)
		}
	}
}

func TestStageRunnerContinuousRunsBackToBack(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeStageClock{now: start}
	executor := &fakeStageExecutor{clock: clock}
	minimum := int32(3)
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeContinuous,
		InvocationTimeout: metav1.Duration{Duration: 2 * time.Second},
		CoverageWindow:    &metav1.Duration{Duration: 3 * time.Second},
		MinExecutions:     &minimum,
		MaxGap:            &metav1.Duration{Duration: 2 * time.Second},
	})

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictPassed || summary.InvocationCount != 3 {
		t.Fatalf("continuous summary = %#v", summary)
	}

	for index := range executor.starts {
		want := start.Add(time.Duration(index) * time.Second)
		if !executor.starts[index].Equal(want) {
			t.Fatalf("invocation %d start = %s, want %s", index, executor.starts[index], want)
		}
	}
}

func TestStageRunnerRetriesOnlyInfrastructureFailures(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	executor := &fakeStageExecutor{
		clock:    clock,
		outcomes: []signalv1.InvocationOutcome{signalv1.InvocationOutcomeInconclusive, signalv1.InvocationOutcomePassed},
		reasons:  []signalv1.ResultReasonCode{signalv1.ResultReasonInfrastructureFailure, signalv1.ResultReasonCompleted},
	}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	request.Stage.Retry = &signalv1.StageRetryPolicy{
		MaxAttempts:       2,
		RetryableFailures: []signalv1.RetryableFailureClass{signalv1.RetryableFailureClassInfrastructure},
	}

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(executor.calls) != 2 || summary.Verdict != signalv1.StageVerdictPassed {
		t.Fatalf("retry calls = %d, summary = %#v", len(executor.calls), summary)
	}
}

func TestStageRunnerRejectsReplayedResumeEvidence(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	executor := &fakeStageExecutor{clock: clock}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	invocation := StageInvocation{
		ID:              stageInvocationID(request, 0, 0),
		RunUID:          request.RunUID,
		PlanDigest:      request.PlanDigest,
		StageOrder:      request.StageOrder,
		Stage:           request.Stage,
		Participants:    request.Participants,
		ParticipantRole: map[int32]string{0: "participant"},
	}

	envelope, err := executor.ExecuteInvocation(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}

	envelope.PlanDigest = testStageDigest("other-plan")
	request.Resume = []signalv1.InvocationResultEnvelope{envelope}

	if _, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request); err == nil {
		t.Fatal("Execute() accepted replayed resume evidence")
	}
}

func TestStageRunnerRequiresWorkloadActivity(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	executor := &activityDroppingExecutor{fakeStageExecutor: fakeStageExecutor{clock: clock}}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictInconclusive {
		t.Fatalf("summary verdict = %s, want Inconclusive", summary.Verdict)
	}
}

func TestStageRunnerDuplicateQualificationIdentityIsInconclusive(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	request.DuplicateQualificationIdentity = true

	summary, err := (StageRunner{
		Clock:    clock,
		Executor: &fakeStageExecutor{clock: clock},
	}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictInconclusive {
		t.Fatalf("summary verdict = %s, want Inconclusive", summary.Verdict)
	}
}

func TestStageRunnerRejectsResolvedInputMismatch(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	request.Stage.ResolvedInputs = []signalv1.ResolvedInputIdentity{{
		Name:     "image",
		Kind:     signalv1.ResolvedInputKindImage,
		State:    signalv1.ResolvedInputStateComplete,
		Resolved: "example.invalid/test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Digest:   testStageDigest("image"),
	}}
	executor := &resolvedInputDroppingExecutor{fakeStageExecutor: fakeStageExecutor{clock: clock}}

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictInconclusive {
		t.Fatalf("summary verdict = %s, want Inconclusive", summary.Verdict)
	}
}

func TestStageRunnerRejectsRawGPUIdentity(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	executor := &rawDeviceExecutor{fakeStageExecutor: fakeStageExecutor{clock: clock}}

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictInconclusive {
		t.Fatalf("summary verdict = %s, want Inconclusive", summary.Verdict)
	}
}

func TestStageRunnerAcceptsSuccessfulRetryOnResume(t *testing.T) {
	t.Parallel()

	clock := &fakeStageClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: time.Minute},
	})
	request.Stage.Retry = &signalv1.StageRetryPolicy{
		MaxAttempts:       2,
		RetryableFailures: []signalv1.RetryableFailureClass{signalv1.RetryableFailureClassInfrastructure},
	}
	fixture := &fakeStageExecutor{clock: clock}
	invocation := StageInvocation{
		ID:              stageInvocationID(request, 0, 1),
		RunUID:          request.RunUID,
		PlanDigest:      request.PlanDigest,
		StageOrder:      request.StageOrder,
		Stage:           request.Stage,
		Participants:    request.Participants,
		ParticipantRole: map[int32]string{0: "participant"},
	}

	envelope, err := fixture.ExecuteInvocation(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}

	request.Resume = []signalv1.InvocationResultEnvelope{envelope}

	executor := &fakeStageExecutor{clock: clock}

	summary, err := (StageRunner{Clock: clock, Executor: executor}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if summary.Verdict != signalv1.StageVerdictPassed || len(executor.calls) != 0 {
		t.Fatalf("resume summary = %#v, new calls = %d", summary, len(executor.calls))
	}
}

func TestStageRunnerBoundsInvocationReferences(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeStageClock{now: start}
	minimum := int32(12)
	request := stageExecutionRequest(signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModePeriodic,
		InvocationTimeout: metav1.Duration{Duration: time.Second},
		CoverageWindow:    &metav1.Duration{Duration: 12 * time.Second},
		Interval:          &metav1.Duration{Duration: time.Second},
		MinExecutions:     &minimum,
		MaxGap:            &metav1.Duration{Duration: 2 * time.Second},
	})

	summary, err := (StageRunner{
		Clock:    clock,
		Executor: &fakeStageExecutor{clock: clock},
	}).Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(summary.InvocationEvidence) != maxInvocationEvidenceReferences {
		t.Fatalf("invocation references = %d, want %d", len(summary.InvocationEvidence), maxInvocationEvidenceReferences)
	}
}

type activityDroppingExecutor struct {
	fakeStageExecutor
}

type resolvedInputDroppingExecutor struct {
	fakeStageExecutor
}

func (e *resolvedInputDroppingExecutor) ExecuteInvocation(
	ctx context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	envelope, err := e.fakeStageExecutor.ExecuteInvocation(ctx, invocation)
	envelope.ResolvedInputs = nil

	return envelope, err
}

type rawDeviceExecutor struct {
	fakeStageExecutor
}

func (e *rawDeviceExecutor) ExecuteInvocation(
	ctx context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	envelope, err := e.fakeStageExecutor.ExecuteInvocation(ctx, invocation)
	envelope.Participants[0].VisibleDevices = []string{"GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}

	return envelope, err
}

func (e *activityDroppingExecutor) ExecuteInvocation(
	ctx context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	envelope, err := e.fakeStageExecutor.ExecuteInvocation(ctx, invocation)
	envelope.Activity = signalv1.WorkloadActivitySummary{}

	return envelope, err
}

func stageExecutionRequest(schedule signalv1.StageSchedule) StageExecutionRequest {
	return StageExecutionRequest{
		RunUID:     types.UID("run-uid"),
		PlanDigest: testStageDigest("plan"),
		PhaseOrder: 0,
		StageOrder: 0,
		PhaseRole:  signalv1.CheckStageRoleValidation,
		Participants: []signalv1.Participant{{
			NodeName: "worker-0",
			NodeUID:  types.UID("node-uid"),
		}},
		Stage: signalv1.ResolvedStagePlan{
			Name: "stage",
			Test: signalv1.ResolvedCertificationTest{
				Identity: signalv1.TestIdentitySnapshot{
					Name:          "test",
					UID:           types.UID("test-uid"),
					Generation:    1,
					ContentDigest: testStageDigest("test"),
					Trust:         signalv1.CertificationTestTrustApproved,
				},
			},
			Schedule:              schedule,
			QualificationEligible: true,
		},
	}
}

func testStageDigest(value string) string {
	digest := sha256.Sum256([]byte(value))

	return "sha256:" + hex.EncodeToString(digest[:])
}
