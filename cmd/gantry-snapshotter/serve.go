// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"google.golang.org/grpc"

	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/members"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/blockmap"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/clean"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// serve builds the stack and runs it until ctx is cancelled.
//
// The ordering here is the load bearing part. Everything that can fail because
// the node is not ready yet, the image devices, the catalog, containerd, is
// built so that it degrades into a miss rather than into a startup error, and
// the socket is bound last so containerd only ever connects to a daemon that
// can already answer.
func serve(ctx context.Context, cfg *Config, log *slog.Logger) error {
	// A reconcile signal rather than direct work: OnChange runs on the
	// watcher's goroutine and attaching a catalog is I/O with retries.
	reconcile := make(chan struct{}, 1)

	watcher := segment.NewWatcher(segment.WatcherOptions{
		Path:     cfg.Devices,
		Interval: cfg.DeviceInterval,
		OnChange: func(*segment.Set) {
			select {
			case reconcile <- struct{}{}:
			default:
			}
		},
	})

	errnos, err := catalogConflictErrnos(cfg)
	if err != nil {
		return err
	}

	cat := &holder{
		log:        log.With(slog.String("subsystem", "catalog")),
		format:     cfg.FormatCatalog,
		adopt:      cfg.AdoptSegments,
		blocks:     cfg.SegmentBlocks,
		watermarks: cfg.WatermarkBlocks,
		errnos:     errnos,
		grace:      cfg.WatermarkGrace,
		node:       catalog.NodeKeyFor(cfg.NodeName),
	}

	defer cat.close() //nolint:errcheck // shutdown

	maps, err := blockmap.New(blockmap.Options{
		Root:      cfg.MapRoot,
		Locator:   blockmap.WatcherLocator{Watcher: watcher},
		Devmapper: blockmap.NewDmsetup(),
		Mounter:   blockmap.SystemMounter{},
	})
	if err != nil {
		return fmt.Errorf("layer mapper: %w", err)
	}

	provider := newLazyProvider(cfg.ContainerdSocket, cfg.ContainerdNamespace, log)
	defer provider.Close() //nolint:errcheck // shutdown

	opener, err := snapshotter.NewContentOpener(provider, cfg.ContainerdNamespace)
	if err != nil {
		return fmt.Errorf("content opener: %w", err)
	}

	builder := ingest.NewBuilder()
	builder.Binary = cfg.MkfsErofs

	ing, err := ingest.New(ingest.Options{
		Catalog:    cat,
		Locator:    blockmap.WatcherLocator{Watcher: watcher},
		Opener:     opener,
		Builder:    builder,
		WorkDir:    cfg.WorkDir,
		Headroom:   cfg.WorkHeadroom,
		SkipVerify: cfg.SkipVerify,
	})
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}

	elector, peers, stopMembers, err := newElector(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("ingest election: %w", err)
	}

	defer stopMembers()

	ingestLog := log.With(slog.String("subsystem", "ingest"))

	// The registry is built whether or not anything serves it. Wiring the
	// observers unconditionally keeps the instrumented paths identical on
	// every node, so a node running without a metrics address is not a node
	// running different code.
	reg := metrics.New()
	reg.RegisterDefaultCollectors()

	daemon := newDaemonMetrics(reg, cat)

	// Set before the reconcile loop starts, which is what makes reading it
	// from the reservation path safe.
	cat.roll = daemon.observeRoll

	queue, err := ingest.NewQueue(ingest.QueueOptions{
		Ingester:   ing,
		Elector:    elector,
		Workers:    cfg.IngestWorkers,
		Depth:      cfg.IngestDepth,
		RetryDelay: cfg.IngestRetry,
		Observe:    chainObserver(observer(ingestLog), daemon.observeIngest),
	})
	if err != nil {
		return fmt.Errorf("ingest queue: %w", err)
	}

	daemon.trackQueue(reg, queue)

	sn, err := snapshotter.New(snapshotter.Options{
		Root:         cfg.Root,
		Catalog:      cat,
		Mapper:       maps,
		Queue:        queue,
		MountOptions: cfg.MountOptions,
		Observe:      daemon.observeAdopt,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("snapshotter: %w", err)
	}

	defer sn.Close() //nolint:errcheck // shutdown

	listener, err := listen(cfg)
	if err != nil {
		return err
	}

	server := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(server, snapshotservice.FromSnapshotter(sn))

	// Every background task below returns only on ctx.Done(), so shutdown
	// needs a cancel this function controls. The parent context is only
	// cancelled by a signal, and the serve error path has to be able to
	// stop the tasks without one.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	background := func(name string, fn func()) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			fn()
			log.Debug("background task stopped", slog.String("task", name))
		}()
	}

	// The cleaner is the only thing that gives a sealed segment back. It is
	// elected per segment so that one node evacuates a victim while the rest
	// carry on serving it, and it is skipped entirely on a platform with no
	// discard, where trimming a page is not something we can ask for.
	cleaner, err := newCleaner(cfg, cat, watcher, peers, daemon, log)
	if err != nil {
		return fmt.Errorf("cleaner: %w", err)
	}

	background("devices", func() { watcher.Run(ctx) })
	background("reconcile", func() { runReconcile(ctx, cfg, watcher, cat, reconcile, log) })
	background("ingest", func() { queue.Run(ctx) })
	background("catalog-sync", func() { runCatalogSync(ctx, cfg, cat, log) })
	background("watermark", func() { runWatermark(ctx, cfg, cat, log) })
	background("cleanup", func() { runCleanup(ctx, cfg, sn, log) })

	if cleaner != nil {
		background("clean", func() { cleaner.Run(ctx) })
	}

	if cfg.MetricsAddr != "" {
		// The prober talks to the socket bound above, so it exercises the
		// same path containerd does rather than a copy of it.
		probe := newProber(cfg)
		defer probe.close() //nolint:errcheck // shutdown

		background("metrics", func() { runMetrics(ctx, cfg, reg, probe.check, log) })
	}

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve(listener) }()

	log.Info("serving", slog.String("socket", cfg.Socket))

	return awaitShutdown(ctx, cancel, server, serveErr, cfg.ShutdownGrace, &wg, log)
}

