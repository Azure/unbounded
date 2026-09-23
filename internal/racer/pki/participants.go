// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const participantLabel = "racer.unbounded-cloud.io/pki-participants"

type shardReference struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type participantShard struct {
	Members map[string]*member `json:"members"`
	Retired map[string]bool    `json:"retired"`
}

type cachedShard struct {
	bucket string
	shard  participantShard
}

// Independent buckets keep a 100,000-node fleet well below the
// per-object limit, including two live leaves and rotation evidence per node.
func participantBucket(key string) string {
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%03x", (uint16(hash[0])<<8|uint16(hash[1]))&1023)
}

func validateShardReferences(s *state) error {
	if s.Version == 1 {
		if len(s.Shards) != 0 {
			return errors.New("legacy state contains participant shards")
		}

		return nil
	}

	for bucket, ref := range s.Shards {
		if len(bucket) != 3 || bucket[0] > '3' || strings.Trim(bucket, "0123456789abcdef") != "" || !hexID.MatchString(ref.Digest) || !strings.HasPrefix(ref.Name, "racer-pki-") {
			return errors.New("invalid participant shard reference")
		}
	}

	if len(s.Members) != 0 || len(s.Retired) != 0 {
		return errors.New("sharded state contains inline participants")
	}

	return nil
}

func cloneMember(p *member) *member {
	copy := *p

	copy.Leaves = make(map[string]leafRecord, len(p.Leaves))
	for k, v := range p.Leaves {
		copy.Leaves[k] = v
	}

	return &copy
}

func (m *Manager) loadParticipants(ctx context.Context, s *state, key string) error {
	if s.Version == 1 {
		return nil
	}

	m.cacheMu.Lock()

	if m.shardCache == nil {
		m.shardCache = make(map[string]cachedShard)
	}
	// Bound the cache to the current committed references, not historical writes.
	for name, cached := range m.shardCache {
		if s.Shards[cached.bucket].Name != name {
			delete(m.shardCache, name)
		}
	}
	m.cacheMu.Unlock()

	wanted := ""
	if key != "" {
		wanted = participantBucket(key)
	}

	references := s.Shards
	if key != "" {
		references = map[string]shardReference{}
		if ref, ok := s.Shards[wanted]; ok {
			references[wanted] = ref
		}
	}

	for bucket, ref := range references {
		if key != "" && bucket != wanted {
			continue
		}

		m.cacheMu.Lock()
		cached, ok := m.shardCache[ref.Name]
		m.cacheMu.Unlock()

		shard := cached.shard

		if !ok {
			// Immutable shards can be fetched concurrently. Keep API latency out
			// of the cache/observation critical section.
			var cm corev1.ConfigMap
			if err := m.client.Get(ctx, m.objectKey(ref.Name), &cm); err != nil {
				return fmt.Errorf("load committed participant shard: %w", err)
			}

			data := []byte(cm.Data[StateKey])
			if cm.Immutable == nil || !*cm.Immutable || digest(data) != ref.Digest {
				return errors.New("participant shard integrity mismatch")
			}

			if err := strictJSON(data, &shard); err != nil {
				return err
			}

			if shard.Members == nil || shard.Retired == nil {
				return errors.New("invalid participant shard")
			}

			for k, p := range shard.Members {
				if participantBucket(k) != bucket || p == nil || k != p.Identity.Key().String() || p.Leaves == nil || shard.Retired[k] {
					return errors.New("invalid sharded member")
				}

				if _, err := p.Identity.URI(); err != nil {
					return err
				}

				for fp, leaf := range p.Leaves {
					if !hexID.MatchString(fp) || !hexID.MatchString(leaf.Root) || leaf.Expiry.IsZero() {
						return errors.New("invalid sharded leaf")
					}
				}
			}

			for k, retired := range shard.Retired {
				if participantBucket(k) != bucket || !retired {
					return errors.New("invalid retirement shard")
				}
			}

			m.cacheMu.Lock()
			m.shardCache[ref.Name] = cachedShard{bucket: bucket, shard: shard}
			m.cacheMu.Unlock()
		}

		for k, p := range shard.Members {
			for _, leaf := range p.Leaves {
				for _, ca := range s.Authorities {
					if leaf.Root == ca.Digest && leaf.Expiry.After(ca.LastIssuedExpiry) {
						return errors.New("sharded leaf exceeds expiry watermark")
					}
				}
			}

			s.Members[k] = cloneMember(p)
		}

		for k, retired := range shard.Retired {
			s.Retired[k] = retired
		}
	}

	return nil
}

