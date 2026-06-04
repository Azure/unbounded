// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package app wires the Orca runtime: origin + cachestore + cluster +
// fetch coordinator + edge / internal HTTP listeners.
//
// Production callers (cmd/orca/orca/orca.go) drive this from a YAML
// config; integration tests (internal/orca/inttest) drive it from a
// programmatic *config.Config plus options that inject in-memory or
// counting decorators around the origin / cachestore.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	cachestores3 "github.com/Azure/unbounded/internal/orca/cachestore/s3"
	"github.com/Azure/unbounded/internal/orca/chunkcatalog"
	"github.com/Azure/unbounded/internal/orca/cluster"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/fetch"
	"github.com/Azure/unbounded/internal/orca/metadata"
	"github.com/Azure/unbounded/internal/orca/origin"
	"github.com/Azure/unbounded/internal/orca/origin/awss3"
	"github.com/Azure/unbounded/internal/orca/origin/azureblob"
	"github.com/Azure/unbounded/internal/orca/server"
)

// App is a running Orca instance.
//
// Construct with Start; tear down with Shutdown. Start is non-blocking:
// the returned App's listeners are accepting connections (via
// net.Listen) before Start returns, so EdgeAddr / InternalAddr / OpsAddr
// are resolved (including any :0 ports) by the time the caller sees them.
type App struct {
	// EdgeAddr is the resolved client-edge listen address (host:port).
	// When the config requested ":0" the port is the OS-assigned one.
	EdgeAddr string

	// InternalAddr is the resolved peer-RPC listen address (host:port).
	InternalAddr string

	// OpsAddr is the resolved /healthz + /readyz listen address.
	OpsAddr string

	// Cluster is exposed so tests can inspect peer state and call
	// Coordinator/Self for assertions. Production callers should treat
	// this as read-only.
	Cluster *cluster.Cluster

	log         *slog.Logger
	edgeSrv     *http.Server
	internalSrv *http.Server
	opsSrv      *http.Server
	wg          sync.WaitGroup
	errCh       chan error

	// cachestoreReady is set true once the cachestore self-test has
	// passed (or skipped via WithSkipCachestoreSelfTest). Gated by
	// the /readyz endpoint.
	cachestoreReady bool
}

type options struct {
	log                 *slog.Logger
	clusterOpt          cluster.Option
	origin              origin.Origin
	cacheStore          cachestore.CacheStore
	skipCacheSelfTest   bool
	internalHandlerWrap func(http.Handler) http.Handler
	edgeListener        net.Listener
	internalListener    net.Listener
	opsListener         net.Listener
}

// Option configures Start.
type Option func(*options)

// WithLogger overrides the slog.Logger used for the App's output. If
// not provided, a JSON handler writing to stdout at LevelInfo is used.
func WithLogger(log *slog.Logger) Option {
	return func(o *options) { o.log = log }
}

// WithPeerSource replaces the cluster's entire peer-discovery
// mechanism. Intended for integration tests that need full control
// (e.g. per-replica peer sets with explicit ports). Only one such
// override is meaningful per App; subsequent calls overwrite.
func WithPeerSource(s cluster.PeerSource) Option {
	return func(o *options) {
		o.clusterOpt = cluster.WithPeerSource(s)
	}
}

// WithOrigin replaces the origin driver constructed from cfg. Tests use
// this to wire counting / fault-injecting decorators around a real
// awss3 or azureblob client.
func WithOrigin(or origin.Origin) Option {
	return func(o *options) { o.origin = or }
}

// WithCacheStore replaces the cachestore driver constructed from cfg.
// Tests use this to wire a counting / fault-injecting decorator around
// a real s3 client (or to use an in-memory implementation).
func WithCacheStore(cs cachestore.CacheStore) Option {
	return func(o *options) { o.cacheStore = cs }
}

// WithSkipCachestoreSelfTest disables the boot-time cachestore
// self-test. Useful only in tests that wire a cachestore decorator
// already known to provide read-after-write visibility.
func WithSkipCachestoreSelfTest() Option {
	return func(o *options) { o.skipCacheSelfTest = true }
}

// WithInternalHandlerWrap installs a decorator around the internal
// peer-RPC handler. The wrap function receives the production handler
// and returns one that the http.Server actually serves. Production
// passes nothing -> identity. Tests use this to count 409 responses
// per source IP for the not-coordinator fallback assertion.
func WithInternalHandlerWrap(wrap func(http.Handler) http.Handler) Option {
	return func(o *options) { o.internalHandlerWrap = wrap }
}

