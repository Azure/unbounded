// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestFetchBlobStripesAcrossOwnersAndDedupesOrigin(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 6, verify: true})

	const cs = 64 << 10

	blob := randomBlob(1, 5*cs+cs/3) // last chunk partial
	d := c.origin.add(blob, "application/octet-stream")

	var wg sync.WaitGroup

	errs := make(chan error, len(c.nodes))

	for _, n := range c.nodes {
		wg.Add(1)

		go func() {
			defer wg.Done()

			started, total, _, body, err := fetch(n, d, ifaces.KindBlob, 0)

			switch {
			case err != nil:
				errs <- err
			case !started:
				errs <- errors.New(n.id + ": not started")
			case total != int64(len(blob)) || !bytes.Equal(body, blob):
				errs <- errors.New(n.id + ": wrong body")
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	for i := range int64(6) {
		if got := c.origin.pullCount(d, i*cs); got != 1 {
			t.Errorf("chunk %d pulled from origin %d times, want 1", i, got)
		}
	}

	// Chunks are owned by more than one member (striping).
	owners := map[string]bool{}

	for i := range int64(6) {
		key := chunkKey{digest: d.String(), kind: kindBlob, index: i}.routeKey(nil)
		dec := c.nodes[0].mesh.Route(c.nodes[0].node.blobKS, key)

		owners[dec.Owner.ID] = true
	}

	if len(owners) < 2 {
		t.Errorf("6 chunks owned by %d member(s), want striping", len(owners))
	}

	// A second fetch is served from the local cache: no new origin pulls.
	before := c.origin.totalPulls()

	if _, _, _, body, err := fetch(c.nodes[3], d, ifaces.KindBlob, 0); err != nil || !bytes.Equal(body, blob) {
		t.Fatalf("second fetch: %v", err)
	}

	if after := c.origin.totalPulls(); after != before {
		t.Fatalf("cached fetch pulled origin %d times", after-before)
	}
}

func TestFetchBlobRangeOffsetAndManifestType(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 3, verify: true})

	const cs = 64 << 10

	blob := randomBlob(2, 3*cs+17)
	d := c.origin.add(blob, "application/octet-stream")

	for _, off := range []int64{1, cs - 1, cs, 2*cs + 5, int64(len(blob)) - 1} {
		started, total, _, body, err := fetch(c.nodes[1], d, ifaces.KindBlob, off)
		if err != nil || !started || total != int64(len(blob)) || !bytes.Equal(body, blob[off:]) {
			t.Fatalf("offset %d: started=%v total=%d len=%d err=%v", off, started, total, len(body), err)
		}
	}

	// An offset at or past the end is declined so the default path
	// answers it.
	if started, _, _, _, _ := fetch(c.nodes[1], d, ifaces.KindBlob, int64(len(blob))); started {
		t.Fatal("offset at end should be declined")
	}

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	md := c.origin.add(manifest, "application/vnd.oci.image.manifest.v1+json")

	started, _, ctype, body, err := fetch(c.nodes[2], md, ifaces.KindManifest, 0)
	if err != nil || !started || !bytes.Equal(body, manifest) || ctype != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("manifest: started=%v ctype=%q err=%v", started, ctype, err)
	}

	empty := c.origin.add(nil, "application/octet-stream")

	started, total, _, body, err := fetch(c.nodes[0], empty, ifaces.KindBlob, 0)
	if err != nil || !started || total != 0 || len(body) != 0 {
		t.Fatalf("empty blob: started=%v total=%d err=%v", started, total, err)
	}
}

