// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
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
	// requester is the member address of the agent whose containerd
	// pull the request serves, carried hop by hop in headerRequester so
	// the owner learns which members hold the chunk afterwards. Empty
	// when unknown.
	requester string
}

func (r *chunkReq) kind() ifaces.OriginRefKind {
	if r.key.kind == kindManifest {
		return ifaces.KindManifest
	}

	return ifaces.KindBlob
}

// decider returns the routing decision for the next fill attempt.
type decider func() (cachemesh.Decision, error)

// obtain returns a reader at byte off of the fill for req: a cached or
// in-progress one, or a new fill routed by decide. peer requests pass the
// sender slot for loop detection and never wait for the fill cap.
func (n *Node) obtain(ctx context.Context, req chunkReq, off int, decide decider, requester string, sender cachemesh.Slot) (*reader, error) {
	r, created, err := n.store.acquire(ctx, req.key, off, requester == "local", requester == "peer")
	if err != nil {
		return nil, err
	}

	f := r.f

	if created {
		n.lookup(requester, "fill")

		go n.runFill(f, req, decide)

		return r, nil
	}

	f.mu.Lock()
	done, upstream := f.done, f.upstream
	f.mu.Unlock()

	if !done && sender != noSlot && upstream == sender {
		r.close()
		return nil, errLoop
	}

	if done {
		n.lookup(requester, "hit")
	} else {
		n.lookup(requester, "joined")
	}

	return r, nil
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
	// The local containerd content store comes first: it needs no
	// network and no routing.
	if n.fillFromLocal(f, req) == nil {
		return nil
	}

	self := func(attempt int, d cachemesh.Decision) error {
		n.log.Debug("chunk fill from origin",
			slog.String("self", n.mesh.ID()),
			slog.String("owner", d.Owner.ID),
			slog.String("prev_owner", d.PrevOwner.ID),
			slog.Int("flags", int(d.Header.Flags)),
			slog.Int64("chunk", req.key.index),
		)

		owner := d.Owner.ID == n.mesh.ID()
		if owner {
			f.owned.Store(true)
		}

		// Read through before going to the origin: from the previous
		// owner during a handoff, and from the owner when standing in for
		// it as a backup (an owner that is merely slow often has the
		// chunk already). An owner whose memory no longer holds the chunk
		// reads it from a member that recently pulled it into its own
		// content store.
		if attempt == 0 {
			switch {
			case !d.PrevOwner.IsZero() && n.fillFromPrev(f, req, d.PrevOwner, peekLocal, "prev_owner") == nil:
				return nil
			case !d.Owner.IsZero() && !owner && n.fillFromPrev(f, req, d.Owner, peekOwner, "owner") == nil:
				return nil
			case owner && n.fillFromHolders(f, req) == nil:
				return nil
			}
		}

		return n.fillFromOrigin(f, req)
	}

	peer := func(h hop) error {
		err := n.fillFromPeer(f, req, h)
		if err != nil && !terminal(err) {
			n.log.Debug("chunk fill attempt failed",
				slog.String("peer", h.to.ID),
				slog.String("digest", req.key.digest),
				slog.Int64("chunk", req.key.index),
				slog.Int("attempt", h.attempt),
				slog.Any("err", err),
			)
		}

		return err
	}

	return n.route(decide, self, peer)
}

// hop is one routed attempt: the peer to ask with its header, and the
// alternate to hedge to after hedgeAfter (zero when hedging is off).
type hop struct {
	attempt    int
	to         cachemesh.PeerRef
	hdr        cachemesh.Header
	hedgeAfter time.Duration
	hedgeTo    cachemesh.PeerRef
	hedgeHdr   cachemesh.Header
}

