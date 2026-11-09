// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// fastTuning shrinks cachemesh timers so a test cluster forms in seconds.
// It mirrors internal/cachemesh's own cluster tests.
func fastTuning() cachemesh.Tuning {
	return cachemesh.Tuning{
		PingInterval:   100 * time.Millisecond,
		PingTimeout:    100 * time.Millisecond,
		SuspectRefresh: 200 * time.Millisecond,
		JoinRetry:      100 * time.Millisecond,
		CallTimeout:    250 * time.Millisecond,
		NeighborLinger: 300 * time.Millisecond,
		LeaderTick:     50 * time.Millisecond,
		TickInterval:   50 * time.Millisecond,
		EpochAdvance:   20 * time.Millisecond,
		Election: cachemesh.ElectionConfig{
			LeaseDuration:      1500 * time.Millisecond,
			RenewDeadline:      1000 * time.Millisecond,
			RetryPeriod:        200 * time.Millisecond,
			NoLeaderTimeout:    2 * time.Second,
			LotteryRound:       100 * time.Millisecond,
			ClockSkewAllowance: 100 * time.Millisecond,
			MinCandidateAge:    300 * time.Millisecond,
		},
		Leader: cachemesh.LeaderTuning{
			QuarantineFor:   100 * time.Millisecond,
			WarmupStep:      100 * time.Millisecond,
			DrainFor:        300 * time.Millisecond,
			SuspicionWindow: 3 * time.Second,
		},
	}
}

// fakeOrigin serves blobs, manifests, and tags from memory and counts
// pulls per chunk start offset.
type fakeOrigin struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	ctypes  map[string]string
	tags    map[string]string // "repo:tag" -> digest
	pulls   map[string]int    // "digest@offset" -> count
	tagHits atomic.Int32
	// failAt, when set, makes pulls of digest return failAfter bytes and
	// then an error, for ranges starting at failAt.
	failDigest string
	failAt     int64
	failAfter  int
	corrupt    string
	delay      time.Duration
}

func newFakeOrigin() *fakeOrigin {
	return &fakeOrigin{
		blobs:  map[string][]byte{},
		ctypes: map[string]string{},
		tags:   map[string]string{},
		pulls:  map[string]int{},
	}
}

func (o *fakeOrigin) add(b []byte, ctype string) digest.Digest {
	sum := sha256.Sum256(b)
	d := digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))

	o.mu.Lock()
	o.blobs[d.String()] = b
	o.ctypes[d.String()] = ctype
	o.mu.Unlock()

	return d
}

func (o *fakeOrigin) pullCount(d digest.Digest, off int64) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.pulls[fmt.Sprintf("%s@%d", d, off)]
}

func (o *fakeOrigin) totalPulls() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	n := 0
	for _, v := range o.pulls {
		n += v
	}

	return n
}

type failingReader struct {
	r   io.Reader
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}

	if len(p) > f.n {
		p = p[:f.n]
	}

	k, err := f.r.Read(p)
	f.n -= k

	return k, err
}

func (o *fakeOrigin) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	o.mu.Lock()
	b, ok := o.blobs[ref.Digest.String()]
	ctype := o.ctypes[ref.Digest.String()]
	o.pulls[fmt.Sprintf("%s@%d", ref.Digest, ref.Offset)]++
	failing := o.failDigest == ref.Digest.String() && o.failAt == ref.Offset
	corrupt := o.corrupt == ref.Digest.String()
	delay := o.delay
	o.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, 0, "", ctx.Err()
		}
	}

	if !ok {
		return nil, 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: errors.New("no such blob")}
	}

	total := int64(len(b))
	if ref.Offset > total || (ref.Offset == total && total > 0) {
		return nil, 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: &ifaces.ErrRangeUnsupported{Offset: ref.Offset, Reason: "416"}}
	}

	end := min(ref.Offset+length, total)
	part := b[ref.Offset:end]

	if corrupt {
		part = bytes.Repeat([]byte{'x'}, len(part))
	}

	var r io.Reader = bytes.NewReader(part)
	if failing {
		r = &failingReader{r: r, n: o.failAfter, err: errors.New("origin reset")}
	}

	return io.NopCloser(r), total, ctype, nil
}

func (o *fakeOrigin) ManifestByTag(_ context.Context, _, repo, tag string) ([]byte, string, digest.Digest, error) {
	o.tagHits.Add(1)
	o.mu.Lock()
	ds, ok := o.tags[repo+":"+tag]
	b := o.blobs[ds]
	ct := o.ctypes[ds]
	o.mu.Unlock()

	if !ok {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Class: ifaces.FailureNotFound, Err: errors.New("no tag")}
	}

	return b, ct, digest.MustParse(ds), nil
}

type testNode struct {
	id     string
	mesh   *cachemesh.Mesh
	node   *Node
	srv    *httptest.Server
	cancel context.CancelFunc
	errc   chan error
}

type cluster struct {
	t      testing.TB
	net    *memtransport.Network
	lease  *memtransport.LeaseStore
	origin *fakeOrigin
	nodes  []*testNode
	byID   map[string]*testNode
	addrMu sync.RWMutex
	addrs  map[string]string
}