func snapshotParticipants(s *state) (map[string][]byte, error) {
	shards := make(map[string]*participantShard)

	get := func(key string) *participantShard {
		bucket := participantBucket(key)
		if shards[bucket] == nil {
			shards[bucket] = &participantShard{Members: map[string]*member{}, Retired: map[string]bool{}}
		}

		return shards[bucket]
	}
	for key, p := range s.Members {
		get(key).Members[key] = p
	}

	for key, retired := range s.Retired {
		get(key).Retired[key] = retired
	}

	result := make(map[string][]byte, len(shards))
	for bucket, shard := range shards {
		data, err := json.Marshal(shard)
		if err != nil {
			return nil, err
		}

		if len(data) > maxStateBytes {
			return nil, errors.New("participant shard capacity exhausted")
		}

		result[bucket] = data
	}

	return result, nil
}

// Write immutable objects first, then commit their references with the CA Secret
// CAS. Crashes and stale leaders can leave only unreachable objects, never a
// partially updated barrier. Version 1 is migrated without deleting its data
// until the same CAS atomically installs all version 2 references.
func (m *Manager) prepareCommit(ctx context.Context, s *state, before map[string][]byte) ([]byte, error) {
	after, err := snapshotParticipants(s)
	if err != nil {
		return nil, err
	}

	if s.Shards == nil {
		s.Shards = make(map[string]shardReference)
	}

	for bucket, data := range after {
		if s.Version == 2 && bytes.Equal(before[bucket], data) {
			continue
		}

		hash := digest(data)
		name := "racer-pki-" + digest([]byte(s.Fence))[:16] + "-" + hash
		immutable := true

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: map[string]string{participantLabel: "true"}}, Immutable: &immutable, Data: map[string]string{StateKey: string(data)}}
		if err := m.client.Create(ctx, cm); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, err
			}

			var existing corev1.ConfigMap
			if err := m.client.Get(ctx, m.objectKey(name), &existing); err != nil {
				return nil, err
			}

			if existing.Immutable == nil || !*existing.Immutable || existing.Data[StateKey] != string(data) {
				return nil, errors.New("participant object collision")
			}
		}

		s.Shards[bucket] = shardReference{Name: name, Digest: hash}
	}

	for bucket := range before {
		if _, ok := after[bucket]; !ok {
			delete(s.Shards, bucket)
		}
	}

	s.Version = 2
	s.Members = map[string]*member{}
	s.Retired = map[string]bool{}

	return encodeState(s)
}

