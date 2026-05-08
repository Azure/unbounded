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
// net.Listen) before Start returns, so EdgeAddr / InternalAddr are
// resolved (including any :0 ports) by the time the caller sees them.
type App struct {
	// EdgeAddr is the resolved client-edge listen address (host:port).
	// When the config requested ":0" the port is the OS-assigned one.
	EdgeAddr string

	// InternalAddr is the resolved peer-RPC listen address (host:port).
	InternalAddr string

	// Cluster is exposed so tests can inspect peer state and call
	// Coordinator/Self for assertions. Production callers should treat
	// this as read-only.
	Cluster *cluster.Cluster

	log         *slog.Logger
	edgeSrv     *http.Server
	internalSrv *http.Server
	wg          sync.WaitGroup
	errCh       chan error
}

type options struct {
	log                 *slog.Logger
	clusterOpts         []cluster.Option
	origin              origin.Origin
	cacheStore          cachestore.CacheStore
	skipCacheSelfTst    bool
	internalHandlerWrap func(http.Handler) http.Handler
	edgeListener        net.Listener
	internalListener    net.Listener
}

// Option configures Start.
type Option func(*options)

// WithLogger overrides the slog.Logger used for the App's output. If
// not provided, a JSON handler writing to stdout at LevelInfo is used.
func WithLogger(log *slog.Logger) Option {
	return func(o *options) { o.log = log }
}

// WithResolver overrides only the DNS resolver inside the default
// peer source. Convenient for tests that want to keep the production
// DNS-discovery shape but substitute the resolver itself.
func WithResolver(r cluster.Resolver) Option {
	return func(o *options) {
		o.clusterOpts = append(o.clusterOpts, cluster.WithResolver(r))
	}
}

// WithPeerSource replaces the cluster's entire peer-discovery
// mechanism. Intended for integration tests that need full control
// (e.g. per-replica peer sets with explicit ports).
func WithPeerSource(s cluster.PeerSource) Option {
	return func(o *options) {
		o.clusterOpts = append(o.clusterOpts, cluster.WithPeerSource(s))
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

// WithSkipCachestoreSelfTest disables the boot-time atomic-commit
// self-test. Useful only in tests that wire a cachestore decorator
// already known to honor If-None-Match: *.
func WithSkipCachestoreSelfTest() Option {
	return func(o *options) { o.skipCacheSelfTst = true }
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
// HTTP server, bypassing app.Start's own net.Listen call. Intended
// for integration tests that need to allocate a port before starting
// the app (so peer sets can advertise the captured port from t=0
// without a close/re-bind race window).
func WithEdgeListener(ln net.Listener) Option {
	return func(o *options) { o.edgeListener = ln }
}

// WithInternalListener supplies a pre-bound listener for the peer-RPC
// internal HTTP server. See WithEdgeListener for rationale.
func WithInternalListener(ln net.Listener) Option {
	return func(o *options) { o.internalListener = ln }
}

// Start wires every dependency and begins serving on the configured
// listeners. It returns once both listeners are accepting connections
// (or returns the error that prevented startup).
//
// The returned App must be Shutdown by the caller; Start does not own
// the parent context's lifetime.
func Start(ctx context.Context, cfg *config.Config, opts ...Option) (*App, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	log := o.log
	if log == nil {
		log = slog.Default()
	}

	or, err := buildOrigin(ctx, cfg, o.origin)
	if err != nil {
		return nil, err
	}

	cs, err := buildCacheStore(ctx, cfg, o.cacheStore)
	if err != nil {
		return nil, err
	}

	if !o.skipCacheSelfTst {
		if err := cs.SelfTestAtomicCommit(ctx); err != nil {
			return nil, fmt.Errorf("cachestore self-test failed: %w", err)
		}

		log.Info("cachestore self-test passed")
	}

	cl, err := cluster.New(ctx, cfg.Cluster, o.clusterOpts...)
	if err != nil {
		return nil, fmt.Errorf("init cluster: %w", err)
	}

	cat := chunkcatalog.New(cfg.ChunkCatalog.MaxEntries)
	mc := metadata.NewCache(cfg.Metadata)
	fc := fetch.NewCoordinator(or, cs, cl, cat, mc, cfg)

	edgeHandler := server.NewEdgeHandler(fc, cfg, log)

	var internalHandler http.Handler = server.NewInternalHandler(fc, cl, log)
	if o.internalHandlerWrap != nil {
		internalHandler = o.internalHandlerWrap(internalHandler)
	}

	edgeLn := o.edgeListener
	if edgeLn == nil {
		ln, err := net.Listen("tcp", cfg.Server.Listen)
		if err != nil {
			cl.Close()
			return nil, fmt.Errorf("edge listener bind %q: %w", cfg.Server.Listen, err)
		}

		edgeLn = ln
	}

	internalLn := o.internalListener
	if internalLn == nil {
		ln, err := net.Listen("tcp", cfg.Cluster.InternalListen)
		if err != nil {
			_ = edgeLn.Close() //nolint:errcheck // best-effort close on bind failure

			cl.Close()

			return nil, fmt.Errorf("internal listener bind %q: %w", cfg.Cluster.InternalListen, err)
		}

		internalLn = ln
	}

	a := &App{
		EdgeAddr:     edgeLn.Addr().String(),
		InternalAddr: internalLn.Addr().String(),
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
		errCh: make(chan error, 2),
	}

	a.wg.Add(1)

	go func() {
		defer a.wg.Done()

		log.Info("edge listener", "addr", a.EdgeAddr)

		if err := a.edgeSrv.Serve(edgeLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.errCh <- fmt.Errorf("edge listener: %w", err)
		}
	}()

	a.wg.Add(1)

	go func() {
		defer a.wg.Done()

		log.Info("internal listener",
			"addr", a.InternalAddr,
			"tls_enabled", cfg.Cluster.InternalTLS.Enabled,
		)

		var lerr error
		if cfg.Cluster.InternalTLS.Enabled {
			lerr = a.internalSrv.ServeTLS(internalLn,
				cfg.Cluster.InternalTLS.CertFile,
				cfg.Cluster.InternalTLS.KeyFile,
			)
		} else {
			log.Warn("internal listener TLS DISABLED - unsafe for production",
				"addr", a.InternalAddr)

			lerr = a.internalSrv.Serve(internalLn)
		}

		if lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			a.errCh <- fmt.Errorf("internal listener: %w", lerr)
		}
	}()

	return a, nil
}

// Wait blocks until either the parent context is canceled or one of
// the listeners exits unexpectedly. It returns the listener error (if
// any) or nil if ctx was canceled. Wait is intended for the production
// "serve until SIGTERM" path; tests typically call Shutdown directly.
func (a *App) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-a.errCh:
		return err
	}
}

