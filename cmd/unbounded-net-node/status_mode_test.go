// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestStatusModeRejectsRemovedPreferredAlias(t *testing.T) {
	if _, err := parseStatusWSAPIServerMode("preferred"); err == nil {
		t.Fatal("removed preferred alias was accepted")
	}

	for _, mode := range []string{"never", "fallback", ""} {
		if _, err := parseStatusWSAPIServerMode(mode); err != nil {
			t.Fatalf("current mode %q rejected: %v", mode, err)
		}
	}
}