// route runs up to tune.attempts attempts of a routed request, shared by
// chunk fills and tag resolutions. Each attempt asks decide where to go: to
// self (this member serves it, from the origin) or to a peer. A failed peer
// attempt is retried at once on the decision's alternate, otherwise after a
// backoff with a fresh decision. route stops at the first success or
// terminal error and returns the last error.
func (n *Node) route(decide decider, self func(attempt int, d cachemesh.Decision) error, peer func(hop) error) error {
	var (
		lastErr error
		alt     hop
	)

	for attempt := range n.cfg.tune.attempts {
		if n.ctx.Err() != nil {
			return errClosed
		}

		h := alt
		alt = hop{}

		if h.to.IsZero() {
			if attempt > 0 {
				if err := sleepCtx(n.ctx, backoff(attempt)); err != nil {
					return errClosed
				}
			}

			d, err := decide()
			if err != nil {
				return err
			}

			switch d.Kind {
			case cachemesh.KindNoRoute:
				return errNoRoute
			case cachemesh.KindSelf:
				err := self(attempt, d)
				if err == nil || terminal(err) {
					return err
				}

				lastErr = err

				continue
			}

			h = hop{to: d.Next, hdr: d.Header}

			if !d.Alternate.IsZero() {
				alt = hop{to: d.Alternate, hdr: d.AlternateHeader()}

				if !n.cfg.tune.noHedge {
					h.hedgeAfter, h.hedgeTo, h.hedgeHdr = d.HedgeAfter, alt.to, alt.hdr
				}
			}
		}

		h.attempt = attempt

		err := peer(h)
		if err == nil || terminal(err) {
			return err
		}

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

// attemptCtx returns a context bounded by the chunk timeout that is also
// canceled when touch is not called for the stall timeout.
func (n *Node) attemptCtx() (ctx context.Context, touch func(), stalled func() bool, cancel func()) {
	ctx, cancelCtx := context.WithTimeout(n.ctx, n.cfg.tune.chunkTimeout)

	var fired atomic.Bool

	stall := n.cfg.tune.stallTimeout
	timer := time.AfterFunc(stall, func() {
		fired.Store(true)
		cancelCtx()
	})

	return ctx, func() { timer.Reset(stall) }, fired.Load, func() {
		timer.Stop()
		cancelCtx()
	}
}

// fillAttempt is one attempt to read the rest of a chunk from one source.
// off is the fill's progress when it starts.
type fillAttempt struct {
	ctx     context.Context
	touch   func()
	stalled func() bool
	off     int
}

// attempt runs fn with an attempt context and reports it to OnFill with
// the bytes it added to f.
func (n *Node) attempt(f *fill, source string, fn func(a fillAttempt) error) error {
	start := time.Now()
	off, _ := f.progress()

	ctx, touch, stalled, cancel := n.attemptCtx()
	defer cancel()

	err := fn(fillAttempt{ctx: ctx, touch: touch, stalled: stalled, off: off})

	if h := n.cfg.Hooks.OnFill; h != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}

		now, _ := f.progress()
		h(source, outcome, int64(now-off), time.Since(start))
	}

	return err
}

// setUpstream records where f reads from: a peer slot for loop detection,
// or noSlot with fromOrigin set while it reads from the origin.
func (f *fill) setUpstream(slot cachemesh.Slot, fromOrigin bool) {
	f.mu.Lock()
	f.upstream = slot
	f.fromOrigin = fromOrigin
	f.mu.Unlock()
}

// fillFromOrigin pulls the rest of the chunk from the origin registry.
func (n *Node) fillFromOrigin(f *fill, req chunkReq) error {
	return n.attempt(f, "origin", func(a fillAttempt) error {
		chunkStart := req.key.index * n.cfg.ChunkSize
		length := n.cfg.ChunkSize - int64(a.off)

		f.mu.Lock()
		if f.metaKnown {
			length = int64(f.chunkLen - a.off)
		}
		f.mu.Unlock()

		if length == 0 {
			return nil
		}

		ref := ifaces.OriginRef{
			Registry:   req.registry,
			Repository: req.repo,
			Digest:     req.d,
			Kind:       req.kind(),
			Offset:     chunkStart + int64(a.off),
		}

		f.setUpstream(noSlot, true)

		rc, total, ctype, err := n.cfg.Origin.PullRange(a.ctx, ref, length)
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

		a.touch()

		return f.readFrom(a.ctx, rc, a.touch)
	})
}

// contentReader reads blob d of src sequentially from off.
type contentReader struct {
	src ContentSource
	d   string
	off int64
	end int64
}

func (r *contentReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}

	p = p[:min(int64(len(p)), r.end-r.off)]

	k, err := r.src.ReadAt(r.d, p, r.off)
	r.off += int64(k)

	return k, err
}