// Shutdown gracefully stops both listeners and the cluster goroutine.
// It is safe to call multiple times; subsequent calls are no-ops.
func (a *App) Shutdown(ctx context.Context) error {
	var firstErr error

	if err := a.edgeSrv.Shutdown(ctx); err != nil {
		a.log.Warn("edge listener shutdown failed", "err", err)

		firstErr = err
	}

	if err := a.internalSrv.Shutdown(ctx); err != nil {
		a.log.Warn("internal listener shutdown failed", "err", err)

		if firstErr == nil {
			firstErr = err
		}
	}

	a.Cluster.Close()
	a.wg.Wait()

	return firstErr
}

func buildOrigin(ctx context.Context, cfg *config.Config, override origin.Origin) (origin.Origin, error) {
	if override != nil {
		return override, nil
	}

	switch cfg.Origin.Driver {
	case "azureblob":
		or, err := azureblob.New(cfg.Origin.Azureblob)
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
		})
		if err != nil {
			return nil, fmt.Errorf("init origin/awss3: %w", err)
		}

		return or, nil
	default:
		return nil, fmt.Errorf("unsupported origin driver: %q", cfg.Origin.Driver)
	}
}

func buildCacheStore(ctx context.Context, cfg *config.Config, override cachestore.CacheStore) (cachestore.CacheStore, error) {
	if override != nil {
		return override, nil
	}

	switch cfg.Cachestore.Driver {
	case "s3":
		cs, err := cachestores3.New(ctx, cachestores3.Config{
			Endpoint:                 cfg.Cachestore.S3.Endpoint,
			Bucket:                   cfg.Cachestore.S3.Bucket,
			Region:                   cfg.Cachestore.S3.Region,
			AccessKey:                cfg.Cachestore.S3.AccessKey,
			SecretKey:                cfg.Cachestore.S3.SecretKey,
			UsePathStyle:             cfg.Cachestore.S3.UsePathStyle,
			RequireUnversionedBucket: cfg.Cachestore.S3.RequireUnversionedBucket,
		})
		if err != nil {
			return nil, fmt.Errorf("init cachestore/s3: %w", err)
		}

		return cs, nil
	default:
		return nil, fmt.Errorf("unsupported cachestore driver: %q", cfg.Cachestore.Driver)
	}
}
