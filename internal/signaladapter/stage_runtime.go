// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	maxInvocationEvidenceReferences = 8
	stageCancellationTimeout        = 30 * time.Second
	stageEvidencePersistenceTimeout = 30 * time.Second
)

type StageClock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type wallStageClock struct{}

func (wallStageClock) Now() time.Time {
	return time.Now().UTC()
}

func (wallStageClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type StageInvocation struct {
	ID              string
	RunUID          types.UID
	PlanDigest      string
	PhaseOrder      int32
	StageOrder      int32
	Ordinal         int32
	Attempt         int32
	Stage           signalv1.ResolvedStagePlan
	Participants    []signalv1.Participant
	ParticipantRole map[int32]string
}

type StageInvocationExecutor interface {
	ExecuteInvocation(context.Context, StageInvocation) (signalv1.InvocationResultEnvelope, error)
	CancelInvocation(context.Context, StageInvocation) error
}

type StageExecutionRequest struct {
	RunUID                         types.UID
	PlanDigest                     string
	PhaseOrder                     int32
	StageOrder                     int32
	PhaseRole                      signalv1.CheckStageRole
	Stage                          signalv1.ResolvedStagePlan
	Participants                   []signalv1.Participant
	Resume                         []signalv1.InvocationResultEnvelope
	DuplicateQualificationIdentity bool
}

type StageRunner struct {
	Clock    StageClock
	Executor StageInvocationExecutor
	Evidence func(context.Context, StageExecutionRequest, []signalv1.InvocationResultEnvelope) error
}

func (r StageRunner) Execute(ctx context.Context, request StageExecutionRequest) (signalv1.StageEvidenceSummary, error) {
	if r.Executor == nil {
		return signalv1.StageEvidenceSummary{}, errors.New("stage invocation executor is required")
	}

	if request.RunUID == "" || !signalv1.IsSHA256Digest(request.PlanDigest) {
		return signalv1.StageEvidenceSummary{}, errors.New("stage execution requires immutable Run and plan identity")
	}

	if err := signalv1.ValidateStageSchedule(request.Stage.Schedule); err != nil {
		return signalv1.StageEvidenceSummary{}, fmt.Errorf("validate stage schedule: %w", err)
	}

	clock := r.Clock
	if clock == nil {
		clock = wallStageClock{}
	}

	roles, err := stageParticipantRoles(request.Stage, len(request.Participants))
	if err != nil {
		return signalv1.StageEvidenceSummary{}, err
	}

	participants := stageInvocationParticipants(request.Participants, roles)

	envelopes := slices.Clone(request.Resume)
	if err := validateResumeEnvelopes(request, participants, roles, envelopes); err != nil {
		return signalv1.StageEvidenceSummary{}, err
	}

	startedAt := clock.Now()
	for index := range envelopes {
		if envelopes[index].StartedAt.Time.Before(startedAt) {
			startedAt = envelopes[index].StartedAt.Time
		}
	}

	nextOrdinal := int32(len(envelopes))
	schedule := request.Stage.Schedule

	windowEnd := startedAt
	if schedule.CoverageWindow != nil {
		windowEnd = startedAt.Add(schedule.CoverageWindow.Duration)
	}

	schedulerCanceled := false

	for schedule.Mode != signalv1.CheckExecutionModeRunOnce || nextOrdinal <= 0 {
		if schedule.Mode != signalv1.CheckExecutionModeRunOnce {
			if nextOrdinal > 0 {
				if schedule.Mode == signalv1.CheckExecutionModePeriodic {
					nextStart := startedAt.Add(time.Duration(nextOrdinal) * schedule.Interval.Duration)
					if !nextStart.Before(windowEnd) {
						break
					}

					if err := clock.Sleep(ctx, maxDuration(0, nextStart.Sub(clock.Now()))); err != nil {
						schedulerCanceled = errors.Is(err, context.Canceled)

						break
					}
				} else if !clock.Now().Before(windowEnd) {
					break
				}
			}

			if !clock.Now().Before(windowEnd) {
				break
			}
		}

		envelope := r.executeWithRetry(ctx, clock, request, participants, roles, nextOrdinal)
		envelopes = append(envelopes, envelope)
		nextOrdinal++

		if schedule.Mode == signalv1.CheckExecutionModeRunOnce {
			break
		}
	}

	if schedule.Mode != signalv1.CheckExecutionModeRunOnce && clock.Now().Before(windowEnd) {
		if err := clock.Sleep(ctx, windowEnd.Sub(clock.Now())); err != nil {
			schedulerCanceled = errors.Is(err, context.Canceled)
		}
	}

	finishedAt := clock.Now()

	if r.Evidence != nil {
		evidenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stageEvidencePersistenceTimeout)
		defer cancel()

		if err := r.Evidence(evidenceCtx, request, envelopes); err != nil {
			return signalv1.StageEvidenceSummary{}, fmt.Errorf("persist invocation evidence: %w", err)
		}
	}

	summary, err := summarizeStageExecution(request, startedAt, finishedAt, envelopes)
	if schedulerCanceled && err == nil {
		summary.Verdict = signalv1.StageVerdictCanceled
	}

	return summary, err
}

