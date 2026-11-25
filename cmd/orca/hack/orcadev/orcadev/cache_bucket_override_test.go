// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestRunCacheInspect_BucketOverrideReachesOrigin drives
// runCacheInspect end-to-end and verifies that the origin client
// factory sees the operator-supplied --bucket value (rather than the
// default that was on globalFlags before runCacheInspect ran). Guards
// the bug fix from a regression that would put the bucket assignment
// back AFTER newOriginClient is called.
//
// Not t.Parallel: swaps the package-level newOriginClient factory.
func TestRunCacheInspect_BucketOverrideReachesOrigin(t *testing.T) {
	var capturedBucket string

	original := newOriginClient
	newOriginClient = func(_ context.Context, g *globalFlags) (originClient, error) {
		capturedBucket = g.originBucket

		return nil, fmt.Errorf("test halt after capture")
	}

	t.Cleanup(func() { newOriginClient = original })

	g := defaultGlobalFlags()
	g.originBucket = "default-bucket"
	g.originID = "origin-id"
	// runCacheInspect now opens port-forwards before constructing
	// the origin client; this test doesn't need (or want) kubectl
	// involvement so we suppress the auto-forward entirely.
	g.autoPortForward = false

	o := &cacheInspectOpts{
		bucket: "requested-bucket",
		key:    "key",
	}

	err := runCacheInspect(context.Background(), g, o)
	if err == nil || !strings.Contains(err.Error(), "test halt") {
		t.Fatalf("runCacheInspect() = %v, want error containing 'test halt'", err)
	}

	if capturedBucket != "requested-bucket" {
		t.Fatalf("captured originBucket = %q, want requested-bucket", capturedBucket)
	}
}
