// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// parseChunkQuery decodes a chunk request.
func (n *Node) parseChunkQuery(q url.Values) (chunkReq, int, error) {
	d, err := digest.Parse(q.Get("d"))
	if err != nil {
		return chunkReq{}, 0, err
	}

	repo := q.Get("n")
	if err := oci.ValidateRepositoryName(repo); err != nil {
		return chunkReq{}, 0, err
	}

	registry := q.Get("r")
	if registry == "" {
		return chunkReq{}, 0, errors.New("missing registry")
	}

	kind, err := strconv.Atoi(q.Get("k"))
	if err != nil || (kind != int(kindBlob) && kind != int(kindManifest)) {
		return chunkReq{}, 0, errors.New("bad kind")
	}

	idx, err := strconv.ParseInt(q.Get("i"), 10, 64)
	if err != nil || idx < 0 || idx > (1<<62)/n.cfg.ChunkSize {
		return chunkReq{}, 0, errors.New("bad chunk index")
	}

	cs, err := strconv.ParseInt(q.Get("cs"), 10, 64)
	if err != nil || cs != n.cfg.ChunkSize {
		return chunkReq{}, 0, errors.New("chunk size mismatch")
	}

	off := 0

	if v := q.Get("o"); v != "" {
		o, err := strconv.ParseInt(v, 10, 64)
		if err != nil || o < 0 || o >= cs {
			return chunkReq{}, 0, errors.New("bad offset")
		}

		off = int(o)
	}

	return chunkReq{
		registry: registry,
		repo:     repo,
		d:        d,
		key:      chunkKey{digest: d.String(), kind: uint8(kind), index: idx},
	}, off, nil
}

func (n *Node) serveHook(outcome string, bytes int64) {
	if h := n.cfg.Hooks.OnServe; h != nil {
		h(outcome, bytes)
	}
}

// handleChunk serves GET ChunkPath: one chunk (from offset o) to a peer,
// from the local cache, an in-progress fill, or a new fill routed by
// Mesh.Forward.
func (n *Node) handleChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	q := r.URL.Query()

	req, off, err := n.parseChunkQuery(q)
	if err != nil {
		n.serveHook("bad_request", 0)
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	select {
	case n.serves <- struct{}{}:
		defer func() { <-n.serves }()
	default:
		n.shed(w)
		return
	}

	f, status, outcome := n.chunkForPeer(r, q.Get("peek"), req)
	if f == nil {
		n.serveHook(outcome, 0)
		http.Error(w, outcome, status)

		return
	}

	defer f.release()

	ctx, cancel := context.WithTimeout(r.Context(), n.cfg.ChunkTimeout)
	defer cancel()

	total, ctype, chunkLen, err := f.meta(ctx)
	if err != nil {
		status, outcome := errStatus(err)
		n.serveHook(outcome, 0)
		http.Error(w, outcome, status)

		return
	}

	if off > chunkLen || (off == chunkLen && chunkLen > 0) {
		n.serveHook("bad_request", 0)
		http.Error(w, "offset beyond chunk", http.StatusRequestedRangeNotSatisfiable)

		return
	}

	h := w.Header()
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}

	h.Set(headerBlobSize, strconv.FormatInt(total, 10))
	h.Set("Content-Length", strconv.Itoa(chunkLen-off))
	w.WriteHeader(http.StatusOK)

	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	written, err := f.streamTo(r.Context(), w, off)
	if err != nil {
		n.serveHook("error", written)
		// The status line is out: abort so the peer sees a truncated
		// body, never a short one that looks complete.
		panic(http.ErrAbortHandler)
	}

	n.serveHook("ok", written)
}

// shed answers 429 so the sender reports Overload and tries elsewhere.
func (n *Node) shed(w http.ResponseWriter) {
	n.serveHook("shed", 0)
	w.Header().Set("Retry-After", "1")
	http.Error(w, "busy", http.StatusTooManyRequests)
}

// chunkForPeer finds or starts the fill for a peer request. peek is the
// request's peek parameter: "" for a routed request, peekHandoff from a new
// owner reading through the previous owner, or peekOwner from a backup
// reading through the owner. On failure it returns a nil fill with the status
// and outcome to report.
func (n *Node) chunkForPeer(r *http.Request, peek string, req chunkReq) (*fill, int, string) {
	if peek != "" {
		// Serve only complete chunks and chunks read from the origin
		// here: an in-progress fill from a peer may be waiting on the
		// very member that is peeking (the new owner).
		if f := n.store.get(req.key); f != nil {
			f.mu.Lock()
			ok := (f.done && f.err == nil) || (!f.done && f.fromOrigin)
			f.mu.Unlock()

			if ok {
				n.lookup("peer", "hit")
				return f, 0, ""
			}

			f.release()

			return nil, http.StatusNotFound, "not_found"
		}

		// A backup standing in for this member asks before going to the
		// origin. If this member really owns the chunk, start the origin
		// fill here and let the backup share it, so the chunk is read from
		// the origin once. The fill never forwards (the owner's own
		// decision is KindSelf), so this cannot loop.
		if peek == peekOwner {
			if f := n.ownerFill(r, req); f != nil {
				return f, 0, ""
			}
		}

		return nil, http.StatusNotFound, "not_found"
	}

	h, err := decodeHeader(r.Header.Get(headerRoute))
	if err != nil {
		return nil, http.StatusBadRequest, "bad_request"
	}

	d, err := n.mesh.Forward(h)

	switch {
	case errors.Is(err, cachemesh.ErrTTLExhausted):
		// The request may not travel further: serve only what is here.
		if f := n.store.get(req.key); f != nil {
			n.lookup("peer", "hit")
			return f, 0, ""
		}

		return nil, http.StatusLoopDetected, "ttl"
	case err != nil:
		return nil, http.StatusBadRequest, "bad_request"
	}

	first := true
	decide := func() (cachemesh.Decision, error) {
		if first {
			first = false
			return d, nil
		}

		return n.mesh.Forward(h)
	}

	f, err := n.obtain(r.Context(), req, decide, "peer", h.Sender)
	if err != nil {
		status, outcome := errStatus(err)
		return nil, status, outcome
	}

	return f, 0, ""
}

// ownerFill starts or joins the fill of req when this member is its owner
// in its own view, for a backup reading through the owner. It returns nil
// when this member is not the owner or the fill cannot start.
func (n *Node) ownerFill(r *http.Request, req chunkReq) *fill {
	key := req.key.routeKey(make([]byte, 0, 96))
	decide := func() (cachemesh.Decision, error) {
		return n.mesh.Route(n.blobKS, key), nil
	}

	d, _ := decide() //nolint:errcheck // decide never fails
	if d.Kind != cachemesh.KindSelf || d.Owner.ID != n.mesh.ID() {
		return nil
	}

	f, err := n.obtain(r.Context(), req, decide, "peer", noSlot)
	if err != nil {
		return nil
	}

	return f
}

// errStatus maps a fill error to the status sent to a peer.
func errStatus(err error) (int, string) {
	switch {
	case errors.Is(err, errBusy), errors.Is(err, errOverload):
		return http.StatusTooManyRequests, "shed"
	case errors.Is(err, errLoop):
		return http.StatusLoopDetected, "loop"
	case errors.Is(err, errNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, errRange):
		return http.StatusRequestedRangeNotSatisfiable, "bad_request"
	case errors.Is(err, errNoRoute), errors.Is(err, errClosed):
		return http.StatusServiceUnavailable, "no_route"
	default:
		return http.StatusBadGateway, "error"
	}
}
