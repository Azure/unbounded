// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

func TestDigestToCID_Deterministic(t *testing.T) {
	d := digest.MustParse("sha256:" + zeros(64))
	c1, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID: %v", err)
	}
	c2, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID (2nd): %v", err)
	}
	if !c1.Equals(c2) {
		t.Errorf("CIDs differ across calls: %s vs %s", c1, c2)
	}
	if c1.Version() != 1 {
		t.Errorf("CID version = %d, want 1", c1.Version())
	}
	// Two distinct digests must produce two distinct CIDs.
	d2 := digest.MustParse("sha256:" + ones(64))
	c3, err := DigestToCID(d2)
	if err != nil {
		t.Fatalf("DigestToCID (d2): %v", err)
	}
	if c1.Equals(c3) {
		t.Error("CIDs equal across different digests")
	}
}

func TestHostBringUpEphemeral(t *testing.T) {
	// Smoke test: New() with an ephemeral identity returns a usable host;
	// Provide on a fresh DHT errors because there are no peers yet, but
	// the host itself must boot cleanly and Close cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := New(ctx, Options{
		IdentityPath:   "",
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers: nil,
		ProtocolPrefix: "/gantry",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() }) //nolint:errcheck // best-effort close

	if h.PeerID() == "" {
		t.Error("PeerID empty")
	}
	if len(h.Addrs()) == 0 {
		t.Error("no listen addrs")
	}
	if got, want := h.Health(), 1.0; got != want {
		t.Errorf("Health() = %v, want %v (no monitor wired in test mode)", got, want)
	}
}

func TestHostPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "libp2p.key")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	id1 := first.PeerID()
	_ = first.Close() //nolint:errcheck // best-effort close

	second, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() { _ = second.Close() }() //nolint:errcheck // best-effort close
	if second.PeerID() != id1 {
		t.Errorf("PeerID changed across restarts: %s vs %s", id1, second.PeerID())
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

func ones(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '1'
	}
	return string(b)
}