// awaitShutdown runs until the gRPC server exits or ctx is cancelled, then
// stops the server and waits for the background tasks to finish.
//
// cancel is what releases those tasks: each of them returns only on ctx.Done().
// The serve error path has to cancel as well as stop the server, because a
// fatal accept error is not a signal. Without it this would wait forever on a
// WaitGroup nothing can release, holding the bbolt lock and the catalog device
// open and skipping every deferred close in serve, until an operator noticed.
func awaitShutdown(
	ctx context.Context,
	cancel context.CancelFunc,
	server *grpc.Server,
	serveErr <-chan error,
	grace time.Duration,
	wg *sync.WaitGroup,
	log *slog.Logger,
) error {
	select {
	case err := <-serveErr:
		cancel()
		stopServer(server, grace)
		wg.Wait()

		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down")
	cancel()
	stopServer(server, grace)
	wg.Wait()
	// Serve returns once the listener is closed, which GracefulStop and Stop
	// both do. Draining it keeps the goroutine from outliving this call.
	<-serveErr

	return nil
}

// stopServer drains in-flight calls, then cuts them off. containerd retries a
// snapshotter call that fails during shutdown, so a hard stop after the grace
// period is better than a daemon that will not exit.
func stopServer(server *grpc.Server, grace time.Duration) {
	if grace <= 0 {
		server.Stop()
		return
	}

	done := make(chan struct{})

	go func() {
		server.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(grace):
		server.Stop()
		<-done
	}
}

// listen binds the snapshotter socket, removing a socket left behind by a
// previous run. Unlinking is safe because only one snapshotter can own a root
// directory at a time: the bbolt metadata store below it is already exclusive.
func listen(cfg *Config) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o755); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	if err := os.Remove(cfg.Socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Socket, err)
	}

	if err := os.Chmod(cfg.Socket, cfg.SocketMode); err != nil {
		_ = listener.Close() //nolint:errcheck // the bind already failed

		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return listener, nil
}

