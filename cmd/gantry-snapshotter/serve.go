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
	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/blockmap"
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
		log:    log.With(slog.String("subsystem", "catalog")),
		format: cfg.FormatCatalog,
		adopt:  cfg.AdoptSegments,
		blocks: cfg.SegmentBlocks,
		errnos: errnos,
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

	elector, stopMembers, err := newElector(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("ingest election: %w", err)
	}

	defer stopMembers()

	ingestLog := log.With(slog.String("subsystem", "ingest"))

	queue, err := ingest.NewQueue(ingest.QueueOptions{
		Ingester:   ing,
		Elector:    elector,
		Workers:    cfg.IngestWorkers,
		Depth:      cfg.IngestDepth,
		RetryDelay: cfg.IngestRetry,
		Observe:    observer(ingestLog),
	})
	if err != nil {
		return fmt.Errorf("ingest queue: %w", err)
	}

	sn, err := snapshotter.New(snapshotter.Options{
		Root:         cfg.Root,
		Catalog:      cat,
		Mapper:       maps,
		Queue:        queue,
		MountOptions: cfg.MountOptions,
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

	var wg sync.WaitGroup

	background := func(name string, fn func()) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			fn()
			log.Debug("background task stopped", slog.String("task", name))
		}()
	}

	background("devices", func() { watcher.Run(ctx) })
	background("reconcile", func() { runReconcile(ctx, cfg, watcher, cat, reconcile, log) })
	background("ingest", func() { queue.Run(ctx) })
	background("catalog-sync", func() { runCatalogSync(ctx, cfg, cat, log) })
	background("cleanup", func() { runCleanup(ctx, cfg, sn, log) })

	if cfg.MetricsAddr != "" {
		// The prober talks to the socket bound above, so it exercises the
		// same path containerd does rather than a copy of it.
		probe := newProber(cfg)
		defer probe.close() //nolint:errcheck // shutdown

		background("metrics", func() { runMetrics(ctx, cfg, probe.check, log) })
	}

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve(listener) }()

	log.Info("serving", slog.String("socket", cfg.Socket))

	select {
	case err := <-serveErr:
		stopServer(server, cfg.ShutdownGrace)
		wg.Wait()

		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down")
	stopServer(server, cfg.ShutdownGrace)
	wg.Wait()
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
func newElector(ctx context.Context, cfg *Config, log *slog.Logger) (ingest.Elector, func(), error) {
	if cfg.MembersSelector == "" {
		log.Info("no peer view configured, this node ingests every layer it unpacks")

		return ingest.Immediate{}, func() {}, nil
	}

	mgr, err := members.New(members.Options{
		NodeName:      cfg.NodeName,
		Namespace:     cfg.MembersNamespace,
		LabelSelector: cfg.MembersSelector,
		ZoneLabelKey:  cfg.ZoneLabel,
		Kubeconfig:    cfg.Kubeconfig,
	})
	if err != nil {
		return nil, func() {}, err
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

		return nil, func() {}, err
	}

	log.Info("ingest election enabled",
		slog.String("node", cfg.NodeName),
		slog.String("selector", cfg.MembersSelector),
		slog.Duration("step", cfg.ElectionStep),
	)

	return elector, mgr.Stop, nil
}