func (r StageRunner) executeWithRetry(
	ctx context.Context,
	clock StageClock,
	request StageExecutionRequest,
	participants []signalv1.Participant,
	roles map[int32]string,
	ordinal int32,
) signalv1.InvocationResultEnvelope {
	maxAttempts := int32(1)
	if request.Stage.Retry != nil {
		maxAttempts = request.Stage.Retry.MaxAttempts
	}

	var last signalv1.InvocationResultEnvelope

	for attempt := int32(0); attempt < maxAttempts; attempt++ {
		invocation := StageInvocation{
			ID:              stageInvocationID(request, ordinal, attempt),
			RunUID:          request.RunUID,
			PlanDigest:      request.PlanDigest,
			PhaseOrder:      request.PhaseOrder,
			StageOrder:      request.StageOrder,
			Ordinal:         ordinal,
			Attempt:         attempt,
			Stage:           *request.Stage.DeepCopy(),
			Participants:    slices.Clone(participants),
			ParticipantRole: roles,
		}

		invocationCtx, cancel := context.WithTimeout(ctx, request.Stage.Schedule.InvocationTimeout.Duration)
		envelope, err := r.Executor.ExecuteInvocation(invocationCtx, invocation)

		cancel()

		if err != nil {
			now := clock.Now()

			envelope = failedInvocationEnvelope(invocation, now, err)
			if errors.Is(err, context.DeadlineExceeded) {
				envelope.Outcome = signalv1.InvocationOutcomeInconclusive

				envelope.ReasonCode = signalv1.ResultReasonTimeout
				cancelCtx, cancelInvocation := context.WithTimeout(
					context.WithoutCancel(ctx),
					stageCancellationTimeout,
				)
				cancelErr := r.Executor.CancelInvocation(cancelCtx, invocation)

				cancelInvocation()

				if cancelErr != nil {
					envelope.Message = boundedStageMessage("cancel timed-out invocation: " + cancelErr.Error())
				}
			} else if errors.Is(err, context.Canceled) {
				envelope.Outcome = signalv1.InvocationOutcomeCanceled

				envelope.ReasonCode = signalv1.ResultReasonCanceled
				cancelCtx, cancelInvocation := context.WithTimeout(
					context.WithoutCancel(ctx),
					stageCancellationTimeout,
				)
				cancelErr := r.Executor.CancelInvocation(cancelCtx, invocation)

				cancelInvocation()

				if cancelErr != nil {
					envelope.Message = boundedStageMessage("cancel interrupted invocation: " + cancelErr.Error())
				}
			}
		}

		if validationErr := validateInvocationEnvelope(invocation, &envelope); validationErr != nil {
			now := clock.Now()
			envelope = failedInvocationEnvelope(invocation, now, validationErr)
			envelope.ReasonCode = signalv1.ResultReasonEvidenceIntegrityFailure
		}

		last = envelope
		if envelope.Outcome == signalv1.InvocationOutcomePassed ||
			envelope.Outcome == signalv1.InvocationOutcomeFailed ||
			envelope.ReasonCode != signalv1.ResultReasonInfrastructureFailure ||
			!stageRetriesInfrastructure(request.Stage.Retry) {
			return envelope
		}
	}

	return last
}

