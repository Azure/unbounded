// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type seedStore struct {
	mu    sync.Mutex
	blobs map[digest.Digest][]byte
}

func newSeedStore() *seedStore {
	return &seedStore{blobs: map[digest.Digest][]byte{}}
}

func (s *seedStore) Has(_ context.Context, d digest.Digest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.blobs[d]

	return ok, nil
}

func (s *seedStore) Open(_ context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, ok := s.blobs[d]
	if !ok {
		return nil, 0, &ifaces.ErrNotFound{}
	}

	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func (s *seedStore) Writer(_ context.Context, d digest.Digest) (ifaces.ContentWriter, error) {
	return &seedWriter{store: s, digest: d}, nil
}

type seedWriter struct {
	store  *seedStore
	digest digest.Digest
	buf    bytes.Buffer
}

func (w *seedWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *seedWriter) Commit(context.Context) error {
	sum := sha256.Sum256(w.buf.Bytes())
	if "sha256:"+hex.EncodeToString(sum[:]) != w.digest.String() {
		return fmt.Errorf("digest mismatch for %s", w.digest)
	}

	w.store.mu.Lock()
	defer w.store.mu.Unlock()

	w.store.blobs[w.digest] = append([]byte(nil), w.buf.Bytes()...)

	return nil
}

func (w *seedWriter) Abort(context.Context) error {
	w.buf.Reset()

	return nil
}

type seedProviderStub struct {
	addr  string
	calls int
	mu    sync.Mutex
}

func (s *seedProviderStub) FindProviders(_ context.Context, _ digest.Digest) ([]ifaces.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++

	return []ifaces.Provider{{NodeID: "peer", Addr: s.addr}}, nil
}

type seedPeerStub struct {
	source *seedStore
	mu     sync.Mutex
	served int
}

func (s *seedPeerStub) FetchFromPeer(ctx context.Context, _ string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	body, size, err := s.source.Open(ctx, ref.Digest)
	if err != nil {
		return nil, 0, "", err
	}

	s.mu.Lock()
	s.served++
	s.mu.Unlock()

	return body, size, "application/octet-stream", nil
}

type seedOriginStub struct {
	source *seedStore
	mu     sync.Mutex
	pulls  int
}

func (s *seedOriginStub) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	body, size, err := s.source.Open(ctx, ref.Digest)
	if err != nil {
		return nil, 0, err
	}

	s.mu.Lock()
	s.pulls++
	s.mu.Unlock()

	return body, size, nil
}

func (s *seedOriginStub) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return 0, "", nil
}

func seedDigest(t *testing.T, payload []byte) digest.Digest {
	t.Helper()

	sum := sha256.Sum256(payload)

	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}

	return d
}

// The requirement: once a node is served a manifest, that node holds every
// child layer locally.
func TestLayerSeederStoresEveryLayerLocally(t *testing.T) {
	remote := newSeedStore()
	local := newSeedStore()

	children := make([]coldstart.ChildDigest, 0, 5)

	for index := range 5 {
		payload := []byte(fmt.Sprintf("layer-%d-payload", index))
		d := seedDigest(t, payload)
		remote.blobs[d] = payload
		children = append(children, coldstart.ChildDigest{Digest: d, Kind: ifaces.KindBlob})
	}

	peer := &seedPeerStub{source: remote}
	origin := &seedOriginStub{source: remote}

	outcomes := map[string]int{}

	var outcomeMu sync.Mutex

	seeder := newLayerSeeder(layerSeederOptions{
		Store:  local,
		DHT:    &seedProviderStub{addr: "10.0.0.2:5001"},
		Peer:   peer,
		Origin: origin,
		OnOutcome: func(outcome string) {
			outcomeMu.Lock()
			outcomes[outcome]++
			outcomeMu.Unlock()
		},
		PeerPatience: time.Second,
		PollInterval: 10 * time.Millisecond,
		Budget:       30 * time.Second,
	})

	seeder.Seed(context.Background(), "registry.example", "team/app", children)

	for _, child := range children {
		has, err := local.Has(context.Background(), child.Digest)
		if err != nil || !has {
			t.Fatalf("layer %s missing locally after seeding (err=%v)", child.Digest, err)
		}
	}

	if outcomes["peer"] != len(children) {
		t.Fatalf("peer-seeded layers = %d, want %d (outcomes=%v)", outcomes["peer"], len(children), outcomes)
	}

	if origin.pulls != 0 {
		t.Fatalf("origin pulls = %d, want 0 while a peer serves the layer", origin.pulls)
	}
}

// With no peer advertising the digest, seeding must still converge via origin
// rather than leaving the node without the layer.
func TestLayerSeederFallsBackToOriginWhenNoPeerHasLayer(t *testing.T) {
	remote := newSeedStore()
	local := newSeedStore()

	payload := []byte("only-at-origin")
	d := seedDigest(t, payload)
	remote.blobs[d] = payload

	origin := &seedOriginStub{source: remote}

	seeder := newLayerSeeder(layerSeederOptions{
		Store:        local,
		DHT:          &emptyProviderStub{},
		Peer:         &seedPeerStub{source: newSeedStore()},
		Origin:       origin,
		PeerPatience: 50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Budget:       30 * time.Second,
	})

	seeder.Seed(context.Background(), "registry.example", "team/app",
		[]coldstart.ChildDigest{{Digest: d, Kind: ifaces.KindBlob}})

	has, err := local.Has(context.Background(), d)
	if err != nil || !has {
		t.Fatalf("layer missing after origin fallback (err=%v)", err)
	}

	if origin.pulls != 1 {
		t.Fatalf("origin pulls = %d, want 1", origin.pulls)
	}
}

// Peers are preferred: origin must not be contacted while patience remains and
// a provider is serving, otherwise a large rollout becomes one pull per node.
func TestLayerSeederSkipsAlreadyPresentLayers(t *testing.T) {
	local := newSeedStore()
	remote := newSeedStore()

	payload := []byte("already-here")
	d := seedDigest(t, payload)
	local.blobs[d] = payload
	remote.blobs[d] = payload

	peer := &seedPeerStub{source: remote}
	origin := &seedOriginStub{source: remote}

	seeder := newLayerSeeder(layerSeederOptions{
		Store:        local,
		DHT:          &seedProviderStub{addr: "10.0.0.2:5001"},
		Peer:         peer,
		Origin:       origin,
		PeerPatience: time.Second,
		PollInterval: 10 * time.Millisecond,
		Budget:       30 * time.Second,
	})

	seeder.Seed(context.Background(), "registry.example", "team/app",
		[]coldstart.ChildDigest{{Digest: d, Kind: ifaces.KindBlob}})

	if peer.served != 0 || origin.pulls != 0 {
		t.Fatalf("re-fetched present layer: peer=%d origin=%d", peer.served, origin.pulls)
	}
}

type emptyProviderStub struct{}

func (*emptyProviderStub) FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error) {
	return nil, nil
}
