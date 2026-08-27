// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command gantry runs the Gantry P2P agent.
//
// Subcommands:
//
//	gantry version print build information and exit
//	gantry agent run the full agent (mirror + transfer + libp2p + ...)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/discovery"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	gantrylog "github.com/Azure/unbounded/internal/gantry/log"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/negcache"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/internal/gantry/transfer"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}

		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return errors.New("gantry: subcommand required")
	}

	switch args[0] {
	case "version", "-version", "--version":
		fmt.Printf("gantry %s %s/%s (go %s, commit %s, built %s)\n",
			version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(),
			version.GitCommit, version.BuildTime)

		return nil
	case "agent":
		return runAgent(args[1:])
	case "help", "-h", "-help", "--help":
		return runHelp(args[1:])
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("gantry: unknown subcommand %q", args[0])
	}
}

func printUsage(w *os.File) {
	//nolint:errcheck // best-effort write
	_, _ = fmt.Fprintln(w, `Usage: gantry <subcommand> [flags]

Subcommands:
  agent      run the Gantry P2P agent
  version    print build information
  help       print help for the agent subcommand`)
}

func runHelp(args []string) error {
	if len(args) == 0 || args[0] == "agent" {
		fs, _ := buildAgentFlagSet(config.NewDefault())
		fs.SetOutput(os.Stdout)
		_, _ = fmt.Fprintln(os.Stdout, "Usage: gantry agent [flags]") //nolint:errcheck // best-effort write
		_, _ = fmt.Fprintln(os.Stdout)                                //nolint:errcheck // best-effort write
		_, _ = fmt.Fprintln(os.Stdout, "Flags:")                      //nolint:errcheck // best-effort write

		fs.PrintDefaults()

		return nil
	}

	return fmt.Errorf("gantry help: unknown topic %q", args[0])
}

// buildAgentFlagSet constructs the `gantry agent` flag set bound to c. The
// returned *string is the --config flag's value (read before re-parsing
// with the file-derived defaults). Exposed for runHelp.
func buildAgentFlagSet(c *config.Config) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	c.BindFlags(fs)

	return fs, configPath
}

