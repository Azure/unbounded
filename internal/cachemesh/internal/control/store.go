// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Record is the cachemesh view of the leader Lease. kubelease maps it onto a
// coordination/v1 Lease: Holder, LeaseDuration, AcquireTime, RenewTime and
// Transitions live in the Lease spec; Incarnation, Term, Addr and Seeds live
// in annotations; ResourceVersion and UID come from object metadata.
type Record struct {
	// Holder is the MemberID of the leader, empty if the Lease was released.
	Holder string
	// Incarnation of the holder process.
	Incarnation uint64
	// Term is the cachemesh leader term the holder publishes under.
	Term uint64
	// Addr is the holder's control-plane address.
	Addr string
	// Seeds are up to MaxSeeds control addresses of members chosen by the
	// leader for joiners to contact.
	Seeds []string
	// LeaseDuration is how long the record stays valid after RenewTime.
	LeaseDuration time.Duration
	// AcquireTime is the wall time the holder took the Lease.
	AcquireTime time.Time
	// RenewTime is the wall time of the last renewal.
	RenewTime time.Time
	// Transitions counts holder changes.
	Transitions int32
	// ResourceVersion is the server-assigned version used for compare-and-swap.
	ResourceVersion string
	// UID is the server-assigned object identity. A Lease that is deleted and
	// recreated gets a new UID.
	UID string
}

// MaxSeeds bounds Record.Seeds.
const MaxSeeds = 16

// Clone returns a deep copy of r.
func (r Record) Clone() Record {
	r.Seeds = slices.Clone(r.Seeds)
	return r
}

// OpKind identifies a Lease operation.
type OpKind uint8

const (
	// OpGet is a consistent (quorum) read.
	OpGet OpKind = iota + 1
	// OpGetCached is a read that may be served from the API server's watch
	// cache (resourceVersion=0). It may return stale data.
	OpGetCached
	// OpCreate creates the Lease. It fails with ErrConflict if it exists.
	OpCreate
	// OpUpdate replaces the Lease if its ResourceVersion still equals
	// Op.Record.ResourceVersion. It fails with ErrConflict on mismatch and
	// ErrNotFound if the Lease does not exist.
	OpUpdate
)

// String returns the operation name.
func (k OpKind) String() string {
	switch k {
	case OpGet:
		return "get"
	case OpGetCached:
		return "get-cached"
	case OpCreate:
		return "create"
	case OpUpdate:
		return "update"
	default:
		return "unknown"
	}
}

// Op is a Lease operation requested by the Elector. The runtime executes it
// against a Store and feeds the Result back with the same Seq.
type Op struct {
	// Seq identifies the operation; results with a stale Seq are ignored.
	Seq  uint64
	Kind OpKind
	// Record is the desired state for OpCreate and OpUpdate.
	Record Record
}

// Result is the outcome of an Op.
type Result struct {
	Seq  uint64
	Kind OpKind
	// Record is the Lease as stored after the operation, valid when Err is nil.
	Record Record
	// Err is nil, ErrNotFound, ErrConflict, or any other (transient) error.
	Err error
}

var (
	// ErrNotFound reports that the Lease does not exist.
	ErrNotFound = errors.New("cachemesh: lease not found")
	// ErrConflict reports a failed compare-and-swap or a create of an
	// existing Lease.
	ErrConflict = errors.New("cachemesh: lease conflict")
)

// Store executes Lease operations. Implementations must map "not found" to
// ErrNotFound and optimistic-concurrency failures to ErrConflict (wrapping is
// allowed; callers use errors.Is).
type Store interface {
	// Get reads the Lease with a consistent read.
	Get(ctx context.Context) (Record, error)
	// GetCached reads the Lease from the API server cache.
	GetCached(ctx context.Context) (Record, error)
	// Create creates the Lease and returns the stored record.
	Create(ctx context.Context, r Record) (Record, error)
	// Update replaces the Lease if r.ResourceVersion matches.
	Update(ctx context.Context, r Record) (Record, error)
}

// Execute runs op against s.
func Execute(ctx context.Context, s Store, op Op) Result {
	res := Result{Seq: op.Seq, Kind: op.Kind}

	switch op.Kind {
	case OpGet:
		res.Record, res.Err = s.Get(ctx)
	case OpGetCached:
		res.Record, res.Err = s.GetCached(ctx)
	case OpCreate:
		res.Record, res.Err = s.Create(ctx, op.Record)
	case OpUpdate:
		res.Record, res.Err = s.Update(ctx, op.Record)
	default:
		res.Err = errors.New("cachemesh: unknown lease op")
	}

	return res
}
