// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package cachestore defines the in-DC chunk store interface and shared
// types. Concrete drivers live under cachestore/<driver>/.
//
// Commit safety rests on the content-addressed chunk layout: a chunk's
// ETag is part of its storage path (see designs/orca/design.md s5), so
// the only way two concurrent fills target the same key is when they
// are writing byte-identical content. PutChunk commits with a
// stat-then-put step (HeadObject; if present, skip the upload and
// report the existing object as the winner). SelfTest is run at boot to
// verify the backend provides read-after-write visibility, which the
// stat-then-put step depends on.
package cachestore

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
)

// CacheStore is where chunk bytes physically live. Source of truth for
// chunk presence; backed by an in-DC S3-like store in production and a
// self-hosted S3-compatible store in dev.
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
	SelfTest(ctx context.Context) error
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
	ErrNotFound  = errors.New("cachestore: not found")
	ErrTransient = errors.New("cachestore: transient")
	ErrAuth      = errors.New("cachestore: auth")
	// ErrCommitLost reports that another replica already committed this
	// chunk: the commit's stat-then-put step found the object already
	// present and skipped the upload. The chunk is in the cachestore;
	// the caller treats the existing object as the truth. Because the
	// ETag is part of the chunk path, the existing object is
	// byte-identical to what this replica would have written.
	ErrCommitLost = errors.New("cachestore: commit lost (chunk already present)")
)