// CollectParticipants removes unreachable immutable versions. Local writes are
// excluded; a takeover changes the Secret fence and stops this collector. Only
// this term's objects are reused, so older terms cannot revive a collected name.
func (m *Manager) CollectParticipants(ctx context.Context) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	fence, err := m.leader()
	if err != nil {
		return err
	}

	var objects corev1.ConfigMapList
	if err := m.client.List(ctx, &objects, client.InNamespace(m.namespace), client.MatchingLabels{participantLabel: "true"}); err != nil {
		return err
	}

	_, s, err := m.readMetadata(ctx)
	if err != nil {
		return err
	}

	if s.Fence != fence {
		return ErrNotLeader
	}

	live := make(map[string]bool, len(s.Shards))
	for _, ref := range s.Shards {
		live[ref.Name] = true
	}

	for i := range objects.Items {
		cm := &objects.Items[i]
		if live[cm.Name] || m.options.Now().Sub(cm.CreationTimestamp.Time) < 10*time.Minute {
			continue
		}
		// Do not collect objects from a later term that began after this list.
		// Its fence-specific names cannot be referenced by our old snapshot.
		if !strings.HasPrefix(cm.Name, "racer-pki-"+digest([]byte(fence))[:16]+"-") && !cm.CreationTimestamp.Time.Before(s.FenceAt) {
			continue
		}
		// A later leader cannot reuse names from this or any earlier term.
		if err := m.client.Delete(ctx, cm, client.Preconditions{UID: &cm.UID, ResourceVersion: &cm.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// Volatile observations deliberately do not cause Kubernetes writes at heartbeat
// frequency. A leadership fence already requires fresh proofs after takeover.
type memberObservation struct {
	member *member
	fence  string
}

func (m *Manager) applyObservations(s *state) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	m.applyObservationsLocked(s)
}

func (m *Manager) applyObservationsLocked(s *state) {
	for key, p := range s.Members {
		observation, ok := m.observations[key]
		if !ok || observation.fence != s.Fence {
			continue
		}

		o := observation.member
		p.Ack, p.ProofGeneration, p.ProofDigest, p.ProofRoot = o.Ack, o.ProofGeneration, o.ProofDigest, o.ProofRoot
		p.ProofAt, p.ProofFence, p.Drained = o.ProofAt, o.ProofFence, o.Drained
	}
}

func (m *Manager) observe(ctx context.Context, key MemberKey, fn func(*state) error) error {
	fence, err := m.leader()
	if err != nil {
		return err
	}

	committed := m.localState.Load()
	if committed == nil || committed.Fence != fence {
		return ErrNotLeader
	}
	// Observations have no durable side effects. Use the last local commit;
	// every transition rechecks the API fence before observations can count.
	copy := *committed
	s := &copy

	s.Members, s.Retired = map[string]*member{}, map[string]bool{}
	if s.Version == 1 {
		for k, p := range committed.Members {
			s.Members[k] = cloneMember(p)
		}

		for k, v := range committed.Retired {
			s.Retired[k] = v
		}
	}

	if err := m.loadParticipants(ctx, s, key.String()); err != nil {
		return err
	}

	// Merge and replace observations atomically so concurrent heartbeats cannot
	// erase proof credit or let the same handshake pass replay validation twice.
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	if m.localState.Load() != committed {
		// A commit may have retired this member or changed the trust bundle
		// while its shard was loading. Retry against committed metadata.
		return ErrNotReady
	}

	m.applyObservationsLocked(s)

	if err := fn(s); err != nil {
		return err
	}

	if m.observations == nil {
		m.observations = make(map[string]memberObservation)
	}

	m.observations[key.String()] = memberObservation{member: cloneMember(s.Members[key.String()]), fence: fence}

	return nil
}

// Member performs a bounded lookup rather than scanning every fleet participant.
func (m *Manager) Member(ctx context.Context, key MemberKey) (Identity, error) {
	_, s, err := m.readMetadata(ctx)
	if err != nil {
		return Identity{}, err
	}

	if err := m.loadParticipants(ctx, s, key.String()); err != nil {
		return Identity{}, err
	}

	if p := s.Members[key.String()]; p != nil {
		return p.Identity, nil
	}

	return Identity{}, errors.New("unknown durable member")
}

// memberState serves the leader's authenticated request path without an API
// round trip per heartbeat. Mutations and rotation still use direct CAS reads.
func (m *Manager) memberState(ctx context.Context, key MemberKey) (*state, error) {
	var s *state

	committed := m.localState.Load()
	if _, err := m.leader(); err == nil && committed != nil && committed.Version == 2 {
		copy := *committed
		s = &copy
		s.Members, s.Retired = map[string]*member{}, map[string]bool{}
	} else {
		_, loaded, err := m.readMetadata(ctx)
		if err != nil {
			return nil, err
		}

		s = loaded
	}

	if err := m.loadParticipants(ctx, s, key.String()); err != nil {
		return nil, err
	}

	return s, nil
}
