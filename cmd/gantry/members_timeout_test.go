// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"
	"time"
)

func TestMemberSyncDefaultTimeout(t *testing.T) {
	t.Parallel()

	if memberSyncDefaultTimeout != 30*time.Minute {
		t.Fatalf("memberSyncDefaultTimeout = %s, want 30m", memberSyncDefaultTimeout)
	}
}