func TestFetchBlobRelaysThroughGrid(t *testing.T) {
	// 16 members in cells of 2 with at most 4 neighbors cannot all be
	// direct neighbors, so some routes relay through an intermediate cell.
	c := newCluster(t, clusterOpts{n: 16, cellSize: 2, maxNeighbors: 4})

	const cs = 64 << 10

	blob := randomBlob(3, 24*cs)
	d := c.origin.add(blob, "application/octet-stream")

	relayed := false

	for _, n := range c.nodes {
		for i := range int64(24) {
			key := chunkKey{digest: d.String(), kind: kindBlob, index: i}.routeKey(nil)
			if n.mesh.Route(n.node.blobKS, key).Kind == cachemesh.KindRelay {
				relayed = true
			}
		}
	}

	if !relayed {
		t.Fatal("no relay decisions; the grid is fully direct")
	}

	// Fills started by peer requests come from owners (origin) or from
	// relays (the next hop). More of them than chunks means relays filled
	// and cached chunks on the way.
	var peerFills atomic.Int32

	for _, n := range c.nodes {
		n.node.cfg.Hooks.OnLookup = func(requester, result string) {
			if requester == "peer" && result == "fill" {
				peerFills.Add(1)
			}
		}
	}

	var wg sync.WaitGroup

	errs := make(chan error, len(c.nodes))

	for _, n := range c.nodes {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _, _, body, err := fetch(n, d, ifaces.KindBlob, 0)
			if err == nil && !bytes.Equal(body, blob) {
				err = errors.New(n.id + ": wrong body")
			}

			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	// Each chunk comes from the origin once, except that a requester
	// whose health view finds a slow owner degraded may route to a backup,
	// which reads through from the owner but goes to the origin when the
	// owner has not started yet.
	for i := range int64(24) {
		if got := c.origin.pullCount(d, i*cs); got < 1 || got > 2 {
			t.Errorf("chunk %d pulled from origin %d times, want 1 (2 with a backup)", i, got)
		}
	}

	if got := peerFills.Load(); got <= 24 {
		t.Errorf("%d fills started by peers, want more than 24 (relay fills)", got)
	}
}

func TestFetchBlobDeclinesBeforeHeaders(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 2})

	// Unknown digest: the origin says not found, nothing is written.
	missing := c.origin.add([]byte("x"), "")

	c.origin.mu.Lock()
	delete(c.origin.blobs, missing.String())
	c.origin.mu.Unlock()

	called := false
	ref := ifaces.OriginRef{Registry: "reg", Repository: "library/app", Digest: missing}

	started, written, err := c.nodes[1].node.FetchBlob(context.Background(), &bytes.Buffer{}, ref, func(int64, string) { called = true })
	if started || written != 0 || err != nil || called {
		t.Fatalf("missing blob: started=%v written=%d err=%v called=%v", started, written, err, called)
	}

	// A member whose mesh is not running is not ready and declines.
	m, err := cachemesh.New(cachemesh.Config{ID: "lonely", Addr: "lonely", Transport: c.net.Transport("lonely")})
	if err != nil {
		t.Fatal(err)
	}

	n, err := New(Config{
		Mesh: m, Origin: c.origin, ChunkSize: 1 << 16, MaxFills: 1, MaxServes: 1,
		Lookahead: 1, ChunkTimeout: time.Second, StallTimeout: time.Second, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	defer n.Close()

	d := c.origin.add([]byte("hello"), "")
	if started, _, _ := n.FetchBlob(context.Background(), &bytes.Buffer{}, ifaces.OriginRef{Registry: "reg", Repository: "r", Digest: d}, func(int64, string) {}); started {
		t.Fatal("unready mesh should decline")
	}
}

func TestFetchBlobAbortsMidStream(t *testing.T) {
	const cs = 64 << 10

	c := newCluster(t, clusterOpts{n: 3, attempts: 2})

	blob := randomBlob(4, 4*cs)
	d := c.origin.add(blob, "")

	// Chunk 2 always fails after 100 bytes at its start offset; retries
	// resume at a later offset and succeed. So make every resume fail too
	// by failing the whole digest after chunk 1.
	c.origin.mu.Lock()
	delete(c.origin.blobs, d.String())
	c.origin.blobs[d.String()] = blob
	c.origin.failDigest = d.String()
	c.origin.failAt = 2 * cs
	c.origin.failAfter = 100
	c.origin.mu.Unlock()

	// Retries resume from offset 2*cs+100, which succeeds: the blob is
	// served complete despite the mid-chunk origin failure.
	started, _, _, body, err := fetch(c.nodes[0], d, ifaces.KindBlob, 0)
	if err != nil || !started || !bytes.Equal(body, blob) {
		t.Fatalf("resume within chunk: started=%v err=%v", started, err)
	}

	// Now a blob whose chunk 1 is gone at the origin: headers are out, so
	// the fetch must report a truncated stream.
	blob2 := randomBlob(5, 3*cs)
	d2 := c.origin.add(blob2, "")

	c.origin.mu.Lock()
	c.origin.failDigest = d2.String()
	c.origin.failAt = cs
	c.origin.failAfter = 0
	c.origin.mu.Unlock()

	// failAfter 0 fails every pull starting at cs; the resume offset never
	// advances, so every attempt fails.
	started, _, _, body, err = fetch(c.nodes[1], d2, ifaces.KindBlob, 0)
	if !started || err == nil {
		t.Fatalf("want started with error, got started=%v err=%v", started, err)
	}

	if len(body) >= len(blob2) {
		t.Fatalf("body should be truncated, got %d bytes", len(body))
	}
}

func TestFetchBlobVerifyFailureEvicts(t *testing.T) {
	const cs = 64 << 10

	c := newCluster(t, clusterOpts{n: 2, verify: true})

	blob := randomBlob(6, 2*cs+3)
	d := c.origin.add(blob, "")

	c.origin.mu.Lock()
	c.origin.corrupt = d.String()
	c.origin.mu.Unlock()

	started, _, _, body, err := fetch(c.nodes[0], d, ifaces.KindBlob, 0)
	if !started || !errors.Is(err, errVerify) {
		t.Fatalf("want verify failure, got started=%v err=%v", started, err)
	}

	if len(body) != len(blob)-1 {
		t.Fatalf("final byte must be held back: wrote %d of %d", len(body), len(blob))
	}

	for _, n := range c.nodes[:1] {
		for i := range int64(3) {
			if r := n.node.store.get(chunkKey{digest: d.String(), index: i}, 0); r != nil {
				r.close()
				t.Fatalf("%s still caches chunk %d of a bad blob", n.id, i)
			}
		}
	}
}

func TestFetchBlobRetriesAroundDeadOwner(t *testing.T) {
	const cs = 64 << 10

	c := newCluster(t, clusterOpts{n: 8, cellSize: 4, attempts: 4, stall: time.Second})

	blob := randomBlob(7, 8*cs)
	d := c.origin.add(blob, "")

	requester := c.nodes[0]

	// Find a chunk owned by another member and take that member's data
	// plane down while its control plane stays up.
	var victim *testNode

	for i := range int64(8) {
		key := chunkKey{digest: d.String(), kind: kindBlob, index: i}.routeKey(nil)

		dec := requester.mesh.Route(requester.node.blobKS, key)
		if dec.Owner.ID != requester.id && dec.Kind == cachemesh.KindOwner {
			victim = c.byID[dec.Owner.ID]
			break
		}
	}

	if victim == nil {
		t.Skip("no chunk with a remote owner")
	}

	victim.srv.CloseClientConnections()
	victim.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	})

	started, _, _, body, err := fetch(requester, d, ifaces.KindBlob, 0)
	if err != nil || !started || !bytes.Equal(body, blob) {
		t.Fatalf("fetch around dead owner: started=%v err=%v", started, err)
	}
}