func runAgent(args []string) error {
	c, err := loadAgentConfig(args)
	if err != nil {
		return err
	}

	if err := c.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := gantrylog.New(os.Stderr, c.LogLevel, c.LogFormat)
	slog.SetDefault(logger)
	logger.Info("gantry starting",
		slog.String("version", version.Version),
		slog.String("go", runtime.Version()),
		slog.String("os", runtime.GOOS),
		slog.String("arch", runtime.GOARCH),
		slog.Any("config", c.Redacted()),
	)

	// Metrics registry + 2 instruments.
	reg := metrics.New()
	reg.RegisterDefaultCollectors()
	inst := newPhase1Metrics(reg)
	p2 := newPhase2Metrics(reg)
	layerProgress := newLayerProgressTracker(p2.layerCompletedAt, c.NodeName, time.Now)
	p9 := newPhase9Metrics(reg)
	rendezvousMetricSet := newRendezvousMetrics(reg)
	// Storage mode info: emit a single time-series at 1 for the
	// active backend so dashboards can filter by it.
	p9.storageMode.WithLabelValues(config.StorageModeContainerd).Set(1)

	// Origin clients (+ live-stream split). See agent_origin.go
	// for the full rationale of the two-client split and the
	// background-success / downstream-failure closure shapes.
	pullOriginClient, mirrorOriginClient, backgroundOriginSuccess, backgroundOriginDownstreamFailure, err := buildOriginClients(c, inst, logger)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// - libp2p Host + DHT.
	disco, err := discovery.New(ctx, discovery.FromConfig(c))
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}

	defer func() { _ = disco.Close() }() //nolint:errcheck // best-effort close

	logger.Info("libp2p host ready", slog.String("peer_id", disco.PeerID().String()))

	// containerd source is constructed once and shared between cdsub
	// (image-events subscription) and the primary local content
	// store. Plan removed the alternative hostPath cache
	// path; containerd is now the only supported storage backend.
	cdsubSrc := newContainerdImageSource(c, logger)

	// Storage backend: containerd content store (Plan ). See
	// agent_storage.go for the wiring details.
	cdstore, cstore, containerdInv, err := buildContainerdStorage(c, cdsubSrc, logger, p9)
	if err != nil {
		return err
	}

	// - peer dialer + transfer endpoint. In containerd-only
	// mode the primary local content store IS the containerd content
	// store; the transfer endpoint reads from it directly with no
	// SecondaryBlobSource hop.
	peerClient := transfer.NewClient(
		transfer.WithRequestTimeout(c.PeerFetchTimeout),
		transfer.WithClientByteMetrics(func(kind string, bytes int64) {
			p2.peerFetchBytes.WithLabelValues(kind).Add(float64(bytes))
		}),
	)
	transferOpts := []transfer.Option{
		transfer.WithLogger(logger),
		transfer.WithDescriber(cdstore),
		transfer.WithMetrics(
			func() { p2.peerServe.Inc() },
			func() { p2.peerMiss.Inc() },
		),
		transfer.WithByteMetrics(func(kind string, bytes int64) {
			p2.peerServeBytes.WithLabelValues(kind).Add(float64(bytes))
		}),
		transfer.WithMaxConcurrentServes(c.TransferMaxConcurrentServes),
	}
	transferSrv := transfer.New(cstore, transferOpts...)

	transferStop, err := transferSrv.ListenAndServe(c.TransferListen)
	if err != nil {
		return fmt.Errorf("transfer listen: %w", err)
	}

	logger.Info("transfer endpoint listening", slog.String("addr", c.TransferListen))

	// - membership view + cold-start orchestrator. Members
	// requires Kubernetes credentials (in-cluster or explicit
	// Lease rendezvous is the only first-contact path: the agent
	// publishes its libp2p addresses into one of a fixed set of Lease
	// slots and dials contacts read from the others. No Pod or Node
	// informer is started, so startup cost is independent of cluster
	// size.
	selfNodeID, rendezvousBootstrap, err := buildLeaseRendezvous(c, disco, logger, rendezvousMetricSet)
	if err != nil {
		return fmt.Errorf("rendezvous: %w", err)
	}

	go rendezvousBootstrap.Run(ctx)

	// noDialableTransferAddr is set when c.TransferListen is
	// wildcard-bound to a single IP family that does not match the
	// pod's IP family - e.g. transfer_listen=0.0.0.0:5001 on an
	// IPv6-only cluster, or transfer_listen=[::]:5001 on a v4-only
	// cluster. In that state peers would see an address the kernel
	// has no socket bound to, and every peer transfer attempt would
	// connection-refused. Computed once at startup since
	// c.TransferListen and c.PodIP do not change after boot.
	var noDialableTransferAddr atomic.Bool
	noDialableTransferAddr.Store(transferAddrFamilyMismatch(c.TransferListen, c.PodIP))

	if noDialableTransferAddr.Load() {
		// Loud diagnostic so the readiness probe's terse message
		// has something concrete in the logs.
		logger.Error("transfer: listener family mismatches Pod IP; advertised transfer address will be empty and peers cannot dial this node for blob fetches",
			slog.String("transfer_listen", c.TransferListen),
			slog.String("pod_ip", c.PodIP),
		)
	}

	// Exact cluster size is intentionally unknown; use the configured
	// minimum as the DHT health monitor's convergence target.
	if monitor := disco.Monitor(); monitor != nil {
		monitor.SetRoutingTableTarget(func() int { return c.Rendezvous.RoutingTableMin })
	}

	// - in-flight map + coord client + coord server + metrics.
	inflightMap := inflight.New(inflight.DefaultStalls(), nil)
	p3 := newPhase3Metrics(reg, inflightMap)

	// - the design doc origin-failure negative cache (puller-local) +
	// stall-takeover metric. Constructed before the pump so the pump
	// can consult it; the coord server is given a thin adapter so
	// pull_intent_query responses surface recently_failed.
	p4 := newPhase4Metrics(reg)
	negCache := negcache.New(negcache.Options{
		Initial:    c.OriginFailureCooldownInitial,
		Max:        c.OriginFailureCooldownMax,
		Multiplier: c.OriginFailureCooldownMultiplier,
		OnEnter:    func(class ifaces.FailureClass) { p4.observeEnter(class) },
		OnHit:      func(class ifaces.FailureClass) { p4.observeHit(class) },
		OnSize:     func(n int) { p4.setSize(n) },
	})

	// - DHT health gauge, direct-origin-fallback origin-fallback counter, and
	// top-K expansion counter. Health source is the discovery host
	// (monitor); when running without monitoring (test mode)
	// it returns 1.0.
	p5 := newPhase5Metrics(reg, disco.Health)

	adv := advertise.New(containerdInv, disco,
		advertise.WithLogger(logger),
		advertise.WithReconcileInterval(c.AdvertiseReconcileInterval),
		advertise.WithMetrics(advertise.MetricsHooks{
			OnReconcileStart: func() { p9.advReconcileTotal.Inc() },
			OnReconcileEnd: func(dur time.Duration, inventorySize, added, removed int) {
				p9.advReconcileDur.Observe(dur.Seconds())
				p9.advReconcileDigestCount.Set(float64(inventorySize))
				p9.advReconcileAdded.Add(float64(added))
				p9.advReconcileRemoved.Add(float64(removed))
			},
			OnReconcileError:       func(_ error) { p9.advReconcileError.Inc() },
			OnReconcileUnavailable: func() { p9.advReconcileUnavailable.Inc(); p9.containerdUnavailable.Inc() },
			OnProvide:              func() { p2.dhtAdvertise.Inc(); p9.advertiseTotal.Inc() },
			OnProvideError:         func() { p2.dhtProvideErr.WithLabelValues("advertise").Inc(); p9.advertiseError.Inc() },
			OnWithdraw:             func() { p9.withdrawTotal.Inc() },
			OnWithdrawError:        func() { p9.withdrawError.Inc() },
		}),
	)

	coordClientOpts := []coord.ClientOption{
		coord.WithClientLogger(logger),
		coord.WithClientMaxDigestsPerPleasePull(c.CoordMaxDigestsPerRequest),
	}
	coordClient := coord.NewClient(disco.LibP2P(), coordClientOpts...)
	// pullerPump bridges inbound please_pull RPCs to the local origin
	// puller (the step 7). The pump itself MUST NOT block the coord
	// stream handler; the actual origin fetch + cache write + advertiser
	// mark-present happen in a detached goroutine. pullerPumpGate tracks
	// outstanding goroutines so graceful shutdown can let the final
	// advertise path flush before disco.Close fires.
	pullerPumpGate := newPullerPumpGate()

	leaseHooks := leaseMetricHooks{
		onCreated:  func() { p9.containerdLeaseCreated.Inc() },
		onReleased: func() { p9.containerdLeaseReleased.Inc() },
	}
	// originSuccessWithIngest wraps the background success hook plus the
	// ingest counter so every
	// committed origin pull also bumps the containerd_ingest_total
	// counter. Failures land in containerdIngestFailure via
	// downstreamFailureMetric below.
	originSuccessWithIngest := func(kind string, bytes int64) {
		if backgroundOriginSuccess != nil {
			backgroundOriginSuccess(kind, bytes)
		}

		p9.containerdIngestTotal.Inc()
	}
	downstreamFailureWithIngest := func(kind, class string) {
		if backgroundOriginDownstreamFailure != nil {
			backgroundOriginDownstreamFailure(kind, class)
		}

		p9.containerdIngestFailure.Inc()
	}
	pullerPump := newPullerPump(inflightMap, pullOriginClient, cstore, negCache, logger, pullerPumpGate, c.CoordMaxConcurrentPulls, func(ctx context.Context, d digest.Digest) bool {
		return adv.Notify(ctx, d, true)
	}, originSuccessWithIngest, downstreamFailureWithIngest, leaseHooks)
	coordOpts := []coord.Option{
		coord.WithLogger(logger),
		coord.WithMetrics(coord.MetricsHooks{
			OnPullIntentServed:             func() { p3.coordPullIntentServed.Inc() },
			OnPullIntentStorageUnavailable: func() { p3.coordPullIntentStorageUnavailable.Inc() },
			OnPleasePullServed:             func() { p3.coordPleasePullServed.Inc() },
			OnPleasePullStarted:            func() { p3.coordPleasePullStarted.Inc() },
			OnPleasePullDeclined:           func() { p3.coordPleasePullDeclined.Inc() },
			OnStreamError:                  func() { p3.coordStreamError.Inc() },
		}),
		coord.WithNegativeCache(negCacheAdapter{c: negCache}),
		coord.WithPullerPump(pullerPump),
		coord.WithMaxDigestsPerPleasePull(c.CoordMaxDigestsPerRequest),
	}

	coordServer := coord.NewServer(cstore, inflightMap, coordOpts...)
	coordServer.Bind(disco.LibP2P())

	// cold-start orchestrator. Candidate pullers come from the DHT,
	// so it is always enabled.
	var (
		coldStartResolver mirror.ColdStartResolver
		layerPrefetcher   mirror.LayerPrefetcher
	)

	{
		realResolver := coldstart.New(coldstart.Options{
			Self:                        selfNodeID,
			Discovery:                   disco,
			Coord:                       coordClient,
			Inflight:                    inflightMap,
			Logger:                      logger,
			TopK:                        c.TopK,
			LocalIntent:                 coordServer,
			LocalPull:                   coordServer,
			PrefetchPullerReplicas:      c.PrefetchPullerReplicas,
			PrefetchCoordinatorReplicas: c.PrefetchCoordinatorReplicas,
			PrefetchMaxConcurrentGroups: c.PrefetchMaxConcurrentGroups,
			PrefetchDispatchJitter:      c.PrefetchDispatchJitter,
			TransientCooldownCap:        c.OriginFailureHonorWindowCap,
			TopKExpansionFactor:         c.TopKExpansionFactorDegraded,
			TrustedFailureClasses:       parseTrustedFailureClasses(c.OriginFailureClassesTrustedClusterWide, logger),
			Metrics: coldstart.MetricsHooks{
				OnDhtFalseEmpty: func() { p3.dhtFalseEmpty.Inc() },
				OnTopKProbeHit:  func() { p3.topkProbeHit.Inc() },
				OnColdStartDuration: func(kindLabel, outcome string, d time.Duration) {
					p3.coldStartDuration.WithLabelValues(kindLabel, outcome).Observe(d.Seconds())
				},
				OnDesignatedPullerTakeover: func(kindLabel string) {
					p4.designatedPullerTakeoverTotal.WithLabelValues(kindLabel).Inc()
				},
				OnTopKExpansion: func(reason string) {
					p5.topkExpansionTotal.WithLabelValues(reason).Inc()
				},
				OnPrefetchBatch: func(pullers, digests int) {
					p3.prefetchBatchesTotal.Inc()
					p3.prefetchDigestsTotal.Add(float64(digests))
					p3.prefetchPullersPerBatch.Observe(float64(pullers))
				},
				OnPrefetchGroup: func(target, outcome string) {
					p3.prefetchGroupsTotal.WithLabelValues(target, outcome).Inc()
				},
			},
		})
		coldStartResolver = coldStartAdapter{r: realResolver}
		layerPrefetcher = newLayerPrefetcher(realResolver, cstore, logger, layerProgress.observeManifest)
		logger.Info("cold-start orchestrator wired", slog.Int("top_k", c.TopK))
	}

	// - direct-origin-fallback controller (the design doc).
	var nf5Ctrl *mirror.DirectOriginFallbackController

	{
		monitor := disco.Monitor()
		nf5Ctrl = mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
			Logger:           logger,
			JitterBase:       c.NF5JitterBase,
			JitterCap:        c.NF5JitterCap,
			PerNodeRateLimit: c.NF5PerNodeRateLimit,
			// Use bootstrapPeerCount (Running pods with published
			// p2p-addrs annotations, irrespective of Ready). direct-origin-fallback
			// jitter spreads thundering-herd risk across all pods
			// that *could* race to origin, not just the Ready-set.
			// During a cold rollout the Ready set is often zero -
			// using Snapshot (Ready-only) would size the jitter
			// window as if the cluster were size 1, collapsing the
			// per-cluster random delay window to zero and routing
			// the entire cluster into origin at the same instant,
			// the exact thundering-herd direct-origin-fallback exists to prevent.
			ClusterSize: func() int {
				return c.Rendezvous.FallbackNodeUpperBound
			},
			InBootstrap: func() bool {
				if monitor == nil {
					return false
				}

				return monitor.InBootstrapWindow(c.BootstrapWindow, c.BootstrapRoutingTablePct)
			},
			HealthyEnough: func() bool {
				// Decline direct-origin-fallback when DHT is Unhealthy (<0.3). The empty
				// DHT answer can't be trusted, and we'd rather 5xx and
				// let kubelet back off than thunder the origin.
				return disco.Health() >= 0.3
			},
			Inflight: inflightMap,
			Recheck: func(ctx context.Context, d digest.Digest) bool {
				// Final post-jitter probe: did anyone publish a
				// provider record while we slept? If so, direct-origin-fallback declines
				// and the client retries through the warm path on its
				// next attempt.
				rcCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
				defer cancel()

				prov, err := disco.FindProviders(rcCtx, d)

				return err == nil && len(prov) > 0
			},
			OnFallback: func() { p5.originFallbackTotal.Inc() },
			OnDecline: func(reason string) {
				p5.originFallbackDeclineTotal.WithLabelValues(reason).Inc()
			},
		})
		logger.Info("NF5 origin-fallback wired",
			slog.Duration("jitter_base", c.NF5JitterBase),
			slog.Int("per_node_rate_limit", c.NF5PerNodeRateLimit),
			slog.Duration("bootstrap_window", c.BootstrapWindow),
		)
	}

	streamCommitTracker := newStreamCommitTracker(containerdInv, logger,
		func(n int) { p9.containerdCommitObserved.Add(float64(n)) },
		func(duration time.Duration) {
			p9.containerdCommitObserveDur.Observe(duration.Seconds())
			p9.containerdCommitLatestDur.Set(duration.Seconds())
			p9.containerdCommitObservedAt.SetToCurrentTime()
		},
		func(n int) { p9.commitMissingAfterStream.Add(float64(n)) },
	)

	go func() {
		if err := streamCommitTracker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("stream-commit tracker exited", slog.Any("err", err))
		}
	}()

	// Mirror with peer fallback. Live cache-miss requests run in
	// stream-through mode: the mirror proxies bytes directly to the local
	// containerd client and the tracker above correlates completed
	// responses with later containerd inventory observations.
	mirrorSrv := mirror.New(c, cstore, mirrorOriginClient,
		mirror.WithLogger(logger),
		mirror.WithLiveStreamThrough(),
		mirror.WithMetrics(
			func() { inst.cacheHit.Inc() },
			func() { inst.cacheMiss.Inc() },
		),
		mirror.WithByteMetrics(
			func(kind, source string, bytes int64) {
				p2.mirrorServeBytes.WithLabelValues(kind, source).Add(float64(bytes))
			},
		),
		mirror.WithMirrorResponseCompletedHook(func(d digest.Digest, kind, source string) {
			p2.mirrorCompletedAt.WithLabelValues(kind, source).SetToCurrentTime()
			layerProgress.completed(d)
		}),
		mirror.WithOriginStreamMetrics(
			func(kind string) { p9.originStreamStarted.WithLabelValues(kind).Inc() },
			func(kind string) { p9.originStreamCompleted.WithLabelValues(kind).Inc() },
			func(kind string) { p9.originStreamFailed.WithLabelValues(kind).Inc() },
		),
		mirror.WithLiveStreamCompletedHook(func(d digest.Digest) {
			streamCommitTracker.RecordCompleted(d)
			// Eagerly advertise as soon as the local containerd commit is
			// visible so a node that just finished a live stream-through
			// becomes a discoverable peer provider within milliseconds,
			// instead of waiting for the periodic advertiser reconcile or
			// the next containerd image event. The stream-commit tracker
			// above remains the correctness backstop.
			go advertiseOnCommit(ctx, adv, cstore, d, logger)
		}),
		mirror.WithDiscovery(disco, peerClient),
		mirror.WithPeerBudgets(0, c.PeerFetchTimeout, 0),
		mirror.WithPeerRediscover(c.PeerRediscoverBudget, c.PeerRediscoverBackoff),
		mirror.WithSelfNodeID(selfNodeID),
		mirror.WithSelfPeerID(ifaces.NodeID(disco.PeerID().String())),
		mirror.WithPeerMetrics(
			func(outcome string) {
				p2.peerFetch.WithLabelValues(outcome).Inc()

				if outcome == "busy" || outcome == "stall" {
					p2.peerFetchLastAt.WithLabelValues(outcome).SetToCurrentTime()
				}
			},
			func(success bool) {
				if success {
					p2.peerDialSuccess.Inc()
				} else {
					p2.peerDialFailure.Inc()
				}
			},
		),
		mirror.WithPeerFetchLatencyMetric(func(outcome string, d time.Duration) {
			p2.peerFetchDur.WithLabelValues(outcome).Observe(d.Seconds())
		}),
		mirror.WithDhtLookupMetric(func(outcome string, dur time.Duration) {
			p2.dhtLookup.WithLabelValues(outcome).Inc()
			p2.dhtLookupDur.WithLabelValues(outcome).Observe(dur.Seconds())
		}),
		mirror.WithProvideErrorMetric(func(op string) {
			p2.dhtProvideErr.WithLabelValues(op).Inc()
		}),
		mirror.WithDhtStaleOnlyMetric(func() {
			p9.dhtStaleOnly.Inc()
		}),
		mirror.WithStaleProviderFilteredMetric(func(n int) {
			p9.staleProviderFiltered.Add(float64(n))
		}),
		mirror.WithColdStart(coldStartResolver),
		mirror.WithLayerPrefetcher(layerPrefetcher),
		mirror.WithNF5(nf5Ctrl),
		// the design doc negative-cache integration for the mirror's direct-
		// origin path (hardening). Before this
		// option existed, mirror-direct origin failures (including
		// the direct-origin-fallback fallback) bumped p2p_origin_pull_failure_total
		// but did NOT seed a recently_failed cooldown - so the next
		// direct-origin-fallback-eligible request could re-fire the direct origin pull
		// at the bottom of the next jitter window, retry-amplifying
		// against an origin that just stalled or digest-mismatched.
		// negCacheRecorderAdapter mirrors what runOriginPull (the
		// please_pull-coordinated path) already does via
		// recordOriginFailure + neg.RecordSuccess.
		mirror.WithNegativeCacheRecorder(mirrorNegCacheRecorder{neg: negCache, lg: logger}),
		// the startup gate: /v2/ returns 503 until the mirror
		// is explicitly marked ready below. Without this gate
		// containerd's hostPort connection lands on the listener the
		// moment ListenAndServe returns - well before members
		// informer sync, DHT routing-table convergence, self-
		// announce, and cache scan complete. Every startup-window
		// /v2/ pull would race those subsystems and route to origin
		// instead of through the coordinated cold-start path,
		// silently breaking the cache-hit 'one origin pull per digest'
		// invariant for the duration of the rollout window.
		mirror.WithStartupReadinessGate(),
	)

	mirrorStop, err := mirrorSrv.ListenAndServe(c.MirrorListen)
	if err != nil {
		return fmt.Errorf("mirror listen: %w", err)
	}

	logger.Info("mirror endpoint listening", slog.String("addr", c.MirrorListen))

	// /4 - cdsub event loop. cdsub no longer calls DHT.Provide
	// directly in containerd mode; every event is routed through the
	// advertiser so one component owns the announced set and delete
	// events can trigger best-effort Withdraw.
	cdSub := cdsub.New(cdsubSrc, nil,
		cdsub.WithLogger(logger),
		cdsub.WithNotifier(func(ctx context.Context, d digest.Digest, present bool) {
			adv.Notify(ctx, d, present)
		}),
		cdsub.WithMetrics(
			nil,
			nil,
			func(int) { p2.dhtReconcile.Inc() },
			func() { p2.cdsubReconnect.Inc() },
		),
	)
	// cdsubDone is buffered so the goroutine can deposit Run's error
	// without blocking even if no select reads it. We ALSO close the
	// channel after the send so the second select below (line ~684,
	// the shutdown drain) returns immediately on every path:
	//
	// - Common case: signal-driven shutdown. ctx is cancelled
	// here, cdSub.Run returns, the goroutine sends-then-closes,
	// the second select reads the buffered value and proceeds.
	// - Rare case: cdsub crashes early. The FIRST select below
	// (line ~648) drains the buffered error, then the second
	// select arrives at an empty-but-closed channel and reads
	// the zero-value immediately, instead of blocking until the
	// 10s shutdown budget expires and emitting a spurious
	// "cdsub did not drain within shutdown budget" warning.
	//
	// Closing a buffered channel after a single send is safe: only
	// this goroutine sends, and it sends exactly once before close.
	// Receiving from a closed channel is always non-blocking and
	// always allowed, so multiple consumers (the two selects) are
	// fine.
	cdsubDone := make(chan error, 1)

	go func() {
		cdsubDone <- cdSub.Run(ctx)

		close(cdsubDone)
	}()

	// - re-announce digests we already serve so peers can discover
	// content held over from a previous boot. The advertiser runs a
	// continuous reconcile loop against containerdstore.Inventory which
	// both re-announces on boot and drains stale records over time
	// (Plan ).
	go func() {
		if err := adv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("advertise: loop exited with error", slog.Any("err", err))
		}
	}()

	logger.Info("advertise: containerd-inventory reconcile loop started")

	// Plan periodic sweep of expired Gantry-owned leases.
	// Without this the lease catalogue grows monotonically (we never
	// delete on the success path because we don't know when kubelet's
	// Image reference will take over). Containerd's own gc.expire
	// label gets the bytes back, but the lease metadata itself only
	// goes away when we Delete it.
	if c.ContainerdLeaseCleanupInterval > 0 {
		interval := c.ContainerdLeaseCleanupInterval
		// Startup sweep - required by "startup lists
		// old Gantry leases and removes/cleans leases older than
		// TTL". Without this the previous process's expired leases
		// linger up to one full cleanup interval after restart,
		// during which they hold bytes that containerd's GC would
		// otherwise reclaim. Run inline (not goroutine) so the
		// agent's first reconcile sees the post-sweep state.
		startupCtx, startupCancel := context.WithTimeout(ctx, 30*time.Second)
		if n, err := cdstore.CleanupExpiredLeases(startupCtx); err != nil {
			p9.containerdLeaseCleanupErr.Inc()
			logger.Warn("containerd startup lease sweep failed", slog.Any("err", err))
		} else {
			p9.containerdLeaseReleased.Add(float64(n))
			logger.Info("containerd startup lease sweep",
				slog.Int("released", n),
				slog.Duration("ttl", c.ContainerdLeaseTTL),
			)
		}

		startupCancel()

		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					n, err := cdstore.CleanupExpiredLeases(sweepCtx)

					cancel()

					if err != nil {
						p9.containerdLeaseCleanupErr.Inc()
						logger.Warn("containerd lease cleanup failed", slog.Any("err", err))

						continue
					}

					if n > 0 {
						p9.containerdLeaseReleased.Add(float64(n))
						logger.Info("containerd lease cleanup", slog.Int("deleted", n))
					}
					// Refresh the active-leases gauge after each
					// sweep. List runs the same filter the sweep
					// uses, so any race window is bounded by the
					// sweep interval - not perfect, but good
					// enough for trend dashboards.
					listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
					active, lerr := cdstore.ListManagedLeases(listCtx)

					listCancel()

					if lerr == nil {
						p9.containerdLeaseActive.Set(float64(len(active)))
					}
				}
			}
		}()

		logger.Info("containerd lease cleanup loop started",
			slog.Duration("interval", interval),
			slog.Duration("ttl", c.ContainerdLeaseTTL),
		)
	}

	// cache.Open runs synchronously above, but retain an explicit gate so
	// the startup dependency remains visible in readiness diagnostics.
	var cacheReady atomic.Bool

	cacheReady.Store(true)

	// Readiness is an ordered list of gates. Order is contract, not
	// style: when several gates are unsatisfied at once the first one
	// supplies the reported reason, so it decides which cause an
	// operator is shown. See TestReadinessGateOrder.
	gates := []readinessGate{
		{
			reason: "cache scan not complete",
			ready:  func() bool { return cacheReady.Load() },
		},
		{
			// Peers cannot be discovered until this agent has joined the
			// DHT. Reported before the address gate because an agent with
			// no peer has nothing to advertise to.
			reason: "lease rendezvous has no connected DHT peer",
			ready:  rendezvousBootstrap.IsReady,
		},
		{
			reason: "libp2p has no dialable advertised address",
			ready:  func() bool { return len(disco.Addrs()) > 0 },
		},
		{
			// Wildcard listen on the wrong family produces an undialable
			// advertised transfer address; peers' transfer pulls would all
			// connection-refused. Fix: align transfer_listen with the Pod's
			// IP family (use `[::]:port` on v6-only / dual-stack clusters,
			// `0.0.0.0:port` on v4-only clusters, or `:port` to let Go open
			// a dual-stack socket on Linux).
			reason: "transfer listener family mismatches Pod IP; check transfer_listen vs Pod IP family",
			ready:  func() bool { return !noDialableTransferAddr.Load() },
		},
		{
			reason: "containerd content store unavailable",
			ready: func() bool {
				pingCtx, pingCancel := context.WithTimeout(ctx, time.Second)
				defer pingCancel()

				return cdstore.Ping(pingCtx) == nil
			},
		},
	}

	readyCheck := func() (string, bool) { return firstUnreadyGate(gates) }

	// the startup mirror gate: poll readyCheck until it returns
	// green once, then flip the mirror's startup gate to "serving".
	// Sticky on the mirror side - a subsequent /readyz flap does not
	// take the mirror back out of service (Drain handles graceful
	// shutdown separately). 250ms polling is a tradeoff between
	// startup latency and CPU; readyCheck is a handful of atomic
	// loads + one cheap routing-table-size call.
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, ok := readyCheck(); ok {
					mirrorSrv.MarkReady()
					logger.Info("mirror: startup gate released; /v2/ now serving")

					return
				}
			}
		}
	}()

	// - operations HTTP listener (/metrics + probes). See
	// agent_readiness.go for the full handler wiring.
	metricsHTTP, metricsErr := startOpsEndpoint(c.MetricsListen, reg, readyCheck, logger)

	var pprofHTTP *http.Server

	if c.PprofListen != "" {
		startedPprofHTTP, pprofErr, pprofListenErr := startPprofEndpoint(c.PprofListen, logger)
		if pprofListenErr != nil {
			logger.Warn("pprof endpoint unavailable", slog.Any("err", pprofListenErr))
		} else {
			pprofHTTP = startedPprofHTTP

			go func() {
				if pprofServeErr, ok := <-pprofErr; ok && pprofServeErr != nil {
					logger.Warn("pprof endpoint died", slog.Any("err", pprofServeErr))
				}
			}()
		}
	}

	// Block until signal or an essential background server crashes. Profiling
	// is diagnostic and never owns data-plane availability.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-metricsErr:
		logger.Error("metrics endpoint died", slog.Any("err", err))
	case err := <-cdsubDone:
		logger.Error("cdsub loop exited unexpectedly", slog.Any("err", err))
	}

	// Graceful shutdown . See agent_shutdown.go for the
	// drain order and rationale.
	gracefulShutdown(shutdownDeps{
		logger:         logger,
		mirrorSrv:      mirrorSrv,
		transferStop:   transferStop,
		mirrorStop:     mirrorStop,
		cdsubSrc:       cdsubSrc,
		cdsubDone:      cdsubDone,
		coordStop:      func() { coordServer.Unbind(disco.LibP2P()) },
		pullerPumpGate: pullerPumpGate,
		metricsHTTP:    metricsHTTP,
		pprofHTTP:      pprofHTTP,
		shutdownBudget: 10 * time.Second,
	})
	logger.Info("gantry stopped")

	return nil
}

