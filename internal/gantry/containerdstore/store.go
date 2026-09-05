// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package containerdstore adapts a containerd content store to the
// gantry ifaces.LocalContentStore contract so the rest of the agent
// can read from and write into containerd as the single local source
// of truth for image content.
//
// This package is the "containerd as source of truth" hop the plan
// introduces: instead of
// maintaining a parallel hostPath cache that drifts out of sync with
// what containerd actually has, every read goes through the live
// content store and every commit lands directly in the same store
// containerd shows to kubelet. Net effect: a successful Gantry pull
// is indistinguishable from a kubelet pull at rest.
//
// All operations apply the configured containerd namespace to ctx
// before delegating. A single Store instance is bound to exactly one
// namespace (typically "k8s.io" for kubelet-managed pods).
//
// Failure-mode discipline (per):
// - cerrdefs.ErrNotFound from the underlying store is downgraded
// to *ifaces.ErrNotFound so transfer-endpoint callers can
// distinguish "definitively missing" from "backend hiccup".
// - Any other underlying error surfaces verbatim so callers treat
// it as "containerd unavailable" and do not advertise stale
// positive availability via has_cached or DHT.Provide.
//
// The wrapped content.Store interface (not a concrete client) keeps
// the package unit-testable on darwin against an in-memory fake; the
// production wiring constructs Store with the real
// containerd.Client.ContentStore in cmd/gantry/main.go.
package containerdstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	cerrdefs "github.com/containerd/errdefs"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// DefaultNamespace is the containerd namespace kubelet places pod
// containers in when it uses containerd as its CRI runtime. Centralized
// here so multiple subsystems (cdsub, containerdstore, advertise) cannot
// drift on what namespace they bind to.
const DefaultNamespace = "k8s.io"

// DefaultRefPrefix is prepended to every ingest ref so concurrent
// gantry writes are namespaced apart from kubelet/containerd's own
// pulls (kubelet uses "default-" prefixed refs from its puller). The
// final ref is `<prefix><digest>` which is stable per-digest so a
// crashed/restarted ingest can be resumed by the same writer slot.
const DefaultRefPrefix = "gantry-"

// defaultDescIndexCap bounds the in-memory descriptor index so a
// pathological walk over a giant content store cannot pin unbounded
// memory. 4096 entries is enough for tens of thousands of digests
// when most entries are blobs (blobs do not need a stored media
// type - only manifests/indexes/configs do) and the random
// eviction below keeps the index bounded.
const defaultDescIndexCap = 4096

// Store wraps a containerd content.Store.
type Store struct {
	cs        content.Store
	namespace string
	refPrefix string

	// leases is optional. When nil, AttachLease and
	// CleanupExpiredLeases return ErrNoLeaseManager and ingest
	// proceeds without lease protection (suitable for tests / dev
	// wiring; production containerd-only wiring should always
	// provide a lease manager via WithLeaseManager).
	leases   LeaseManager
	leaseTTL time.Duration

	// descMu guards descIndex. The index is an advisory media-type
	// cache populated by cdsub walks (every walked descriptor carries
	// its MediaType) and by callers that have parsed a manifest body.
	// It is NOT proof of content presence - callers MUST still call
	// Has/Open before serving - per // "Descriptor index" ("advisory metadata only").
	descMu    sync.Mutex
	descIndex map[gdigest.Digest]string
	descCap   int

	// metrics fires hit/miss/unavailable/open-error counters from
	// Has/Open. Any callback may be nil - the zero value is the
	// "no metrics" path used by tests.
	metrics MetricsHooks
}

