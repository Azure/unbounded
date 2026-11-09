// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// maxManifestSize bounds a manifest resolved by tag.
const maxManifestSize = 4 << 20

// metaEntry is a resolved tag: the manifest bytes, their media type and
// digest, and the owner's stamp, which every copy keeps.
type metaEntry struct {
	body   []byte
	ctype  string
	digest digest.Digest
	stamp  cachemesh.Stamp
}

type metaCall struct {
	done chan struct{}
	e    *metaEntry
	err  error
}

// metaCache holds resolved tags with singleflight per key.
type metaCache struct {
	mu    sync.Mutex
	max   int
	m     map[string]*metaEntry
	calls map[string]*metaCall
}

func newMetaCache(maxEntries int) *metaCache {
	return &metaCache{max: maxEntries, m: make(map[string]*metaEntry), calls: make(map[string]*metaCall)}
}

func metaKey(registry, repo, tag string) string {
	return registry + "/" + repo + ":" + tag
}

func validTag(tag string) bool {
	return tag != "" && len(tag) <= 128 && !strings.ContainsAny(tag, "/:@")
}

// ResolveTag returns the manifest a tag points to, from the local metadata
// cache while its stamp is fresh, otherwise from the owner of the tag in the
// "gantry-meta" keyspace, which asks the origin at most once per TTL. It
// fails when the mesh is not ready, tag resolution is not configured, or no
// source answers; the caller then serves the request another way.
func (n *Node) ResolveTag(ctx context.Context, registry, repo, tag string) ([]byte, string, digest.Digest, error) {
	if !n.mesh.Ready() || n.cfg.Tags == nil {
		return nil, "", digest.Digest{}, errNoRoute
	}

	if !validTag(tag) || oci.ValidateRepositoryName(repo) != nil {
		return nil, "", digest.Digest{}, errNotFound
	}

	key := metaKey(registry, repo, tag)
	decide := func() (cachemesh.Decision, error) {
		return n.mesh.Route(n.metaKS, []byte(key)), nil
	}

	e, err := n.resolveMeta(ctx, registry, repo, tag, decide)
	if err != nil {
		n.hook(n.cfg.Hooks.OnMeta, "error")
		return nil, "", digest.Digest{}, err
	}

	return e.body, e.ctype, e.digest, nil
}

