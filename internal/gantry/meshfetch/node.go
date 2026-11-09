// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package meshfetch distributes image content between Gantry agents with the
// cachemesh routing library (internal/cachemesh).
//
// Blobs are striped into fixed-size chunks. Every chunk is a key in the
// immutable "gantry-blob" keyspace, so each chunk has one owner agent that
// pulls it from the origin registry with a bounded Range request. Other
// agents fetch chunks hop by hop through the cachemesh grid: each hop asks
// Mesh.Forward where the request goes next, coalesces concurrent requests for
// the same chunk into one upstream fetch, keeps the chunk in bounded memory
// backed by the local containerd content store, and streams bytes
// downstream as they arrive (cut-through). Tag resolutions live in a
// separate "gantry-meta" keyspace with a TTL.
//
// The cachemesh library only routes; this package owns the data-plane
// transport (HTTP on the transfer listener), the chunk store, coalescing,
// retries, hedging, and metrics. See designs/gantry-cachemesh.md.
package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

const noSlot = cachemesh.NoSlot

// Endpoint paths on the transfer listener.
const (
	ControlPath = "/cachemesh/control"
	ChunkPath   = "/cachemesh/chunk"
	MetaPath    = "/cachemesh/meta"
)

// Request and response headers of the chunk and metadata endpoints.
const (
	headerRoute       = "Gantry-Cachemesh-Route"
	headerRequester   = "Gantry-Cachemesh-Requester"
	headerBlobSize    = "Gantry-Blob-Size"
	headerStampExpire = "Gantry-Stamp-Expires"
	headerStampGen    = "Gantry-Stamp-Gen"
	headerDigest      = "Docker-Content-Digest"
)

// Origin is the bounded-range origin capability the chunk owner uses.
type Origin interface {
	PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error)
}

// TagOrigin resolves a tag at the origin. Optional.
type TagOrigin interface {
	ManifestByTag(ctx context.Context, registry, repo, tag string) ([]byte, string, digest.Digest, error)
}

// Hooks are optional metric callbacks. Every hook may be nil.
type Hooks struct {
	// OnLookup reports a chunk lookup by a local blob request ("local") or
	// a peer ("peer"): result is "hit" (complete in cache), "joined" (an
	// in-progress fill), "fill" (a new fill started), or "local" (a peek
	// served from the local content store).
	OnLookup func(requester, result string)
	// OnFill reports a finished fill attempt: source is "origin", "peer",
	// "prev_owner", "owner" (a backup reading through from the owner),
	// "local" (the local content store), or "holder" (a recent
	// requester's content store);
	// outcome is "ok" or "error".
	OnFill func(source, outcome string, bytes int64, d time.Duration)
	// OnPeerResult reports every peer request outcome as given to
	// Mesh.Observe ("success", "failure", "timeout", "overload") with the
	// time to response headers.
	OnPeerResult func(result string, d time.Duration)
	// OnHedge reports hedges: "sent", "won", or "denied".
	OnHedge func(outcome string)
	// OnServe reports a chunk response to a peer: outcome is "ok",
	// "error", "shed", "loop", "no_route", "ttl", "not_found", or
	// "bad_request"; bytes is the body bytes sent.
	OnServe func(outcome string, bytes int64)
	// OnBlob reports a local blob request: outcome is "served",
	// "declined", "aborted", or "verify_failed".
	OnBlob func(outcome string, bytes int64, d time.Duration)
	// OnMeta reports a tag lookup: "hit", "origin", "peer", "error".
	OnMeta func(outcome string)
}

// Config configures a Node.
type Config struct {
	// Mesh routes requests. Required. The caller runs it.
	Mesh *cachemesh.Mesh
	// Origin fills chunks this node owns. Required.
	Origin Origin
	// Tags resolves tags this node owns. Nil disables tag resolution.
	Tags TagOrigin

	// ChunkSize is the stripe size. Required; every member must agree.
	ChunkSize int64
	// Content is the local containerd content store, the only disk tier:
	// chunks behind a fill's window and chunks no longer in memory are
	// read from it. Nil serves peers from memory only.
	Content ContentSource
	// InflightBytes bounds pages that fills borrow beyond their window:
	// for a reader that lags behind a fill that peers read, and to keep
	// chunks that peers read in memory for later peers. When it is
	// exhausted the slowest reader the content store does not cover paces
	// the fill. Zero disables borrowing.
	InflightBytes int64
	// MaxFills caps chunk fills in progress. Zero uses 64.
	MaxFills int
	// Verify hashes every full blob FetchBlob serves.
	Verify bool

	// Client sends data-plane requests. Nil uses NewDataClient().
	Client *http.Client
	// DataAddr maps a peer to its data-plane host:port. Nil uses the
	// peer's control address (the same transfer listener).
	DataAddr func(cachemesh.PeerRef) string

	Logger *slog.Logger
	Hooks  Hooks

	// tune overrides internal limits in tests. Zero fields use defaults.
	tune tuning
}

