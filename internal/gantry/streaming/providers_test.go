// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"errors"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestProviderFailuresSuppressAndExpire(t *testing.T) {
	d, err := digest.Parse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(100, 0)
	failures := newProviderFailures()
	failures.now = func() time.Time { return now }

	stale := ifaces.Provider{NodeID: "stale", Addr: "10.0.0.1:5001"}
	unavailable := ifaces.Provider{NodeID: "down", Addr: "10.0.0.2:5001"}
	suspicious := ifaces.Provider{NodeID: "bad", Addr: "10.0.0.3:5001"}
	healthy := ifaces.Provider{NodeID: "healthy", Addr: "10.0.0.4:5001"}

	failures.record(d, stale, &ifaces.ErrNotFound{Digest: d})
	failures.record(d, unavailable, errors.New("connection refused"))
	failures.record(d, suspicious, &ifaces.ErrPeerProtocol{PeerAddr: suspicious.Addr, Err: errors.New("invalid range")})

	filtered := failures.filter(d, []ifaces.Provider{stale, unavailable, suspicious, healthy}, "self")
	if len(filtered) != 1 || filtered[0] != healthy {
		t.Fatalf("filtered = %+v, want only healthy", filtered)
	}

	now = now.Add(defaultSuspiciousProviderTTL + time.Second)

	filtered = failures.filter(d, []ifaces.Provider{stale, unavailable, suspicious, healthy}, "self")
	if len(filtered) != 4 {
		t.Fatalf("after expiry filtered = %+v, want all providers", filtered)
	}
}
