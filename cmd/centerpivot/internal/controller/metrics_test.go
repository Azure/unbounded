// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPhaseTimeIsTheTimeInThePhaseANodeLeft(t *testing.T) {
	t.Parallel()

	now := start.Add(10 * time.Minute)

	tests := map[string]struct {
		before, next State
		spent        time.Duration
		left         bool
	}{
		"to the next phase": {
			before: State{Phase: PhaseDraining, Since: start.Add(7 * time.Minute)},
			next:   State{Phase: PhaseFinalizing},
			spent:  3 * time.Minute, left: true,
		},
		"to a hold": {
			before: State{Phase: PhaseStaging, Since: start},
			next:   State{Phase: PhaseHeld},
			spent:  10 * time.Minute, left: true,
		},
		"back to Idle after the update": {
			before: State{Phase: PhaseRecovering, Since: start.Add(9 * time.Minute)},
			next:   State{Phase: PhaseIdle},
			spent:  time.Minute, left: true,
		},
		"still in the phase": {
			before: State{Phase: PhaseDraining, Since: start},
			next:   State{Phase: PhaseDraining},
		},
		"a phase that isn't an update's": {
			before: State{Phase: PhaseHeld, Since: start},
			next:   State{Phase: PhaseIdle},
		},
		"no start recorded": {
			before: State{Phase: PhaseStaging},
			next:   State{Phase: PhaseDraining},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			spent, left := phaseTime(tc.before, tc.next, now)
			assert.Equal(t, tc.left, left)
			assert.Equal(t, tc.spent, spent)
		})
	}
}
