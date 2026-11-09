// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Fill errors. errNotFound and errRange are terminal: no other source can
// do better, so the fill fails at once.
var (
	errNotFound = errors.New("meshfetch: content not found")
	errRange    = errors.New("meshfetch: chunk outside the blob")
	errNoRoute  = errors.New("meshfetch: no route")
	errLoop     = errors.New("meshfetch: request loop")
	errOverload = errors.New("meshfetch: peer overloaded")
)

func terminal(err error) bool {
	return errors.Is(err, errNotFound) || errors.Is(err, errRange) || errors.Is(err, errMetaMismatch) ||
		errors.Is(err, errClosed) || errors.Is(err, errNoRoute)
}

// chunkReq identifies a chunk and where its blob lives at the origin.
type chunkReq struct {
	registry string
	repo     string
	d        digest.Digest
	key      chunkKey
}

func (r *chunkReq) kind() ifaces.OriginRefKind {
	if r.key.kind == kindManifest {
		return ifaces.KindManifest
	}

	return ifaces.KindBlob
}

// decider returns the routing decision for the next fill attempt.
type decider func() (cachemesh.Decision, error)

// obtain returns a referenced fill for req: a cached or in-progress one, or
// a new fill routed by decide. peer requests pass the sender slot for loop
// detection and never wait for budget.
func (n *Node) obtain(ctx context.Context, req chunkReq, decide decider, requester string, sender cachemesh.Slot) (*fill, error) {
	f, created, err := n.store.acquire(ctx, req.key, requester == "local")
	if err != nil {
		return nil, err
	}

	if created {
		n.lookup(requester, "fill")

		go n.runFill(f, req, decide)

		return f, nil
	}

	f.mu.Lock()
	done, upstream := f.done, f.upstream
	f.mu.Unlock()

	if !done && sender != noSlot && upstream == sender {
		f.release()
		return nil, errLoop
	}

	if done {
		n.lookup(requester, "hit")
	} else {
		n.lookup(requester, "joined")
	}

	return f, nil
}

func (n *Node) lookup(requester, result string) {
	if h := n.cfg.Hooks.OnLookup; h != nil {
		h(requester, result)
	}
}

func (n *Node) runFill(f *fill, req chunkReq, decide decider) {
	defer f.release()

	err := n.fillChunk(f, req, decide)
	if err != nil {
		n.log.Debug("chunk fill failed",
			slog.String("digest", req.key.digest),
			slog.Int64("chunk", req.key.index),
			slog.Any("err", err),
		)
	}

	f.finish(err)
}

func (n *Node) fillChunk(f *fill, req chunkReq, decide decider) error {
	var (
		lastErr error
		alt     cachemesh.PeerRef
		altHdr  cachemesh.Header
	)

	for attempt := range n.cfg.Attempts {
		if n.ctx.Err() != nil {
			return errClosed
		}

		if attempt > 0 && alt.IsZero() {
			if err := sleepCtx(n.ctx, backoff(attempt)); err != nil {
				return errClosed
			}
		}

		var (
			target     cachemesh.PeerRef
			hdr        cachemesh.Header
			hedgeAfter time.Duration
			hedgeTo    cachemesh.PeerRef
			hedgeHdr   cachemesh.Header
		)

		if attempt > 0 && !alt.IsZero() {
			target, hdr = alt, altHdr
			alt = cachemesh.PeerRef{}
		} else {
			d, err := decide()
			if err != nil {
				return err
			}

			switch d.Kind {
			case cachemesh.KindNoRoute:
				return errNoRoute
			case cachemesh.KindSelf:
				n.log.Debug("chunk fill from origin",
					slog.String("self", n.mesh.ID()),
					slog.String("owner", d.Owner.ID),
					slog.String("prev_owner", d.PrevOwner.ID),
					slog.Int("flags", int(d.Header.Flags)),
					slog.Int64("chunk", req.key.index),
				)

				if d.Owner.ID == n.mesh.ID() {
					n.store.markOwned(f)
				}

				// Read through before going to the origin: from the
				// previous owner during a handoff, and from the owner
				// when standing in for it as a backup (an owner that is
				// merely slow often has the chunk already).
				if attempt == 0 && !d.PrevOwner.IsZero() && n.fillFromPrev(f, req, d.PrevOwner, peekHandoff, "prev_owner") == nil {
					return nil
				}

				if attempt == 0 && !d.Owner.IsZero() && d.Owner.ID != n.mesh.ID() && n.fillFromPrev(f, req, d.Owner, peekOwner, "owner") == nil {
					return nil
				}

				err := n.fillFromOrigin(f, req)
				if err == nil || terminal(err) {
					return err
				}

				lastErr = err

				continue
			}

			target, hdr = d.Next, d.Header

			if d.Kind == cachemesh.KindBackup {
				n.log.Debug("chunk routed to backup",
					slog.String("self", n.mesh.ID()),
					slog.String("owner", d.Owner.ID),
					slog.String("next", d.Next.ID),
					slog.Int64("chunk", req.key.index),
				)
			}

			if !d.Alternate.IsZero() {
				alt, altHdr = d.Alternate, d.AlternateHeader()

				if n.cfg.Hedge {
					hedgeAfter, hedgeTo, hedgeHdr = d.HedgeAfter, alt, altHdr
				}
			}
		}

		err := n.fillFromPeer(f, req, target, hdr, hedgeAfter, hedgeTo, hedgeHdr)
		if err == nil || terminal(err) {
			return err
		}

		n.log.Debug("chunk fill attempt failed",
			slog.String("peer", target.ID),
			slog.String("digest", req.key.digest),
			slog.Int64("chunk", req.key.index),
			slog.Int("attempt", attempt),
			slog.Any("err", err),
		)

		lastErr = err
	}

	return lastErr
}

