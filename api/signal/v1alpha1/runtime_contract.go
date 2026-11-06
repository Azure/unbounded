// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"time"
)

const MinimumBackendDeadlineReserve = time.Minute

func ValidateExecutionDeadlines(backendDeadline, outerDeadline time.Duration) error {
	if backendDeadline <= 0 {
		return errors.New("backend active deadline must be positive")
	}

	if backendDeadline%time.Second != 0 || outerDeadline%time.Second != 0 {
		return errors.New("backend and outer deadlines must use whole seconds")
	}

	if outerDeadline-backendDeadline < MinimumBackendDeadlineReserve {
		return fmt.Errorf(
			"backend active deadline must leave at least %s before the outer deadline",
			MinimumBackendDeadlineReserve,
		)
	}

	return nil
}

func DeriveSuiteDeadline(backendDeadlines []time.Duration) (time.Duration, error) {
	var required time.Duration

	for _, backendDeadline := range backendDeadlines {
		if backendDeadline <= 0 || backendDeadline%time.Second != 0 {
			return 0, errors.New("backend active deadlines must be positive whole numbers of seconds")
		}

		if backendDeadline > time.Duration(1<<63-1)-required-MinimumBackendDeadlineReserve {
			return 0, errors.New("combined check deadlines overflow")
		}

		required += backendDeadline + MinimumBackendDeadlineReserve
	}

	if required == 0 {
		return 0, errors.New("at least one backend active deadline is required")
	}

	return required, nil
}

// EffectiveCheckExecutionMode returns the explicit execution mode or the
// backward-compatible legacy RunOnce behavior.
func EffectiveCheckExecutionMode(execution *CheckExecutionPolicy) CheckExecutionMode {
	if execution == nil {
		return CheckExecutionModeRunOnce
	}

	return execution.Mode
}

// SupportsSustainedExecution reports whether the runner owns repeated
// invocation across an explicit coverage window. Legacy minDuration alone
// measures backend lifetime and must not qualify as sustained execution.
func SupportsSustainedExecution(execution *CheckExecutionPolicy) bool {
	mode := EffectiveCheckExecutionMode(execution)

	return mode == CheckExecutionModeContinuous || mode == CheckExecutionModePeriodic
}

func ValidateCheckExecutionPolicy(backendDeadline time.Duration, execution *CheckExecutionPolicy) error {
	if execution == nil {
		return nil
	}

	if execution.Role != CheckStageRoleStartup &&
		execution.Role != CheckStageRoleValidation &&
		execution.Role != CheckStageRoleTerminal {
		return errors.New("execution role is invalid")
	}

	if execution.Mode != CheckExecutionModeRunOnce &&
		execution.Mode != CheckExecutionModeContinuous &&
		execution.Mode != CheckExecutionModePeriodic {
		return errors.New("execution mode is invalid")
	}

	if execution.Role != CheckStageRoleValidation && execution.Mode != CheckExecutionModeRunOnce {
		return errors.New("startup and terminal stages must use RunOnce mode")
	}

	if execution.Mode == CheckExecutionModeRunOnce {
		if execution.CoverageWindow != nil ||
			execution.IterationTimeout != nil ||
			execution.Interval != nil ||
			execution.MinExecutions != nil ||
			execution.MaxGap != nil {
			return errors.New("RunOnce mode must not declare repeated-execution fields")
		}

		return nil
	}

	if backendDeadline <= 0 || backendDeadline%time.Second != 0 {
		return errors.New("backend active deadline must be a positive whole number of seconds")
	}

	if execution.CoverageWindow == nil ||
		execution.IterationTimeout == nil ||
		execution.MinExecutions == nil ||
		execution.MaxGap == nil {
		return errors.New(
			"continuous and periodic modes require coverageWindow, iterationTimeout, minExecutions, and maxGap",
		)
	}

	coverageWindow := execution.CoverageWindow.Duration
	iterationTimeout := execution.IterationTimeout.Duration
	maxGap := execution.MaxGap.Duration

	for name, duration := range map[string]time.Duration{
		"coverageWindow":   coverageWindow,
		"iterationTimeout": iterationTimeout,
		"maxGap":           maxGap,
	} {
		if duration <= 0 || duration%time.Second != 0 {
			return fmt.Errorf("%s must be a positive whole number of seconds", name)
		}
	}

	if coverageWindow > backendDeadline {
		return errors.New("coverageWindow must not exceed backend active deadline")
	}

	if iterationTimeout > coverageWindow {
		return errors.New("iterationTimeout must not exceed coverageWindow")
	}

	if maxGap > coverageWindow {
		return errors.New("maxGap must not exceed coverageWindow")
	}

	if *execution.MinExecutions < 2 {
		return errors.New("minExecutions must be at least 2")
	}

	if execution.Mode == CheckExecutionModeContinuous {
		if execution.Interval != nil {
			return errors.New("continuous mode must not declare interval")
		}

		return nil
	}

	if execution.Interval == nil {
		return errors.New("periodic mode requires interval")
	}

	interval := execution.Interval.Duration
	if interval <= 0 || interval%time.Second != 0 {
		return errors.New("interval must be a positive whole number of seconds")
	}

	if iterationTimeout > interval {
		return errors.New("periodic iterationTimeout must not exceed interval")
	}

	if interval > maxGap {
		return errors.New("periodic maxGap must not be less than interval")
	}

	if interval > coverageWindow {
		return errors.New("periodic interval must not exceed coverageWindow")
	}

	maxExecutions := int64((coverageWindow-iterationTimeout)/interval) + 1
	if int64(*execution.MinExecutions) > maxExecutions {
		return fmt.Errorf(
			"minExecutions %d cannot fit in coverageWindow with interval and iterationTimeout",
			*execution.MinExecutions,
		)
	}

	return nil
}

