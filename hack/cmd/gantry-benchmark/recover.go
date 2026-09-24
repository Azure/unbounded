// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
)

func (b *benchmark) recoverDirectConfig(ctx context.Context, expectedCurrentSHA string) error {
	if err := b.validateContext(ctx); err != nil {
		return err
	}

	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	lockedBy, err := b.lockRunID(ctx)
	if err != nil {
		return err
	}

	if lockedBy != state.RunID {
		return fmt.Errorf("benchmark lock is owned by %q, state belongs to %q", lockedBy, state.RunID)
	}

	current, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	state, err = acceptCurrentDirectConfig(state, current, expectedCurrentSHA)
	if err != nil {
		return err
	}

	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf(
		"accepted current Gantry config sha256=%s for direct-mode cleanup\n",
		state.OriginalGantryConfigSHA,
	))

	return b.disable(ctx)
}

func acceptCurrentDirectConfig(
	state benchmarkState,
	current string,
	expectedCurrentSHA string,
) (benchmarkState, error) {
	if state.usesProxy() {
		return benchmarkState{}, fmt.Errorf("config-drift recovery requires a direct-mode benchmark")
	}

	if state.Status != "restore-failed" {
		return benchmarkState{}, fmt.Errorf("benchmark state is %q, want restore-failed", state.Status)
	}

	currentSHA := gantryConfigSHA(current)
	if currentSHA != expectedCurrentSHA {
		return benchmarkState{}, fmt.Errorf(
			"current Gantry config sha256 is %s, expected %s",
			currentSHA,
			expectedCurrentSHA,
		)
	}

	if currentSHA == state.OriginalGantryConfigSHA {
		return benchmarkState{}, fmt.Errorf("current Gantry config still matches the recorded original; run disable")
	}

	state.OriginalGantryConfig = current
	state.OriginalGantryConfigSHA = currentSHA
	state.GantryRestored = false

	return state, nil
}
