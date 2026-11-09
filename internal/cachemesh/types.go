// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"errors"
	"fmt"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/control"
	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Routing types. They are aliases of the internal types, so values pass
// between the public API and the routing core without conversion.
type (
	// Decision is the result of routing one request. See route.Decision.
	Decision = route.Decision
	// Kind classifies a Decision.
	Kind = route.Kind
	// PeerRef identifies a peer. The zero value means no peer.
	PeerRef = route.PeerRef
	// Header is the fixed-size data-plane header sent with every request.
	Header = route.Header
	// Result is the outcome of a request reported to Observe.
	Result = route.Result
	// Stamp is an expiry stamp carried with a cached metadata value.
	Stamp = route.Stamp
	// Version is a membership view version, ordered by term then seq.
	Version = view.Version
	// Slot is a dense member index in a view. NoSlot means none.
	Slot = view.Slot
)

// Lease types. kubelease.Store implements LeaseStore.
type (
	// LeaseStore executes Lease operations for leader election.
	LeaseStore = control.Store
	// LeaseRecord is the cachemesh view of the leader Lease.
	LeaseRecord = control.Record
)

// Decision kinds.
const (
	// KindNoRoute means there is no route (no view and no gateway).
	KindNoRoute = route.KindNoRoute
	// KindSelf means serve the request locally.
	KindSelf = route.KindSelf
	// KindRelay means send to Next, an intermediate hop.
	KindRelay = route.KindRelay
	// KindOwner means send to Next, the owner.
	KindOwner = route.KindOwner
	// KindBackup means send to Next, a backup in the owner's cell.
	KindBackup = route.KindBackup
	// KindGateway means send to Next, a gateway, because this process is
	// not yet a member.
	KindGateway = route.KindGateway
	// KindDirect means send to Next, the owner, outside the neighbor
	// budget.
	KindDirect = route.KindDirect
)

// Request outcomes for Observe.
const (
	Success  = route.Success
	Failure  = route.Failure
	Timeout  = route.Timeout
	Overload = route.Overload
)

// Header constants.
const (
	// HeaderSize is the encoded size of a Header in bytes.
	HeaderSize = route.HeaderSize
	// FlagHedge marks a hedged request. The caller sets it; it is kept
	// across hops.
	FlagHedge = route.FlagHedge
	// FlagDirect marks a request sent outside the neighbor budget.
	FlagDirect = route.FlagDirect
	// FlagGateway marks a request sent through a gateway.
	FlagGateway = route.FlagGateway
	// FlagBackup marks a request sent to (or served by) a backup.
	FlagBackup = route.FlagBackup
)

// NoSlot is the Slot of a peer that is not identified by slot (a gateway).
const NoSlot = view.NoSlot

var (
	// ErrBadHeader reports a malformed header.
	ErrBadHeader = route.ErrBadHeader
	// ErrTTLExhausted reports a request that must not be forwarded again.
	ErrTTLExhausted = route.ErrTTLExhausted
	// ErrLeaseNotFound is what a LeaseStore returns when the Lease does not
	// exist.
	ErrLeaseNotFound = control.ErrNotFound
	// ErrLeaseConflict is what a LeaseStore returns on a failed
	// compare-and-swap or a create of an existing Lease.
	ErrLeaseConflict = control.ErrConflict
)

// ParseHeader decodes a header from b.
func ParseHeader(b []byte) (Header, error) { return route.ParseHeader(b) }

// MinTTL is the smallest nonzero metadata TTL.
const MinTTL = route.MinTTL

// Keyspace names an independent set of keys. Each keyspace hashes into the
// shared partition table with its own seed, so placements of different
// keyspaces are independent. A Keyspace is a small value; build it once and
// reuse it.
type Keyspace struct {
	name string
	ttl  time.Duration
	seed uint64
}

// NewKeyspace returns the keyspace called name. A ttl of zero means values
// never expire (content-addressed blobs: evictable, never stale); otherwise
// ttl must be at least MinTTL and bounds how long a Stamp stays Fresh.
func NewKeyspace(name string, ttl time.Duration) (Keyspace, error) {
	if name == "" {
		return Keyspace{}, errors.New("cachemesh: keyspace name must not be empty")
	}

	if ttl < 0 || (ttl != 0 && ttl < MinTTL) {
		return Keyspace{}, fmt.Errorf("cachemesh: keyspace %q: ttl %v must be 0 or at least %v", name, ttl, MinTTL)
	}

	return Keyspace{name: name, ttl: ttl, seed: view.Seed(name)}, nil
}

// MustKeyspace is NewKeyspace that panics on error, for package-level
// variables.
func MustKeyspace(name string, ttl time.Duration) Keyspace {
	ks, err := NewKeyspace(name, ttl)
	if err != nil {
		panic(err)
	}

	return ks
}

// Name returns the keyspace name.
func (k Keyspace) Name() string { return k.name }

// TTL returns the metadata TTL, or zero if values never expire.
func (k Keyspace) TTL() time.Duration { return k.ttl }

// Seed returns the hashing seed, view.Seed(Name()).
func (k Keyspace) Seed() uint64 { return k.seed }

// Hash returns the key hash of key in this keyspace, for RouteHash.
func (k Keyspace) Hash(key []byte) uint64 { return view.KeyHash(k.seed, key) }