// tuning holds internal limits. Production uses the defaults; tests shrink
// them.
type tuning struct {
	// pageSize is the unit of in-flight buffers, content store reads, and
	// read coalescing. It divides ChunkSize.
	pageSize int
	// fillPages is the number of in-flight pages each fill holds.
	fillPages int
	// lingerTTL drops a complete chunk held in borrowed pages once it is
	// unused for this long.
	lingerTTL time.Duration
	// maxServes caps concurrent chunk responses to peers.
	maxServes int
	// lookahead is the number of chunks of one blob in flight ahead of
	// the one being written.
	lookahead int
	// chunkTimeout bounds one fill attempt.
	chunkTimeout time.Duration
	// stallTimeout fails a fill attempt without progress for this long.
	stallTimeout time.Duration
	// attempts bounds fill attempts per chunk.
	attempts int
	// noHedge disables hedged chunk requests.
	noHedge bool
}

// Defaults of the internal limits.
const (
	defaultPageSize     = 1 << 20
	defaultFillPages    = 4
	defaultMaxFills     = 64
	defaultMaxServes    = 512
	defaultLookahead    = 8
	defaultChunkTimeout = 5 * time.Minute
	defaultStallTimeout = 30 * time.Second
	defaultAttempts     = 4
)

func (t *tuning) setDefaults(chunkSize int64) {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}

	def(&t.pageSize, int(min(chunkSize, defaultPageSize)))
	def(&t.fillPages, defaultFillPages)
	def(&t.maxServes, defaultMaxServes)
	def(&t.lookahead, defaultLookahead)
	def(&t.attempts, defaultAttempts)
	defDur(&t.lingerTTL, defaultLingerTTL)
	defDur(&t.chunkTimeout, defaultChunkTimeout)
	defDur(&t.stallTimeout, defaultStallTimeout)
}

// Node serves and fetches chunks and metadata for one agent.
type Node struct {
	cfg     Config
	mesh    *cachemesh.Mesh
	blobKS  cachemesh.Keyspace
	metaKS  cachemesh.Keyspace
	store   *store
	holders *holders
	meta    *metaCache
	client  *http.Client
	log     *slog.Logger
	serves  chan struct{}

	holderPeeks  atomic.Int64
	holderMisses atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
}

