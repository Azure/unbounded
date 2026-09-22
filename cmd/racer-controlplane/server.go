// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
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
}

// Server serves immutable configurations from persisted topology generations.
type Server struct {
	pkiReady       <-chan struct{}
	trustHeartbeat func(*http.Request, string) error
	mu             sync.Mutex
	source         *generationSource
	controlStore   stateStore
	rollouts       map[string]*rollout
	credentials    credentialCache
	reviewClient   client.Client // Dedicated TokenReview limiter; configured before serving.
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

	snapshot := t.snapshot(hex.EncodeToString(key.node[:]))
	if snapshot == nil {
		return nil, nil
	}

	serialized, err := marshalSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	next, err := newEntry(serialized, snapshot.Revision)
	if err != nil {
		return nil, err
	}

	size := len(next.snapshot) + len(next.body)
	if size <= snapshotCacheBytes {
		for x.bytes+size > snapshotCacheBytes {
			x.remove(x.lru.Back())
		}

		x.cache[key] = x.lru.PushFront(cachedEntry{key, next})
		x.bytes += size
	}

	return next, nil
}