type clusterOpts struct {
	n            int
	chunkSize    int64
	cacheBytes   int64
	cellSize     int
	maxNeighbors int
	lookahead    int
	attempts     int
	hedge        bool
	verify       bool
	stall        time.Duration
	tweak        func(*Config)
}

func newCluster(t testing.TB, o clusterOpts) *cluster {
	t.Helper()

	if o.chunkSize == 0 {
		o.chunkSize = 64 << 10
	}

	if o.cacheBytes == 0 {
		o.cacheBytes = 256 * o.chunkSize
	}

	if o.lookahead == 0 {
		o.lookahead = 4
	}

	if o.attempts == 0 {
		o.attempts = 4
	}

	if o.stall == 0 {
		o.stall = 5 * time.Second
	}

	c := &cluster{
		t:      t,
		net:    memtransport.NewNetwork(),
		lease:  memtransport.NewLeaseStore(),
		origin: newFakeOrigin(),
		byID:   map[string]*testNode{},
		addrs:  map[string]string{},
	}

	logger := slog.New(slog.DiscardHandler)
	if os.Getenv("MESHFETCH_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	for i := range o.n {
		id := fmt.Sprintf("node-%02d", i)

		var seeds []string
		if i > 0 {
			seeds = []string{c.nodes[0].id}
		}

		tuning := fastTuning()

		m, err := cachemesh.New(cachemesh.Config{
			ID:            id,
			Addr:          id,
			Transport:     c.net.Transport(id),
			Seeds:         seeds,
			Lease:         c.lease,
			ExpectedSize:  1,
			Logger:        logger,
			PartitionBits: 12,
			CellSize:      o.cellSize,
			MaxNeighbors:  o.maxNeighbors,
			EpochPeriod:   200 * time.Millisecond,
			Tuning:        tuning,
		})
		if err != nil {
			t.Fatal(err)
		}

		cfg := Config{
			Mesh:         m,
			Origin:       c.origin,
			Tags:         c.origin,
			ChunkSize:    o.chunkSize,
			CacheBytes:   o.cacheBytes,
			MaxFills:     1024,
			MaxServes:    1024,
			Lookahead:    o.lookahead,
			ChunkTimeout: 20 * time.Second,
			StallTimeout: o.stall,
			Attempts:     o.attempts,
			Hedge:        o.hedge,
			Verify:       o.verify,
			MetadataTTL:  cachemesh.MinTTL,
			DataAddr:     c.dataAddr,
			Logger:       logger,
		}
		if o.tweak != nil {
			o.tweak(&cfg)
		}

		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}

		tn := &testNode{id: id, mesh: m, node: node, errc: make(chan error, 1)}

		mux := http.NewServeMux()
		node.Register(mux)

		tn.srv = httptest.NewServer(mux)

		c.addrMu.Lock()
		c.addrs[id] = strings.TrimPrefix(tn.srv.URL, "http://")
		c.addrMu.Unlock()

		c.net.Register(id, m.ServeControl)

		ctx, cancel := context.WithCancel(context.Background())
		tn.cancel = cancel

		go func() { tn.errc <- m.Run(ctx) }()

		c.nodes = append(c.nodes, tn)
		c.byID[id] = tn

		if i == 0 {
			c.waitFor("bootstrap", func() bool { return m.Leading() && m.Members() == 1 })
		}
	}

	t.Cleanup(c.stop)

	c.waitFor("cluster ready", func() bool {
		for _, n := range c.nodes {
			if !n.mesh.Ready() || n.mesh.Members() != o.n {
				return false
			}
		}

		return true
	})

	// Let placement settle: the leader moves partitions in steps after
	// members join, so wait out the warmup and then for every member to
	// reach the same view.
	time.Sleep(1500 * time.Millisecond)
	c.waitFor("views converge", func() bool {
		v := c.nodes[0].mesh.Version()
		for _, n := range c.nodes[1:] {
			if n.mesh.Version() != v {
				return false
			}
		}

		return true
	})

	return c
}

func (c *cluster) dataAddr(p cachemesh.PeerRef) string {
	c.addrMu.RLock()
	defer c.addrMu.RUnlock()

	if a, ok := c.addrs[p.ID]; ok {
		return a
	}

	return c.addrs[p.Addr]
}

func (c *cluster) stop() {
	for _, n := range c.nodes {
		n.cancel()
	}

	for _, n := range c.nodes {
		select {
		case <-n.errc:
		case <-time.After(5 * time.Second):
		}

		n.srv.Close()
		n.node.Close()
	}
}

func (c *cluster) waitFor(what string, f func() bool) {
	c.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func randomBlob(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// fetch runs FetchBlob on node n into a buffer.
func fetch(n *testNode, d digest.Digest, kind ifaces.OriginRefKind, off int64) (started bool, total int64, ctype string, body []byte, err error) {
	var buf bytes.Buffer

	ref := ifaces.OriginRef{Registry: "reg", Repository: "library/app", Digest: d, Kind: kind, Offset: off}
	started, written, err := n.node.FetchBlob(context.Background(), &buf, ref, func(t int64, ct string) {
		total, ctype = t, ct
	})

	if written != int64(buf.Len()) {
		err = errors.Join(err, fmt.Errorf("written %d, buffered %d", written, buf.Len()))
	}

	return started, total, ctype, buf.Bytes(), err
}