// MetricsHooks are optional callbacks invoked from Has/Open so the
// containerd-as-truth model is observable on the wire (// "gantry_containerd_*_total"). Any callback may be nil; nil callbacks
// are skipped. Hooks fire on every call, not just sampled ones, so
// keep them cheap (typical implementation: prometheus.Counter.Inc).
type MetricsHooks struct {
	// OnHit fires when Has returns (true, nil) or Open returns a reader.
	OnHit func()
	// OnMiss fires when Has returns (false, nil) or Open returns ErrNotFound.
	OnMiss func()
	// OnUnavailable fires when Has/Open/Descriptor/Inventory/Writer
	// return ErrUnavailable (containerd unreachable or sick).
	OnUnavailable func()
	// OnOpenError fires only from Open's non-ErrNotFound path - kept
	// separate so dashboards can tell "open returned anything else"
	// from generic backend unavailability.
	OnOpenError func()
}

// WithMetrics registers metric callbacks. The hooks struct is copied
// by value so subsequent mutations by the caller do not affect the
// Store.
func WithMetrics(h MetricsHooks) Option {
	return func(s *Store) { s.metrics = h }
}

// Option configures a Store at construction.
type Option func(*Store)

// WithNamespace overrides the containerd namespace ctx is bound to
// before delegating. Empty string is ignored.
func WithNamespace(ns string) Option {
	return func(s *Store) {
		if ns != "" {
			s.namespace = ns
		}
	}
}

// New builds a Store bound to cs and the supplied namespace. Panics on
// nil cs because constructing a Store without a backing content store
// is always a programming error, not a runtime one.
func New(cs content.Store, opts ...Option) *Store {
	if cs == nil {
		panic("containerdstore: nil content.Store")
	}

	s := &Store{
		cs:        cs,
		namespace: DefaultNamespace,
		refPrefix: DefaultRefPrefix,
		leaseTTL:  DefaultLeaseTTL,
		descIndex: make(map[gdigest.Digest]string),
		descCap:   defaultDescIndexCap,
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// withNS attaches the configured namespace to ctx. Centralized so
// every code path goes through the same wrapper and we cannot
// accidentally call the content store with an unscoped context (which
// containerd silently treats as namespace "default" - the wrong
// answer for kubelet-managed pods).
func (s *Store) withNS(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, s.namespace)
}

// Has reports whether d is present AND openable in the wrapped content
// store. We probe via ReaderAt - opening and immediately closing the
// handle - rather than the cheaper Info lookup because coord
// PullIntent uses Has to drive HasCached on the wire: a peer that
// answers HasCached=true must be able to serve the bytes through the
// transfer endpoint, and Info-only Has would have lied for digests
// whose content file is missing or unreadable on disk (rare after
// disk issues but possible - and the resulting peer 404 would burn
// a wasted dial round-trip on every requester). This matches the
// openability semantics already used by Inventory and the
// advertiser's Notify pre-check.
//
//	(true, nil) -> present and openable (transfer endpoint will serve it).
//	(false, nil) -> definitively absent (ErrNotFound from backend) OR
//	 present in Info but not openable (corrupt / partial /
//	 unreadable file). Either way callers should treat as miss.
//	(false, err) -> backend failure (containerd unreachable, namespace
//	 misconfigured, etc.). Callers MUST NOT treat the
//	 false return as "definitively absent" in this case;
//	 coord.computeLocalIntent surfaces this distinctly via
//	 OnPullIntentStorageUnavailable.
func (s *Store) Has(ctx context.Context, d gdigest.Digest) (bool, error) {
	ra, err := s.cs.ReaderAt(s.withNS(ctx), ocispec.Descriptor{Digest: godigest.Digest(d.String())})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}

		if errors.Is(err, cerrdefs.ErrNotFound) {
			if s.metrics.OnMiss != nil {
				s.metrics.OnMiss()
			}

			return false, nil
		}

		if s.metrics.OnUnavailable != nil {
			s.metrics.OnUnavailable()
		}

		return false, &ifaces.ErrUnavailable{Op: "ReaderAt", Cause: err}
	}

	_ = ra.Close() //nolint:errcheck // best-effort close

	if s.metrics.OnHit != nil {
		s.metrics.OnHit()
	}

	return true, nil
}

