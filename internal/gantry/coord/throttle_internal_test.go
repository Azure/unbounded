// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coord

import (
	"testing"
	"time"
)

// TestLogThrottle_AllowsFirstThenSuppressesWithinInterval verifies the
// throttle emits the first event, suppresses subsequent events inside the
// interval, and re-allows once the interval elapses. Time is injected so the
// test never sleeps.
func TestLogThrottle_AllowsFirstThenSuppressesWithinInterval(t *testing.T) {
	base := time.Unix(0, 0)
	tr := &logThrottle{interval: 30 * time.Second}

	if !tr.allow(base) {
		t.Fatal("first event = suppressed, want allowed")
	}

	if tr.allow(base.Add(time.Second)) {
		t.Fatal("event 1s later = allowed, want suppressed (within interval)")
	}

	if tr.allow(base.Add(29 * time.Second)) {
		t.Fatal("event 29s later = allowed, want suppressed (within interval)")
	}

	if !tr.allow(base.Add(30 * time.Second)) {
		t.Fatal("event at interval boundary = suppressed, want allowed")
	}

	// After re-allowing, the window resets from the new timestamp.
	if tr.allow(base.Add(30*time.Second + time.Second)) {
		t.Fatal("event just after re-allow = allowed, want suppressed")
	}
}

// TestLogThrottle_NilSafeViaServerHelper ensures the production default
// (constructed in NewServer) allows the first warning.
func TestLogThrottle_ZeroValueAllowsFirst(t *testing.T) {
	tr := &logThrottle{interval: time.Minute}
	if !tr.allow(time.Unix(100, 0)) {
		t.Fatal("zero-value throttle suppressed the first event")
	}
}