func TestChunkHandlerShedsAndValidates(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 1, tweak: func(cfg *Config) { cfg.MaxServes = 1 }})
	n := c.nodes[0]

	d := c.origin.add(randomBlob(8, 1000), "")

	// Fill the serve slot.
	n.node.serves <- struct{}{}

	q := n.node.chunkQuery(chunkReq{registry: "reg", repo: "library/app", d: d, key: chunkKey{digest: d.String()}}, 0, "")

	rec := httptest.NewRecorder()
	n.node.handleChunk(rec, httptest.NewRequest(http.MethodGet, ChunkPath+"?"+q, nil))

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the serve cap: %d", rec.Code)
	}

	<-n.node.serves

	for name, query := range map[string]string{
		"bad digest":     "r=reg&n=library/app&d=sha256:zz&k=0&i=0&cs=65536",
		"size mismatch":  "r=reg&n=library/app&d=" + d.String() + "&k=0&i=0&cs=1024",
		"bad kind":       "r=reg&n=library/app&d=" + d.String() + "&k=7&i=0&cs=65536",
		"missing header": "r=reg&n=library/app&d=" + d.String() + "&k=0&i=0&cs=65536",
	} {
		bad := httptest.NewRecorder()
		n.node.handleChunk(bad, httptest.NewRequest(http.MethodGet, ChunkPath+"?"+query, nil))

		if bad.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, bad.Code)
		}
	}

	// Peek misses return 404 without a fill.
	rec = httptest.NewRecorder()
	n.node.handleChunk(rec, httptest.NewRequest(http.MethodGet, ChunkPath+"?"+n.node.chunkQuery(chunkReq{registry: "reg", repo: "library/app", d: d, key: chunkKey{digest: d.String()}}, 0, peekHandoff), nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("peek miss: %d", rec.Code)
	}
}