func stageRetriesInfrastructure(policy *signalv1.StageRetryPolicy) bool {
	if policy == nil {
		return false
	}

	return slices.Contains(policy.RetryableFailures, signalv1.RetryableFailureClassInfrastructure)
}

func failedInvocationEnvelope(invocation StageInvocation, now time.Time, cause error) signalv1.InvocationResultEnvelope {
	message := cause.Error()
	if len(message) > 1024 {
		message = message[:1024]
	}

	participants := make([]signalv1.InvocationParticipantResult, 0, len(invocation.Participants))
	for _, participant := range invocation.Participants {
		participants = append(participants, signalv1.InvocationParticipantResult{
			Index:   participant.Index,
			Role:    invocation.ParticipantRole[participant.Index],
			Outcome: signalv1.InvocationOutcomeInconclusive,
			Start:   signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
			End:     signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
		})
	}

	return signalv1.InvocationResultEnvelope{
		SchemaVersion:     signalv1.InvocationResultSchemaV1,
		InvocationID:      invocation.ID,
		RunUID:            invocation.RunUID,
		StageID:           invocation.Stage.Name,
		StageOrder:        invocation.StageOrder,
		PlanDigest:        invocation.PlanDigest,
		Test:              invocation.Stage.Test.Identity,
		ResolvedInputs:    slices.Clone(invocation.Stage.ResolvedInputs),
		StartedAt:         metav1.NewTime(now),
		FinishedAt:        metav1.NewTime(now),
		MonotonicDuration: metav1.Duration{},
		Outcome:           signalv1.InvocationOutcomeInconclusive,
		ReasonCode:        signalv1.ResultReasonInfrastructureFailure,
		Message:           message,
		Participants:      participants,
		Cleanup:           signalv1.InvocationCleanupResult{Outcome: signalv1.CleanupOutcomeFailed},
	}
}

func validateInvocationEnvelope(invocation StageInvocation, envelope *signalv1.InvocationResultEnvelope) error {
	if envelope.SchemaVersion != signalv1.InvocationResultSchemaV1 ||
		envelope.InvocationID != invocation.ID ||
		envelope.RunUID != invocation.RunUID ||
		envelope.StageID != invocation.Stage.Name ||
		envelope.StageOrder != invocation.StageOrder ||
		envelope.PlanDigest != invocation.PlanDigest ||
		envelope.Test != invocation.Stage.Test.Identity ||
		!reflect.DeepEqual(envelope.ResolvedInputs, invocation.Stage.ResolvedInputs) {
		return errors.New("invocation result identity does not match the immutable stage")
	}

	if envelope.FinishedAt.Before(&envelope.StartedAt) ||
		envelope.MonotonicDuration.Duration < 0 ||
		envelope.MonotonicDuration.Duration > envelope.FinishedAt.Sub(envelope.StartedAt.Time) {
		return errors.New("invocation result timing is invalid")
	}

	if envelope.Outcome == signalv1.InvocationOutcomePassed {
		if envelope.Cleanup.Outcome == signalv1.CleanupOutcomeFailed {
			return errors.New("passed invocation reports failed cleanup")
		}

		if envelope.Activity.SampleCount < 1 {
			return errors.New("passed invocation has no workload-produced activity")
		}
	}

	expectedParticipants := make(map[int32]signalv1.Participant, len(invocation.Participants))
	for _, participant := range invocation.Participants {
		expectedParticipants[participant.Index] = participant
	}

	for _, participant := range envelope.Participants {
		expected, exists := expectedParticipants[participant.Index]
		if !exists ||
			invocation.ParticipantRole[participant.Index] != participant.Role ||
			participant.Start.NodeUID != expected.NodeUID ||
			participant.End.NodeUID != expected.NodeUID {
			return errors.New("invocation participant identity or role does not match the immutable stage")
		}

		if envelope.Outcome == signalv1.InvocationOutcomePassed &&
			participant.Outcome != signalv1.InvocationOutcomePassed {
			return errors.New("passed invocation includes a participant without a passed outcome")
		}

		for _, device := range append(
			append([]string(nil), participant.VisibleDevices...),
			participant.CoveredDevices...,
		) {
			if strings.HasPrefix(device, "GPU-") || strings.HasPrefix(device, "MIG-") {
				return errors.New("invocation result exposes a raw GPU device identity")
			}
		}

		delete(expectedParticipants, participant.Index)
	}

	if len(expectedParticipants) != 0 {
		return errors.New("invocation result omits participants")
	}

	return nil
}