// Ping checks that the configured containerd namespace/content store is
// reachable without mutating metrics. ErrNotFound from the impossible probe
// digest means the content API is healthy enough for readiness purposes.
func (s *Store) Ping(ctx context.Context) error {
	probe := godigest.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

	_, err := s.cs.Info(s.withNS(ctx), probe)
	if err == nil || errors.Is(err, cerrdefs.ErrNotFound) {
		return nil
	}

	return &ifaces.ErrUnavailable{Op: "Info", Cause: err}
}

// Open returns a streaming reader for d. Returns *ifaces.ErrNotFound
// when the digest is not present so the transfer endpoint and
// ContentWriter callers can distinguish miss from error.
func (s *Store) Open(ctx context.Context, d gdigest.Digest) (io.ReadCloser, int64, error) {
	desc := ocispec.Descriptor{Digest: godigest.Digest(d.String())}

	ra, err := s.cs.ReaderAt(s.withNS(ctx), desc)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}

		if errors.Is(err, cerrdefs.ErrNotFound) {
			if s.metrics.OnMiss != nil {
				s.metrics.OnMiss()
			}

			return nil, 0, &ifaces.ErrNotFound{Digest: d}
		}

		if s.metrics.OnUnavailable != nil {
			s.metrics.OnUnavailable()
		}

		if s.metrics.OnOpenError != nil {
			s.metrics.OnOpenError()
		}

		return nil, 0, &ifaces.ErrUnavailable{Op: "ReaderAt", Cause: err}
	}

	if s.metrics.OnHit != nil {
		s.metrics.OnHit()
	}

	size := ra.Size()

	return &readerAtCloser{
		SectionReader: io.NewSectionReader(ra, 0, size),
		closer:        ra,
	}, size, nil
}

// Descriptor returns the OCI descriptor (mediatype unknown; size from
// the store) for d. Used by callers that need to construct an
// ocispec.Descriptor without opening a reader handle. Returns
// *ifaces.ErrNotFound for missing digests.
func (s *Store) Descriptor(ctx context.Context, d gdigest.Digest) (ocispec.Descriptor, error) {
	info, err := s.cs.Info(s.withNS(ctx), godigest.Digest(d.String()))
	if err != nil {
		if errors.Is(err, cerrdefs.ErrNotFound) {
			return ocispec.Descriptor{}, &ifaces.ErrNotFound{Digest: d}
		}

		return ocispec.Descriptor{}, &ifaces.ErrUnavailable{Op: "Info", Cause: err}
	}

	mt := s.lookupMediaType(d)

	return ocispec.Descriptor{Digest: info.Digest, Size: info.Size, MediaType: mt}, nil
}

// Writer returns a ContentWriter that streams bytes into containerd's
// content store. Commit verifies the streamed bytes against d (the
// content store enforces this on Writer.Commit; mismatch surfaces as
// a non-nil error). Abort cancels the in-progress ingest.
//
// The ref is `<refPrefix><digest>` so concurrent calls for the same
// digest resume the same ingest slot rather than racing for distinct
// staging areas; this is the standard containerd pattern and matches
// what their own puller does.
//
// If the digest is already committed in the store, Writer returns
// ErrAlreadyExists wrapped - callers who want "treat-as-committed"
// semantics should call Has first.
func (s *Store) Writer(ctx context.Context, d gdigest.Digest) (ifaces.ContentWriter, error) {
	ref := s.refPrefix + d.String()
	expected := godigest.Digest(d.String())
	desc := ocispec.Descriptor{Digest: expected}

	w, err := s.cs.Writer(s.withNS(ctx),
		content.WithRef(ref),
		content.WithDescriptor(desc),
	)
	if err != nil {
		if errors.Is(err, cerrdefs.ErrAlreadyExists) {
			ok, hasErr := s.Has(ctx, d)
			if hasErr != nil {
				return nil, hasErr
			}

			if ok {
				return alreadyCommittedContentWriter{}, nil
			}
		}

		return nil, &ifaces.ErrUnavailable{Op: "Writer", Cause: err}
	}

	// If a previous crashed/failed pull left a partial ingest, the
	// writer may have a non-zero offset. Callers always stream from
	// byte 0, so appending to stale data would produce a corrupt
	// commit. Abort and re-acquire a clean writer.
	if st, stErr := w.Status(); stErr == nil && st.Offset > 0 {
		_ = w.Close() //nolint:errcheck // closing stale writer

		if abErr := s.cs.Abort(s.withNS(ctx), ref); abErr != nil && !errors.Is(abErr, cerrdefs.ErrNotFound) {
			return nil, &ifaces.ErrUnavailable{Op: "Writer(abort-stale)", Cause: abErr}
		}

		w, err = s.cs.Writer(s.withNS(ctx),
			content.WithRef(ref),
			content.WithDescriptor(desc),
		)
		if err != nil {
			return nil, &ifaces.ErrUnavailable{Op: "Writer(retry)", Cause: err}
		}
	}

	return &contentWriter{
		inner:    w,
		expected: expected,
		ref:      ref,
		store:    s,
	}, nil
}

