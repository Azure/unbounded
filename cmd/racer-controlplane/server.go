// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Immutable publication and signing-key rotation.

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
	mu           sync.Mutex
	signer       *signer // Protected by mu, including publication and rotation.
	source       *generationSource
	controlStore stateStore
	rollouts     map[string]*rollout
	credentials  credentialCache
	reviewClient client.Client // Dedicated TokenReview limiter; configured before serving.
}

func marshalSnapshot(snapshot *pb.Snapshot) ([]byte, error) {
	return (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot)
}

func newEntry(snapshot []byte, revision uint64, key *signer) (*entry, error) {
	body, err := configuration(snapshot, key)
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

// rotate installs a complete signed generation under the publication lock.
// Readers and publishers cannot observe a mixture of old and new signers.
func (s *Server) rotate(key *signer) error {
	if key == nil {
		return errors.New("cannot rotate to an unsigned configuration")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.signer != nil && s.signer.id == key.id {
		return nil
	}

	if s.source != nil {
		// Lazy generations are signed on demand after invalidating the cache.
		for s.source.lru.Len() != 0 {
			s.source.remove(s.source.lru.Back())
		}
	}

	s.signer = key

	return nil
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
	raw, _ := hex.DecodeString(identity("universe", t.g.Universe))

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
	c := el.Value.(cachedEntry)
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
		return el.Value.(cachedEntry).entry, nil
	}

	snapshot := t.snapshot(hex.EncodeToString(key.node[:]))
	if snapshot == nil {
		return nil, nil
	}

	serialized, err := marshalSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	next, err := newEntry(serialized, snapshot.Revision, s.signer)
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