func summarizeStageExecution(
	request StageExecutionRequest,
	startedAt time.Time,
	finishedAt time.Time,
	envelopes []signalv1.InvocationResultEnvelope,
) (signalv1.StageEvidenceSummary, error) {
	if len(envelopes) == 0 {
		return signalv1.StageEvidenceSummary{}, errors.New("stage produced no invocation evidence")
	}

	summary := signalv1.StageEvidenceSummary{
		PhaseOrder:             request.PhaseOrder,
		StageOrder:             request.StageOrder,
		Name:                   request.Stage.Name,
		Role:                   request.PhaseRole,
		Test:                   request.Stage.Test.Identity,
		Mode:                   request.Stage.Schedule.Mode,
		StartedAt:              metav1.NewTime(startedAt),
		FinishedAt:             metav1.NewTime(finishedAt),
		ObservedCoverageWindow: metav1.Duration{Duration: finishedAt.Sub(startedAt)},
		InvocationCount:        int32(len(envelopes)),
		QualificationEligible:  request.Stage.QualificationEligible,
		DiagnosticOnly:         request.Stage.DiagnosticOnly,
		DeviceIntervalCoverage: deviceIntervalCoverage(request, startedAt, envelopes),
	}

	durableDigest, err := invocationSequenceDigest(envelopes)
	if err != nil {
		return signalv1.StageEvidenceSummary{}, err
	}

	summary.DurableEvidenceDigest = durableDigest

	if request.Stage.Schedule.CoverageWindow != nil {
		coverage := *request.Stage.Schedule.CoverageWindow
		summary.ConfiguredCoverageWindow = &coverage
	}

	starts := make([]time.Time, 0, len(envelopes))
	for index := range envelopes {
		envelope := &envelopes[index]

		starts = append(starts, envelope.StartedAt.Time)
		switch envelope.Outcome {
		case signalv1.InvocationOutcomePassed:
			summary.SuccessfulInvocations++
		case signalv1.InvocationOutcomeFailed:
			summary.FailedInvocations++
		case signalv1.InvocationOutcomeCanceled:
			summary.CanceledInvocations++
		case signalv1.InvocationOutcomeInconclusive:
			if envelope.ReasonCode == signalv1.ResultReasonTimeout {
				summary.TimedOutInvocations++
			} else {
				summary.InconclusiveInvocations++
			}
		}
	}

	sort.Slice(starts, func(left, right int) bool { return starts[left].Before(starts[right]) })
	first := metav1.NewTime(starts[0])
	last := metav1.NewTime(starts[len(starts)-1])
	summary.FirstInvocationAt = &first
	summary.LastInvocationAt = &last

	if len(starts) > 1 {
		gap := maximumStartGap(starts)
		summary.MaxStartGap = &metav1.Duration{Duration: gap}
	}

	assertions, assertionsPass, err := evaluateStageAssertions(request.Stage, envelopes)
	if err != nil {
		return signalv1.StageEvidenceSummary{}, err
	}

	summary.Assertions = assertions

	invocationEvidence, err := boundedInvocationReferences(envelopes)
	if err != nil {
		return signalv1.StageEvidenceSummary{}, err
	}

	summary.InvocationEvidence = invocationEvidence
	summary.Verdict = stageVerdict(request, summary, assertionsPass)

	return summary, nil
}