// Inventory enumerates every sha256 digest currently present and
// openable in the content store. Used by the advertiser as the
// periodic reconciliation source of truth so DHT provider records
// reflect content this node can actually serve right now. Non-sha256
// digests are silently skipped because the rest of the agent only
// handles sha256 (per internal/digest). The ReaderAt probe is bounded
// to opening and closing the handle; it does not read content bytes.
func (s *Store) Inventory(ctx context.Context) ([]gdigest.Digest, error) {
	var (
		out  []gdigest.Digest
		seen = map[string]struct{}{}
	)

	nsCtx := s.withNS(ctx)

	walkErr := s.cs.Walk(nsCtx, func(info content.Info) error {
		ds := info.Digest.String()
		if _, ok := seen[ds]; ok {
			return nil
		}

		seen[ds] = struct{}{}

		d, parseErr := gdigest.Parse(ds)
		if parseErr != nil {
			return nil //nolint:nilerr // unsupported algorithm; skip
		}

		ra, openErr := s.cs.ReaderAt(nsCtx, ocispec.Descriptor{Digest: info.Digest, Size: info.Size})
		if openErr != nil {
			if errors.Is(openErr, cerrdefs.ErrNotFound) {
				return nil
			}

			return &ifaces.ErrUnavailable{Op: "ReaderAt", Cause: openErr}
		}

		_ = ra.Close() //nolint:errcheck // best-effort close

		out = append(out, d)

		return nil
	})
	if walkErr != nil {
		var unavailable *ifaces.ErrUnavailable
		if errors.As(walkErr, &unavailable) {
			return nil, walkErr
		}

		return nil, &ifaces.ErrUnavailable{Op: "Walk", Cause: walkErr}
	}

	return out, nil
}

type alreadyCommittedContentWriter struct{}

func (alreadyCommittedContentWriter) Write(p []byte) (int, error)  { return len(p), nil }
func (alreadyCommittedContentWriter) Commit(context.Context) error { return nil }
func (alreadyCommittedContentWriter) Abort(context.Context) error  { return nil }

// contentWriter adapts a containerd content.Writer to ifaces.ContentWriter.
type contentWriter struct {
	inner    content.Writer
	expected godigest.Digest
	ref      string
	store    *Store

	// committedOrAborted is set after the first successful Commit or
	// Abort so subsequent calls are no-ops (the iface contract says
	// Abort is idempotent).
	committedOrAborted bool
}

// Write streams bytes into the underlying content store. Errors are
// returned verbatim so the caller can distinguish backend hiccups
// from digest-mismatch (which only surfaces at Commit time).
func (w *contentWriter) Write(p []byte) (int, error) {
	return w.inner.Write(p)
}