// loadAgentConfig merges YAML, env, and flags into a *config.Config. Two-
// pass parsing: first pass reads --config; second pass overlays flags onto
// (defaults < YAML < env).
func loadAgentConfig(args []string) (*config.Config, error) {
	c := config.NewDefault()
	fs, configPath := buildAgentFlagSet(c)
	fs.SetOutput(os.Stderr)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *configPath != "" {
		c2, _, err := config.Load(args, os.Getenv, *configPath)
		if err != nil {
			return nil, err
		}

		return c2, nil
	}

	if err := c.LoadEnv(os.Getenv); err != nil {
		return nil, err
	}

	fs, _ = buildAgentFlagSet(c) //nolint:errcheck // best-effort in test
	fs.SetOutput(os.Stderr)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	return c, nil
}

// parseTrustedFailureClasses converts the string-form config slice
// (`origin_failure_classes_trusted_cluster_wide`) to the typed
// ifaces.FailureClass values consumed by the cold-start rule-1
// short-circuit. Unknown class names are logged and dropped; an
// empty / all-unknown result lets coldstart.New fall back to its
// default {auth, not_found, rate_limited}.
func parseTrustedFailureClasses(raw []string, logger *slog.Logger) []ifaces.FailureClass {
	if len(raw) == 0 {
		return nil
	}

	known := map[string]ifaces.FailureClass{
		string(ifaces.FailureAuth):        ifaces.FailureAuth,
		string(ifaces.FailureNotFound):    ifaces.FailureNotFound,
		string(ifaces.FailureRateLimited): ifaces.FailureRateLimited,
		string(ifaces.FailureTransient):   ifaces.FailureTransient,
	}

	out := make([]ifaces.FailureClass, 0, len(raw))
	for _, s := range raw {
		if fc, ok := known[s]; ok {
			out = append(out, fc)
			continue
		}

		if logger != nil {
			logger.Warn("config: unknown origin_failure_classes_trusted_cluster_wide entry; dropped",
				slog.String("value", s),
			)
		}
	}

	return out
}