// WithEdgeListener supplies a pre-bound listener for the client-edge
// HTTP server, bypassing app.Start's own net.Listen call.
//
// TEST-ONLY: production callers must not use this option. It is
// exposed for integration tests (internal/orca/inttest) that allocate
// the listener before the app starts so peer sets can advertise the
// captured port from t=0 without a close-and-rebind race. Using it in
// production silently disables the cfg.Server.Listen address.
func WithEdgeListener(ln net.Listener) Option {
	return func(o *options) { o.edgeListener = ln }
}

// WithInternalListener supplies a pre-bound listener for the peer-RPC
// internal HTTP server.
//
// TEST-ONLY: see WithEdgeListener.
func WithInternalListener(ln net.Listener) Option {
	return func(o *options) { o.internalListener = ln }
}

// WithOpsListener supplies a pre-bound listener for the ops HTTP
// server (/healthz, /readyz).
//
// TEST-ONLY: see WithEdgeListener.
func WithOpsListener(ln net.Listener) Option {
	return func(o *options) { o.opsListener = ln }
}

// Start wires every dependency and begins serving on the configured
// listeners. It returns once all listeners are accepting connections
// (or returns the error that prevented startup).
//
// The returned App must be Shutdown by the caller; Start does not own
// the parent context's lifetime.
//
// Ordering note: cluster.New is called before any listener is bound.
// Peers can therefore attempt internal-fill RPCs against this replica
// before its listener is accepting; those connects fail and the
// requester falls back to local fill via fetch.Coordinator.GetChunk's
// peer-fallback path. This is transient (sub-second between cluster
// construction and listener bind) and harmless.
func Start(ctx context.Context, cfg *config.Config, opts ...Option) (*App, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	log := o.log
	if log == nil {
		log = slog.Default()
	}

	or, err := buildOrigin(ctx, cfg, o.origin, log)
	if err != nil {
		return nil, err
	}

	cs, err := buildCacheStore(ctx, cfg, o.cacheStore, log)
	if err != nil {
		return nil, err
	}

	cachestoreReady := false

	if o.skipCacheSelfTest {
		// Caller has asserted the cachestore decorator provides
		// read-after-write visibility (the in-memory store used by
		// tests). Treat readiness as satisfied immediately.
		cachestoreReady = true
	} else {
		if err := cs.SelfTest(ctx); err != nil {
			return nil, fmt.Errorf("cachestore self-test failed: %w", err)
		}

		log.LogAttrs(ctx, slog.LevelInfo, "cachestore self-test passed")

		cachestoreReady = true
	}

	clusterOpts := []cluster.Option{cluster.WithLogger(log)}
	if o.clusterOpt != nil {
		clusterOpts = append(clusterOpts, o.clusterOpt)
	}

	cl, err := cluster.New(ctx, cfg.Cluster, clusterOpts...)
	if err != nil {
		return nil, fmt.Errorf("init cluster: %w", err)
	}

	cat := chunkcatalog.New(cfg.ChunkCatalog.MaxEntries, log)
	mc := metadata.NewCache(cfg.Metadata, log)
	fc := fetch.NewCoordinator(or, cs, cl, cat, mc, cfg, log)

	edgeHandler := server.NewEdgeHandler(fc, cfg, log)

	var internalHandler http.Handler = server.NewInternalHandler(fc, cl, log)
	if o.internalHandlerWrap != nil {
		internalHandler = o.internalHandlerWrap(internalHandler)
	}

	edgeLn := o.edgeListener
	if edgeLn == nil {
		ln, err := net.Listen("tcp", cfg.Server.Listen)
		if err != nil {
			cleanupStartFailure(cl, nil, nil)

			return nil, fmt.Errorf("edge listener bind %q: %w", cfg.Server.Listen, err)
		}

		edgeLn = ln
	}

	internalLn := o.internalListener
	if internalLn == nil {
		ln, err := net.Listen("tcp", cfg.Cluster.InternalListen)
		if err != nil {
			cleanupStartFailure(cl, edgeLn, nil)

			return nil, fmt.Errorf("internal listener bind %q: %w", cfg.Cluster.InternalListen, err)
		}

		internalLn = ln
	}

	opsLn := o.opsListener
	if opsLn == nil {
		ln, err := net.Listen("tcp", cfg.Server.OpsListen)
		if err != nil {
			cleanupStartFailure(cl, edgeLn, internalLn)

			return nil, fmt.Errorf("ops listener bind %q: %w", cfg.Server.OpsListen, err)
		}

		opsLn = ln
	}

	a := &App{
		EdgeAddr:     edgeLn.Addr().String(),
		InternalAddr: internalLn.Addr().String(),
		OpsAddr:      opsLn.Addr().String(),
		Cluster:      cl,
		log:          log,
		edgeSrv: &http.Server{
			Handler:           edgeHandler,
			ReadHeaderTimeout: 10 * time.Second,
		},
		internalSrv: &http.Server{
			Handler:           internalHandler,
			ReadHeaderTimeout: 10 * time.Second,
		},
		errCh:           make(chan error, 3),
		cachestoreReady: cachestoreReady,
	}

	a.opsSrv = &http.Server{
		Handler:           newOpsHandler(a.isReady),
		ReadHeaderTimeout: 5 * time.Second,
	}

	a.wg.Add(1)

	go func() {
		defer a.wg.Done()

		log.LogAttrs(ctx, slog.LevelInfo, "edge listener",
			slog.String("addr", a.EdgeAddr),
		)

		if err := a.edgeSrv.Serve(edgeLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.errCh <- fmt.Errorf("edge listener: %w", err)
		}
	}()

	a.wg.Add(1)

	go func() {
		defer a.wg.Done()

		log.LogAttrs(ctx, slog.LevelInfo, "internal listener",
			slog.String("addr", a.InternalAddr),
			slog.Bool("tls_enabled", cfg.Cluster.InternalTLS.Enabled),
		)

		var lerr error
		if cfg.Cluster.InternalTLS.Enabled {
			lerr = a.internalSrv.ServeTLS(internalLn,
				cfg.Cluster.InternalTLS.CertFile,
				cfg.Cluster.InternalTLS.KeyFile,
			)
		} else {
			log.LogAttrs(ctx, slog.LevelWarn, "internal listener TLS DISABLED - unsafe for production",
				slog.String("addr", a.InternalAddr),
			)

			lerr = a.internalSrv.Serve(internalLn)
		}

		if lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			a.errCh <- fmt.Errorf("internal listener: %w", lerr)
		}
	}()

	a.wg.Add(1)

	go func() {
		defer a.wg.Done()

		log.LogAttrs(ctx, slog.LevelInfo, "ops listener",
			slog.String("addr", a.OpsAddr),
		)

		if err := a.opsSrv.Serve(opsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.errCh <- fmt.Errorf("ops listener: %w", err)
		}
	}()

	return a, nil
}

