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

// metaCacheBytes bounds the manifest bytes the metadata cache retains,
// outside the chunk memory budget.
const metaCacheBytes = 64 << 20

// metaCache holds resolved tags with singleflight per key. It keeps at most
// max entries and maxBytes of manifest bodies.
type metaCache struct {
	mu       sync.Mutex
	max      int
	maxBytes int
	bytes    int
	m        map[string]*metaEntry
	calls    map[string]*metaCall
}

func newMetaCache(maxEntries, maxBytes int) *metaCache {
	return &metaCache{max: maxEntries, maxBytes: maxBytes, m: make(map[string]*metaEntry), calls: make(map[string]*metaCall)}
}

// remove drops key. The caller holds mu.
func (c *metaCache) remove(key string) {
	if e := c.m[key]; e != nil {
		c.bytes -= len(e.body)
		delete(c.m, key)
	}
}

// put caches e under key, first dropping expired entries and then arbitrary
// ones until it fits. A body larger than the budget is not cached. The
// caller holds mu.
func (c *metaCache) put(key string, e *metaEntry, fresh func(cachemesh.Stamp) bool) {
	c.remove(key)

	if len(e.body) > c.maxBytes {
		return
	}

	if len(c.m) >= c.max || c.bytes+len(e.body) > c.maxBytes {
		for k, old := range c.m {
			if !fresh(old.stamp) {
				c.remove(k)
			}
		}
	}

	for k := range c.m {
		if len(c.m) < c.max && c.bytes+len(e.body) <= c.maxBytes {
			break
		}

		c.remove(k)
	}

	c.m[key] = e
	c.bytes += len(e.body)
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

		c.remove(key)
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
				c.put(key, call.e, func(s cachemesh.Stamp) bool { return n.mesh.Fresh(n.metaKS, s) })
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
	var e *metaEntry

	self := func(int, cachemesh.Decision) error {
		var err error

		e, err = n.metaFromOrigin(registry, repo, tag)
		if err == nil {
			n.hook(n.cfg.Hooks.OnMeta, "origin")
		}

		return err
	}

	peer := func(h hop) error {
		ctx, cancel := context.WithTimeout(n.ctx, min(n.cfg.tune.chunkTimeout, n.cfg.tune.stallTimeout))
		defer cancel()

		t0 := time.Now()

		var err error

		e, err = n.metaFromPeer(ctx, h.to, h.hdr, registry, repo, tag)
		n.observeErr(ctx, h.to, err, time.Since(t0), false)

		if err == nil {
			n.hook(n.cfg.Hooks.OnMeta, "peer")
		}

		return err
	}

	if err := n.route(decide, self, peer); err != nil {
		return nil, err
	}

	return e, nil
}

func (n *Node) metaFromOrigin(registry, repo, tag string) (*metaEntry, error) {
	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.tune.chunkTimeout)
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

func (n *Node) metaFromPeer(ctx context.Context, p cachemesh.PeerRef, hdr cachemesh.Header, registry, repo, tag string) (*metaEntry, error) {
	q := url.Values{}
	q.Set("r", registry)
	q.Set("n", repo)
	q.Set("t", tag)

	resp, err := n.get(ctx, p, MetaPath, q, http.Header{headerRoute: {encodeHeader(hdr)}})
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

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
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	q := r.URL.Query()
	registry, repo, tag := q.Get("r"), q.Get("n"), q.Get("t")

	if n.cfg.Tags == nil || registry == "" || !validTag(tag) || oci.ValidateRepositoryName(repo) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	_, decide, err := n.forward(r)

	switch {
	case errors.Is(err, cachemesh.ErrTTLExhausted):
		// Answer only from the local cache.
		decide = func() (cachemesh.Decision, error) { return cachemesh.Decision{}, errLoop }
	case err != nil:
		http.Error(w, "bad route header", http.StatusBadRequest)
		return
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
