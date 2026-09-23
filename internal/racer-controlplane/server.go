// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Immutable publication.

type recipient struct {
	universe [32]byte
	node     [32]byte
}

// Entries are immutable after publication.
type entry struct {
	snapshot []byte
	body     []byte
	etag     string
	revision uint64
	digest   [32]byte
}

// Server serves immutable configurations from persisted topology generations.
type Server struct {
	pkiReady        <-chan struct{}
	trustHeartbeat  func(*http.Request, string) error
	mu              sync.Mutex
	snapshotBuild   chan struct{}                                   // One cold builder; waiters never hold mu.
	buildSnapshot   func(*topologyIndex, recipient) (*entry, error) // Optional builder override, set before serving.
	source          *generationSource
	controlStore    stateStore
	rollouts        map[string]*rollout
	credentials     credentialCache
	reviewClient    client.Client                  // Dedicated TokenReview limiter; configured before serving.
	storagePolicies map[string]storagePolicyRecord // Node identity, independent of topology.
	storageReports  map[recipient]storageReport
}

func marshalSnapshot(snapshot *pb.Snapshot) ([]byte, error) {
	return (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot)
}

func newEntry(snapshot []byte, revision uint64) (*entry, error) {
	body, err := configuration(snapshot)
	if err != nil {
		return nil, err
	}

	return &entry{
		snapshot: snapshot,
		body:     body,
		etag:     fmt.Sprintf(`"%x"`, sha256.Sum256(body)),
		revision: revision,
		digest:   sha256.Sum256(snapshot),
	}, nil
}

func configuration(snapshot []byte) ([]byte, error) {
	// Embed the deterministic snapshot bytes verbatim in Configuration.snapshot.
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), snapshot), nil
}

// Lazy generation installation and bounded serialized-response cache.

// These fields are protected by Server.mu. LRU entries retain serialized replies.
type generationSource struct {
	topologies map[[32]byte]*topologyIndex
	cache      map[recipient]*list.Element
	lru        *list.List
	bytes      int
}
type cachedEntry struct {
	key   recipient
	entry *entry
}

const snapshotCacheBytes = 64 * 1024 * 1024

func (s *Server) install(t *topologyIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.installLocked(t)
}

func (s *Server) installLocked(t *topologyIndex) error {
	if s.source == nil {
		s.source = &generationSource{topologies: map[[32]byte]*topologyIndex{}, cache: map[recipient]*list.Element{}, lru: list.New()}
	}

	x := s.source
	raw := identityBytes("universe", t.g.Universe)

	key := [32]byte(raw)
	if old := x.topologies[key]; old != nil {
		if t.g.Revision < old.g.Revision {
			return fmt.Errorf("persisted generation rollback")
		}

		if t.g.Revision == old.g.Revision {
			if !reflect.DeepEqual(t.g, old.g) {
				return fmt.Errorf("generation revision reused for different contents")
			}

			return nil
		}
	}

	x.topologies[key] = t
	for key, el := range x.cache {
		if key.universe == [32]byte(raw) {
			x.remove(el)
		}
	}

	return nil
}

func (x *generationSource) remove(el *list.Element) {
	c, ok := el.Value.(cachedEntry)
	if !ok {
		panic("invalid snapshot cache entry")
	}

	x.bytes -= len(c.entry.snapshot) + len(c.entry.body)
	delete(x.cache, c.key)
	x.lru.Remove(el)
}

func (s *Server) current(key recipient) (*entry, error) {
	if s.source == nil {
		return nil, nil
	}

	x := s.source

	t := x.topologies[key.universe]
	if t == nil {
		return nil, nil
	}

	if el := x.cache[key]; el != nil {
		x.lru.MoveToFront(el)

		cached, ok := el.Value.(cachedEntry)
		if !ok {
			return nil, fmt.Errorf("invalid snapshot cache entry")
		}

		return cached.entry, nil
	}

	next, err := buildSnapshotEntry(t, key)
	if err != nil || next == nil {
		return next, err
	}

	x.retain(t, key, next)

	return next, nil
}

func buildSnapshotEntry(t *topologyIndex, key recipient) (*entry, error) {
	snapshot := t.snapshot(hex.EncodeToString(key.node[:]))
	if snapshot == nil {
		return nil, nil
	}

	serialized, err := marshalSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	return newEntry(serialized, snapshot.Revision)
}

func (x *generationSource) retain(t *topologyIndex, key recipient, next *entry) {
	if t.snapshotDigests == nil {
		t.snapshotDigests = make(map[[32]byte][32]byte)
	}

	t.snapshotDigests[key.node] = next.digest
	if el := x.cache[key]; el != nil {
		x.remove(el)
	}

	size := len(next.snapshot) + len(next.body)
	if size <= snapshotCacheBytes {
		for x.bytes+size > snapshotCacheBytes {
			x.remove(x.lru.Back())
		}

		x.cache[key] = x.lru.PushFront(cachedEntry{key, next})
		x.bytes += size
	}
}

type controlSnapshot struct {
	topology *topologyIndex
	payload  *entry // Nil when the recipient already has the exact snapshot.
	digest   [32]byte
}

// snapshotForControl is entered and returns with mu held. Cold construction is
// serialized separately to bound transient memory without blocking heartbeats
// that need only a digest. Recheck publication and authorization after every wait.
func (s *Server) snapshotForControl(ctx context.Context, key recipient, podUID, reported string, needsConfig bool) (*controlSnapshot, int, error) {
	haveBuilder := false

	defer func() {
		if haveBuilder {
			<-s.snapshotBuild
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		if s.source == nil {
			return nil, 503, fmt.Errorf("controller unavailable")
		}

		x := s.source

		t := x.topologies[key.universe]
		if t == nil {
			return nil, 404, fmt.Errorf("unknown universe")
		}

		name, ok := t.byID[hex.EncodeToString(key.node[:])]
		if !ok || t.g.Nodes[name].PodUID != podUID {
			return nil, 403, fmt.Errorf("pod is not selected for node")
		}

		if digest, ok := t.snapshotDigests[key.node]; ok && !needsConfig && reported == hex.EncodeToString(digest[:]) {
			return &controlSnapshot{topology: t, digest: digest}, 0, nil
		}

		if el := x.cache[key]; el != nil {
			x.lru.MoveToFront(el)

			cached, ok := el.Value.(cachedEntry)
			if !ok {
				return nil, 503, fmt.Errorf("invalid snapshot cache entry")
			}

			payload := cached.entry

			return &controlSnapshot{t, payload, t.snapshotDigests[key.node]}, 0, nil
		}

		if !haveBuilder {
			if s.snapshotBuild == nil {
				s.snapshotBuild = make(chan struct{}, 1)
			}

			gate := s.snapshotBuild
			s.mu.Unlock()

			select {
			case gate <- struct{}{}:
				haveBuilder = true
			case <-ctx.Done():
			}

			s.mu.Lock()

			continue
		}

		build := s.buildSnapshot
		if build == nil {
			build = buildSnapshotEntry
		}
		s.mu.Unlock()

		payload, err := build(t, key)

		s.mu.Lock()
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}

		if s.source != x || x.topologies[key.universe] != t {
			continue
		}

		if err != nil || payload == nil {
			return nil, 503, fmt.Errorf("snapshot unavailable: %v", err)
		}

		x.retain(t, key, payload)

		return &controlSnapshot{t, payload, t.snapshotDigests[key.node]}, 0, nil
	}
}
