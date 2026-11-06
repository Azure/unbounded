// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateCheckExecutionPolicy(t *testing.T) {
	t.Parallel()

	duration := func(value time.Duration) *metav1.Duration {
		return &metav1.Duration{Duration: value}
	}
	count := func(value int32) *int32 {
		return &value
	}

	tests := []struct {
		name      string
		deadline  time.Duration
		execution *CheckExecutionPolicy
		wantError string
	}{
		{
			name:     "legacy run once",
			deadline: 10 * time.Minute,
		},
		{
			name:     "explicit startup",
			deadline: 10 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role: CheckStageRoleStartup,
				Mode: CheckExecutionModeRunOnce,
			},
		},
		{
			name:     "continuous",
			deadline: 25 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModeContinuous,
				CoverageWindow:   duration(20 * time.Minute),
				IterationTimeout: duration(5 * time.Minute),
				MinExecutions:    count(3),
				MaxGap:           duration(6 * time.Minute),
			},
		},
		{
			name:     "periodic",
			deadline: 65 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModePeriodic,
				CoverageWindow:   duration(time.Hour),
				IterationTimeout: duration(5 * time.Minute),
				Interval:         duration(15 * time.Minute),
				MinExecutions:    count(4),
				MaxGap:           duration(20 * time.Minute),
			},
		},
		{
			name:     "terminal cannot repeat",
			deadline: 10 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleTerminal,
				Mode:             CheckExecutionModeContinuous,
				CoverageWindow:   duration(5 * time.Minute),
				IterationTimeout: duration(time.Minute),
				MinExecutions:    count(2),
				MaxGap:           duration(2 * time.Minute),
			},
			wantError: "startup and terminal",
		},
		{
			name:     "run once rejects repeated fields",
			deadline: 10 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role:           CheckStageRoleValidation,
				Mode:           CheckExecutionModeRunOnce,
				CoverageWindow: duration(5 * time.Minute),
			},
			wantError: "must not declare",
		},
		{
			name:     "coverage exceeds deadline",
			deadline: 10 * time.Minute,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModeContinuous,
				CoverageWindow:   duration(11 * time.Minute),
				IterationTimeout: duration(time.Minute),
				MinExecutions:    count(2),
				MaxGap:           duration(2 * time.Minute),
			},
			wantError: "must not exceed backend",
		},
		{
			name:     "periodic minimum is unschedulable",
			deadline: time.Hour,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModePeriodic,
				CoverageWindow:   duration(30 * time.Minute),
				IterationTimeout: duration(5 * time.Minute),
				Interval:         duration(10 * time.Minute),
				MinExecutions:    count(4),
				MaxGap:           duration(15 * time.Minute),
			},
			wantError: "cannot fit",
		},
		{
			name:     "fractional repeated duration",
			deadline: time.Hour,
			execution: &CheckExecutionPolicy{
				Role:             CheckStageRoleValidation,
				Mode:             CheckExecutionModeContinuous,
				CoverageWindow:   duration(1500 * time.Millisecond),
				IterationTimeout: duration(time.Second),
				MinExecutions:    count(2),
				MaxGap:           duration(time.Second),
			},
			wantError: "whole number of seconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateCheckExecutionPolicy(tt.deadline, tt.execution)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateCheckExecutionPolicy() error = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateCheckExecutionPolicy() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestSupportsSustainedExecution(t *testing.T) {
	t.Parallel()

	if SupportsSustainedExecution(nil) {
		t.Fatal("legacy execution unexpectedly supports sustained qualification")
	}

	if SupportsSustainedExecution(&CheckExecutionPolicy{Mode: CheckExecutionModeRunOnce}) {
		t.Fatal("RunOnce unexpectedly supports sustained qualification")
	}

	for _, mode := range []CheckExecutionMode{
		CheckExecutionModeContinuous,
		CheckExecutionModePeriodic,
	} {
		if !SupportsSustainedExecution(&CheckExecutionPolicy{Mode: mode}) {
			t.Fatalf("%s does not support sustained qualification", mode)
		}
	}
}

