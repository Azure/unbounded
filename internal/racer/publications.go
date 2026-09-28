// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"io"
	"net/http"
	"strings"
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

// PreparedPublication owns encoded candidate bytes. Publishers require CAS;
// replicas require canonical validation and an authoritative durable confirmation.
type PreparedPublication struct {
	owner           *Publications
	previous        VersionRecord
	resourceVersion string
	record          VersionRecord
	encoded         string
	delta           string
	deltaBase       string
}

type CommittedPublication struct {
	owner      *Publications
	record     VersionRecord
	encoded    string
	delta      string
	deltaBase  string
	leadership context.Context
	authority  context.Context
}

func (p *CommittedPublication) Version() VersionRecord { return p.record }

// Encoding is an immutable shared string, never a mutable byte slice. HTTP serving
// should use WriteTo under its write semaphore/deadline; it makes no snapshot copy.
func (p *CommittedPublication) Encoding() string { return p.encoded }

func (p *CommittedPublication) WriteTo(w io.Writer) (int64, error) {
	if p == nil || p.leadership == nil {
		return 0, wire.Unavailable
	}

	ctx, cancel, err := p.writeContext(p.leadership)
	if err != nil {
		return 0, err
	}
	defer cancel()

	var written int64

	for remaining := p.encoded; remaining != ""; {
		if p.authority != nil && p.authority.Err() != nil {
			return written, wire.Unavailable
		}

		if err := ctx.Err(); err != nil {
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

	if p.authority != nil && p.authority.Err() != nil {
		return written, wire.Unavailable
	}

	return written, ctx.Err()
}

// writeContext pins one response to its image's revocable authority and the
// freshness deadline at write admission. Later confirmations cannot extend an
// in-flight response, and supersession or suspension permanently revokes it.
func (p *CommittedPublication) writeContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if p.owner == nil {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}

	owner := p.owner
	owner.mu.Lock()
	defer owner.mu.Unlock()

	if _, err := owner.currentLocked(); err != nil {
		return nil, nil, err
	}

	if p.authority == nil || p.authority != owner.current.authority || p.authority.Err() != nil {
		return nil, nil, wire.Unavailable
	}

	ctx, cancel := context.WithDeadline(parent, owner.confirmed.Add(owner.maxAge))
	stop := context.AfterFunc(p.authority, cancel)

	return ctx, func() { stop(); cancel() }, nil
}

// Publications owns only the current immutable publication and one broadcast
// notification. Older state belongs to dataplanes; poll admission belongs to Server.
type Publications struct {
	mu        sync.Mutex
	current   *CommittedPublication
	changed   chan struct{}
	suspended bool
	process   context.Context
	confirmed time.Time
	maxAge    time.Duration
	observed  VersionRecord
	revoke    context.CancelFunc
}

func NewPublications() *Publications {
	return &Publications{changed: make(chan struct{}), maxAge: 30 * time.Second}
}

func (p *Publications) bindProcess(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.process = ctx
}

func (p *Publications) notifyLocked() { close(p.changed); p.changed = make(chan struct{}) }

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

	candidate, err := wire.NewCanonicalCandidate(v)
	if err != nil {
		return nil, err
	}

	content, membership, err := candidate.ContentHashes()
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

	encoded, err := candidate.EncodePublication(record.Sequence, record.MembershipVersion)
	if err != nil {
		return nil, err
	}

	prepared := &PreparedPublication{owner: p, previous: previous, resourceVersion: resourceVersion, record: record, encoded: string(encoded)}
	v.Sequence, v.MembershipVersion = record.Sequence, record.MembershipVersion
	// Capture immutable base under the lock, then diff/encode entirely outside it.
	if current, err := p.Current(); err == nil && current.record == previous && record.Sequence > previous.Sequence {
		if base, err := wire.DecodePublication(strings.NewReader(current.encoded)); err == nil {
			if delta, err := wire.EncodeDelta(base, v); err == nil && len(delta) < len(encoded) {
				prepared.delta, prepared.deltaBase = string(delta), previous.ContentHash
			}
		}
	}

	return prepared, nil
}

