// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// VersionRecord is the only persisted topology bookkeeping. Hashes cover
// canonical content excluding counters. No member history or publication bytes
// are stored. ResourceVersion CAS must precede installing a publication.
type VersionRecord struct {
	Cluster           wire.ClusterID         `json:"cluster"`
	Sequence          wire.Sequence          `json:"sequence,string"`
	MembershipVersion wire.MembershipVersion `json:"membership_version,string"`
	ContentHash       string                 `json:"content_hash"`
	MembershipHash    string                 `json:"membership_hash"`
}

// PreparedPublication owns encoded candidate bytes. Only CommitVersion can mint
// the installable type. Fields stay private so callers cannot bypass the CAS.
type PreparedPublication struct {
	owner           *Publications
	previous        VersionRecord
	resourceVersion string
	record          VersionRecord
	encoded         string
}

type CommittedPublication struct {
	owner      *Publications
	record     VersionRecord
	encoded    string
	leadership context.Context
}

func (p *CommittedPublication) Version() VersionRecord { return p.record }

// Encoding is an immutable shared string, never a mutable byte slice. HTTP serving
// should use WriteTo under its write semaphore/deadline; it makes no snapshot copy.
func (p *CommittedPublication) Encoding() string { return p.encoded }

func (p *CommittedPublication) WriteTo(w io.Writer) (int64, error) {
	if p == nil || p.leadership == nil {
		return 0, wire.Unavailable
	}

	var written int64

	for remaining := p.encoded; remaining != ""; {
		if err := p.leadership.Err(); err != nil {
			return written, err
		}
		// ResponseWriter need not implement StringWriter. Limit conversion scratch
		// to 32 KiB rather than allocating a full publication for every response.
		chunk := remaining[:min(len(remaining), 32*1024)]
		n, err := io.WriteString(w, chunk)

		written += int64(n)
		if err != nil {
			return written, err
		}

		if n != len(chunk) {
			return written, io.ErrShortWrite
		}

		remaining = remaining[n:]
	}

	return written, nil
}

// Publications owns only the current immutable publication, bounded poll
// admission, and one broadcast notification. Older state belongs to dataplanes.
type Publications struct {
	Limits    Limits
	mu        sync.Mutex
	current   *CommittedPublication
	changed   chan struct{}
	polls     map[wire.NodeID]struct{}
	suspended bool
}

func NewPublications(limits Limits) *Publications {
	return &Publications{Limits: limits, changed: make(chan struct{}), polls: make(map[wire.NodeID]struct{})}
}

func (p *Publications) Prepare(previous VersionRecord, resourceVersion string, members AcceptedMembers, caches []wire.CacheDefinition) (*PreparedPublication, error) {
	if !previous.valid() || resourceVersion == "" {
		return nil, wire.InvalidRequest
	}

	v := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: previous.Cluster, Caches: caches, Members: make([]wire.Member, 0, len(members))}
	for id, member := range members {
		if id != member.Node {
			return nil, wire.InvalidRequest
		}

		v.Members = append(v.Members, member)
	}

	content, membership, err := wire.ContentHashes(v)
	if err != nil {
		return nil, err
	}

	record := previous
	if content != previous.ContentHash {
		if record.Sequence == ^wire.Sequence(0) {
			return nil, wire.Unavailable
		}

		record.Sequence++
	}

	if membership != previous.MembershipHash {
		if content == previous.ContentHash || record.MembershipVersion == ^wire.MembershipVersion(0) {
			return nil, wire.Unavailable
		}

		record.MembershipVersion++
	}

	record.ContentHash, record.MembershipHash = content, membership
	v.Sequence, v.MembershipVersion = record.Sequence, record.MembershipVersion

	encoded, err := wire.EncodePublication(v)
	if err != nil {
		return nil, err
	}

	return &PreparedPublication{owner: p, previous: previous, resourceVersion: resourceVersion, record: record, encoded: string(encoded)}, nil
}

func (p *Publications) Install(next *CommittedPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if next == nil || next.owner != p || next.leadership == nil || !next.record.valid() || next.encoded == "" {
		return wire.InvalidRequest
	}

	if err := next.leadership.Err(); err != nil {
		return err
	}

	if current := p.current; current != nil {
		if current.record.Cluster != next.record.Cluster || next.record.Sequence < current.record.Sequence || next.record.MembershipVersion < current.record.MembershipVersion {
			return wire.Conflict
		}

		if next.record.Sequence == current.record.Sequence {
			if next.record != current.record || next.encoded != current.encoded {
				return wire.Conflict
			}

			if p.suspended {
				p.suspended = false
				close(p.changed)
				p.changed = make(chan struct{})
			}

			return nil
		}
	}

	p.current = next
	p.suspended = false
	close(p.changed)
	p.changed = make(chan struct{})

	return nil
}

func (p *Publications) Current() (*CommittedPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.currentLocked()
}

func (p *Publications) currentLocked() (*CommittedPublication, error) {
	if p.current == nil || p.suspended {
		return nil, wire.Unavailable
	}

	if err := p.current.leadership.Err(); err != nil {
		return nil, err
	}

	return p.current, nil
}

// Suspend withdraws readiness and wakes polls after failure to validate durable
// authority. Keep bytes/history for a later successful CAS, never serve them until
// then. Invalid desired inputs alone do not suspend the last valid publication.
func (p *Publications) Suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.suspended {
		p.suspended = true
		close(p.changed)
		p.changed = make(chan struct{})
	}
}

// Wait admits one poll per node, rejects future cursors, and honors context
// cancellation/certificate expiration. It never allocates a publication per poll.
func (p *Publications) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*CommittedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !wire.ValidUUID(string(identity.node)) || !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	p.mu.Lock()

	current, err := p.currentLocked()
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}

	if identity.cluster != current.record.Cluster {
		p.mu.Unlock()
		return nil, wire.Forbidden
	}

	if after != nil && (*after == 0 || *after > current.record.Sequence) {
		p.mu.Unlock()
		return nil, wire.Conflict
	}

	if _, exists := p.polls[identity.node]; exists || len(p.polls) >= p.Limits.MaxPolls {
		p.mu.Unlock()
		return nil, wire.Overloaded
	}

	p.polls[identity.node] = struct{}{}
	p.mu.Unlock()

	defer func() { p.mu.Lock(); delete(p.polls, identity.node); p.mu.Unlock() }()

	timer := time.NewTimer(wire.PollWait)
	defer timer.Stop()

	expiration := time.NewTimer(time.Until(identity.expires))
	defer expiration.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !time.Now().Before(identity.expires) {
			return nil, wire.Unauthenticated
		}

		p.mu.Lock()
		current, err = p.currentLocked()
		changed := p.changed
		p.mu.Unlock()

		if err != nil {
			return nil, err
		}

		if after == nil || current.record.Sequence > *after {
			return current, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.leadership.Done():
			return nil, current.leadership.Err()
		case <-expiration.C:
			return nil, wire.Unauthenticated
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			if err := current.leadership.Err(); err != nil {
				return nil, err
			}

			if !time.Now().Before(identity.expires) {
				return nil, wire.Unauthenticated
			}

			return nil, nil
		case <-changed:
		}
	}
}

func (p *Publications) Ready(_ *http.Request) error { _, err := p.Current(); return err }