func stageVerdict(
	request StageExecutionRequest,
	summary signalv1.StageEvidenceSummary,
	assertionsPass bool,
) signalv1.StageVerdict {
	stage := request.Stage

	if summary.CanceledInvocations > 0 {
		return signalv1.StageVerdictCanceled
	}

	if summary.FailedInvocations > 0 {
		return signalv1.StageVerdictFailed
	}

	if summary.TimedOutInvocations > 0 || summary.InconclusiveInvocations > 0 {
		return signalv1.StageVerdictInconclusive
	}

	if !assertionsPass {
		return signalv1.StageVerdictFailed
	}

	if request.DuplicateQualificationIdentity {
		return signalv1.StageVerdictInconclusive
	}

	if stage.Schedule.Mode == signalv1.CheckExecutionModeRunOnce {
		if summary.SuccessfulInvocations == 1 {
			return signalv1.StageVerdictPassed
		}

		return signalv1.StageVerdictInconclusive
	}

	if stage.Schedule.CoverageWindow == nil ||
		stage.Schedule.MinExecutions == nil ||
		stage.Schedule.MaxGap == nil ||
		summary.SuccessfulInvocations < *stage.Schedule.MinExecutions ||
		summary.ObservedCoverageWindow.Duration < stage.Schedule.CoverageWindow.Duration ||
		summary.MaxStartGap == nil ||
		summary.MaxStartGap.Duration > stage.Schedule.MaxGap.Duration {
		return signalv1.StageVerdictInconclusive
	}

	for _, coverage := range summary.DeviceIntervalCoverage {
		if coverage.CoveredIntervals < coverage.RequiredIntervals {
			return signalv1.StageVerdictInconclusive
		}
	}

	return signalv1.StageVerdictPassed
}

func evaluateStageAssertions(
	stage signalv1.ResolvedStagePlan,
	envelopes []signalv1.InvocationResultEnvelope,
) ([]signalv1.AssertionResult, bool, error) {
	measurements := make(map[string]signalv1.TypedValue)

	for _, envelope := range envelopes {
		for _, measurement := range envelope.Measurements {
			measurements[measurement.Name] = measurement.Value
		}
	}

	results := make([]signalv1.AssertionResult, 0, len(stage.Assertions))
	allPassed := true

	for _, assertion := range stage.Assertions {
		actual, exists := measurements[assertion.Metric]
		if !exists {
			results = append(results, signalv1.AssertionResult{
				Name:    assertion.Name,
				Passed:  false,
				Message: missingAssertionMetricMessage(assertion),
			})
			allPassed = false

			continue
		}

		passed, err := evaluateAssertion(assertion, actual, measurements)
		if err != nil {
			results = append(results, signalv1.AssertionResult{
				Name:    assertion.Name,
				Passed:  false,
				Actual:  actual.DeepCopy(),
				Message: boundedStageMessage("assertion evaluation failed: " + err.Error()),
			})
			allPassed = false

			continue
		}

		results = append(results, signalv1.AssertionResult{
			Name:   assertion.Name,
			Passed: passed,
			Actual: actual.DeepCopy(),
		})
		allPassed = allPassed && passed
	}

	return results, allPassed, nil
}