// runReconcile attaches the catalog whenever the published device set changes,
// and keeps retrying while it cannot.
//
// The retry is on the device poll interval rather than a backoff of its own.
// The failures this recovers from are "the operator has not allocated a
// catalog yet" and "racer-ctrl has not staged the device", both of which
// resolve on a human or controller timescale.
func runReconcile(
	ctx context.Context,
	cfg *Config,
	watcher *segment.Watcher,
	cat *holder,
	signal <-chan struct{},
	log *slog.Logger,
) {
	interval := cfg.DeviceInterval
	if interval <= 0 {
		interval = segment.DefaultWatchInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr string

	attempt := func() {
		set := watcher.Current()
		if set == nil {
			return
		}

		err := cat.reconcile(set)
		if err == nil {
			lastErr = ""
			return
		}

		// The same failure every tick is not worth a log line every tick.
		if msg := err.Error(); msg != lastErr {
			lastErr = msg

			log.Warn("catalog unavailable, images will be unpacked locally", slog.Any("err", err))
		}
	}

	for {
		attempt()

		select {
		case <-ctx.Done():
			return
		case <-signal:
		case <-ticker.C:
		}
	}
}

// runCatalogSync polls the catalog so a node that starts no pods still learns
// about layers other nodes ingested. The start path syncs on its own misses,
// so this only bounds staleness while the node is idle.
//
// It is also where a hole left by a crashed writer gets retired. Every node
// polls, so every node eventually notices, which is what makes the recovery
// independent of whichever node died.
func runCatalogSync(ctx context.Context, cfg *Config, cat *holder, log *slog.Logger) {
	if cfg.CatalogSync <= 0 {
		return
	}

	ticker := time.NewTicker(cfg.CatalogSync)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		changed, err := cat.Sync()
		if err != nil {
			if !errors.Is(err, errNotReady) {
				log.Warn("catalog sync failed", slog.Any("err", err))
			}

			continue
		}

		if changed {
			log.Debug("catalog synced", slog.Int("records", cat.Len()))
		}

		repairHole(cfg, cat, log)
	}
}

// repairHole retires record slots a writer reserved and never filled.
//
// This is loud on purpose. Voiding slots means a layer somebody was ingesting
// is gone and, if the writer wrote its bytes before dying, that those bytes are
// stranded in a segment with nothing naming them. It is the right trade against
// a catalog that never advances again, but it is not routine.
func repairHole(cfg *Config, cat *holder, log *slog.Logger) {
	if cfg.HoleGrace <= 0 {
		return
	}

	voided, err := cat.Repair(cfg.HoleGrace)
	if err != nil {
		if !errors.Is(err, errNotReady) {
			log.Warn("catalog repair failed", slog.Any("err", err))
		}

		return
	}

	if voided > 0 {
		log.Warn("retired catalog record slots whose writer never returned",
			slog.Int("slots", voided),
			slog.Duration("grace", cfg.HoleGrace),
		)
	}
}

// watermarkInterval is how often this node refreshes its drain-gate entry,
// derived from the grace rather than configured separately so the two cannot
// be set into a combination that expires healthy nodes. A fifth of the grace
// tolerates four consecutive missed refreshes before the cleaner writes this
// node off.
func watermarkInterval(grace time.Duration) time.Duration {
	interval := grace / 5
	if interval < time.Second {
		return time.Second
	}

	return interval
}

// runWatermark keeps this node's entry in the catalog's drain-gate table live.
//
// The cleaner will not trim a segment until every node it expects has reported
// a generation past the point where blobs were repointed out of it, and it
// stops waiting for entries that have gone stale. Refreshing only from the
// sweep in runCleanup would tie liveness to a walk of every snapshot on the
// node, so an ordinary slow sweep would read as a departed node. This loop
// republishes what the sweep last proved, and nothing more.
func runWatermark(ctx context.Context, cfg *Config, cat *holder, log *slog.Logger) {
	ticker := time.NewTicker(watermarkInterval(cfg.WatermarkGrace))
	defer ticker.Stop()

	var lastErr string

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		err := cat.Refresh()
		if err == nil {
			lastErr = ""

			continue
		}

		// No catalog is not a failure to report: there is no gate to be
		// visible to, and reconcile is already logging why.
		if errors.Is(err, errNotReady) {
			continue
		}

		// Loud, because the consequence of a slot this node cannot keep
		// fresh is another node trimming pages out from under its mounts
		// once the grace runs out.
		if msg := err.Error(); msg != lastErr {
			lastErr = msg

			log.Warn("could not refresh this node's drain-gate watermark",
				slog.Duration("grace", cfg.WatermarkGrace),
				slog.Any("err", err))
		}
	}
}

// runCleanup sweeps snapshot directories containerd has released and layer
// mappings nothing refers to any more.
func runCleanup(ctx context.Context, cfg *Config, sn *snapshotter.Snapshotter, log *slog.Logger) {
	if cfg.CleanupInterval <= 0 {
		return
	}

	ticker := time.NewTicker(cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := sn.Cleanup(ctx); err != nil && ctx.Err() == nil {
			log.Warn("cleanup failed", slog.Any("err", err))
		}
	}
}

