// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestMissingRangeDoesNotSuppressOtherRanges(t *testing.T) {
	for _, statusError := range []bool{false, true} {
		failures := newProviderFailures()
		d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		peer := ifaces.Provider{NodeID: "partial", Addr: "10.0.0.1:5001"}
		missing := httprange.Range{Start: 100, End: 199}

		var err error = &ifaces.ErrNotFound{Digest: d}
		if statusError {
			err = &ifaces.ErrPeerHTTPStatus{StatusCode: http.StatusNotFound}
		}

		failures.record(d, missing, peer, err)

		if got := failures.filter(d, missing, []ifaces.Provider{peer}, "self"); len(got) != 0 {
			t.Fatalf("missing interval retained peer: %v", got)
		}

		for _, available := range []httprange.Range{{Start: 0, End: 99}, {Start: 200, End: 299}, {Start: 0, End: 199}} {
			if got := failures.filter(d, available, []ifaces.Provider{peer}, "self"); len(got) != 1 {
				t.Fatalf("different interval %+v suppressed partial holder: %v", available, got)
			}
		}

		other := digest.MustParse("sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
		if got := failures.filter(other, missing, []ifaces.Provider{peer}, "self"); len(got) != 1 {
			t.Fatalf("another digest suppressed: %v", got)
		}
	}
}

func TestPeerAttemptReservesCallerDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	server := &Server{opts: Options{PeerAttemptTimeout: 10 * time.Second}}
	before := time.Now()

	attempt, stop := server.peerAttemptContext(parent)
	defer stop()

	deadline, ok := attempt.Deadline()
	if !ok || deadline.After(before.Add(time.Second/2)) {
		t.Fatalf("attempt deadline = %v, must leave half the caller budget", deadline)
	}

	stop()

	if parent.Err() != nil {
		t.Fatalf("ending peer attempt canceled caller: %v", parent.Err())
	}
}
