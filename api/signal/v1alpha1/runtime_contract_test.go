// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
	"time"
)

func TestValidateRuntimeContractAcceptsExactDeadlineReserve(t *testing.T) {
	t.Parallel()

	err := ValidateRuntimeContract(
		true,
		&RuntimeStage{Name: "collective-bandwidth"},
		9*time.Minute,
		10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateRuntimeContractAllowsNonStageScript(t *testing.T) {
	t.Parallel()

	if err := ValidateRuntimeContract(false, nil, time.Minute, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRuntimeContractRejectsInvalidStageAndDeadlines(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		requiresStage   bool
		stage           *RuntimeStage
		backendDeadline time.Duration
		outerDeadline   time.Duration
		want            string
	}{
		"missing required stage": {
			requiresStage:   true,
			backendDeadline: time.Minute,
			outerDeadline:   2 * time.Minute,
			want:            "runtime stage is required",
		},
		"empty stage": {
			stage:           &RuntimeStage{},
			backendDeadline: time.Minute,
			outerDeadline:   2 * time.Minute,
			want:            "runtime stage name is invalid",
		},
		"malformed stage": {
			stage:           &RuntimeStage{Name: "Collective/Bandwidth"},
			backendDeadline: time.Minute,
			outerDeadline:   2 * time.Minute,
			want:            "runtime stage name is invalid",
		},
		"zero backend deadline": {
			backendDeadline: 0,
			outerDeadline:   2 * time.Minute,
			want:            "must be positive",
		},
		"fractional seconds": {
			backendDeadline: time.Minute + time.Millisecond,
			outerDeadline:   2 * time.Minute,
			want:            "whole seconds",
		},
		"reserve one second too short": {
			backendDeadline: 61 * time.Second,
			outerDeadline:   2 * time.Minute,
			want:            "at least 1m0s",
		},
		"backend equals outer": {
			backendDeadline: 2 * time.Minute,
			outerDeadline:   2 * time.Minute,
			want:            "at least 1m0s",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := ValidateRuntimeContract(
				test.requiresStage,
				test.stage,
				test.backendDeadline,
				test.outerDeadline,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRuntimeContract() error = %v, want %q", err, test.want)
			}
		})
	}
}