// observer logs ingest outcomes. Ingest is the part of this daemon nobody
// watches until an image is unexpectedly slow, so every terminal outcome gets
// a line.
func observer(log *slog.Logger) func(ingest.Request, ingest.Result, error) {
	return func(req ingest.Request, res ingest.Result, err error) {
		if err != nil {
			log.Warn("ingest failed", slog.String("request", req.String()), slog.Any("err", err))
			return
		}

		log.Info("ingest complete",
			slog.String("request", req.String()),
			slog.String("outcome", res.Outcome.String()),
			slog.Uint64("bytes", res.Blob.Address.ByteLength),
		)
	}
}

// newElector builds the ingest election. Without a membership view every node
// is eager, which is right on a single node and wrong on a thousand, so the
// Kubernetes view is what turns the delay ladder on.
func newElector(ctx context.Context, cfg *Config, log *slog.Logger) (ingest.Elector, func() []ifaces.Node, func(), error) {
	if cfg.MembersSelector == "" {
		log.Info("no peer view configured, this node ingests every layer it unpacks")

		return ingest.Immediate{}, nil, func() {}, nil
	}

	mgr, err := members.New(members.Options{
		NodeName:      cfg.NodeName,
		Namespace:     cfg.MembersNamespace,
		LabelSelector: cfg.MembersSelector,
		ZoneLabelKey:  cfg.ZoneLabel,
		Kubeconfig:    cfg.Kubeconfig,
	})
	if err != nil {
		return nil, nil, func() {}, err
	}

	mgr.Start()

	// Waiting for the informer cache here is deliberate but bounded: an
	// unsynced view makes every node eager, which would have a thousand
	// nodes ingest the same layer. If it does not sync we carry on anyway,
	// because duplicated ingest is a waste and a stalled snapshotter is an
	// outage.
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := mgr.WaitForSync(syncCtx); err != nil {
		log.Warn("peer view did not sync, ingest election starts blind", slog.Any("err", err))
	}

	elector, err := ingest.NewHRW(ingest.HRWOptions{
		Self:     ifaces.NodeID(cfg.NodeName),
		Members:  mgr.Snapshot,
		Step:     cfg.ElectionStep,
		MaxDelay: cfg.ElectionMaxDelay,
		Scope:    hrw.ScopeCluster,
	})
	if err != nil {
		mgr.Stop()

		return nil, nil, func() {}, err
	}

	log.Info("ingest election enabled",
		slog.String("node", cfg.NodeName),
		slog.String("selector", cfg.MembersSelector),
		slog.Duration("step", cfg.ElectionStep),
	)

	return elector, mgr.Snapshot, mgr.Stop, nil
}

// newCleaner builds the segment cleaner, or returns nil when it is switched
// off. Election reuses the peer view the ingest election already needs, so
// enabling the cleaner costs no extra Kubernetes access; without that view a
// single node cleans on its own behalf, which is right on one node and would
// be wasteful on many.
func newCleaner(
	cfg *Config,
	cat clean.Catalog,
	watcher *segment.Watcher,
	peers func() []ifaces.Node,
	daemon *daemonMetrics,
	log *slog.Logger,
) (*clean.Cleaner, error) {
	if !cfg.Clean {
		log.Info("segment reclamation disabled, a sealed segment will not come back")

		return nil, nil
	}

	elector := clean.Elector(clean.Always{})
	if peers != nil && cfg.NodeName != "" {
		elector = clean.HRW{Self: ifaces.NodeID(cfg.NodeName), Members: peers}
	}

	var onCycle func(clean.Result)
	if daemon != nil {
		onCycle = daemon.observeClean
	}

	return clean.New(clean.Options{
		Catalog:         cat,
		Locator:         blockmap.WatcherLocator{Watcher: watcher},
		Discarder:       clean.SystemDiscarder{},
		Elector:         elector,
		Interval:        cfg.CleanInterval,
		LowWater:        cfg.CleanLowWater,
		MaxLiveFraction: cfg.CleanMaxLive,
		Grace:           cfg.WatermarkGrace,
		Log:             log.With(slog.String("component", "clean")),
		OnCycle:         onCycle,
	})
}
