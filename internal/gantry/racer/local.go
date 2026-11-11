// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Azure/unbounded/internal/gantry/contentstore"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// LocalBlob is an open, immutable, content-addressed object.
type LocalBlob interface {
	io.ReaderAt
	io.Closer
}

// LocalStore opens content-addressed objects held on this node, such as
// containerd's content store. Open takes a "sha256:<hex>" digest and returns
// the object with its size. Any error is treated as a local miss.
type LocalStore interface {
	Open(digest string) (LocalBlob, int64, error)
}

// ContentStore adapts a containerd content store to LocalStore.
func ContentStore(store *contentstore.Store) LocalStore {
	if store == nil {
		return nil
	}

	return contentStore{store}
}

type contentStore struct{ store *contentstore.Store }

func (c contentStore) Open(d string) (LocalBlob, int64, error) {
	f, size, err := c.store.Open(d)
	if err != nil {
		return nil, 0, err
	}

	return f, size, nil
}

// OriginOption configures optional Origin behavior.
type OriginOption func(*originOptions)

type originOptions struct {
	local    LocalStore
	verifier *ReplicaVerifier
	// localOnly is isLocalOnly unless a test overrides it.
	localOnly func(racersdk.OriginRequest) bool
}

// WithLocalStore lets the origin serve pages from objects already held on this
// node instead of the registry. It is required for local-only requests, which
// Racer issues when the ClusterCache keeps no copy of its own.
func WithLocalStore(store LocalStore) OriginOption {
	return func(o *originOptions) { o.local = store }
}

// WithReplicaVerifier shares v with the mirror Handler (see
// WithLocalReplicaReset). Without it, Origin uses a private verifier.
func WithReplicaVerifier(v *ReplicaVerifier) OriginOption {
	return func(o *originOptions) { o.verifier = v }
}

// isLocalOnly reports whether Racer asked for a page from local storage only.
// Racer sets it for caches with spec.persistence Adapter.
func isLocalOnly(request racersdk.OriginRequest) bool {
	return request.LocalOnly
}

// localMiss is the error for a page this node does not hold. Racer maps
// ErrNotFound on a local-only request to "ask peers or the origin next".
func localMiss(err error) error {
	return fmt.Errorf("%w: local replica: %w", racersdk.ErrNotFound, err)
}

// serveLocal answers a local-only request from store. handled reports whether
// the result is final; it is false only for ordinary requests, which keep using
// the registry so page metadata (notably the registry's Content-Type) stays
// consistent across a pinned version. A local-only request never reaches the
// registry, and every local failure becomes ErrNotFound.
//
// Containerd verifies a blob's digest before committing it, but the file can
// later rot or be altered. verifier hashes each file once before its first
// page is served; an unverified file whose hash does not finish before ctx
// ends, or a corrupt one, is a miss.
func serveLocal(ctx context.Context, store LocalStore, verifier *ReplicaVerifier, ref ifaces.OriginRef, request racersdk.OriginRequest, localOnly bool) (racersdk.Metadata, io.ReadCloser, bool, error) {
	if !localOnly {
		return racersdk.Metadata{}, nil, false, nil
	}

	miss := func(err error) (racersdk.Metadata, io.ReadCloser, bool, error) {
		return racersdk.Metadata{}, nil, true, localMiss(err)
	}

	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, true, err
	}

	tag := `"` + ref.Digest.String() + `"`
	if request.ETag != "" && request.ETag != tag {
		return racersdk.Metadata{}, nil, true, racersdk.ErrVersionMismatch
	}

	if store == nil {
		return miss(errors.New("no local store"))
	}

	blob, size, err := store.Open(ref.Digest.String())
	if err != nil {
		return miss(err)
	}

	if size < 0 {
		_ = blob.Close() //nolint:errcheck // best-effort close on a miss

		return miss(errors.New("unusable local object"))
	}

	if verifier != nil {
		if err := verifier.check(ctx, store, ref.Digest.String(), blob, size); err != nil {
			_ = blob.Close() //nolint:errcheck // best-effort close on a miss

			return miss(err)
		}
	}

	metadata := racersdk.Metadata{
		Size: size, ETag: tag, ContentType: objectContentType,
		ExpiresAt: time.Now().Add(metadataTTL).Truncate(time.Millisecond),
	}

	if request.Head || request.Offset >= size || request.Length <= 0 || request.Offset < 0 {
		_ = blob.Close() //nolint:errcheck // metadata-only answer

		return metadata, nil, true, nil
	}

	// The range may extend past the end; serve only the bytes that exist, as
	// a bounded registry read would.
	length := min(request.Length, size-request.Offset)

	return metadata, &sectionCloser{io.NewSectionReader(blob, request.Offset, length), blob}, true, nil
}

type sectionCloser struct {
	*io.SectionReader
	io.Closer
}
