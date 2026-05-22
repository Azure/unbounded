// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"errors"
	"testing"
	"time"
)

func TestRecordDropCacheStepReturnsError(t *testing.T) {
	t.Parallel()

	res := &scenarioResult{Result: "pass"}
	dropErr := errors.New("drop failed")

	if err := recordDropCacheStep(res, time.Now(), dropErr); !errors.Is(err, dropErr) {
		t.Fatalf("recordDropCacheStep() = %v, want %v", err, dropErr)
	}

	if len(res.Steps) != 1 || res.Steps[0].OK {
		t.Fatalf("drop_cache step = %#v, want failed step", res.Steps)
	}
}