// localChunk returns the blob size and chunk length of req when the local
// content store holds the whole chunk. Only blobs are read locally:
// manifests need their content type, which the store does not keep.
func (n *Node) localChunk(req chunkReq) (total int64, chunkLen int, ok bool) {
	if req.key.kind != kindBlob || !n.store.localOK(req.key.digest) {
		return 0, 0, false
	}

	avail, total := n.cfg.Content.Available(req.key.digest)
	if total <= 0 {
		return 0, 0, false
	}

	start := req.key.index * n.cfg.ChunkSize

	cl := min(n.cfg.ChunkSize, total-start)
	if cl <= 0 || avail < start+cl {
		return 0, 0, false
	}

	return total, int(cl), true
}

// fillFromLocal reads the chunk from the local content store when it
// holds all of it.
func (n *Node) fillFromLocal(f *fill, req chunkReq) error {
	total, chunkLen, ok := n.localChunk(req)
	if !ok {
		return errNotFound
	}

	return n.attempt(f, "local", func(a fillAttempt) error {
		if err := f.setMeta(total, "", chunkLen); err != nil {
			return err
		}

		f.setUpstream(noSlot, false)
		n.store.localFills.Add(1)

		chunkStart := req.key.index * n.cfg.ChunkSize
		r := &contentReader{src: n.cfg.Content, d: req.key.digest, off: chunkStart + int64(a.off), end: chunkStart + int64(chunkLen)}

		err := f.readFrom(a.ctx, r, a.touch)
		if err != nil {
			n.store.localErrors.Add(1)
		}

		return err
	})
}

// Peek requests ask a member for a chunk it may already hold, without
// routing. Values of the peek query parameter:
const (
	// peekLocal is served only from what the member already has: memory,
	// a fill it reads from the origin, or its content store. New owners
	// ask the previous owner during a handoff, and owners ask members
	// that recently pulled the chunk (holders).
	peekLocal = "local"
	// peekOwner asks the owner on behalf of a backup standing in for it.
	// An owner without the chunk starts its origin fill and serves it.
	peekOwner = "owner"
)

// Response header timeouts of peeks: a holder that is slow to answer is
// skipped sooner.
const (
	peekHeaderTimeout = 2 * time.Second
	holderPeekTimeout = time.Second
	holderPeekMax     = 2
	sourceHolder      = "holder"
)