// cleanupStartFailure unwinds partially-constructed Start state when
// a subsequent step (e.g. a later net.Listen) fails. Closes any
// listeners already bound and tells the cluster to stop its refresh
// goroutine within a bounded budget.
func cleanupStartFailure(cl *cluster.Cluster, listeners ...net.Listener) {
	for _, ln := range listeners {
		if ln == nil {
			continue
		}

		_ = ln.Close() //nolint:errcheck // best-effort close on bind failure
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = cl.Close(closeCtx) //nolint:errcheck // best-effort cleanup on bind failure
}

// newOpsHandler returns the http.Handler serving /healthz and
// /readyz for kubelet probes. /healthz is unconditional 200
// (process-alive); /readyz returns 200 only when isReady reports
// true. isReady is injected so tests can drive the readiness
// signal independently of the surrounding App.
func newOpsHandler(isReady func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok")) //nolint:errcheck // best-effort probe response
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !isReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready")) //nolint:errcheck // best-effort probe response

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready")) //nolint:errcheck // best-effort probe response
	})

	return mux
}

// isReady reports whether the app is ready to serve traffic.
// Both conditions must hold:
//   - cachestore self-test passed (or skipped via the test option).
//   - cluster has loaded an initial peer-set snapshot.
func (a *App) isReady() bool {
	return a.cachestoreReady && a.Cluster.HasInitialSnapshot()
}

// Wait blocks until either the parent context is canceled or one of
// the listeners exits unexpectedly. It returns the first listener
// error (if any) or nil if ctx was canceled. Wait is intended for
// the production "serve until SIGTERM" path; tests typically call
// Shutdown directly.
//
// Any listener errors that arrive concurrently with the wait-return
// (ctx-cancel branch or first-error branch) are drained and logged
// at Warn so they aren't silently discarded. Without this, a
// shutdown that overlaps with a listener failure - or a multi-
// listener crash where two listeners errored within the same tick -
// would lose all but the first error.
//
// Priority: when ctx is already canceled at the time Wait is called,
// the ctx-cancel branch is taken deterministically even if errCh
// also has buffered errors. Go's select non-determinism would
// otherwise flip the return value between nil and a buffered error
// on a tick race, contradicting the documented "nil if ctx was
// canceled" contract. The buffered errors are still logged via
// drainErrCh; only their effect on Wait's return value is
// suppressed in this specific overlap.
func (a *App) Wait(ctx context.Context) error {
	// Non-blocking pre-check: if ctx is already canceled, take the
	// shutdown branch without exposing the select-randomization
	// race against any errors that may have arrived alongside the
	// cancellation. See the function comment for rationale.
	select {
	case <-ctx.Done():
		a.drainErrCh(ctx, "listener error received during shutdown")

		return nil
	default:
	}

	select {
	case <-ctx.Done():
		a.drainErrCh(ctx, "listener error received during shutdown")

		return nil
	case err := <-a.errCh:
		a.drainErrCh(ctx, "additional listener error after first")

		return err
	}
}

