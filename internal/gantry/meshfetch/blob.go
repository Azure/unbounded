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
	mk := func(i int64) chunkReq {
		return chunkReq{
			registry: ref.Registry,
			repo:     ref.Repository,
			d:        ref.Digest,
			key:      chunkKey{digest: ref.Digest.String(), kind: kind, index: i},
		}
	}

	c0 := ref.Offset / cs

	f0, err := n.obtainLocal(ctx, mk(c0))
	if err != nil {
		return false, 0, nil
	}

	mctx, mcancel := context.WithTimeout(ctx, n.cfg.ChunkTimeout)
	total, ctype, _, err := f0.meta(mctx)

	mcancel()

	if err != nil || ref.Offset > total || (ref.Offset == total && total > 0) {
		f0.release()
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

	err = n.streamChunks(ctx, out, f0, c0, last, ref.Offset-c0*cs, mk)

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
	f   *fill
	err error
}

// streamChunks writes chunks c0..last to w in order, starting at byte
// within of c0, while a producer keeps up to Lookahead later chunks in
// flight.
func (n *Node) streamChunks(ctx context.Context, w io.Writer, f0 *fill, c0, last, within int64, mk func(int64) chunkReq) error {
	pctx, pcancel := context.WithCancel(ctx)
	ch := make(chan pending, n.cfg.Lookahead)

	go func() {
		defer close(ch)

		for i := c0 + 1; i <= last; i++ {
			f, err := n.obtainLocal(pctx, mk(i))

			select {
			case ch <- pending{f: f, err: err}:
			case <-pctx.Done():
				if f != nil {
					f.release()
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
			if p.f != nil {
				p.f.release()
			}
		}
	}()

	cur := f0

	for idx := c0; ; idx++ {
		if err := n.copyChunk(ctx, w, cur, mk(idx), within); err != nil {
			return err
		}

		if idx == last {
			return nil
		}

		within = 0

		select {
		case p, ok := <-ch:
			if !ok {
				return ctx.Err()
			}

			if p.err != nil {
				return p.err
			}

			cur = p.f
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// copyChunk writes chunk f from byte pos to w. When the fill fails it
// starts a new fill (which routes afresh) and continues where it stopped.
// It releases f and any replacement.
func (n *Node) copyChunk(ctx context.Context, w io.Writer, f *fill, req chunkReq, pos int64) error {
	for attempt := 1; ; attempt++ {
		k, err := f.streamTo(ctx, w, int(pos))
		f.release()

		pos += k

		if err == nil {
			return nil
		}

		if ctx.Err() != nil || errors.Is(err, errDownstream) || terminal(err) || attempt >= n.cfg.Attempts {
			return err
		}

		f, err = n.obtainLocal(ctx, req)
		if err != nil {
			return err
		}
	}
}

// obtainLocal returns the fill for a chunk this agent needs.
func (n *Node) obtainLocal(ctx context.Context, req chunkReq) (*fill, error) {
	key := req.key.routeKey(make([]byte, 0, 96))
	decide := func() (cachemesh.Decision, error) {
		return n.mesh.Route(n.blobKS, key), nil
	}

	return n.obtain(ctx, req, decide, "local", noSlot)
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
