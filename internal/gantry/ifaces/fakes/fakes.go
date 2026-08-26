// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package fakes provides in-memory implementations of the ifaces interfaces
// for unit and integration tests. They are intentionally simple and exposed
// at package scope so test code in any package can wire up a complete agent
// without touching libp2p, Kubernetes, or the filesystem.
package fakes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/errdefs"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// Cache is an in-memory ifaces.LocalContentStore. Safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	entries map[string][]byte
}

func NewCache() *Cache { return &Cache{entries: map[string][]byte{}} }

func (c *Cache) Has(_ context.Context, d digest.Digest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, ok := c.entries[d.String()]

	return ok, nil
}

func (c *Cache) Open(_ context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, ok := c.entries[d.String()]
	if !ok {
		return nil, 0, &ifaces.ErrNotFound{Digest: d}
	}

	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (c *Cache) Writer(_ context.Context, d digest.Digest) (ifaces.ContentWriter, error) {
	return &contentWriter{cache: c, want: d, h: sha256.New()}, nil
}

// Put injects a pre-verified entry directly. Intended for test setup.
func (c *Cache) Put(d digest.Digest, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[d.String()] = body
}

type contentWriter struct {
	cache *Cache
	want  digest.Digest
	h     interface {
		io.Writer
		Sum([]byte) []byte
	}
	buf  bytes.Buffer
	done bool
}

func (w *contentWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, errors.New("write after commit/abort")
	}

	_, _ = w.h.Write(p) //nolint:errcheck // best-effort write

	return w.buf.Write(p)
}

func (w *contentWriter) Commit(_ context.Context) error {
	if w.done {
		return errors.New("commit after commit/abort")
	}

	w.done = true

	got := hex.EncodeToString(w.h.Sum(nil))
	if got != w.want.Hex() {
		// Wrap errdefs.ErrFailedPrecondition so callers classify this the
		// same way they classify a real containerd content-store Commit
		// digest/size mismatch (which wraps the same sentinel), rather
		// than relying on the human-readable message text.
		return fmt.Errorf("digest mismatch: got sha256:%s, want %s: %w", got, w.want.String(), errdefs.ErrFailedPrecondition)
	}

	w.cache.mu.Lock()
	defer w.cache.mu.Unlock()

	w.cache.entries[w.want.String()] = append([]byte(nil), w.buf.Bytes()...)

	return nil
}

func (w *contentWriter) Abort(_ context.Context) error {
	w.done = true
	w.buf.Reset()

	return nil
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// Members is an ifaces.Members backed by a static slice.
type Members struct {
	self  ifaces.NodeID
	nodes []ifaces.Node
}

func NewMembers(self ifaces.NodeID, nodes ...ifaces.Node) *Members {
	return &Members{self: self, nodes: nodes}
}

func (m *Members) Self() ifaces.NodeID { return m.self }

func (m *Members) Snapshot() []ifaces.Node {
	out := make([]ifaces.Node, len(m.nodes))
	copy(out, m.nodes)

	return out
}

func (m *Members) WaitForSync(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// OriginPuller
// ---------------------------------------------------------------------------

// OriginPuller is an in-memory ifaces.OriginPuller. Entries seeded via Put
// are served verbatim; unset references return *ifaces.OriginError with
// FailureNotFound.
type OriginPuller struct {
	mu      sync.Mutex
	entries map[string][]byte
	// PullCount records pull attempts per digest for assertions.
	pullCount map[string]int
}

func NewOriginPuller() *OriginPuller {
	return &OriginPuller{
		entries:   map[string][]byte{},
		pullCount: map[string]int{},
	}
}

func (o *OriginPuller) Put(d digest.Digest, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.entries[d.String()] = body
}

func (o *OriginPuller) Pull(_ context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.pullCount[ref.Digest.String()]++

	b, ok := o.entries[ref.Digest.String()]
	if !ok {
		return nil, 0, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: errors.New("404")}
	}

	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// Head implements ifaces.OriginPuller. The fake returns the same size
// it would have served from Pull (so callers that HEAD-then-GET see a
// consistent Content-Length) without consuming a Pull slot. The fake
// returns an empty content-type - tests that care about the
// HEAD-time media type should use a custom origin double.
func (o *OriginPuller) Head(_ context.Context, ref ifaces.OriginRef) (int64, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	b, ok := o.entries[ref.Digest.String()]
	if !ok {
		return 0, "", &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: errors.New("404")}
	}

	return int64(len(b)), "", nil
}

// PullCount returns the number of Pull invocations seen for d.
func (o *OriginPuller) PullCount(d digest.Digest) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.pullCount[d.String()]
}