// drainErrCh non-blockingly consumes any remaining errors from
// a.errCh and logs them at Warn with the given message. Used by
// Wait on both return paths to ensure no listener error is silently
// dropped.
func (a *App) drainErrCh(ctx context.Context, msg string) {
	for {
		select {
		case err := <-a.errCh:
			a.log.LogAttrs(ctx, slog.LevelWarn, msg,
				slog.Any("err", err),
			)
		default:
			return
		}
	}
}

// Shutdown gracefully stops every listener and the cluster goroutine.
// It is safe to call multiple times; subsequent calls are no-ops.
func (a *App) Shutdown(ctx context.Context) error {
	var firstErr error

	if err := a.edgeSrv.Shutdown(ctx); err != nil {
		a.log.LogAttrs(ctx, slog.LevelWarn, "edge listener shutdown failed",
			slog.Any("err", err),
		)

		firstErr = err
	}

	if err := a.internalSrv.Shutdown(ctx); err != nil {
		a.log.LogAttrs(ctx, slog.LevelWarn, "internal listener shutdown failed",
			slog.Any("err", err),
		)

		if firstErr == nil {
			firstErr = err
		}
	}

	if a.opsSrv != nil {
		if err := a.opsSrv.Shutdown(ctx); err != nil {
			a.log.LogAttrs(ctx, slog.LevelWarn, "ops listener shutdown failed",
				slog.Any("err", err),
			)

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if err := a.Cluster.Close(ctx); err != nil {
		a.log.LogAttrs(ctx, slog.LevelWarn, "cluster close did not finish before ctx deadline",
			slog.Any("err", err),
		)

		if firstErr == nil {
			firstErr = err
		}
	}

	a.wg.Wait()

	return firstErr
}

func buildOrigin(ctx context.Context, cfg *config.Config, override origin.Origin, log *slog.Logger) (origin.Origin, error) {
	if override != nil {
		return override, nil
	}

	switch cfg.Origin.Driver {
	case "azureblob":
		or, err := azureblob.New(cfg.Origin.Azureblob, log)
		if err != nil {
			return nil, fmt.Errorf("init origin/azureblob: %w", err)
		}

		return or, nil
	case "awss3":
		or, err := awss3.New(ctx, awss3.Config{
			Endpoint:     cfg.Origin.AWSS3.Endpoint,
			Region:       cfg.Origin.AWSS3.Region,
			Bucket:       cfg.Origin.AWSS3.Bucket,
			AccessKey:    cfg.Origin.AWSS3.AccessKey,
			SecretKey:    cfg.Origin.AWSS3.SecretKey,
			UsePathStyle: cfg.Origin.AWSS3.UsePathStyle,
		}, log)
		if err != nil {
			return nil, fmt.Errorf("init origin/awss3: %w", err)
		}

		return or, nil
	default:
		return nil, fmt.Errorf("unsupported origin driver: %q", cfg.Origin.Driver)
	}
}

func buildCacheStore(ctx context.Context, cfg *config.Config, override cachestore.CacheStore, log *slog.Logger) (cachestore.CacheStore, error) {
	if override != nil {
		return override, nil
	}

	switch cfg.Cachestore.Driver {
	case "s3":
		cs, err := cachestores3.New(ctx, cachestores3.Config{
			Endpoint:     cfg.Cachestore.S3.Endpoint,
			Bucket:       cfg.Cachestore.S3.Bucket,
			Region:       cfg.Cachestore.S3.Region,
			AccessKey:    cfg.Cachestore.S3.AccessKey,
			SecretKey:    cfg.Cachestore.S3.SecretKey,
			UsePathStyle: cfg.Cachestore.S3.UsePathStyle,
		}, log)
		if err != nil {
			return nil, fmt.Errorf("init cachestore/s3: %w", err)
		}

		return cs, nil
	default:
		return nil, fmt.Errorf("unsupported cachestore driver: %q", cfg.Cachestore.Driver)
	}
}