func missingAssertionMetricMessage(assertion signalv1.TestAssertion) string {
	requirement := struct {
		Metric   string                     `json:"metric"`
		Operator signalv1.AssertionOperator `json:"operator"`
		Expected *signalv1.TypedValue       `json:"expected,omitempty"`
		Minimum  *signalv1.TypedValue       `json:"minimum,omitempty"`
		Maximum  *signalv1.TypedValue       `json:"maximum,omitempty"`
	}{
		Metric:   assertion.Metric,
		Operator: assertion.Operator,
		Expected: assertion.Expected,
		Minimum:  assertion.Minimum,
		Maximum:  assertion.Maximum,
	}

	content, err := json.Marshal(requirement)
	if err != nil {
		return "required assertion metric is absent"
	}

	return boundedStageMessage("required assertion metric is absent: " + string(content))
}

func evaluateAssertion(
	assertion signalv1.TestAssertion,
	actual signalv1.TypedValue,
	measurements map[string]signalv1.TypedValue,
) (bool, error) {
	switch assertion.Operator {
	case signalv1.AssertionOperatorBooleanEquals, signalv1.AssertionOperatorStatusEquals:
		return typedValueEqual(actual, assertion.Expected)
	case signalv1.AssertionOperatorMinimum, signalv1.AssertionOperatorCountMinimum:
		comparison, err := compareTypedValues(actual, assertion.Expected)
		return comparison >= 0, err
	case signalv1.AssertionOperatorMaximum, signalv1.AssertionOperatorPercentileMaximum:
		comparison, err := compareTypedValues(actual, assertion.Expected)
		return comparison <= 0, err
	case signalv1.AssertionOperatorRange:
		minimum, err := compareTypedValues(actual, assertion.Minimum)
		if err != nil {
			return false, err
		}

		maximum, err := compareTypedValues(actual, assertion.Maximum)

		return minimum >= 0 && maximum <= 0, err
	case signalv1.AssertionOperatorErrorRateMaximum:
		requests, requestFound := measurements["requestCount"]

		errorsValue, errorFound := measurements["errorCount"]
		if !requestFound || !errorFound {
			return false, errors.New("error-rate assertion requires requestCount and errorCount")
		}

		requestCount, err := typedValueRat(requests)
		if err != nil || requestCount.Sign() <= 0 {
			return false, errors.New("requestCount must be positive")
		}

		errorCount, err := typedValueRat(errorsValue)
		if err != nil {
			return false, err
		}

		limit, err := typedValueRat(*assertion.Expected)
		if err != nil {
			return false, err
		}

		return new(big.Rat).Quo(errorCount, requestCount).Cmp(limit) <= 0, nil
	default:
		return false, fmt.Errorf("unsupported assertion operator %q", assertion.Operator)
	}
}

func typedValueEqual(actual signalv1.TypedValue, expected *signalv1.TypedValue) (bool, error) {
	if expected == nil || actual.Type != expected.Type {
		return false, errors.New("assertion type mismatch")
	}

	left, err := json.Marshal(actual)
	if err != nil {
		return false, err
	}

	right, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}

	return slices.Equal(left, right), nil
}

func compareTypedValues(actual signalv1.TypedValue, expected *signalv1.TypedValue) (int, error) {
	if expected == nil {
		return 0, errors.New("assertion expected value is absent")
	}

	left, err := typedValueRat(actual)
	if err != nil {
		return 0, err
	}

	right, err := typedValueRat(*expected)
	if err != nil {
		return 0, err
	}

	return left.Cmp(right), nil
}