func TestValidateStageSchedule(t *testing.T) {
	t.Parallel()

	duration := func(value time.Duration) *metav1.Duration {
		return &metav1.Duration{Duration: value}
	}
	count := func(value int32) *int32 {
		return &value
	}

	tests := []struct {
		name      string
		schedule  StageSchedule
		wantError string
	}{
		{
			name: "run once",
			schedule: StageSchedule{
				Mode:              CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
			},
		},
		{
			name: "continuous",
			schedule: StageSchedule{
				Mode:              CheckExecutionModeContinuous,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
				CoverageWindow:    duration(10 * time.Minute),
				MinExecutions:     count(2),
				MaxGap:            duration(2 * time.Minute),
			},
		},
		{
			name: "periodic",
			schedule: StageSchedule{
				Mode:              CheckExecutionModePeriodic,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
				CoverageWindow:    duration(10 * time.Minute),
				Interval:          duration(2 * time.Minute),
				MinExecutions:     count(4),
				MaxGap:            duration(3 * time.Minute),
			},
		},
		{
			name: "run once with coverage",
			schedule: StageSchedule{
				Mode:              CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
				CoverageWindow:    duration(10 * time.Minute),
			},
			wantError: "must not declare",
		},
		{
			name: "unschedulable periodic minimum",
			schedule: StageSchedule{
				Mode:              CheckExecutionModePeriodic,
				InvocationTimeout: metav1.Duration{Duration: 2 * time.Minute},
				CoverageWindow:    duration(10 * time.Minute),
				Interval:          duration(4 * time.Minute),
				MinExecutions:     count(4),
				MaxGap:            duration(5 * time.Minute),
			},
			wantError: "cannot fit",
		},
		{
			name: "fractional repeated duration",
			schedule: StageSchedule{
				Mode:              CheckExecutionModeContinuous,
				InvocationTimeout: metav1.Duration{Duration: time.Second},
				CoverageWindow:    duration(1500 * time.Millisecond),
				MinExecutions:     count(2),
				MaxGap:            duration(time.Second),
			},
			wantError: "whole number of seconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateStageSchedule(tt.schedule)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateStageSchedule() error = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateStageSchedule() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestDeriveResolvedPlanDeadlineUsesConcurrentPhaseMaximum(t *testing.T) {
	t.Parallel()

	phases := []ResolvedPhasePlan{
		{
			Name: "startup",
			Role: CheckStageRoleStartup,
			Stages: []ResolvedStagePlan{
				{
					Name: "short",
					Schedule: StageSchedule{
						Mode:              CheckExecutionModeRunOnce,
						InvocationTimeout: metav1.Duration{Duration: 2 * time.Minute},
					},
				},
				{
					Name: "long",
					Schedule: StageSchedule{
						Mode:              CheckExecutionModeRunOnce,
						InvocationTimeout: metav1.Duration{Duration: 5 * time.Minute},
					},
				},
			},
		},
		{
			Name: "validation",
			Role: CheckStageRoleValidation,
			Stages: []ResolvedStagePlan{{
				Name: "periodic",
				Schedule: StageSchedule{
					Mode:              CheckExecutionModePeriodic,
					InvocationTimeout: metav1.Duration{Duration: time.Minute},
					CoverageWindow:    &metav1.Duration{Duration: 10 * time.Minute},
					Interval:          &metav1.Duration{Duration: 2 * time.Minute},
					MinExecutions:     func() *int32 { value := int32(4); return &value }(),
					MaxGap:            &metav1.Duration{Duration: 3 * time.Minute},
				},
			}},
		},
	}

	got, err := DeriveResolvedPlanDeadline(phases)
	if err != nil {
		t.Fatalf("DeriveResolvedPlanDeadline() error = %v", err)
	}

	const want = 18 * time.Minute
	if got != want {
		t.Fatalf("DeriveResolvedPlanDeadline() = %s, want %s", got, want)
	}
}

func TestDeriveResolvedPlanDeadlineIncludesRetryTail(t *testing.T) {
	t.Parallel()

	phases := []ResolvedPhasePlan{{
		Name: "validation",
		Role: CheckStageRoleValidation,
		Stages: []ResolvedStagePlan{
			{
				Name: "run-once",
				Schedule: StageSchedule{
					Mode:              CheckExecutionModeRunOnce,
					InvocationTimeout: metav1.Duration{Duration: 2 * time.Minute},
				},
				Retry: &StageRetryPolicy{MaxAttempts: 3},
			},
			{
				Name: "periodic",
				Schedule: StageSchedule{
					Mode:              CheckExecutionModePeriodic,
					InvocationTimeout: metav1.Duration{Duration: time.Minute},
					CoverageWindow:    &metav1.Duration{Duration: 10 * time.Minute},
					Interval:          &metav1.Duration{Duration: 2 * time.Minute},
					MinExecutions:     func() *int32 { value := int32(4); return &value }(),
					MaxGap:            &metav1.Duration{Duration: 3 * time.Minute},
				},
				Retry: &StageRetryPolicy{MaxAttempts: 2},
			},
		},
	}}

	got, err := DeriveResolvedPlanDeadline(phases)
	if err != nil {
		t.Fatalf("DeriveResolvedPlanDeadline() error = %v", err)
	}

	const want = 13 * time.Minute
	if got != want {
		t.Fatalf("DeriveResolvedPlanDeadline() = %s, want %s", got, want)
	}
}