// transferAddrFamilyMismatch reports whether the transfer listener is
// wildcard-bound to a single IP family that does not match the pod's
// IP family - a misconfiguration that produces an undialable
// advertised address and must fail /readyz so the rollout halts
// instead of silently shipping a broken pod.
//
// True iff: listen host is `0.0.0.0` or `::`, podIP is non-empty,
// and the listener's family != podIP family. The empty-host case
// (`:port`) is Go's dual-stack default on Linux and is never a
// mismatch. The non-K8s path (podIP == "") is never a mismatch
// because advertising nothing is the intended behaviour there.
//
// The discovery address factory applies the corresponding family check to
// advertised libp2p addresses.
func transferAddrFamilyMismatch(transferListen, podIP string) bool {
	if podIP == "" {
		return false
	}

	host, _, err := net.SplitHostPort(transferListen)
	if err != nil {
		return false
	}

	ip := net.ParseIP(podIP)
	if ip == nil {
		return false
	}

	switch host {
	case "0.0.0.0":
		return ip.To4() == nil // pod is v6, listener is v4
	case "::":
		return ip.To4() != nil // pod is v4, listener is v6
	}

	return false
}

// coldStartAdapter bridges *coldstart.Resolver to mirror.ColdStartResolver
// without forcing the mirror package to import internal/coldstart.
type coldStartAdapter struct{ r *coldstart.Resolver }

