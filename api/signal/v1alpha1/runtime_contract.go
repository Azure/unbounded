// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	MinimumBackendDeadlineReserve = time.Minute
	MaxStageExecutionInputBytes   = 96 * 1024
)

func MarshalStageExecutionInput(value any) ([]byte, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode stage execution input: %w", err)
	}

	if len(content) > MaxStageExecutionInputBytes {
		return nil, fmt.Errorf(
			"stage execution input is %d bytes, maximum is %d",
			len(content),
			MaxStageExecutionInputBytes,
		)
	}

	return content, nil
}

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

type resolvedStageDependencyNode struct {
	stage        *ResolvedStagePlan
	dependencies []string
}

func resolvedStageDependencyGraph(
	phases []ResolvedPhasePlan,
) (map[string]resolvedStageDependencyNode, error) {
	if len(phases) == 0 {
		return nil, errors.New("at least one phase is required")
	}

	nodes := make(map[string]resolvedStageDependencyNode)
	for phaseIndex := range phases {
		if len(phases[phaseIndex].Stages) == 0 {
			return nil, fmt.Errorf("phase %d must contain at least one stage", phaseIndex)
		}

		for stageIndex := range phases[phaseIndex].Stages {
			stage := &phases[phaseIndex].Stages[stageIndex]
			if _, exists := nodes[stage.Name]; exists {
				return nil, fmt.Errorf("resolved stage name %s is duplicated", stage.Name)
			}

			nodes[stage.Name] = resolvedStageDependencyNode{stage: stage}
		}
	}

	for phaseIndex := range phases {
		for stageIndex := range phases[phaseIndex].Stages {
			stage := &phases[phaseIndex].Stages[stageIndex]
			node := nodes[stage.Name]
			seen := make(map[string]struct{}, len(stage.DependsOn))

			if len(stage.DependsOn) == 0 && phaseIndex > 0 {
				for previousIndex := range phases[phaseIndex-1].Stages {
					node.dependencies = append(
						node.dependencies,
						phases[phaseIndex-1].Stages[previousIndex].Name,
					)
				}
			} else {
				for dependencyIndex := range stage.DependsOn {
					dependency := stage.DependsOn[dependencyIndex]
					if dependency.Stage == stage.Name {
						return nil, fmt.Errorf("resolved stage %s cannot depend on itself", stage.Name)
					}

					if _, exists := seen[dependency.Stage]; exists {
						return nil, fmt.Errorf(
							"resolved stage %s dependency %s is duplicated",
							stage.Name,
							dependency.Stage,
						)
					}

					seen[dependency.Stage] = struct{}{}
					if _, exists := nodes[dependency.Stage]; !exists {
						return nil, fmt.Errorf(
							"resolved stage %s dependency %s does not exist",
							stage.Name,
							dependency.Stage,
						)
					}

					switch dependency.Condition {
					case "", StageDependencyConditionSucceeded, StageDependencyConditionCompleted:
					default:
						return nil, fmt.Errorf(
							"resolved stage %s dependency %s has invalid condition %q",
							stage.Name,
							dependency.Stage,
							dependency.Condition,
						)
					}

					node.dependencies = append(node.dependencies, dependency.Stage)
				}
			}

			nodes[stage.Name] = node
		}
	}

	const (
		unvisited = iota
		visiting
		visited
	)

	states := make(map[string]int, len(nodes))

	var visit func(string) error
	visit = func(stage string) error {
		switch states[stage] {
		case visiting:
			return fmt.Errorf("resolved stage dependency graph contains a cycle at %s", stage)
		case visited:
			return nil
		}

		states[stage] = visiting
		for _, dependency := range nodes[stage].dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}

		states[stage] = visited

		return nil
	}

	for stage := range nodes {
		if err := visit(stage); err != nil {
			return nil, err
		}
	}

	return nodes, nil
}

func resolvedStageDeadline(stage *ResolvedStagePlan) (time.Duration, error) {
	schedule := stage.Schedule
	if err := ValidateStageSchedule(schedule); err != nil {
		return 0, err
	}

	maxAttempts := int32(1)
	if stage.Retry != nil {
		maxAttempts = stage.Retry.MaxAttempts
		if maxAttempts < 1 {
			return 0, errors.New("retry maxAttempts must be positive")
		}
	}

	invocationTimeout := schedule.InvocationTimeout.Duration
	if invocationTimeout > time.Duration(1<<63-1)/time.Duration(maxAttempts) {
		return 0, errors.New("stage retry deadline overflows")
	}

	deadline := invocationTimeout * time.Duration(maxAttempts)
	if schedule.CoverageWindow != nil {
		if schedule.CoverageWindow.Duration > time.Duration(1<<63-1)-deadline {
			return 0, errors.New("stage deadline overflows")
		}

		deadline += schedule.CoverageWindow.Duration
	}

	if deadline > time.Duration(1<<63-1)-MinimumBackendDeadlineReserve {
		return 0, errors.New("stage deadline reserve overflows")
	}

	return deadline + MinimumBackendDeadlineReserve, nil
}

// DeriveResolvedPlanDeadline returns the longest stage dependency path. Stages
// without explicit dependencies retain the previous-phase barrier semantics.
// The retry budget extends the final invocation tail; retries started during a
// repeated coverage window remain inside that window.
func DeriveResolvedPlanDeadline(phases []ResolvedPhasePlan) (time.Duration, error) {
	nodes, err := resolvedStageDependencyGraph(phases)
	if err != nil {
		return 0, err
	}

	deadlines := make(map[string]time.Duration, len(nodes))

	var derive func(string) (time.Duration, error)
	derive = func(name string) (time.Duration, error) {
		if deadline, exists := deadlines[name]; exists {
			return deadline, nil
		}

		node := nodes[name]
		ownDeadline, err := resolvedStageDeadline(node.stage)
		if err != nil {
			return 0, fmt.Errorf("stage %s schedule is invalid: %w", name, err)
		}

		var dependencyDeadline time.Duration
		for _, dependency := range node.dependencies {
			candidate, err := derive(dependency)
			if err != nil {
				return 0, err
			}

			if candidate > dependencyDeadline {
				dependencyDeadline = candidate
			}
		}

		if dependencyDeadline > time.Duration(1<<63-1)-ownDeadline {
			return 0, errors.New("combined stage deadlines overflow")
		}

		deadlines[name] = dependencyDeadline + ownDeadline

		return deadlines[name], nil
	}

	var required time.Duration
	for name := range nodes {
		deadline, err := derive(name)
		if err != nil {
			return 0, err
		}

		if deadline > required {
			required = deadline
		}
	}

	return required, nil
}
