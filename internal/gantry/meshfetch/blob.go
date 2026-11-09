// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

var (
	errDownstream = errors.New("meshfetch: downstream write failed")
	errVerify     = errors.New("meshfetch: blob digest mismatch")
)

// FetchBlob serves ref (a blob or a manifest by digest) from byte
// ref.Offset to the end through the mesh.
//
// When the mesh is not ready, has no route, or the first chunk cannot be
// fetched, FetchBlob returns started == false having written nothing, and
// the caller should serve the request another way. Otherwise it calls start
// with the blob size and upstream content type exactly once (the caller
// writes the status line and headers there) and then streams the body to w.
// A non-nil error after start means the body is truncated; the caller must
// abort the response so the client retries with a Range request.
//
// Chunks are fetched Lookahead ahead of the one being written. With Verify
// set and Offset zero, the whole blob is hashed and the final byte is held
// back until the digest matches; on a mismatch the cached chunks of the
// blob are dropped and the error is errVerify.
func (n *Node) FetchBlob(ctx context.Context, w io.Writer, ref ifaces.OriginRef, start func(total int64, contentType string)) (started bool, written int64, err error) {
	t0 := time.Now()
	outcome := "declined"

	defer func() {
		if h := n.cfg.Hooks.OnBlob; h != nil {
			h(outcome, written, time.Since(t0))
		}
	}()

	if !n.mesh.Ready() || ref.Offset < 0 || ref.Digest.IsZero() {
		return false, 0, nil
	}

	kind := kindBlob
	if ref.Kind == ifaces.KindManifest {
		kind = kindManifest
	}

	cs := n.cfg.ChunkSize
	self := n.mesh.Addr()
	mk := func(i int64) chunkReq {
		return chunkReq{
			registry:  ref.Registry,
			repo:      ref.Repository,
			d:         ref.Digest,
			key:       chunkKey{digest: ref.Digest.String(), kind: kind, index: i},
			requester: self,
		}
	}

	c0 := ref.Offset / cs

	r0, err := n.obtainLocal(ctx, mk(c0), int(ref.Offset-c0*cs))
	if err != nil {
		return false, 0, nil
	}

	mctx, mcancel := context.WithTimeout(ctx, n.cfg.tune.chunkTimeout)
	total, ctype, _, err := r0.f.meta(mctx)

	mcancel()

	if err != nil || ref.Offset > total || (ref.Offset == total && total > 0) {
		r0.close()
		return false, 0, nil
	}

	last := c0
	if total > 0 {
		last = (total - 1) / cs
	}

	start(total, ctype)

	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	cw := &countWriter{w: w}

	var (
		out io.Writer = cw
		vw  *verifyWriter
	)

	if n.cfg.Verify && ref.Offset == 0 {
		vw = &verifyWriter{w: cw, h: sha256.New(), remaining: total}
		out = vw
	}

	err = n.streamChunks(ctx, out, r0, c0, last, mk)

	if err == nil && vw != nil {
		if err = vw.finish(ref.Digest.Hex()); errors.Is(err, errVerify) {
			n.store.evictDigest(ref.Digest.String())
			n.log.Warn("cachemesh blob failed verification; dropped cached chunks",
				"digest", ref.Digest.String(), "size", total)

			outcome = "verify_failed"

			return true, cw.n, err
		}
	}

	if err != nil {
		outcome = "aborted"
		return true, cw.n, err
	}

	outcome = "served"

	return true, cw.n, nil
}

type pending struct {
	r   *reader
	err error
}

// streamChunks writes chunks c0..last to w in order, starting with reader
// r0 of c0, while a producer keeps up to Lookahead later chunks in flight.
func (n *Node) streamChunks(ctx context.Context, w io.Writer, r0 *reader, c0, last int64, mk func(int64) chunkReq) error {
	pctx, pcancel := context.WithCancel(ctx)
	ch := make(chan pending, n.cfg.tune.lookahead)

	go func() {
		defer close(ch)

		for i := c0 + 1; i <= last; i++ {
			r, err := n.obtainLocal(pctx, mk(i), 0)

			select {
			case ch <- pending{r: r, err: err}:
			case <-pctx.Done():
				if r != nil {
					r.close()
				}

				return
			}

			if err != nil {
				return
			}
		}
	}()

	defer func() {
		pcancel()

		for p := range ch {
			if p.r != nil {
				p.r.close()
			}
		}
	}()

	cur := r0

	for idx := c0; ; idx++ {
		if err := n.copyChunk(ctx, w, cur, mk(idx)); err != nil {
			return err
		}

		if idx == last {
			return nil
		}

		select {
		case p, ok := <-ch:
			if !ok {
				return ctx.Err()
			}

			if p.err != nil {
				return p.err
			}

			cur = p.r
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// copyChunk writes the chunk from r's position to w. When the fill fails
// or no longer holds the bytes the reader needs, it obtains a new reader
// (which may start a new fill that routes afresh) and continues where it
// stopped. It closes r and any replacement.
func (n *Node) copyChunk(ctx context.Context, w io.Writer, r *reader, req chunkReq) error {
	for attempt := 1; ; attempt++ {
		r.f.mu.Lock()
		pos := r.pos
		r.f.mu.Unlock()

		k, err := r.streamTo(ctx, w)
		r.close()

		if err == nil {
			return nil
		}

		if ctx.Err() != nil || errors.Is(err, errDownstream) || terminal(err) || attempt >= n.cfg.tune.attempts {
			return err
		}

		r, err = n.obtainLocal(ctx, req, pos+int(k))
		if err != nil {
			return err
		}
	}
}

// obtainLocal returns a reader at byte off of a chunk this agent needs.
func (n *Node) obtainLocal(ctx context.Context, req chunkReq, off int) (*reader, error) {
	key := req.key.routeKey(make([]byte, 0, 96))
	decide := func() (cachemesh.Decision, error) {
		return n.mesh.Route(n.blobKS, key), nil
	}

	return n.obtain(ctx, req, off, decide, "local", noSlot)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	c.n += int64(k)

	if err != nil {
		return k, fmt.Errorf("%w: %w", errDownstream, err)
	}

	return k, nil
}

// verifyWriter hashes everything written and holds back the final byte of
// the blob until finish confirms the digest.
type verifyWriter struct {
	w         io.Writer
	h         hash.Hash
	remaining int64
	held      [1]byte
	hasHeld   bool
}

func (v *verifyWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	_, _ = v.h.Write(p) //nolint:errcheck // hash writes never fail

	if int64(len(p)) < v.remaining {
		v.remaining -= int64(len(p))
		return v.w.Write(p)
	}

	// p ends the blob: hold its last byte.
	last := len(p) - 1
	v.held[0] = p[last]
	v.hasHeld = true
	v.remaining = 0

	k, err := v.w.Write(p[:last])
	if err != nil {
		return k, err
	}

	return len(p), nil
}

func (v *verifyWriter) finish(wantHex string) error {
	if got := hex.EncodeToString(v.h.Sum(nil)); got != wantHex {
		return fmt.Errorf("%w: got sha256:%s", errVerify, got)
	}

	if v.hasHeld {
		if _, err := v.w.Write(v.held[:]); err != nil {
			return err
		}
	}

	return nil
}