func (a coldStartAdapter) Resolve(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64) (*mirror.ColdStartResolution, error) {
	res, err := a.r.Resolve(ctx, d, kind, registry, repository, expectedSize)
	if err != nil {
		// Translate the cold-start cascade-exhausted sentinel to the
		// mirror-package sentinel that direct-origin-fallback fallback gates on. Other
		// cold-start errors (failure short-circuit, transient
		// cooldown) are deliberately not translated so the mirror
		// treats them as opaque 5xx - direct-origin-fallback cannot fire on them.
		if errors.Is(err, coldstart.ErrExhausted) {
			return nil, mirror.ErrColdStartExhausted
		}

		return nil, err
	}

	return &mirror.ColdStartResolution{Providers: res.Providers, Outcome: res.Outcome}, nil
}

// layerPrefetchAdapter implements mirror.LayerPrefetcher: after a
// manifest serve it reads the manifest body back from cache, extracts
// the child layer/config digests, filters out digests already in the
// local cache, and asks the cold-start resolver to issue batched
// please_pull RPCs grouped by closest-peer puller.
//
// The implementation runs in a goroutine spawned by the mirror; it
// MUST NOT panic. All errors are logged at DEBUG.
type layerPrefetchAdapter struct {
	resolver   *coldstart.Resolver
	cache      ifaces.LocalContentStore
	logger     *slog.Logger
	onManifest func(digest.Digest, []manifest.TypedChild)
}

// maxManifestBytes caps the size of a manifest body the prefetcher
// is willing to parse. OCI Distribution recommends manifests stay
// well under 4 MiB; a body larger than that almost certainly indicates
// a misconfigured upstream (or attack), and we'd rather skip prefetch
// than allocate a multi-MB buffer per manifest serve.
const maxManifestBytes int64 = 4 * 1024 * 1024

