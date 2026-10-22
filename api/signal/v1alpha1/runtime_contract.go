// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const MinimumBackendDeadlineReserve = time.Minute

func ValidateRuntimeContract(
	scriptRequiresStage bool,
	stage *RuntimeStage,
	backendDeadline, outerDeadline time.Duration,
) error {
	if err := ValidateRuntimeStage(scriptRequiresStage, stage); err != nil {
		return err
	}

	return ValidateExecutionDeadlines(backendDeadline, outerDeadline)
}

func ValidateRuntimeStage(scriptRequiresStage bool, stage *RuntimeStage) error {
	if scriptRequiresStage && stage == nil {
		return errors.New("runtime stage is required by the script")
	}

	if stage != nil {
		if errors := validation.IsDNS1123Label(stage.Name); len(errors) > 0 {
			return fmt.Errorf("runtime stage name is invalid: %v", errors)
		}
	}

	return nil
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