func typedValueRat(value signalv1.TypedValue) (*big.Rat, error) {
	switch value.Type {
	case signalv1.TypedValueTypeInteger:
		if value.IntegerValue == nil {
			return nil, errors.New("integer value is absent")
		}

		return new(big.Rat).SetInt64(*value.IntegerValue), nil
	case signalv1.TypedValueTypeDecimal:
		if value.DecimalValue == nil {
			return nil, errors.New("decimal value is absent")
		}

		result, ok := new(big.Rat).SetString(*value.DecimalValue)
		if !ok {
			return nil, errors.New("decimal value is invalid")
		}

		return result, nil
	case signalv1.TypedValueTypeDuration:
		if value.DurationValue == nil {
			return nil, errors.New("duration value is absent")
		}

		return new(big.Rat).SetInt64(value.DurationValue.Nanoseconds()), nil
	case signalv1.TypedValueTypeQuantity:
		if value.QuantityValue == nil {
			return nil, errors.New("quantity value is absent")
		}

		quantity, err := resource.ParseQuantity(*value.QuantityValue)
		if err != nil {
			return nil, err
		}

		return new(big.Rat).SetInt64(quantity.MilliValue()), nil
	default:
		return nil, fmt.Errorf("typed value %q is not numeric", value.Type)
	}
}

func stageParticipantRoles(stage signalv1.ResolvedStagePlan, participantCount int) (map[int32]string, error) {
	roles := make(map[int32]string, participantCount)
	if len(stage.ParticipantRoles) == 0 {
		for index := 0; index < participantCount; index++ {
			roles[int32(index)] = "participant"
		}

		return roles, nil
	}

	for _, binding := range stage.ParticipantRoles {
		for _, index := range binding.ParticipantIndexes {
			if index < 0 || int(index) >= participantCount {
				return nil, errors.New("stage participant role index is out of range")
			}

			if _, duplicate := roles[index]; duplicate {
				return nil, errors.New("stage participant has multiple roles")
			}

			roles[index] = binding.Role
		}
	}

	if len(roles) == 0 {
		return nil, errors.New("stage has no assigned participants")
	}

	return roles, nil
}

func stageInvocationParticipants(
	participants []signalv1.Participant,
	roles map[int32]string,
) []signalv1.Participant {
	selected := make([]signalv1.Participant, 0, len(roles))
	for index := range participants {
		if _, assigned := roles[int32(index)]; assigned {
			selected = append(selected, participants[index])
		}
	}

	return selected
}

func validateResumeEnvelopes(
	request StageExecutionRequest,
	participants []signalv1.Participant,
	roles map[int32]string,
	envelopes []signalv1.InvocationResultEnvelope,
) error {
	maxAttempts := int32(1)
	if request.Stage.Retry != nil {
		maxAttempts = request.Stage.Retry.MaxAttempts
	}

	for index := range envelopes {
		invocation := StageInvocation{
			RunUID:          request.RunUID,
			PlanDigest:      request.PlanDigest,
			StageOrder:      request.StageOrder,
			Stage:           request.Stage,
			Participants:    participants,
			ParticipantRole: roles,
		}

		var validationErr error

		for attempt := int32(0); attempt < maxAttempts; attempt++ {
			invocation.ID = stageInvocationID(request, int32(index), attempt)

			validationErr = validateInvocationEnvelope(invocation, &envelopes[index])
			if validationErr == nil {
				break
			}
		}

		if validationErr != nil {
			return fmt.Errorf("resume invocation %d: %w", index, validationErr)
		}
	}

	return nil
}

func stageInvocationID(request StageExecutionRequest, ordinal, attempt int32) string {
	content := fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		request.RunUID,
		request.PlanDigest,
		request.PhaseOrder,
		request.StageOrder,
		ordinal,
		attempt,
	)
	digest := sha256.Sum256([]byte(content))

	return "inv-" + hex.EncodeToString(digest[:16])
}