func TestResolveTagCachesAtOwnerAndExpires(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 4})

	manifest := []byte(`{"schemaVersion":2}`)
	d := c.origin.add(manifest, "application/vnd.oci.image.manifest.v1+json")

	c.origin.mu.Lock()
	c.origin.tags["library/app:v1"] = d.String()
	c.origin.mu.Unlock()

	for _, n := range c.nodes {
		body, ctype, got, err := n.node.ResolveTag(context.Background(), "reg", "library/app", "v1")
		if err != nil || !bytes.Equal(body, manifest) || got != d || ctype != "application/vnd.oci.image.manifest.v1+json" {
			t.Fatalf("%s: err=%v digest=%s", n.id, err, got)
		}
	}

	if hits := c.origin.tagHits.Load(); hits != 1 {
		t.Fatalf("origin resolved the tag %d times, want 1", hits)
	}

	// Expire every cached copy: the next lookup goes back to the origin.
	for _, n := range c.nodes {
		n.node.meta.mu.Lock()
		for _, e := range n.node.meta.m {
			e.stamp = cachemesh.Stamp{Expires: 1}
		}
		n.node.meta.mu.Unlock()
	}

	if _, _, _, err := c.nodes[2].node.ResolveTag(context.Background(), "reg", "library/app", "v1"); err != nil {
		t.Fatal(err)
	}

	if hits := c.origin.tagHits.Load(); hits != 2 {
		t.Fatalf("after expiry origin resolved %d times, want 2", hits)
	}

	if _, _, _, err := c.nodes[1].node.ResolveTag(context.Background(), "reg", "library/app", "nope"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing tag: %v", err)
	}

	if _, _, _, err := c.nodes[1].node.ResolveTag(context.Background(), "reg", "library/app", "bad:tag"); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestSendChunkHedgesSlowPeer(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 3, hedge: true})

	blob := randomBlob(9, 1000)
	d := c.origin.add(blob, "")

	n0, slow, fast := c.nodes[0], c.nodes[1], c.nodes[2]

	inner := slow.srv.Config.Handler
	slow.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The hedge may be relayed through the slow member when it owns
		// the chunk; only the original request is slow.
		if h, err := decodeHeader(r.Header.Get(headerRoute)); err == nil && h.Flags&cachemesh.FlagHedge != 0 {
			inner.ServeHTTP(w, r)
			return
		}

		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}

		inner.ServeHTTP(w, r)
	})

	var hedges sync.Map

	n0.node.cfg.Hooks.OnHedge = func(outcome string) { hedges.Store(outcome, true) }

	// Build hedge budget: the tracker allows hedges as a fraction of
	// routed requests.
	for i := range 20000 {
		n0.mesh.Route(n0.node.blobKS, []byte{byte(i), byte(i >> 8)})
	}

	refs := map[string]cachemesh.PeerRef{}
	for _, p := range n0.mesh.Neighbors() {
		refs[p.ID] = p
	}

	req := chunkReq{registry: "reg", repo: "library/app", d: d, key: chunkKey{digest: d.String()}}
	dec := n0.mesh.Route(n0.node.blobKS, req.key.routeKey(nil))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()

	resp, peer, err := n0.node.sendChunk(ctx, func() bool { return false }, req, 0,
		refs[slow.id], dec.Header, 20*time.Millisecond, refs[fast.id], dec.Header)
	if err != nil {
		t.Fatalf("sendChunk: %v", err)
	}

	_ = resp.Body.Close()

	if peer.ID != fast.id || time.Since(start) > 2*time.Second {
		t.Fatalf("winner %s after %v, want the hedge to %s", peer.ID, time.Since(start), fast.id)
	}

	if _, ok := hedges.Load("won"); !ok {
		t.Fatal("hedge not reported as won")
	}
}

