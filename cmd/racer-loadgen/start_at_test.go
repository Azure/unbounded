// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"
	"time"
)

func TestUntilStart(t *testing.T) {
	now := time.Unix(1000, 0)

	for name, tc := range map[string]struct {
		startAt int64
		want    time.Duration
	}{
		"unset":  {0, 0},
		"past":   {900, 0},
		"now":    {1000, 0},
		"future": {1030, 30 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if got := untilStart(tc.startAt, now); got != tc.want {
				t.Fatalf("untilStart(%d) = %v, want %v", tc.startAt, got, tc.want)
			}
		})
	}
}

func TestParseRejectsNegativeStartAt(t *testing.T) {
	if _, err := parseOptions([]string{"--start-at=-1"}, io.Discard); err == nil {
		t.Fatal("expected an error for a negative start-at")
	}
}
