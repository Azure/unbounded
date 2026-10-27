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