// resolveMeta returns a fresh entry for the tag, joining or starting one
// resolution per key.
func (n *Node) resolveMeta(ctx context.Context, registry, repo, tag string, decide decider) (*metaEntry, error) {
	key := metaKey(registry, repo, tag)
	c := n.meta

	c.mu.Lock()

	if e := c.m[key]; e != nil {
		if n.mesh.Fresh(n.metaKS, e.stamp) {
			c.mu.Unlock()
			n.hook(n.cfg.Hooks.OnMeta, "hit")

			return e, nil
		}

		delete(c.m, key)
	}

	call := c.calls[key]
	if call == nil {
		call = &metaCall{done: make(chan struct{})}
		c.calls[key] = call

		go func() {
			call.e, call.err = n.fetchMeta(registry, repo, tag, decide)

			c.mu.Lock()
			delete(c.calls, key)

			if call.err == nil {
				if len(c.m) >= c.max {
					for k := range c.m {
						delete(c.m, k)
						break
					}
				}

				c.m[key] = call.e
			}
			c.mu.Unlock()

			close(call.done)
		}()
	}

	c.mu.Unlock()

	select {
	case <-call.done:
		return call.e, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchMeta resolves a tag at the owner: from the origin here, or through
// the next hop.
func (n *Node) fetchMeta(registry, repo, tag string, decide decider) (*metaEntry, error) {
	var (
		lastErr error
		alt     cachemesh.PeerRef
		altHdr  cachemesh.Header
	)

	for attempt := range n.cfg.Attempts {
		if attempt > 0 && alt.IsZero() {
			if err := sleepCtx(n.ctx, backoff(attempt)); err != nil {
				return nil, errClosed
			}
		}

		var (
			target cachemesh.PeerRef
			hdr    cachemesh.Header
		)

		if attempt > 0 && !alt.IsZero() {
			target, hdr = alt, altHdr
			alt = cachemesh.PeerRef{}
		} else {
			d, err := decide()
			if err != nil {
				return nil, err
			}

			switch d.Kind {
			case cachemesh.KindNoRoute:
				return nil, errNoRoute
			case cachemesh.KindSelf:
				e, err := n.metaFromOrigin(registry, repo, tag)
				if err == nil {
					n.hook(n.cfg.Hooks.OnMeta, "origin")
					return e, nil
				}

				if terminal(err) {
					return nil, err
				}

				lastErr = err

				continue
			}

			target, hdr = d.Next, d.Header
			if !d.Alternate.IsZero() {
				alt, altHdr = d.Alternate, d.AlternateHeader()
			}
		}

		t0 := time.Now()

		e, err := n.metaFromPeer(target, hdr, registry, repo, tag)

		switch {
		case err == nil:
			n.observe(target, cachemesh.Success, time.Since(t0))
			n.hook(n.cfg.Hooks.OnMeta, "peer")

			return e, nil
		case errors.Is(err, errOverload):
			n.observe(target, cachemesh.Overload, time.Since(t0))
		case errors.Is(err, errNotFound):
			n.observe(target, cachemesh.Success, time.Since(t0))
			return nil, err
		case errors.Is(err, context.DeadlineExceeded):
			n.observe(target, cachemesh.Timeout, time.Since(t0))
		default:
			n.observe(target, cachemesh.Failure, time.Since(t0))
		}

		lastErr = err
	}

	return nil, lastErr
}

func (n *Node) metaFromOrigin(registry, repo, tag string) (*metaEntry, error) {
	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.ChunkTimeout)
	defer cancel()

	// Stamp before asking the origin, so the value cannot outlive the TTL
	// measured from when it was read.
	stamp := n.mesh.Stamp(n.metaKS)

	body, ctype, d, err := n.cfg.Tags.ManifestByTag(ctx, registry, repo, tag)
	if err != nil {
		return nil, classifyOrigin(err)
	}

	return &metaEntry{body: body, ctype: ctype, digest: d, stamp: stamp}, nil
}

func (n *Node) metaFromPeer(p cachemesh.PeerRef, hdr cachemesh.Header, registry, repo, tag string) (*metaEntry, error) {
	ctx, cancel := context.WithTimeout(n.ctx, min(n.cfg.ChunkTimeout, n.cfg.StallTimeout))
	defer cancel()

	q := url.Values{}
	q.Set("r", registry)
	q.Set("n", repo)
	q.Set("t", tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.cfg.DataAddr(p)+MetaPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set(headerRoute, encodeHeader(hdr))

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, p)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, err
	}

	if len(body) > maxManifestSize {
		return nil, errors.New("meshfetch: manifest too large")
	}

	sum := sha256.Sum256(body)

	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}

	if got := resp.Header.Get(headerDigest); got != d.String() {
		return nil, fmt.Errorf("meshfetch: peer %s sent manifest %s for digest %q", p.ID, d, got)
	}

	expires, err1 := strconv.ParseUint(resp.Header.Get(headerStampExpire), 10, 64)
	gen, err2 := strconv.ParseUint(resp.Header.Get(headerStampGen), 10, 32)

	if err1 != nil || err2 != nil {
		return nil, errors.New("meshfetch: bad stamp headers")
	}

	return &metaEntry{
		body:   body,
		ctype:  resp.Header.Get("Content-Type"),
		digest: d,
		stamp:  cachemesh.Stamp{Expires: expires, Gen: uint32(gen)},
	}, nil
}

// handleMeta serves GET MetaPath: a tag resolution for a peer.
func (n *Node) handleMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	q := r.URL.Query()
	registry, repo, tag := q.Get("r"), q.Get("n"), q.Get("t")

	if n.cfg.Tags == nil || registry == "" || !validTag(tag) || oci.ValidateRepositoryName(repo) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	h, err := decodeHeader(r.Header.Get(headerRoute))
	if err != nil {
		http.Error(w, "bad route header", http.StatusBadRequest)
		return
	}

	d, err := n.mesh.Forward(h)

	var decide decider

	switch {
	case errors.Is(err, cachemesh.ErrTTLExhausted):
		// Answer only from the local cache.
		decide = func() (cachemesh.Decision, error) { return cachemesh.Decision{}, errLoop }
	case err != nil:
		http.Error(w, "bad route header", http.StatusBadRequest)
		return
	default:
		first := true
		decide = func() (cachemesh.Decision, error) {
			if first {
				first = false
				return d, nil
			}

			return n.mesh.Forward(h)
		}
	}

	e, err := n.resolveMeta(r.Context(), registry, repo, tag, decide)
	if err != nil {
		status, outcome := errStatus(err)
		http.Error(w, outcome, status)

		return
	}

	hd := w.Header()
	if e.ctype != "" {
		hd.Set("Content-Type", e.ctype)
	}

	hd.Set(headerDigest, e.digest.String())
	hd.Set(headerStampExpire, strconv.FormatUint(e.stamp.Expires, 10))
	hd.Set(headerStampGen, strconv.FormatUint(uint64(e.stamp.Gen), 10))
	hd.Set("Content-Length", strconv.Itoa(len(e.body)))
	_, _ = w.Write(e.body) //nolint:errcheck // the peer detects a short body
}