// New validates cfg and returns a Node.
func New(cfg Config) (*Node, error) {
	if cfg.Mesh == nil || cfg.Origin == nil {
		return nil, errors.New("meshfetch: Mesh and Origin are required")
	}

	if cfg.ChunkSize <= 0 || cfg.ChunkSize > 1<<30 {
		return nil, fmt.Errorf("meshfetch: bad chunk size %d", cfg.ChunkSize)
	}

	if cfg.InflightBytes < 0 {
		return nil, errors.New("meshfetch: negative in-flight budget")
	}

	if cfg.MaxFills <= 0 {
		cfg.MaxFills = defaultMaxFills
	}

	cfg.tune.setDefaults(cfg.ChunkSize)

	if cfg.ChunkSize%int64(cfg.tune.pageSize) != 0 {
		return nil, fmt.Errorf("meshfetch: page size %d must divide chunk size %d", cfg.tune.pageSize, cfg.ChunkSize)
	}

	metaKS, err := cachemesh.NewKeyspace("gantry-meta", cachemesh.MinTTL)
	if err != nil {
		return nil, err
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	if cfg.Client == nil {
		cfg.Client = NewDataClient()
	}

	if cfg.DataAddr == nil {
		cfg.DataAddr = func(p cachemesh.PeerRef) string { return p.Addr }
	}

	log := cfg.Logger.With(slog.String("subsystem", "meshfetch"))
	st := newStore(storeConfig{
		chunkSize:    cfg.ChunkSize,
		pageSize:     cfg.tune.pageSize,
		window:       cfg.tune.fillPages,
		maxFills:     cfg.MaxFills,
		extraBudget:  cfg.InflightBytes,
		lingerTTL:    cfg.tune.lingerTTL,
		content:      cfg.Content,
		stallTimeout: cfg.tune.stallTimeout,
		logger:       log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		cfg:     cfg,
		mesh:    cfg.Mesh,
		blobKS:  cachemesh.MustKeyspace("gantry-blob", 0),
		metaKS:  metaKS,
		store:   st,
		holders: newHolders(holderChunks),
		meta:    newMetaCache(4096),
		client:  cfg.Client,
		log:     log,
		serves:  make(chan struct{}, cfg.tune.maxServes),
		ctx:     ctx,
		cancel:  cancel,
	}

	return n, nil
}

// Close stops background fills and releases idle buffers.
func (n *Node) Close() {
	n.cancel()
	n.store.close()
}

// holderChunks bounds the chunks the holder directory remembers.
const holderChunks = 1 << 15

// Stats returns the chunk store state.
func (n *Node) Stats() Stats {
	st := n.store.stats()
	st.HolderPeeks = n.holderPeeks.Load()
	st.HolderMisses = n.holderMisses.Load()

	return st
}

// Register adds the chunk and metadata endpoints to mux.
func (n *Node) Register(mux *http.ServeMux) {
	mux.HandleFunc(ChunkPath, n.handleChunk)
	mux.HandleFunc(MetaPath, n.handleMeta)
}

// NewDataClient returns an HTTP/1.1 client tuned for bulk chunk transfer:
// one TCP connection per concurrent request avoids HTTP/2 flow-control
// windows capping per-stream throughput, and large buffers cut syscalls.
func NewDataClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

	return &http.Client{
		Transport: &http.Transport{
			Protocols:             protocols,
			DialContext:           dialer.DialContext,
			MaxIdleConns:          4096,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			ReadBufferSize:        256 << 10,
			WriteBufferSize:       64 << 10,
			ExpectContinueTimeout: 0,
		},
	}
}

func (n *Node) observe(p cachemesh.PeerRef, r cachemesh.Result, d time.Duration) {
	n.mesh.Observe(p, r, d)

	if h := n.cfg.Hooks.OnPeerResult; h != nil {
		h(resultName(r), d)
	}
}

func resultName(r cachemesh.Result) string {
	switch r {
	case cachemesh.Success:
		return "success"
	case cachemesh.Timeout:
		return "timeout"
	case cachemesh.Overload:
		return "overload"
	default:
		return "failure"
	}
}

func (n *Node) hook(f func(string), v string) {
	if f != nil {
		f(v)
	}
}

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
	if !allowMethod(w, r, http.MethodGet) {
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

	rd, status, outcome := n.chunkForPeer(r, q.Get("peek"), req, off)
	if rd == nil {
		n.serveHook(outcome, 0)
		http.Error(w, outcome, status)

		return
	}

	defer rd.close()

	ctx, cancel := context.WithTimeout(r.Context(), n.cfg.tune.chunkTimeout)
	defer cancel()

	total, ctype, chunkLen, err := rd.f.meta(ctx)
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

	written, err := rd.streamTo(r.Context(), w)
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
// request's peek parameter: "" for a routed request, peekLocal from a new
// owner reading through the previous owner or an owner reading through a
// recent requester, or peekOwner from a backup reading through the owner.
// On failure it returns a nil fill with the status and
// outcome to report.
func (n *Node) chunkForPeer(r *http.Request, peek string, req chunkReq, off int) (*reader, int, string) {
	if peek != "" {
		// Serve only complete chunks and chunks read from the origin
		// here: an in-progress fill from a peer may be waiting on the
		// very member that is peeking (the new owner).
		if rd := n.store.get(req.key, off); rd != nil {
			f := rd.f
			f.mu.Lock()
			ok := (f.done && f.err == nil) || (!f.done && f.fromOrigin)
			f.mu.Unlock()

			if ok {
				n.lookup("peer", "hit")
				return rd, 0, ""
			}

			rd.close()
		}

		if rd := n.localOnly(req, off); rd != nil {
			return rd, 0, ""
		}

		// A backup standing in for this member asks before going to the
		// origin. If this member really owns the chunk, start the origin
		// fill here and let the backup share it, so the chunk is read from
		// the origin once. The fill never forwards (the owner's own
		// decision is KindSelf), so this cannot loop.
		if peek == peekOwner {
			if rd := n.ownerFill(r, req, off); rd != nil {
				return rd, 0, ""
			}
		}

		return nil, http.StatusNotFound, "not_found"
	}

	h, decide, err := n.forward(r)
	exhausted := errors.Is(err, cachemesh.ErrTTLExhausted)

	if a := r.Header.Get(headerRequester); (err == nil || exhausted) && validHolder(a) {
		req.requester = a
		n.holders.add(req.key, a)
	}

	switch {
	case exhausted:
		// The request may not travel further: serve only what is here.
		if rd := n.store.get(req.key, off); rd != nil {
			n.lookup("peer", "hit")
			return rd, 0, ""
		}

		if rd := n.localOnly(req, off); rd != nil {
			return rd, 0, ""
		}

		return nil, http.StatusLoopDetected, "ttl"
	case err != nil:
		return nil, http.StatusBadRequest, "bad_request"
	}

	rd, err := n.obtain(r.Context(), req, off, decide, "peer", h.Sender)
	if err != nil {
		status, outcome := errStatus(err)
		return nil, status, outcome
	}

	return rd, 0, ""
}

// localOnly returns a reader of the chunk read from the local content
// store when it holds the whole chunk, else nil. The fill is outside the
// store map, so it never joins an in-progress fill that may wait on the
// requester.
func (n *Node) localOnly(req chunkReq, off int) *reader {
	if _, _, ok := n.localChunk(req); !ok {
		return nil
	}

	f, r := n.store.newLocalFill(req.key, off)

	n.lookup("peer", "local")

	go func() {
		defer f.release()

		err := n.fillFromLocal(f, req)
		f.finish(err)
	}()

	return r
}

// ownerFill starts or joins the fill of req when this member is its owner
// in its own view, for a backup reading through the owner. It returns nil
// when this member is not the owner or the fill cannot start.
func (n *Node) ownerFill(r *http.Request, req chunkReq, off int) *reader {
	key := req.key.routeKey(make([]byte, 0, 96))
	decide := func() (cachemesh.Decision, error) {
		return n.mesh.Route(n.blobKS, key), nil
	}

	d, _ := decide() //nolint:errcheck // decide never fails
	if d.Kind != cachemesh.KindSelf || d.Owner.ID != n.mesh.ID() {
		return nil
	}

	rd, err := n.obtain(r.Context(), req, off, decide, "peer", noSlot)
	if err != nil {
		return nil
	}

	return rd
}

// forward decodes the route header of a peer request and returns it with
// the decider for the request's fills: the first decision is the one
// Mesh.Forward made here, later ones forward afresh. The error is
// cachemesh.ErrTTLExhausted when the request may not travel further.
func (n *Node) forward(r *http.Request) (cachemesh.Header, decider, error) {
	h, err := decodeHeader(r.Header.Get(headerRoute))
	if err != nil {
		return h, nil, err
	}

	d, err := n.mesh.Forward(h)
	if err != nil {
		return h, nil, err
	}

	first := true

	return h, func() (cachemesh.Decision, error) {
		if first {
			first = false
			return d, nil
		}

		return n.mesh.Forward(h)
	}, nil
}

// allowMethod answers 405 unless r uses method.
func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}

	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

	return false
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

// ControlTransport sends cachemesh control messages as HTTP POSTs to
// ControlPath on the member's address.
type ControlTransport struct {
	Client *http.Client
}

// NewControlTransport returns a ControlTransport with a small dedicated
// keep-alive pool. Send is bounded by the context the Mesh passes.
func NewControlTransport() ControlTransport {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}

	return ControlTransport{Client: &http.Client{Transport: &http.Transport{
		Protocols:           protocols,
		DialContext:         dialer.DialContext,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}}}
}