// TestOwnerPeekStartsOneOriginFill covers a backup standing in for an owner:
// the owner, asked with peekOwner before it has the chunk, starts its own
// origin fill and serves it, so the chunk is read from the origin once. A
// member that is not the owner still answers 404 without touching the origin.
func TestOwnerPeekStartsOneOriginFill(t *testing.T) {
	const cs = 64 << 10

	c := newCluster(t, clusterOpts{n: 4})

	blob := randomBlob(21, 4*cs)
	d := c.origin.add(blob, "")
	req := chunkReq{registry: "reg", repo: "library/app", d: d, key: chunkKey{digest: d.String(), kind: kindBlob, index: 1}}
	key := req.key.routeKey(nil)

	dec := c.nodes[0].mesh.Route(c.nodes[0].node.blobKS, key)

	owner := c.byID[dec.Owner.ID]
	if owner == nil {
		t.Fatalf("no owner for chunk 1: %+v", dec)
	}

	var other *testNode

	for _, n := range c.nodes {
		if n != owner {
			other = n
			break
		}
	}

	peek := func(n *testNode) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		n.node.handleChunk(rec, httptest.NewRequest(http.MethodGet, ChunkPath+"?"+n.node.chunkQuery(req, 0, peekOwner), nil))

		return rec
	}

	// Not the owner: no fill, no origin read.
	if rec := peek(other); rec.Code != http.StatusNotFound {
		t.Fatalf("owner peek at a non-owner: status %d, want 404", rec.Code)
	}

	if got := c.origin.totalPulls(); got != 0 {
		t.Fatalf("non-owner peek read the origin %d times", got)
	}

	// A handoff peek at the owner still never starts a fill.
	rec := httptest.NewRecorder()
	owner.node.handleChunk(rec, httptest.NewRequest(http.MethodGet, ChunkPath+"?"+owner.node.chunkQuery(req, 0, peekHandoff), nil))

	if rec.Code != http.StatusNotFound || c.origin.totalPulls() != 0 {
		t.Fatalf("handoff peek at the owner: status %d, origin pulls %d", rec.Code, c.origin.totalPulls())
	}

	// The owner starts the origin fill and serves it.
	rec = peek(owner)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), blob[cs:2*cs]) {
		t.Fatalf("owner peek: status %d, %d bytes", rec.Code, rec.Body.Len())
	}

	if got := c.origin.pullCount(d, cs); got != 1 {
		t.Fatalf("origin pulls of chunk 1 = %d, want 1", got)
	}

	// The owner keeps the chunk on its owned list.
	if r := owner.node.store.get(req.key, 0); r == nil {
		t.Fatal("owner does not cache chunk 1")
	} else {
		owner.node.store.mu.Lock()
		owned := r.f.owned
		owner.node.store.mu.Unlock()
		r.close()

		if !owned {
			t.Fatal("owner's chunk 1 is not marked owned")
		}
	}

	// A later routed fetch of the whole blob reuses the owner's chunk.
	started, _, _, body, err := fetch(other, d, ifaces.KindBlob, 0)
	if err != nil || !started || !bytes.Equal(body, blob) {
		t.Fatalf("fetch: started=%v err=%v", started, err)
	}

	if got := c.origin.pullCount(d, cs); got != 1 {
		t.Fatalf("origin pulls of chunk 1 after fetch = %d, want 1", got)
	}
}