func ValidateStageSchedule(schedule StageSchedule) error {
	invocationTimeout := schedule.InvocationTimeout.Duration
	if invocationTimeout <= 0 || invocationTimeout%time.Second != 0 {
		return errors.New("invocationTimeout must be a positive whole number of seconds")
	}

	if schedule.Mode == CheckExecutionModeRunOnce {
		if schedule.CoverageWindow != nil ||
			schedule.Interval != nil ||
			schedule.MinExecutions != nil ||
			schedule.MaxGap != nil {
			return errors.New("RunOnce schedules must not declare repeated-execution fields")
		}

		return nil
	}

	if schedule.Mode != CheckExecutionModeContinuous &&
		schedule.Mode != CheckExecutionModePeriodic {
		return errors.New("stage schedule mode is invalid")
	}

	if schedule.CoverageWindow == nil ||
		schedule.MinExecutions == nil ||
		schedule.MaxGap == nil {
		return errors.New(
			"continuous and periodic schedules require coverageWindow, minExecutions, and maxGap",
		)
	}

	coverageWindow := schedule.CoverageWindow.Duration

	maxGap := schedule.MaxGap.Duration
	for name, duration := range map[string]time.Duration{
		"coverageWindow": coverageWindow,
		"maxGap":         maxGap,
	} {
		if duration <= 0 || duration%time.Second != 0 {
			return fmt.Errorf("%s must be a positive whole number of seconds", name)
		}
	}

	if invocationTimeout > coverageWindow {
		return errors.New("invocationTimeout must not exceed coverageWindow")
	}

	if maxGap > coverageWindow {
		return errors.New("maxGap must not exceed coverageWindow")
	}

	if *schedule.MinExecutions < 2 {
		return errors.New("minExecutions must be at least 2")
	}

	if schedule.Mode == CheckExecutionModeContinuous {
		if schedule.Interval != nil {
			return errors.New("continuous schedules must not declare interval")
		}

		return nil
	}

	if schedule.Interval == nil {
		return errors.New("periodic schedules require interval")
	}

	interval := schedule.Interval.Duration
	if interval <= 0 || interval%time.Second != 0 {
		return errors.New("interval must be a positive whole number of seconds")
	}

	if invocationTimeout > interval {
		return errors.New("periodic invocationTimeout must not exceed interval")
	}

	if interval > maxGap {
		return errors.New("periodic maxGap must not be less than interval")
	}

	maxExecutions := int64((coverageWindow-invocationTimeout)/interval) + 1
	if int64(*schedule.MinExecutions) > maxExecutions {
		return fmt.Errorf(
			"minExecutions %d cannot fit in coverageWindow with interval and invocationTimeout",
			*schedule.MinExecutions,
		)
	}

	return nil
}

// DeriveResolvedPlanDeadline sums ordered phase ceilings. Stages within a
// phase are concurrent, so each phase contributes only its longest stage plus
// the cleanup reserve. The retry budget extends the final invocation tail;
// retries started during a repeated coverage window remain inside that window.
func DeriveResolvedPlanDeadline(phases []ResolvedPhasePlan) (time.Duration, error) {
	var required time.Duration

	if len(phases) == 0 {
		return 0, errors.New("at least one phase is required")
	}

	for phaseIndex := range phases {
		var phaseDeadline time.Duration

		if len(phases[phaseIndex].Stages) == 0 {
			return 0, fmt.Errorf("phase %d must contain at least one stage", phaseIndex)
		}

		for stageIndex := range phases[phaseIndex].Stages {
			stage := phases[phaseIndex].Stages[stageIndex]

			schedule := stage.Schedule
			if err := ValidateStageSchedule(schedule); err != nil {
				return 0, fmt.Errorf(
					"phase %d stage %d schedule is invalid: %w",
					phaseIndex,
					stageIndex,
					err,
				)
			}

			maxAttempts := int32(1)
			if stage.Retry != nil {
				maxAttempts = stage.Retry.MaxAttempts
				if maxAttempts < 1 {
					return 0, fmt.Errorf(
						"phase %d stage %d retry maxAttempts must be positive",
						phaseIndex,
						stageIndex,
					)
				}
			}

			invocationTimeout := schedule.InvocationTimeout.Duration
			if invocationTimeout > time.Duration(1<<63-1)/time.Duration(maxAttempts) {
				return 0, errors.New("stage retry deadline overflows")
			}

			stageDeadline := invocationTimeout * time.Duration(maxAttempts)
			if schedule.CoverageWindow != nil {
				if schedule.CoverageWindow.Duration >
					time.Duration(1<<63-1)-stageDeadline {
					return 0, errors.New("stage deadline overflows")
				}

				stageDeadline += schedule.CoverageWindow.Duration
			}

			if stageDeadline > phaseDeadline {
				phaseDeadline = stageDeadline
			}
		}

		if required > time.Duration(1<<63-1)-phaseDeadline-MinimumBackendDeadlineReserve {
			return 0, errors.New("combined phase deadlines overflow")
		}

		required += phaseDeadline + MinimumBackendDeadlineReserve
	}

	return required, nil
}
