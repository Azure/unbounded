// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cachestore defines the in-DC chunk store interface and shared
// types. Concrete drivers live under cachestore/<driver>/.
//
// See design/orca/design.md s7 for the full interface and s10.1 for the
// atomic-commit contract.
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
// LocalStack in dev (Scope A+B).
type CacheStore interface {
	GetChunk(ctx context.Context, k chunk.Key, off, n int64) (io.ReadCloser, error)
	PutChunk(ctx context.Context, k chunk.Key, size int64, r io.Reader) error
	Stat(ctx context.Context, k chunk.Key) (Info, error)
	Delete(ctx context.Context, k chunk.Key) error
	SelfTestAtomicCommit(ctx context.Context) error
}

// Info is the result of a successful Stat.
type Info struct {
	Size      int64
	Committed time.Time
}

// Sentinel errors. Wrap with %w so callers use errors.Is.
var (
	ErrNotFound   = errors.New("cachestore: not found")
	ErrTransient  = errors.New("cachestore: transient")
	ErrAuth       = errors.New("cachestore: auth")
	ErrCommitLost = errors.New("cachestore: commit lost (no-clobber denied)")
)