// fillFromHolders reads the chunk from members that recently requested it
// for their own containerd pull. They serve it only from memory or their
// content store (peekLocal), never by fetching it.
func (n *Node) fillFromHolders(f *fill, req chunkReq) error {
	hs := n.holders.list(req.key, n.mesh.Addr())

	for i, h := range hs {
		if i == holderPeekMax {
			break
		}

		err := n.fillFromPrev(f, req, cachemesh.PeerRef{ID: h, Addr: h}, peekLocal, sourceHolder)
		if err == nil {
			n.holderPeeks.Add(1)
			return nil
		}

		n.holderMisses.Add(1)

		if errors.Is(err, errMetaMismatch) {
			return err
		}
	}

	return errNotFound
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

// fillFromPrev reads the chunk from another member that may already hold
// it (a previous owner, a holder, or the owner a backup stands in for) with
// a peek request. That member serves a complete chunk or one it is reading
// from the origin; for peekOwner an owner without the chunk also starts its
// origin fill.
func (n *Node) fillFromPrev(f *fill, req chunkReq, prev cachemesh.PeerRef, peek, source string) error {
	return n.attempt(f, source, func(a fillAttempt) error {
		timeout := peekHeaderTimeout
		if source == sourceHolder {
			timeout = holderPeekTimeout
		}

		ctx, cancel := context.WithCancel(a.ctx)
		defer cancel()

		headers := time.AfterFunc(timeout, cancel)

		resp, err := n.doChunk(ctx, prev, nil, peek, req, a.off)

		headers.Stop()

		if err != nil {
			return err
		}

		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

		return n.readChunkResponse(ctx, f, req, resp, a.off, a.touch)
	})
}

// fillFromPeer fetches the rest of the chunk from h.to, hedging to
// h.hedgeTo after h.hedgeAfter when allowed.
func (n *Node) fillFromPeer(f *fill, req chunkReq, h hop) error {
	return n.attempt(f, "peer", func(a fillAttempt) error {
		start := time.Now()

		// Record the upstream before sending, so a request that comes
		// back from it (a routing loop while views disagree) is refused
		// at once.
		f.setUpstream(h.to.Slot, false)

		resp, peer, err := n.sendChunk(a.ctx, a.stalled, req, a.off, h)
		if err != nil {
			return err
		}

		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

		f.setUpstream(peer.Slot, false)

		err = n.readChunkResponse(a.ctx, f, req, resp, a.off, a.touch)
		if err != nil && !terminal(err) {
			// Headers arrived but the body failed: the peer is unhealthy
			// for this request even though Observe already saw a success.
			n.observeErr(a.ctx, peer, err, time.Since(start), a.stalled())
		}

		return err
	})
}

// readChunkResponse validates a 200 chunk response and reads its body.
func (n *Node) readChunkResponse(ctx context.Context, f *fill, req chunkReq, resp *http.Response, off int, touch func()) error {
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

	return f.readFrom(ctx, resp.Body, touch)
}

// observeErr reports the outcome of a peer request to the health tracker:
// success (also for a correct "not found"), overload, timeout (the deadline
// passed or the attempt stalled), or failure. A request canceled by the
// caller is not the peer's fault and is not reported.
func (n *Node) observeErr(ctx context.Context, p cachemesh.PeerRef, err error, took time.Duration, stalled bool) {
	r := cachemesh.Failure

	switch {
	case err == nil, errors.Is(err, errNotFound):
		r = cachemesh.Success
	case errors.Is(err, errOverload):
		r = cachemesh.Overload
	case errors.Is(err, context.DeadlineExceeded) || stalled:
		r = cachemesh.Timeout
	case ctx.Err() != nil:
		return
	}

	n.observe(p, r, took)
}

type peerResult struct {
	resp   *http.Response
	err    error
	peer   cachemesh.PeerRef
	cancel context.CancelFunc
	took   time.Duration
}

// sendChunk sends the chunk request to h.to and, after h.hedgeAfter, a
// hedged copy to h.hedgeTo. It returns the first successful response; the
// caller closes its body. Every outcome except a canceled loser is
// reported to Observe.
func (n *Node) sendChunk(ctx context.Context, stalled func() bool, req chunkReq, off int, h hop) (*http.Response, cachemesh.PeerRef, error) {
	results := make(chan peerResult, 2)
	launch := func(p cachemesh.PeerRef, hdr cachemesh.Header) {
		rctx, rcancel := context.WithCancel(ctx)

		go func() {
			t0 := time.Now()

			resp, err := n.doChunk(rctx, p, &hdr, "", req, off)
			results <- peerResult{resp: resp, err: err, peer: p, cancel: rcancel, took: time.Since(t0)}
		}()
	}

	launch(h.to, h.hdr)

	pending := 1
	hedged := false

	var hedgeC <-chan time.Time

	if !h.hedgeTo.IsZero() && h.hedgeAfter > 0 {
		t := time.NewTimer(h.hedgeAfter)
		defer t.Stop()

		hedgeC = t.C
	}

	var firstErr error

	for pending > 0 {
		select {
		case <-hedgeC:
			hedgeC = nil

			if !n.mesh.TryHedge(h.hdr) {
				n.hook(n.cfg.Hooks.OnHedge, "denied")
				continue
			}

			hdr := h.hedgeHdr
			hdr.Flags |= cachemesh.FlagHedge

			launch(h.hedgeTo, hdr)

			pending++
			hedged = true

			n.hook(n.cfg.Hooks.OnHedge, "sent")
		case r := <-results:
			pending--

			n.observeErr(ctx, r.peer, r.err, r.took, stalled())

			if r.err == nil {
				if hedged && r.peer.ID == h.hedgeTo.ID {
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

			if firstErr == nil || terminal(r.err) {
				firstErr = r.err
			}
		case <-ctx.Done():
			if pending > 0 {
				go drainLosers(results, pending)
			}

			if stalled() {
				n.observe(h.to, cachemesh.Timeout, n.cfg.tune.stallTimeout)
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
func (n *Node) chunkQuery(req chunkReq, off int, peek string) url.Values {
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

	return q
}

// doChunk sends one chunk request. A nil hdr sends a peek request of kind
// peek. A non-200 status maps to an error.
func (n *Node) doChunk(ctx context.Context, p cachemesh.PeerRef, hdr *cachemesh.Header, peek string, req chunkReq, off int) (*http.Response, error) {
	h := http.Header{}

	if hdr != nil {
		peek = ""

		h.Set(headerRoute, encodeHeader(*hdr))

		if req.requester != "" {
			h.Set(headerRequester, req.requester)
		}
	}

	return n.get(ctx, p, ChunkPath, n.chunkQuery(req, off, peek), h)
}

// get sends a data-plane GET to peer p and maps a non-200 status to an
// error.
func (n *Node) get(ctx context.Context, p cachemesh.PeerRef, path string, q url.Values, h http.Header) (*http.Response, error) {
	u := "http://" + n.cfg.DataAddr(p) + path + "?" + q.Encode()

	return send(ctx, n.client, http.MethodGet, u, nil, h, func(code int, _ string) error { return statusError(code, p) })
}

// send sends one request with client. A response other than 200 is drained
// and closed, and bad maps its status to the returned error.
func send(ctx context.Context, client *http.Client, method, u string, body io.Reader, h http.Header, bad func(code int, status string) error) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}

	for k, v := range h {
		req.Header[k] = v
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for reuse
	_ = resp.Body.Close()                                       //nolint:errcheck // best-effort close

	return nil, bad(resp.StatusCode, resp.Status)
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

// holdersPerChunk is how many recent requesters a member remembers per
// chunk.
const holdersPerChunk = 4

// holderMinAge is how long after its request a requester is assumed to
// hold the chunk: a member that asked moments ago is most likely still
// pulling it (often it is the very request being filled).
const holderMinAge = 20 * time.Second

// holderTTL bounds how long a requester is assumed to still hold a chunk
// in its content store (containerd may garbage collect it).
const holderTTL = 6 * time.Hour

// holders is a bounded directory of members that recently pulled a chunk
// for their own containerd, and so likely hold it in their content store.
// An owner that has the chunk neither in memory nor locally reads it from
// one of them before going to the origin.
type holders struct {
	mu   sync.Mutex
	m    map[chunkKey]*holderSet
	ring []chunkKey
	next int
	now  func() time.Time
	// minAge skips requesters recorded less than this long ago.
	minAge time.Duration
}

type holderSet struct {
	addrs [holdersPerChunk]string
	at    [holdersPerChunk]time.Time
	n     int
}

func newHolders(capacity int) *holders {
	return &holders{
		m:      make(map[chunkKey]*holderSet, capacity),
		ring:   make([]chunkKey, max(capacity, 1)),
		now:    time.Now,
		minAge: holderMinAge,
	}
}

// validHolder reports whether addr looks like a host:port worth dialing.
func validHolder(addr string) bool {
	if addr == "" || len(addr) > 255 {
		return false
	}

	_, port, err := net.SplitHostPort(addr)

	return err == nil && port != ""
}

// add records addr as a recent requester of k.
func (h *holders) add(k chunkKey, addr string) {
	if !validHolder(addr) {
		return
	}

	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	set := h.m[k]
	if set == nil {
		// Reuse the oldest slot of the ring, dropping its key.
		if old := h.ring[h.next]; old.digest != "" {
			delete(h.m, old)
		}

		h.ring[h.next] = k
		h.next = (h.next + 1) % len(h.ring)

		set = &holderSet{}
		h.m[k] = set
	}

	for i := range set.n {
		if set.addrs[i] == addr {
			set.at[i] = now
			return
		}
	}

	// Replace the oldest entry when full.
	i := set.n
	if i == holdersPerChunk {
		i = 0

		for j := 1; j < holdersPerChunk; j++ {
			if set.at[j].Before(set.at[i]) {
				i = j
			}
		}
	} else {
		set.n++
	}

	set.addrs[i], set.at[i] = addr, now
}

// list returns the requesters of k other than self recorded between
// holderTTL and minAge ago, most recent first.
func (h *holders) list(k chunkKey, self string) []string {
	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff, newest := now.Add(-holderTTL), now.Add(-h.minAge)

	set := h.m[k]
	if set == nil {
		return nil
	}

	type entry struct {
		addr string
		at   time.Time
	}

	var es []entry

	for i := range set.n {
		if set.addrs[i] != self && set.at[i].After(cutoff) && !set.at[i].After(newest) {
			es = append(es, entry{set.addrs[i], set.at[i]})
		}
	}

	// Insertion sort, newest first: at most holdersPerChunk entries.
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].at.After(es[j-1].at); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}

	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.addr
	}

	return out
}
