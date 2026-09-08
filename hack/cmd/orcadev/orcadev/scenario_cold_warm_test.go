// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"context"
	"errors"
	"strings"
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

// TestRunScenarioColdWarmWith_DropCacheFailurePropagates drives the
// cold-warm scenario end-to-end with a fake cachestore that fails
// the chunk Delete, and verifies the scenario aborts with the
// drop_cache step marked failed (vs. silently proceeding into a
// false-positive cold GET).
//
// Uses fakeCachestore.matchAll so the clear loop's first Head probe
// reports the chunk as present and the loop enters the Delete
// branch where the injected failure surfaces.
func TestRunScenarioColdWarmWith_DropCacheFailurePropagates(t *testing.T) {
	t.Parallel()

	const (
		bucket   = "scenarios-bucket"
		originID = "test-origin"
	)

	g := defaultGlobalFlags()
	g.originID = originID
	g.originBucket = bucket
	g.ensureContainer = false

	o := &scenarioOpts{
		sizeStr: "1KiB",
		output:  "text",
	}

	res := &scenarioResult{
		SchemaVersion: 1,
		Tool:          "orcadev",
		Subcommand:    "scenario",
		Scenario:      "cold-warm",
		Config:        map[string]any{},
		Result:        "pass",
	}

	oc := newFakeOriginClient("fake", bucket)

	deleteErr := errors.New("simulated cachestore delete failure")
	cs := newFakeCachestore()
	cs.matchAll = true
	cs.deleteErr = deleteErr

	// edge is unreached because drop_cache failure aborts before
	// the cold GET step; a dial-immediately-refused URL is fine.
	edge := newEdgeClient("http://127.0.0.1:1", time.Millisecond)

	err := runScenarioColdWarmWith(context.Background(), g, o, res, oc, cs, edge)
	if err == nil {
		t.Fatal("runScenarioColdWarmWith() = nil, want drop_cache failure")
	}

	if !strings.Contains(err.Error(), "simulated cachestore delete failure") {
		t.Fatalf("err = %v, want injected delete error", err)
	}

	// Locate the drop_cache step and confirm it was marked failed.
	var dropStep *scenarioStep

	for i := range res.Steps {
		if res.Steps[i].Name == "drop_cache" {
			dropStep = &res.Steps[i]

			break
		}
	}

	if dropStep == nil {
		t.Fatalf("steps = %+v, want a drop_cache step", res.Steps)
	}

	if dropStep.OK {
		t.Fatalf("drop_cache step OK = true, want false")
	}

	// Confirm the cold GET step did NOT run (we aborted at
	// drop_cache).
	for _, s := range res.Steps {
		if s.Name == "cold_get" {
			t.Fatalf("cold_get step ran despite drop_cache failure: %+v", s)
		}
	}
}