// Commit finalizes the ingest. The wrapped containerd writer verifies
// the digest of accumulated bytes against w.expected and returns a
// non-nil error on mismatch - surfacing as a normal Commit failure to
// the caller. Already-committed entries (ErrAlreadyExists) are
// downgraded to nil because the iface contract semantically means
// "the digest is now in the store", which both fresh-commit and
// already-present satisfy.
func (w *contentWriter) Commit(ctx context.Context) error {
	if w.committedOrAborted {
		return nil
	}

	if err := w.inner.Commit(w.store.withNS(ctx), 0, w.expected); err != nil {
		if errors.Is(err, cerrdefs.ErrAlreadyExists) {
			w.committedOrAborted = true

			return nil
		}

		return fmt.Errorf("containerdstore: Commit: %w", err)
	}

	w.committedOrAborted = true

	return nil
}

// Abort cancels the ingest. Idempotent. Close + IngestManager.Abort
// is the documented containerd cancellation sequence.
func (w *contentWriter) Abort(ctx context.Context) error {
	if w.committedOrAborted {
		return nil
	}

	w.committedOrAborted = true
	// Close releases any buffered state. We tolerate the error: the
	// authoritative cancellation is IngestManager.Abort below.
	_ = w.inner.Close() //nolint:errcheck // best-effort
	if err := w.store.cs.Abort(w.store.withNS(ctx), w.ref); err != nil {
		if errors.Is(err, cerrdefs.ErrNotFound) {
			return nil
		}

		return fmt.Errorf("containerdstore: Abort: %w", err)
	}

	return nil
}

// readerAtCloser bundles a SectionReader (Read+Seek) with the
// underlying content.ReaderAt's Close so the transfer endpoint can
// type-assert io.ReadSeeker for Range serving while still releasing
// the content-store handle.
type readerAtCloser struct {
	*io.SectionReader
	closer io.Closer
}

func (r *readerAtCloser) Close() error { return r.closer.Close() }

// RememberMediaType records mediaType for d in the advisory descriptor
// index so a later Descriptor call can return the correct
// MediaType field without re-parsing manifest JSON. Empty mediaType
// values are ignored. Per "Descriptor index" the index is
// populated from:
//
// 1. containerd image target descriptors on startup/reconcile,
// 2. walked manifest/index child descriptors (cdsub.walk),
// 3. response headers observed during live stream-through,
// 4. JSON parse fallback (callers may compute and Remember).
//
// The index is bounded; when the cap is reached the oldest entry
// (random one - we do not track LRU recency) is evicted. Callers
// MUST treat a missing entry as "unknown media type", not "absent".
func (s *Store) RememberMediaType(d gdigest.Digest, mediaType string) {
	if mediaType == "" {
		return
	}

	s.descMu.Lock()
	defer s.descMu.Unlock()

	if existing, ok := s.descIndex[d]; ok && existing == mediaType {
		return
	}

	if len(s.descIndex) >= s.descCap {
		// Random eviction is sufficient: the index is advisory and
		// the cdsub reconcile loop will refresh it. Deterministic LRU
		// would buy us nothing here and would cost an extra
		// container-list-element per entry.
		for k := range s.descIndex {
			delete(s.descIndex, k)
			break
		}
	}

	s.descIndex[d] = mediaType
}

// LookupMediaType returns the advisory descriptor-index media type for d.
// It satisfies transfer.Describer so peer manifest responses can carry the
// exact OCI/Docker media type when cdsub has walked the descriptor graph.
func (s *Store) LookupMediaType(d gdigest.Digest) string {
	return s.lookupMediaType(d)
}

// lookupMediaType returns the cached media type for d, or "" if no
// entry exists. Used by Descriptor to populate the OCI descriptor's
// MediaType field opportunistically.
func (s *Store) lookupMediaType(d gdigest.Digest) string {
	s.descMu.Lock()
	defer s.descMu.Unlock()

	return s.descIndex[d]
}

// Compile-time interface checks.
var (
	_ ifaces.LocalContentStore = (*Store)(nil)
	_ ifaces.ContentWriter     = (*contentWriter)(nil)
)