// advertiseOnCommit eagerly advertises d on the DHT as soon as the
// local containerd content store can serve it, so a node that just
// finished a live stream-through becomes a discoverable peer provider
// within milliseconds instead of waiting for the periodic advertiser
// reconcile or the next containerd image event. It polls Has briefly to
// cover the short window between the mirror stream completing and
// containerd finalizing the commit; adv.Notify re-verifies the digest is
// openable before Provide, so a premature call is a harmless no-op. The
// periodic reconcile remains the backstop, and a requester that races
// the pre-commit window simply falls back to another provider or origin.
func advertiseOnCommit(ctx context.Context, adv *advertise.Advertiser, store ifaces.LocalContentStore, d digest.Digest, logger *slog.Logger) {
	const (
		maxWait   = 5 * time.Second
		pollEvery = 150 * time.Millisecond
	)

	deadline := time.Now().Add(maxWait)

	for {
		if ctx.Err() != nil {
			return
		}

		hasCtx, cancel := context.WithTimeout(ctx, time.Second)
		has, err := store.Has(hasCtx, d)

		cancel()

		if err == nil && has {
			adv.Notify(ctx, d, true)
			return
		}

		if time.Now().After(deadline) {
			logger.Debug("advertise: eager post-stream advertise did not converge; reconcile will backstop",
				slog.String("digest", d.String()),
			)

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
}

func newLayerPrefetcher(
	r *coldstart.Resolver,
	cache ifaces.LocalContentStore,
	logger *slog.Logger,
	onManifest func(digest.Digest, []manifest.TypedChild),
) mirror.LayerPrefetcher {
	return &layerPrefetchAdapter{
		resolver:   r,
		cache:      cache,
		logger:     logger.With(slog.String("subsystem", "prefetch")),
		onManifest: onManifest,
	}
}

// openManifest reads the manifest body from the shared content store. Under
// live stream-through the mirror proxies the body straight to containerd, so
// it only appears once containerd commits it; retry briefly rather than
// dropping the prefetch and losing cold-start seeding entirely.
func (p *layerPrefetchAdapter) openManifest(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
	const attempts = 8

	delay := 100 * time.Millisecond

	var lastErr error

	for attempt := range attempts {
		rc, _, err := p.cache.Open(ctx, d)
		if err == nil {
			return rc, nil
		}

		lastErr = err

		if attempt == attempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}

		if delay < time.Second {
			delay *= 2
		}
	}

	return nil, lastErr
}

func (p *layerPrefetchAdapter) OnManifestServed(ctx context.Context, registry, repository string, manifestDigest digest.Digest) {
	if p.resolver == nil && p.onManifest == nil {
		return
	}
	// Use a fresh deadline so the prefetch survives the request
	// context that just finished; cap at 30s so a stuck prefetch
	// can't pin a goroutine forever.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rc, err := p.openManifest(ctx, manifestDigest)
	if err != nil {
		p.logger.Debug("prefetch: manifest not in cache",
			slog.String("digest", manifestDigest.String()),
			slog.Any("err", err),
		)

		return
	}

	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	_ = rc.Close() //nolint:errcheck // best-effort close

	if err != nil {
		p.logger.Debug("prefetch: manifest read failed",
			slog.String("digest", manifestDigest.String()),
			slog.Any("err", err),
		)

		return
	}

	if int64(len(body)) >= maxManifestBytes {
		// Likely truncated; refuse to parse.
		p.logger.Debug("prefetch: manifest exceeds size cap",
			slog.String("digest", manifestDigest.String()),
			slog.Int64("cap", maxManifestBytes),
		)

		return
	}

	children, err := manifest.TypedChildren(body)
	if err != nil {
		p.logger.Debug("prefetch: manifest parse failed",
			slog.String("digest", manifestDigest.String()),
			slog.Any("err", err),
		)

		return
	}

	if p.onManifest != nil {
		p.onManifest(manifestDigest, children)
	}

	if len(children) == 0 || p.resolver == nil {
		// Image index or no children - nothing to fan out.
		return
	}

	// Filter out digests already present locally; they don't need
	// prefetching. We carry the per-child Kind through the filter so
	// the downstream PrefetchChildren call can keep the
	// config-vs-layer distinction end-to-end (so the
	// p2p_origin_pull_total{kind="config"} bucket actually counts).
	pending := make([]coldstart.ChildDigest, 0, len(children))
	for _, c := range children {
		has, err := p.cache.Has(ctx, c.Digest)
		if err != nil {
			// Treat error as "unknown" - include the digest; the
			// puller's in-flight dedupe handles the case where it's
			// already there.
			pending = append(pending, coldstart.ChildDigest{Digest: c.Digest, Kind: c.Kind})
			continue
		}

		if !has {
			pending = append(pending, coldstart.ChildDigest{Digest: c.Digest, Kind: c.Kind})
		}
	}

	if len(pending) == 0 {
		return
	}

	if err := p.resolver.PrefetchManifestChildren(ctx, manifestDigest, pending, registry, repository); err != nil {
		p.logger.Debug("prefetch: PrefetchChildren reported errors",
			slog.String("manifest", manifestDigest.String()),
			slog.Int("children", len(pending)),
			slog.Any("err", err),
		)
	}
}

// newPullerPump returns the coord.PullerPump that backs inbound
// please_pull RPCs. Per the step 7, the pump's job is to dedupe via
// the in-flight map, kick off the origin pull on a background
// goroutine, and return promptly so the coord stream handler can
// reply with OUTCOME_STARTED or OUTCOME_ALREADY_PULLING.
//
// On success, the pulled bytes land in the local cache/containerd store,
// are reopened to prove serveability, and are then marked present through
// the advertiser so peer requesters can discover them through the warm path.
//
// On failure, the negative cache is consulted/updated:
// - Before starting an origin pull, the pump checks negCache for an
// active cooldown; if present, please_pull short-circuits with
// OUTCOME_RECENTLY_FAILED (cluster-wide propagation).
// - On terminal origin failure, the goroutine classifies via the
// *ifaces.OriginError wrapper and records the failure so the next
// pull_intent_query response surfaces recently_failed.
type leaseMetricHooks struct {
	onCreated  func()
	onReleased func()
}

type preIngestLeaseStore interface {
	CreateLease(ctx context.Context, d digest.Digest, registry, repository string) (*containerdstore.LeaseGuard, error)
}

type pullerPumpGate struct {
	mu        sync.Mutex
	accepting bool
	wg        sync.WaitGroup
}

func newPullerPumpGate() *pullerPumpGate {
	return &pullerPumpGate{accepting: true}
}

func (g *pullerPumpGate) TryAdd() bool {
	if g == nil {
		return true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.accepting {
		return false
	}

	g.wg.Add(1)

	return true
}

func (g *pullerPumpGate) Done() {
	if g == nil {
		return
	}

	g.wg.Done()
}

func (g *pullerPumpGate) StopAccepting() {
	if g == nil {
		return
	}

	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

func (g *pullerPumpGate) Wait() {
	if g == nil {
		return
	}

	g.wg.Wait()
}

func newPullerPump(infl *inflight.Map, originClient ifaces.OriginPuller, cstore ifaces.LocalContentStore, neg *negcache.Cache, logger *slog.Logger, gate *pullerPumpGate, maxConcurrentPulls int, markPresent func(ctx context.Context, d digest.Digest) bool, onOriginSuccess func(kind string, bytes int64), onDownstreamFailure func(kind, class string), leaseHooks leaseMetricHooks) coord.PullerPump {
	lg := logger.With(slog.String("subsystem", "puller-pump"))

	if maxConcurrentPulls < 1 {
		maxConcurrentPulls = 1
	}

	pullSem := make(chan struct{}, maxConcurrentPulls)

	return func(pumpCtx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) coord.PumpResult {
		// the design doc short-circuit: if we're inside a cooldown window, refuse
		// to start a new origin pull and surface the existing entry so
		// the requester gets recently_failed without round-tripping.
		if neg != nil {
			if e, ok := neg.Lookup(d); ok {
				// A credential-specific cooldown produced under another identity
				// is not authoritative for a request carrying its own token.
				if !isCredentialSpecificOriginFailure(e.Class) || registryauth.Authorization(pumpCtx) == "" {
					return coord.PumpResult{
						Status:        coord.PumpRecentlyFailed,
						CooldownUntil: e.CooldownUntil,
						FailureClass:  e.Class,
					}
				}
			}
		}
		// Cache short-circuit: a previously-completed pull leaves the
		// bytes in our cache/containerd store (and the digest in the DHT via
		// the advertiser). The next please_pull for the
		// same digest must NOT trigger a fresh origin pull - both the
		// in-flight registry and the negcache are empty by then, so
		// without this check we'd loop through runOriginPull on every
		// kubelet retry and inflate p2p_origin_pull_total by the retry
		// count (commonly 7-10 per stuck digest until ImagePullBackOff
		// converges). Surface ALREADY_PULLING with start=now so the
		// caller's cold-start cascade polls DHT once and finds us as a
		// provider, same flow as the in-flight case below.
		if cstore != nil {
			ctxHas, cancelHas := context.WithTimeout(pumpCtx, 100*time.Millisecond)
			has, hasErr := cstore.Has(ctxHas, d)

			cancelHas()

			if hasErr != nil {
				var unavailable *ifaces.ErrUnavailable
				if errors.As(hasErr, &unavailable) {
					lg.Warn("puller-pump: storage unavailable during already-present check",
						slog.String("digest", d.String()),
						slog.Any("err", hasErr),
					)
				} else {
					lg.Debug("puller-pump: cache Has failed during already-present check",
						slog.String("digest", d.String()),
						slog.Any("err", hasErr),
					)
				}
			}

			if has {
				return coord.PumpResult{Status: coord.PumpAlreadyPulling, StartedAt: time.Now()}
			}
		}

		if err := pumpCtx.Err(); err != nil {
			return coord.PumpResult{Status: coord.PumpDeclined}
		}

		// Dedupe at this node BEFORE reserving a fanout slot: if a pull is
		// already running, report ALREADY_PULLING with the existing start
		// time so the requester can run the stall check. A piggybacking
		// request starts no new work, so it must NOT be gated by the
		// concurrent-pull ceiling - otherwise a saturated node would wrongly
		// decline same-digest requests that should ride the in-flight pull.
		// This is a peek (no claim), so it leaves no entry behind.
		if existing, inflightNow := infl.LookupForIntent(d); inflightNow {
			return coord.PumpResult{Status: coord.PumpAlreadyPulling, StartedAt: existing.StartedAt}
		}

		// Reserve the right to start a NEW pull before claiming the inflight
		// entry. Claiming first and then declining (gate closed or fanout
		// saturated) would insert a transient entry and cancel it via
		// h.Done(); a concurrent please_pull for the same digest could observe
		// that entry as ALREADY_PULLING and then wait on a pull that never
		// actually starts.
		//
		// Refuse new work once graceful shutdown has closed the gate. The gate
		// also tracks outstanding pulls so shutdown can wait for the advertise
		// flush at the end of runOriginPull before closing the libp2p host
		// (graceful-shutdown contract).
		if !gate.TryAdd() {
			return coord.PumpResult{Status: coord.PumpDeclined}
		}

		// Bound please-pull fanout: if we're already at the concurrent-pull
		// ceiling, release the gate slot we just took and decline so the
		// requester falls through to another provider instead of queueing
		// unbounded origin fetches on this node.
		select {
		case pullSem <- struct{}{}:
		default:
			gate.Done()

			return coord.PumpResult{Status: coord.PumpDeclined}
		}

		// Atomically claim the inflight entry. LookupForIntent above was only
		// a peek, so a concurrent request for the same digest may have claimed
		// it in between; Start is the authoritative check. If we lost that
		// race, release the gate and fanout slots we reserved (the winner owns
		// the real pull) and report its in-flight entry.
		h, existing, already := infl.Start(d, kind, 0)
		if already {
			<-pullSem
			gate.Done()

			return coord.PumpResult{Status: coord.PumpAlreadyPulling, StartedAt: existing.StartedAt}
		}

		startedAt := existing.StartedAt
		pullCtx := registryauth.Detach(pumpCtx)

		// Detach the actual fetch from the stream handler. The pump returns
		// immediately; the goroutine owns the inflight handle, the gate slot,
		// and the fanout semaphore slot, releasing all three on exit.
		go func() {
			defer gate.Done()
			defer func() { <-pullSem }()

			runOriginPull(pullCtx, originClient, cstore, neg, lg, h, registry, repository, d, kind, markPresent, onOriginSuccess, onDownstreamFailure, leaseHooks)
		}()

		return coord.PumpResult{Status: coord.PumpStarted, StartedAt: startedAt}
	}
}

// runOriginPull executes an origin pull -> cache write -> reopen check -> advertiser mark-present
// pipeline for d. Caller owns the inflight handle and must arrange for
// Done to be called exactly once; we do that here on every exit path.
//
// the design doc wiring:
// - Terminal origin errors are classified via *ifaces.OriginError and
// recorded into the negative cache so the next probe surfaces
// recently_failed.
// - I/O / cache-side failures (copy + commit) are recorded as
// FailureTransient: they are not the origin's fault, but treating
// them as transient blocks the cluster from re-hammering the same
// puller on a flapping local disk while still self-healing.
// - On commit success, we clear any prior entry so the ladder resets
// for the next failure run.
func runOriginPull(baseCtx context.Context, originClient ifaces.OriginPuller, cstore ifaces.LocalContentStore, neg *negcache.Cache, lg *slog.Logger, h *inflight.Handle, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind, markPresent func(ctx context.Context, d digest.Digest) bool, onOriginSuccess func(kind string, bytes int64), onDownstreamFailure func(kind, class string), leaseHooks leaseMetricHooks) {
	defer h.Done()

	// Background context: the requesting peer's stream is already
	// closed by the time we get here. We bound the pull by a budget
	// so a hung origin can't leak the in-flight slot forever, but
	// the 5-minute fixed ceiling from earlier was too tight for
	// real-world image sizes (e.g. a 5 GB GPU image at the-default
	// 10 MB/s throughput floor needs ~8.5 min on its own). Start with
	// a default budget that covers HEAD/auth and small blobs, then
	// extend post-Pull once we know expectedSize.
	const (
		originPullDefaultBudget = 5 * time.Minute
		originPullMinThroughput = 10 * 1024 * 1024 // 10 MB/s, matches the 7 stall-detection floor
		originPullCeiling       = 30 * time.Minute // absolute ceiling so a stuck pull still releases the slot
	)

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	budget := time.AfterFunc(originPullDefaultBudget, cancel)
	defer budget.Stop()

	ref := ifaces.OriginRef{
		Registry:   registry,
		Repository: repository,
		Digest:     d,
		Kind:       kind,
	}

	rc, expectedSize, err := originClient.Pull(ctx, ref)
	if err != nil {
		// A delegated credential is requester-specific. Its origin failure
		// must not poison the digest-wide cache for another requester.
		recordOriginFailure(neg, d, err, lg, "origin pull failed", registry, repository, registryauth.Authorization(ctx) == "")
		return
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	// Extend the budget based on expectedSize / floor-throughput. The
	// default-budget slack is kept on top so the io.Copy starts with
	// at least originPullDefaultBudget of headroom regardless of size.
	if expectedSize > 0 {
		needed := time.Duration(expectedSize/originPullMinThroughput)*time.Second + originPullDefaultBudget
		if needed > originPullCeiling {
			needed = originPullCeiling
		}

		if needed > originPullDefaultBudget {
			budget.Reset(needed)
		}
	}

	var leaseGuard *containerdstore.LeaseGuard

	if leased, ok := cstore.(preIngestLeaseStore); ok {
		leaseCtx, leaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		guard, leaseErr := leased.CreateLease(leaseCtx, d, registry, repository)

		leaseCancel()

		if leaseErr != nil {
			recordOriginFailure(neg, d, leaseErr, lg, "containerd lease create failed", registry, repository, true)

			if onDownstreamFailure != nil {
				onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
			}

			return
		}

		leaseGuard = guard

		if leaseHooks.onCreated != nil {
			leaseHooks.onCreated()
		}
	}

	releaseLeaseOnFailure := func() {
		if leaseGuard == nil {
			return
		}

		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := leaseGuard.Release(releaseCtx); err != nil {
			lg.Warn("containerd lease release after failed ingest failed",
				slog.String("digest", d.String()),
				slog.Any("err", err),
			)
		} else if leaseHooks.onReleased != nil {
			leaseHooks.onReleased()
		}

		releaseCancel()
	}

	w, err := cstore.Writer(ctx, d)
	if err != nil {
		releaseLeaseOnFailure()
		recordOriginFailure(neg, d, err, lg, "cache writer open failed", registry, repository, true)
		// Origin returned 2xx (we got past originClient.Pull above)
		// but the cache writer couldn't open - terminal downstream
		// failure. Bump p2p_origin_pull_failure_total{class=transient}
		// so the per-pull arithmetic
		// (started == success + failure + in_flight) holds.
		// Origin's failure family is NOT bumped: origin gave us
		// 2xx, the failure is downstream.
		if onDownstreamFailure != nil {
			onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
		}

		return
	}

	defer func() { _ = w.Abort(ctx) }() //nolint:errcheck // best-effort abort

	written, err := io.Copy(w, rc)
	if err != nil {
		releaseLeaseOnFailure()
		recordOriginFailure(neg, d, err, lg, "origin pull copy failed", registry, repository, true)
		// io.Copy could have failed because origin truncated the
		// stream OR because the local cache writer errored. We
		// can't easily distinguish - but we already passed origin's
		// boundary (it returned 2xx), so we count this as a
		// downstream-class failure, same as the mirror path's
		// io.Copy-stalled bucket. Operators correlate with origin's
		// upstream health via other signals (DNS, TCP, registry
		// SLO).
		if onDownstreamFailure != nil {
			onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
		}

		return
	}

	if err := w.Commit(ctx); err != nil {
		releaseLeaseOnFailure()
		recordOriginFailure(neg, d, err, lg, "cache commit failed (digest mismatch or io error)", registry, repository, true)
		// Commit failure means EITHER the cache's internal
		// digestpipe caught a content mismatch (origin lied) OR
		// the local cache had an I/O error at finalize. Either
		// way it's a terminal downstream failure of this pull;
		// no usable cached copy exists.
		if onDownstreamFailure != nil {
			onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
		}

		return
	}

	reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 10*time.Second)
	rcCommitted, _, reopenErr := cstore.Open(reopenCtx, d)

	reopenCancel()

	if reopenErr != nil {
		// w.Commit succeeded; the bytes are in the content store and the
		// lease's Resource{ID: d, Type: "content"} binding is the only
		// thing keeping containerd GC from reaping them until kubelet
		// adopts the image. Release the lease ONLY when Open is
		// definitively NotFound (commit didn't land). Any other reopen
		// error (containerd hiccup, ctx deadline) is "we don't know" -
		// keep the lease so committed content survives; CleanupExpired
		// reclaims abandoned leases later. Reverse direction here turns
		// one transient origin pull into many under flapping.
		var notFound *ifaces.ErrNotFound
		if errors.As(reopenErr, &notFound) {
			releaseLeaseOnFailure()
		}

		recordOriginFailure(neg, d, reopenErr, lg, "cache reopen failed after commit", registry, repository, true)

		if onDownstreamFailure != nil {
			onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
		}

		return
	}

	_ = rcCommitted.Close() //nolint:errcheck // best-effort close

	if markPresent != nil {
		advCtx, advCancel := context.WithTimeout(context.Background(), 30*time.Second)
		advertised := markPresent(advCtx, d)

		advCancel()

		if !advertised {
			lg.Warn("advertise mark-present failed after commit",
				slog.String("digest", d.String()),
				slog.String("registry", registry),
				slog.String("repository", repository),
			)

			if onDownstreamFailure != nil {
				onDownstreamFailure(kind.MetricLabel(), string(ifaces.FailureTransient))
			}

			return
		}
	}

	// Origin pull SUCCEEDED on the please_pull-coordinated path:
	// the body streamed to completion, Commit passed, the digest
	// reopened successfully, and the advertiser accepted the
	// mark-present. Fire success only after all of those checks so the
	// background-origin metric means "usable and discoverable".
	if onOriginSuccess != nil {
		onOriginSuccess(kind.MetricLabel(), written)
	}

	// Success: clear any prior negative-cache entry so the next
	// failure starts the ladder from Initial again (the design doc "Self-healing").
	if neg != nil {
		neg.RecordSuccess(d)
	}

	lg.Info("please_pull served",
		slog.String("digest", d.String()),
		slog.String("registry", registry),
		slog.String("repository", repository),
	)
}

// recordOriginFailure classifies err and records the failure into the
// per-puller the design doc negative cache. Non-the design doc callers (e.g. cache I/O
// errors not covered by *ifaces.OriginError) are bucketed as
// FailureTransient: see runOriginPull's docs for why we still record
// them. The log is emitted at WARN regardless of class. recordCooldown is
// false for requester-specific origin failures that are unsafe to store in a
// digest-only shared cache.
func recordOriginFailure(neg *negcache.Cache, d digest.Digest, err error, lg *slog.Logger, msg, registry, repository string, recordCooldown bool) {
	class := ifaces.FailureTransient

	var oe *ifaces.OriginError
	if errors.As(err, &oe) && oe.Class != ifaces.FailureUnspecified {
		class = oe.Class
	}

	lg.Warn(msg,
		slog.String("digest", d.String()),
		slog.String("registry", registry),
		slog.String("repository", repository),
		slog.String("failure_class", string(class)),
		slog.Any("err", err),
	)

	if neg != nil && recordCooldown {
		neg.RecordFailure(d, class)
	}
}

func isCredentialSpecificOriginFailure(class ifaces.FailureClass) bool {
	switch class {
	case ifaces.FailureAuth, ifaces.FailureNotFound, ifaces.FailureRateLimited:
		return true
	default:
		return false
	}
}

// negCacheAdapter bridges *negcache.Cache to coord.NegativeCache.
// Required because internal/negcache must not import internal/coord
// (would cycle on the metric hooks the coord server uses).
type negCacheAdapter struct{ c *negcache.Cache }

func (a negCacheAdapter) Lookup(d digest.Digest) (coord.NegativeEntry, bool) {
	e, ok := a.c.Lookup(d)
	if !ok {
		return coord.NegativeEntry{}, false
	}

	return coord.NegativeEntry{
		CooldownUntil: e.CooldownUntil,
		Class:         e.Class,
	}, true
}

// mirrorNegCacheRecorder bridges *negcache.Cache to
// mirror.NegativeCacheRecorder for the mirror's direct-origin path.
// Symmetric with runOriginPull's recordOriginFailure +
// neg.RecordSuccess wiring on the please_pull-coordinated path:
// every terminal direct-origin failure (origin error / io.Copy stall
// / cw.Commit mismatch / directVerifier mismatch) seeds a cooldown,
// and every successful commit clears any prior entry so the ladder
// resets per the design doc "Self-healing".
//
// Logs at WARN on failure so the operator-facing log surface matches
// what recordOriginFailure already emits for the coordinated path.
// The mirror's own per-call logger already includes registry/repo/
// digest fields at WARN/Error/Debug; this adapter intentionally does
// not re-log the failure (the mirror's site-local log carries more
// context than we have here).
type mirrorNegCacheRecorder struct {
	neg *negcache.Cache
	lg  *slog.Logger
}

func (r mirrorNegCacheRecorder) RecordFailure(d digest.Digest, class ifaces.FailureClass) {
	if r.neg == nil {
		return
	}

	r.neg.RecordFailure(d, class)
}

func (r mirrorNegCacheRecorder) RecordSuccess(d digest.Digest) {
	if r.neg == nil {
		return
	}

	r.neg.RecordSuccess(d)
}