func backoff(attempt int) time.Duration {
	d := 50 * time.Millisecond << min(attempt, 6)
	return min(d, 2*time.Second)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attemptCtx returns a context bounded by ChunkTimeout that is also
// canceled when touch is not called for StallTimeout.
func (n *Node) attemptCtx() (ctx context.Context, touch func(), stalled func() bool, cancel func()) {
	ctx, cancelCtx := context.WithTimeout(n.ctx, n.cfg.ChunkTimeout)

	var fired atomic.Bool

	timer := time.AfterFunc(n.cfg.StallTimeout, func() {
		fired.Store(true)
		cancelCtx()
	})

	return ctx, func() { timer.Reset(n.cfg.StallTimeout) }, fired.Load, func() {
		timer.Stop()
		cancelCtx()
	}
}

func (n *Node) fillHook(source string, err error, bytes int64, start time.Time) {
	if h := n.cfg.Hooks.OnFill; h != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}

		h(source, outcome, bytes, time.Since(start))
	}
}

// fillFromOrigin pulls the rest of the chunk from the origin registry.
func (n *Node) fillFromOrigin(f *fill, req chunkReq) (err error) {
	start := time.Now()
	off, metaKnown := f.progress()

	defer func() {
		now, _ := f.progress()
		n.fillHook("origin", err, int64(now-off), start)
	}()

	ctx, touch, _, cancel := n.attemptCtx()
	defer cancel()

	chunkStart := req.key.index * n.cfg.ChunkSize

	length := n.cfg.ChunkSize - int64(off)

	if metaKnown {
		f.mu.Lock()
		length = int64(len(f.buf) - off)
		f.mu.Unlock()
	}

	if length == 0 {
		return nil
	}

	ref := ifaces.OriginRef{
		Registry:   req.registry,
		Repository: req.repo,
		Digest:     req.d,
		Kind:       req.kind(),
		Offset:     chunkStart + int64(off),
	}

	f.mu.Lock()
	f.fromOrigin = true
	f.upstream = noSlot
	f.mu.Unlock()

	rc, total, ctype, err := n.cfg.Origin.PullRange(ctx, ref, length)
	if err != nil {
		return classifyOrigin(err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	chunkLen := min(n.cfg.ChunkSize, total-chunkStart)
	if chunkLen < 0 || (chunkLen == 0 && chunkStart != 0) {
		return errRange
	}

	if err := f.setMeta(total, ctype, int(chunkLen)); err != nil {
		return err
	}

	touch()

	return f.readFrom(rc, touch)
}

func classifyOrigin(err error) error {
	var oe *ifaces.OriginError
	if errors.As(err, &oe) {
		switch oe.Class {
		case ifaces.FailureNotFound, ifaces.FailureAuth:
			return fmt.Errorf("%w: %w", errNotFound, err)
		}

		var ru *ifaces.ErrRangeUnsupported
		if errors.As(err, &ru) {
			return fmt.Errorf("%w: %w", errRange, err)
		}
	}

	return err
}

// Values of the peek query parameter.
const (
	// peekHandoff asks the previous owner of a chunk during a handoff. It
	// is served only from what the receiver already has.
	peekHandoff = "1"
	// peekOwner asks the owner on behalf of a backup standing in for it.
	// An owner without the chunk starts its origin fill and serves it.
	peekOwner = "owner"
)

// peekHeaderTimeout bounds the wait for response headers of a peek.
const peekHeaderTimeout = 2 * time.Second

// fillFromPrev reads the chunk from another member that may already hold
// it (a previous owner, or the owner a backup stands in for). That member
// serves a complete chunk or one it is reading from the origin; for a
// peekOwner request an owner without the chunk also starts its origin fill.
func (n *Node) fillFromPrev(f *fill, req chunkReq, prev cachemesh.PeerRef, peek, source string) (err error) {
	start := time.Now()
	off, _ := f.progress()

	defer func() {
		now, _ := f.progress()
		n.fillHook(source, err, int64(now-off), start)
	}()

	ctx, touch, _, cancel := n.attemptCtx()
	defer cancel()

	headers := time.AfterFunc(peekHeaderTimeout, cancel)

	resp, err := n.doChunk(ctx, prev, nil, peek, req, off)

	headers.Stop()

	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	return n.readChunkResponse(f, req, resp, off, touch)
}

// fillFromPeer fetches the rest of the chunk from target, hedging to
// hedgeTo after hedgeAfter when allowed.
func (n *Node) fillFromPeer(f *fill, req chunkReq, target cachemesh.PeerRef, hdr cachemesh.Header,
	hedgeAfter time.Duration, hedgeTo cachemesh.PeerRef, hedgeHdr cachemesh.Header,
) (err error) {
	start := time.Now()
	off, _ := f.progress()

	ctx, touch, stalled, cancel := n.attemptCtx()
	defer cancel()

	// Record the upstream before sending, so a request that comes back
	// from it (a routing loop while views disagree) is refused at once.
	f.mu.Lock()
	f.upstream = target.Slot
	f.fromOrigin = false
	f.mu.Unlock()

	resp, peer, err := n.sendChunk(ctx, stalled, req, off, target, hdr, hedgeAfter, hedgeTo, hedgeHdr)
	if err != nil {
		n.fillHook("peer", err, 0, start)
		return err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	f.mu.Lock()
	f.upstream = peer.Slot
	f.fromOrigin = false
	f.mu.Unlock()

	err = n.readChunkResponse(f, req, resp, off, touch)

	now, _ := f.progress()
	n.fillHook("peer", err, int64(now-off), start)

	if err != nil && !terminal(err) {
		// Headers arrived but the body failed: the peer is unhealthy for
		// this request even though Observe already saw a success.
		r := cachemesh.Failure
		if stalled() {
			r = cachemesh.Timeout
		}

		n.observe(peer, r, time.Since(start))
	}

	return err
}

// readChunkResponse validates a 200 chunk response and reads its body.
func (n *Node) readChunkResponse(f *fill, req chunkReq, resp *http.Response, off int, touch func()) error {
	total, err := strconv.ParseInt(resp.Header.Get(headerBlobSize), 10, 64)
	if err != nil || total < 0 {
		return fmt.Errorf("meshfetch: bad %s header", headerBlobSize)
	}

	chunkStart := req.key.index * n.cfg.ChunkSize

	chunkLen := min(n.cfg.ChunkSize, total-chunkStart)
	if chunkLen < 0 || (chunkLen == 0 && chunkStart != 0) {
		return errRange
	}

	if resp.ContentLength >= 0 && resp.ContentLength != chunkLen-int64(off) {
		return fmt.Errorf("meshfetch: chunk response length %d, want %d", resp.ContentLength, chunkLen-int64(off))
	}

	if err := f.setMeta(total, resp.Header.Get("Content-Type"), int(chunkLen)); err != nil {
		return err
	}

	touch()

	return f.readFrom(resp.Body, touch)
}

type peerResult struct {
	resp   *http.Response
	err    error
	peer   cachemesh.PeerRef
	cancel context.CancelFunc
	took   time.Duration
}

// sendChunk sends the chunk request to target and, after hedgeAfter, a
// hedged copy to hedgeTo. It returns the first successful response; the
// caller closes its body. Every outcome except a canceled loser is
// reported to Observe.
func (n *Node) sendChunk(ctx context.Context, stalled func() bool, req chunkReq, off int,
	target cachemesh.PeerRef, hdr cachemesh.Header,
	hedgeAfter time.Duration, hedgeTo cachemesh.PeerRef, hedgeHdr cachemesh.Header,
) (*http.Response, cachemesh.PeerRef, error) {
	results := make(chan peerResult, 2)
	launch := func(p cachemesh.PeerRef, h cachemesh.Header) {
		rctx, rcancel := context.WithCancel(ctx)

		go func() {
			t0 := time.Now()

			resp, err := n.doChunk(rctx, p, &h, "", req, off)
			results <- peerResult{resp: resp, err: err, peer: p, cancel: rcancel, took: time.Since(t0)}
		}()
	}

	launch(target, hdr)

	pending := 1
	hedged := false

	var hedgeC <-chan time.Time

	if !hedgeTo.IsZero() && hedgeAfter > 0 {
		t := time.NewTimer(hedgeAfter)
		defer t.Stop()

		hedgeC = t.C
	}

	var firstErr error

	for pending > 0 {
		select {
		case <-hedgeC:
			hedgeC = nil

			if !n.mesh.TryHedge(hdr) {
				n.hook(n.cfg.Hooks.OnHedge, "denied")
				continue
			}

			h := hedgeHdr
			h.Flags |= cachemesh.FlagHedge

			launch(hedgeTo, h)

			pending++
			hedged = true

			n.hook(n.cfg.Hooks.OnHedge, "sent")
		case r := <-results:
			pending--

			if r.err == nil {
				n.observe(r.peer, cachemesh.Success, r.took)

				if hedged && r.peer.ID == hedgeTo.ID {
					n.hook(n.cfg.Hooks.OnHedge, "won")
				}

				if pending > 0 {
					go drainLosers(results, pending)
				}

				resp := r.resp
				resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: r.cancel}

				return resp, r.peer, nil
			}

			r.cancel()

			switch {
			case errors.Is(r.err, errOverload):
				n.observe(r.peer, cachemesh.Overload, r.took)
			case errors.Is(r.err, context.DeadlineExceeded) || stalled():
				n.observe(r.peer, cachemesh.Timeout, r.took)
			case ctx.Err() != nil:
				// Canceled by the caller: not the peer's fault.
			case errors.Is(r.err, errNotFound):
				// The peer answered correctly: the content is absent.
				n.observe(r.peer, cachemesh.Success, r.took)
			default:
				n.observe(r.peer, cachemesh.Failure, r.took)
			}

			if firstErr == nil || terminal(r.err) {
				firstErr = r.err
			}
		case <-ctx.Done():
			if pending > 0 {
				go drainLosers(results, pending)
			}

			if stalled() {
				n.observe(target, cachemesh.Timeout, n.cfg.StallTimeout)
			}

			return nil, cachemesh.PeerRef{}, ctx.Err()
		}
	}

	return nil, cachemesh.PeerRef{}, firstErr
}

func drainLosers(results <-chan peerResult, n int) {
	for range n {
		r := <-results
		r.cancel()

		if r.resp != nil {
			_ = r.resp.Body.Close() //nolint:errcheck // best-effort close
		}
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()

	return err
}

// chunkQuery builds the query string of a chunk request.
func (n *Node) chunkQuery(req chunkReq, off int, peek string) string {
	q := url.Values{}
	q.Set("r", req.registry)
	q.Set("n", req.repo)
	q.Set("d", req.key.digest)
	q.Set("k", strconv.Itoa(int(req.key.kind)))
	q.Set("i", strconv.FormatInt(req.key.index, 10))
	q.Set("cs", strconv.FormatInt(n.cfg.ChunkSize, 10))

	if off > 0 {
		q.Set("o", strconv.Itoa(off))
	}

	if peek != "" {
		q.Set("peek", peek)
	}

	return q.Encode()
}

// doChunk sends one chunk request. A nil hdr sends a peek request of kind
// peek (peekHandoff or peekOwner). A non-200 status maps to an error.
func (n *Node) doChunk(ctx context.Context, p cachemesh.PeerRef, hdr *cachemesh.Header, peek string, req chunkReq, off int) (*http.Response, error) {
	if hdr != nil {
		peek = ""
	}

	u := "http://" + n.cfg.DataAddr(p) + ChunkPath + "?" + n.chunkQuery(req, off, peek)

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	if hdr != nil {
		hreq.Header.Set(headerRoute, encodeHeader(*hdr))
	}

	resp, err := n.client.Do(hreq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for reuse
	_ = resp.Body.Close()                                       //nolint:errcheck // best-effort close

	return nil, statusError(resp.StatusCode, p)
}

func statusError(code int, p cachemesh.PeerRef) error {
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: peer %s", errNotFound, p.ID)
	case http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("%w: peer %s", errRange, p.ID)
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return fmt.Errorf("%w: peer %s: %d", errOverload, p.ID, code)
	default:
		return fmt.Errorf("meshfetch: peer %s: status %d", p.ID, code)
	}
}

func encodeHeader(h cachemesh.Header) string {
	var b [cachemesh.HeaderSize]byte

	h.Put(&b)

	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decodeHeader(s string) (cachemesh.Header, error) {
	var b [cachemesh.HeaderSize]byte

	if len(s) != base64.RawURLEncoding.EncodedLen(cachemesh.HeaderSize) {
		return cachemesh.Header{}, cachemesh.ErrBadHeader
	}

	k, err := base64.RawURLEncoding.Decode(b[:], []byte(s))
	if err != nil || k != cachemesh.HeaderSize {
		return cachemesh.Header{}, cachemesh.ErrBadHeader
	}

	return cachemesh.ParseHeader(b[:])
}
