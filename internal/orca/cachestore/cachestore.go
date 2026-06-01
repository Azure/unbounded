// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cachestore defines the in-DC chunk store interface and shared
// types. Concrete drivers live under cachestore/<driver>/.
//
// All drivers must implement atomic commit (CAS-style PutChunk that
// rejects overwrites) so concurrent fills across replicas converge
// without clobbering each other; SelfTestAtomicCommit is run at boot
// to verify the backend honors the precondition.
package cachestore

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
)

// CacheStore is where chunk bytes physically live. Source of truth for
// chunk presence; backed by an in-DC S3-like store in production and
// LocalStack in dev.
type CacheStore interface {
	GetChunk(ctx context.Context, k chunk.Key, off, n int64) (io.ReadCloser, error)
	PutChunk(ctx context.Context, k chunk.Key, size int64, r io.Reader) error
	Stat(ctx context.Context, k chunk.Key) (Info, error)
	// Delete removes a committed chunk. No production code calls this
	// today: routine space reclamation is handled outside Orca by the
	// cachestore bucket's age-based lifecycle policy (passive
	// eviction; see designs/orca/design.md s11.1). It is kept on the
	// interface for the deferred, opt-in active-eviction loop that
	// would delete cold chunks itself (see s13 "Active eviction
	// loop").
	Delete(ctx context.Context, k chunk.Key) error
	SelfTestAtomicCommit(ctx context.Context) error
}

// Info is the result of a successful Stat.
type Info struct {
	Size int64
	// Committed is when the chunk was written to the cachestore (the
	// stored object's last-modified time). Nothing reads it today; it
	// is filled in as the age signal that the deferred active-eviction
	// loop would use to find cold chunks (see designs/orca/design.md
	// s11.1 and s13 "Active eviction loop"). It pairs with Delete.
	Committed time.Time
}

// Sentinel errors. Wrap with %w so callers use errors.Is.
var (
	ErrNotFound   = errors.New("cachestore: not found")
	ErrTransient  = errors.New("cachestore: transient")
	ErrAuth       = errors.New("cachestore: auth")
	ErrCommitLost = errors.New("cachestore: commit lost (no-clobber denied)")
)
