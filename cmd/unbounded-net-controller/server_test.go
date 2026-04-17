// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"
	"time"
)

// TestFormatDurationAgo tests FormatDurationAgo.
func TestFormatDurationAgo(t *testing.T) {
	if got := formatDurationAgo(42 * time.Second); got != "42s ago" {
		t.Fatalf("unexpected seconds format: %s", got)
	}

	if got := formatDurationAgo(3 * time.Minute); got != "3m ago" {
		t.Fatalf("unexpected minutes format: %s", got)
	}

	if got := formatDurationAgo(5 * time.Hour); got != "5h ago" {
		t.Fatalf("unexpected hours format: %s", got)
	}
}