// Send implements cachemesh.ControlTransport.
func (t ControlTransport) Send(ctx context.Context, addr cachemesh.Endpoint, msg []byte) ([]byte, error) {
	h := http.Header{"Content-Type": {"application/octet-stream"}}

	resp, err := send(ctx, t.Client, http.MethodPost, "http://"+addr+ControlPath, bytes.NewReader(msg), h, func(_ int, status string) error {
		return fmt.Errorf("cachemesh control %s: %s", addr, status)
	})
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	return readBounded(resp.Body)
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, cachemesh.MaxControlMessageSize+1))
	if err == nil && len(b) > cachemesh.MaxControlMessageSize {
		err = cachemesh.ErrMessageTooLarge
	}

	return b, err
}

// ControlHandler serves cachemesh control messages for the Mesh in m. A nil
// Mesh, a Mesh that is not running, or an oversized message gets 503 or
// 400, which the sender treats as a transport failure.
func ControlHandler(m *atomic.Pointer[cachemesh.Mesh]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodPost) {
			return
		}

		mesh := m.Load()
		if mesh == nil {
			http.Error(w, "cachemesh not running", http.StatusServiceUnavailable)
			return
		}

		body, err := readBounded(http.MaxBytesReader(w, r.Body, cachemesh.MaxControlMessageSize+1))
		if err != nil {
			http.Error(w, "bad control message", http.StatusBadRequest)
			return
		}

		resp, err := mesh.ServeControl(r.Context(), r.RemoteAddr, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp) //nolint:errcheck // the sender times out on a broken write
	})
}