// ForBase returns a shared bounded delta only for the exact authenticated cursor.
// Coalesced/skipped updates and controller restarts automatically use the full image.
func (p *CommittedPublication) ForBase(hash string) *CommittedPublication {
	if hash == "" || hash != p.deltaBase || p.delta == "" {
		return p
	}

	return &CommittedPublication{owner: p.owner, record: p.record, encoded: p.delta, leadership: p.leadership, authority: p.authority}
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

	if err := p.observeLocked(next.record); err != nil {
		return err
	}

	if p.process != nil {
		copy := *next
		copy.leadership = p.process
		next = &copy
	}

	if current := p.current; current != nil {
		if current.record.Cluster != next.record.Cluster || next.record.Sequence < current.record.Sequence || next.record.MembershipVersion < current.record.MembershipVersion {
			return wire.Conflict
		}

		if next.record.Sequence == current.record.Sequence {
			if next.record != current.record || next.encoded != current.encoded {
				return wire.Conflict
			}

			p.confirmed = time.Now()
			if current.leadership.Err() != nil || p.suspended {
				next = p.authorizeLocked(next)
				p.current = next
			}

			if p.suspended {
				p.suspended = false
				p.notifyLocked()
			}

			return nil
		}
	}

	p.current = p.authorizeLocked(next)
	p.confirmed = time.Now()
	p.suspended = false
	p.notifyLocked()

	return nil
}

func (p *Publications) authorizeLocked(next *CommittedPublication) *CommittedPublication {
	if p.revoke != nil {
		p.revoke()
	}

	copy := *next
	copy.authority, p.revoke = context.WithCancel(copy.leadership)

	return &copy
}

func (p *Publications) Current() (*CommittedPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.currentLocked()
}

// CurrentAndSubscribe atomically reads the current publication and subscribes to
// changes, including when unavailable. The channel closes on install or suspension;
// leadership cancellation must be observed separately by the caller.
func (p *Publications) CurrentAndSubscribe() (*CommittedPublication, <-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, err := p.currentLocked()

	return current, p.changed, err
}

func (p *Publications) currentLocked() (*CommittedPublication, error) {
	if p.current == nil || p.suspended || time.Since(p.confirmed) >= p.maxAge {
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

	p.suspendLocked()
}

func (p *Publications) suspendLocked() {
	if p.revoke != nil {
		p.revoke()
	}

	if !p.suspended {
		p.suspended = true
		p.notifyLocked()
	}
}

// Wait rejects invalid identities/cursors and honors context cancellation and
// certificate expiration. It shares publication bytes and broadcast notifications;
// callers own admission for the full response lifetime, including writes and flush.
func (p *Publications) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*CommittedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !wire.ValidUUID(string(identity.node)) || !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	current, changed, err := p.CurrentAndSubscribe()
	if err != nil {
		return nil, err
	}

	if identity.cluster != current.record.Cluster {
		return nil, wire.Forbidden
	}

	if after != nil && *after == 0 {
		return nil, wire.Conflict
	}

	if after != nil && *after > current.record.Sequence {
		return nil, wire.Unavailable
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	if after == nil || current.record.Sequence > *after {
		if err := current.leadership.Err(); err != nil {
			return nil, err
		}

		return current, nil
	}

	timer := time.NewTimer(wire.PollWait)
	defer timer.Stop()

	expiration := time.NewTimer(time.Until(identity.expires))
	defer expiration.Stop()

	freshness := time.NewTicker(min(p.maxAge, time.Second))
	defer freshness.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.leadership.Done():
			return nil, current.leadership.Err()
		case <-expiration.C:
			return nil, wire.Unauthenticated
		case <-freshness.C:
			if _, err := p.Current(); err != nil {
				return nil, err
			}

			continue
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

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !time.Now().Before(identity.expires) {
			return nil, wire.Unauthenticated
		}

		current, changed, err = p.CurrentAndSubscribe()
		if err != nil {
			return nil, err
		}

		if current.record.Sequence > *after {
			return current, nil
		}
	}
}

func (p *Publications) Ready(_ *http.Request) error { _, err := p.Current(); return err }