func invocationSequenceDigest(envelopes []signalv1.InvocationResultEnvelope) (string, error) {
	digest := sha256.New()

	for index := range envelopes {
		content, err := json.Marshal(envelopes[index])
		if err != nil {
			return "", fmt.Errorf("encode invocation evidence digest: %w", err)
		}

		sum := sha256.Sum256(content)
		_, _ = digest.Write(sum[:])
	}

	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func boundedInvocationReferences(
	envelopes []signalv1.InvocationResultEnvelope,
) ([]signalv1.InvocationEvidenceReference, error) {
	indexes := make([]int, 0, min(len(envelopes), maxInvocationEvidenceReferences))
	for index := 0; index < len(envelopes) && len(indexes) < maxInvocationEvidenceReferences; index++ {
		indexes = append(indexes, index)
	}

	if len(envelopes) > maxInvocationEvidenceReferences {
		indexes[len(indexes)-1] = len(envelopes) - 1
	}

	result := make([]signalv1.InvocationEvidenceReference, 0, len(indexes))
	for _, index := range indexes {
		envelope := envelopes[index]

		content, err := json.Marshal(envelope)
		if err != nil {
			return nil, fmt.Errorf("encode invocation evidence reference: %w", err)
		}

		digest := sha256.Sum256(content)
		result = append(result, signalv1.InvocationEvidenceReference{
			InvocationID: envelope.InvocationID,
			Digest:       "sha256:" + hex.EncodeToString(digest[:]),
			Outcome:      envelope.Outcome,
			ReasonCode:   envelope.ReasonCode,
			StartedAt:    envelope.StartedAt,
			FinishedAt:   envelope.FinishedAt,
		})
	}

	return result, nil
}

func deviceIntervalCoverage(
	request StageExecutionRequest,
	startedAt time.Time,
	envelopes []signalv1.InvocationResultEnvelope,
) []signalv1.DeviceIntervalCoverageSummary {
	if request.Stage.PerDeviceCoverageInterval == nil || request.Stage.Schedule.CoverageWindow == nil {
		return nil
	}

	required := int32(
		(request.Stage.Schedule.CoverageWindow.Duration +
			request.Stage.PerDeviceCoverageInterval.Duration - 1) /
			request.Stage.PerDeviceCoverageInterval.Duration,
	)
	covered := map[string]map[int32]struct{}{}

	for _, envelope := range envelopes {
		if envelope.Outcome != signalv1.InvocationOutcomePassed {
			continue
		}

		bucket := int32(envelope.StartedAt.Sub(startedAt) / request.Stage.PerDeviceCoverageInterval.Duration)
		if bucket < 0 || bucket >= required {
			continue
		}

		for _, participant := range envelope.Participants {
			for _, device := range participant.CoveredDevices {
				key := strconv.FormatInt(int64(participant.Index), 10) + "\x00" + device
				if covered[key] == nil {
					covered[key] = map[int32]struct{}{}
				}

				covered[key][bucket] = struct{}{}
			}
		}
	}

	result := make([]signalv1.DeviceIntervalCoverageSummary, 0, len(covered))
	for key, buckets := range covered {
		separator := slices.Index([]byte(key), byte(0))

		index, err := strconv.ParseInt(key[:separator], 10, 32)
		if err != nil {
			continue
		}

		result = append(result, signalv1.DeviceIntervalCoverageSummary{
			ParticipantIndex:  int32(index),
			DeviceID:          key[separator+1:],
			RequiredIntervals: required,
			CoveredIntervals:  int32(len(buckets)),
		})
	}

	sort.Slice(result, func(left, right int) bool {
		if result[left].ParticipantIndex != result[right].ParticipantIndex {
			return result[left].ParticipantIndex < result[right].ParticipantIndex
		}

		return result[left].DeviceID < result[right].DeviceID
	})

	return result
}

func maximumStartGap(starts []time.Time) time.Duration {
	var maximum time.Duration
	for index := 1; index < len(starts); index++ {
		if gap := starts[index].Sub(starts[index-1]); gap > maximum {
			maximum = gap
		}
	}

	return maximum
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}

	return right
}
