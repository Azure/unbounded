// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"testing"
	"unsafe"
)

// TestPeerLayout checks that the three access groups of Peer each fill one
// 64-byte line.
func TestPeerLayout(t *testing.T) {
	var p Peer

	if got := unsafe.Sizeof(p); got != 192 {
		t.Fatalf("sizeof(Peer) = %d, want 192", got)
	}

	if got := unsafe.Offsetof(p.latSum); got != 64 {
		t.Fatalf("offset of latSum = %d, want 64", got)
	}

	if got := unsafe.Offsetof(p.until); got != 128 {
		t.Fatalf("offset of until = %d, want 128", got)
	}

	if got := unsafe.Offsetof(p.degraded); got >= 64 {
		t.Fatalf("offset of degraded = %d, want < 64", got)
	}
}
