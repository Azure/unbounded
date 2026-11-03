// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"strings"
	"testing"
	"time"
)

func TestValidatePrefetchID(t *testing.T) {
	for _, id := range []string{"a", "epoch-17", "A.b_c-9", "...", ".a", strings.Repeat("x", 128)} {
		if err := ValidatePrefetchID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}

	for _, id := range []string{"", ".", "..", "a/b", "a b", "a?b", "é", strings.Repeat("x", 129)} {
		if err := ValidatePrefetchID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestFormatWait(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{2 * time.Minute, "120s"},
		{500 * time.Millisecond, "500ms"},
		{1500 * time.Millisecond, "1500ms"},
		{time.Nanosecond, "1ms"},
		{time.Second + time.Microsecond, "1001ms"},
	} {
		if got := FormatWait(tt.d); got != tt.want {
			t.Errorf("FormatWait(%v) = %q, want %q", tt.d, got, tt.want)
		}

		if parsed, err := time.ParseDuration(FormatWait(tt.d)); err != nil || parsed < tt.d {
			t.Errorf("FormatWait(%v) does not round-trip: %v %v", tt.d, parsed, err)
		}
	}
}

func TestPrefetchQueryEncodeOrder(t *testing.T) {
	if got := (PrefetchQuery{}).Encode(); got != "" {
		t.Fatal(got)
	}

	got := PrefetchQuery{Wait: time.Second, Coverage: true, Refs: "failed", Cursor: "a+b/="}.Encode()
	if got != "wait=1s&coverage=1&refs=failed&cursor=a%2Bb%2F%3D" {
		t.Fatal(got)
	}

	q, err := ParsePrefetchQuery(got, time.Minute)
	if err != nil || q != (PrefetchQuery{Wait: time.Second, Coverage: true, Refs: "failed", Cursor: "a+b/="}) {
		t.Fatal(q, err)
	}
}

func TestParsePrefetchQueryRejects(t *testing.T) {
	for _, raw := range []string{
		"bogus=1", "wait=1s&wait=2s", "wait=-1s", "wait=61s", "wait=abc", "coverage=yes",
		"refs=some", "cursor=", "%zz",
	} {
		if _, err := ParsePrefetchQuery(raw, time.Minute); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}

	if q, err := ParsePrefetchQuery("wait=60s&coverage=0&refs=all", time.Minute); err != nil || q.Wait != time.Minute || q.Coverage || q.Refs != "all" {
		t.Fatal(q, err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "5": 5 * time.Second, " 60 ": time.Minute, "-1": 0, "1.5": 0, "soon": 0} {
		if got := ParseRetryAfter(value); got != want {
			t.Errorf("%q: %v, want %v", value, got, want)
		}
	}
}