// ---------------------------------------------------------------------------
// PeerDialer
// ---------------------------------------------------------------------------

// PeerDialer routes FetchFromPeer to a per-address ifaces.LocalContentStore. Tests wire
// each "peer's" local cache into this map.
type PeerDialer struct {
	mu    sync.Mutex
	peers map[string]ifaces.LocalContentStore
}

func NewPeerDialer() *PeerDialer {
	return &PeerDialer{peers: map[string]ifaces.LocalContentStore{}}
}

func (p *PeerDialer) Register(addr string, cache ifaces.LocalContentStore) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.peers[addr] = cache
}

func (p *PeerDialer) FetchFromPeer(ctx context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	p.mu.Lock()
	cache, ok := p.peers[addr]
	p.mu.Unlock()

	if !ok {
		return nil, 0, fmt.Errorf("fakes: no peer registered at %q", addr)
	}

	rc, size, err := cache.Open(ctx, ref.Digest)
	if err != nil {
		return nil, 0, err
	}

	if ref.Offset <= 0 {
		return rc, size, nil
	}

	if ref.Offset >= size {
		_ = rc.Close() //nolint:errcheck // best-effort close
		return nil, 0, fmt.Errorf("fakes: peer offset %d outside content size %d", ref.Offset, size)
	}

	if _, err := io.CopyN(io.Discard, rc, ref.Offset); err != nil {
		_ = rc.Close() //nolint:errcheck // best-effort close
		return nil, 0, err
	}

	return rc, size, nil
}

// ---------------------------------------------------------------------------
// DHT
// ---------------------------------------------------------------------------

// DHT is an in-memory ifaces.DHT. Provides and FindProviders share a
// digest->providers map, and Health returns a configurable score.
type DHT struct {
	mu           sync.Mutex
	providers    map[string][]ifaces.Provider
	health       float64
	provideCall  map[string]int
	withdrawCall map[string]int
	findErr      error
}

func NewDHT() *DHT {
	return &DHT{
		providers:    map[string][]ifaces.Provider{},
		health:       1.0,
		provideCall:  map[string]int{},
		withdrawCall: map[string]int{},
	}
}

// SetFindProvidersError programs the next (and all subsequent)
// FindProviders calls to return err. Pass nil to clear. Useful for
// regression tests that exercise the DHT-error fallback path.
func (d *DHT) SetFindProvidersError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.findErr = err
}

func (d *DHT) Health() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.health
}

func (d *DHT) Provide(_ context.Context, dg digest.Digest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.provideCall[dg.String()]++

	return nil
}

// ProvideCount returns the number of times Provide was called for dg.
// Used by tests that assert the-step-7 re-advertise path fires.
func (d *DHT) ProvideCount(dg digest.Digest) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.provideCall[dg.String()]
}

// Withdraw implements ifaces.DHT. Tracks per-digest call counts so
// tests of the advertiser's eviction path can assert the hint fires
// exactly once per withdrawn digest.
func (d *DHT) Withdraw(_ context.Context, dg digest.Digest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.withdrawCall[dg.String()]++

	return nil
}

// WithdrawCount returns the number of times Withdraw was called for dg.
func (d *DHT) WithdrawCount(dg digest.Digest) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.withdrawCall[dg.String()]
}

// Inject seeds the provider list for a digest.
func (d *DHT) Inject(dg digest.Digest, providers ...ifaces.Provider) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.providers[dg.String()] = append([]ifaces.Provider(nil), providers...)
}

func (d *DHT) FindProviders(_ context.Context, dg digest.Digest) ([]ifaces.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.findErr != nil {
		return nil, d.findErr
	}

	src := d.providers[dg.String()]
	out := make([]ifaces.Provider, len(src))
	copy(out, src)

	return out, nil
}

// Compile-time assertions that the fakes implement the interfaces.
var (
	_ ifaces.LocalContentStore = (*Cache)(nil)
	_ ifaces.Members           = (*Members)(nil)
	_ ifaces.OriginPuller      = (*OriginPuller)(nil)
	_ ifaces.PeerDialer        = (*PeerDialer)(nil)
	_ ifaces.DHT               = (*DHT)(nil)
)
