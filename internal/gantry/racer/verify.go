// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// invalidateTimeout bounds the best-effort purge after a digest mismatch.
const invalidateTimeout = 5 * time.Second

// errDigestMismatch reports that a full object's bytes do not hash to the
// requested digest.
var errDigestMismatch = errors.New("racer object does not match its digest")

// Invalidator drops one cached version of an object cluster-wide, best
// effort. *racersdk.Client satisfies it.
type Invalidator interface {
	Invalidate(ctx context.Context, key racersdk.Key, etag string) error
}

var _ Invalidator = (*racersdk.Client)(nil)

// WithInvalidator purges a cached version that fails digest verification so
// the consumer's retry fetches fresh bytes instead of the same bad copy.
func WithInvalidator(inv Invalidator) HandlerOption {
	return func(h *Handler) { h.invalidator = inv }
}

// WithLocalReplicaReset makes a digest mismatch drop v's verification result
// for that digest, so this node's local replica is hashed again.
func WithLocalReplicaReset(v *ReplicaVerifier) HandlerOption {
	return func(h *Handler) { h.replicas = v }
}

// WithDigestMismatchObserver reports each full response aborted because its
// bytes did not match the digest. observe must be concurrency-safe and must
// not block.
func WithDigestMismatchObserver(observe func()) HandlerOption {
	return func(h *Handler) { h.onMismatch = observe }
}

// verifyingWriter hashes a full object (offset 0) as it streams and withholds
// its final byte until the hash matches. With adapter persistence, Racer pages
// may come from a node's local containerd store or a peer instead of the
// registry, so the mirror checks the assembled object before completing it.
//
// Holding back one byte means a corrupt object never reaches its declared
// Content-Length: the handler aborts the connection and containerd sees a
// truncated transfer, so it retries or falls back without committing. (It
// would also reject the commit by digest, but aborting surfaces the failure
// as a transport error.) The handler then invalidates the cached version so a
// retry does not read the same bad pages. Ranged resumes (offset > 0) cannot
// be verified here and are left to containerd's commit-time check.
//
// Hashing forces bytes through process memory, so it forgoes the SDK's
// splice fast path for full responses.
//
// The body goes out through an io.Writer and the deadline through the
// response controller. Callers have already set the attachment and nosniff
// download headers (writeRacerDownloadHeaders), so these bytes are never
// rendered as a document.
type verifyingWriter struct {
	w     io.Writer
	rc    *http.ResponseController
	want  digest.Digest
	hash  hash.Hash
	total int64
	seen  int64
}

func newVerifyingWriter(w http.ResponseWriter, want digest.Digest, total int64) *verifyingWriter {
	return &verifyingWriter{w: w, rc: http.NewResponseController(w), want: want, hash: sha256.New(), total: total}
}

// Write hashes only bytes that w accepted, so resumes from the accepted count
// keep the running hash exact.
func (v *verifyingWriter) Write(p []byte) (int, error) {
	if v.seen+int64(len(p)) > v.total {
		return 0, errors.New("racer object longer than its size")
	}

	last := v.seen+int64(len(p)) == v.total && len(p) > 0

	body := p
	if last {
		body = p[:len(p)-1]
	}

	n, err := v.write(body)
	if err != nil || !last {
		return n, err
	}

	v.hash.Write(p[len(p)-1:]) //nolint:errcheck // hash writes never fail

	if !v.matches() {
		return n, errDigestMismatch
	}

	m, err := v.w.Write(p[len(p)-1:])
	v.seen += int64(m)

	return n + m, err
}

func (v *verifyingWriter) write(p []byte) (int, error) {
	n, err := v.w.Write(p)
	v.hash.Write(p[:n]) //nolint:errcheck // hash writes never fail
	v.seen += int64(n)

	return n, err
}

// SetWriteDeadline lets the SDK manage write deadlines through the wrapper.
func (v *verifyingWriter) SetWriteDeadline(t time.Time) error {
	return v.rc.SetWriteDeadline(t)
}

func (v *verifyingWriter) matches() bool {
	return "sha256:"+hex.EncodeToString(v.hash.Sum(nil)) == v.want.String()
}

// verify reports whether a completed stream matched. Nonempty objects were
// already checked before their final byte; this covers empty objects.
func (v *verifyingWriter) verify() bool {
	return v.seen == v.total && v.matches()
}

func (s *Handler) digestMismatch(ctx context.Context, id string, request racersdk.Request, metadata racersdk.Metadata, remaining, written int64) {
	// Not rate limited: a mismatch means a corrupt cached or local replica.
	s.logger.Error("Racer object failed digest verification; aborting response",
		"request_id", id, "digest", "sha256:"+request.Key.String(),
		"expected_bytes", remaining, "written_bytes", written)

	if s.onMismatch != nil {
		s.onMismatch()
	}

	// The bad bytes may have come from this node's containerd store.
	s.replicas.Forget("sha256:" + request.Key.String())

	if s.invalidator == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invalidateTimeout)
	defer cancel()

	if err := s.invalidator.Invalidate(ctx, request.Key, metadata.ETag); err != nil {
		s.logger.Warn("Racer invalidate after digest mismatch failed",
			"request_id", id, "error_kind", racerFailureKind(err))
	}
}
