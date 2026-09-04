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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/chairs"
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
	"github.com/Azure/unbounded/internal/gantry/members"
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

	selfHolder := chairSelfHolder(c, disco)

	var (
		chairClient   kubernetes.Interface
		chairStore    *chairs.Store
		chairCache    *chairs.Cache
		selfAnnounced atomic.Bool
	)

	noDialableP2PAddrs := len(selfHolder.P2PAddrs) == 0
	noDialableTransferAddr := transferAddrFamilyMismatch(c.TransferListen, c.PodIP)
	requireSelfAnnounce := c.PodName != "" && c.ChairNamespace != ""

	if c.ChairNamespace != "" {
		chairClient, err = chairs.NewClientset(c.MembersKubeconfig)
		if err != nil {
			return err
		}

		chairStore = chairs.NewStore(chairClient.CoordinationV1().Leases(c.ChairNamespace))
		chairCache = chairs.NewCache(chairStore)

		if requireSelfAnnounce {
			go announceLegacySelf(ctx, chairClient, c.ChairNamespace, c.PodName, selfHolder, logger, func() {
				selfAnnounced.Store(true)
			})
		}
	}

	const kademliaMaxRoutingTable = 256

	if monitor := disco.Monitor(); monitor != nil {
		target := c.ChairClusterSizeEstimate - 1
		if target < 0 {
			target = 0
		}

		if target > kademliaMaxRoutingTable {
			target = kademliaMaxRoutingTable
		}

		monitor.SetRoutingTableTarget(func() int { return target })
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

	coordClient := coord.NewClient(disco.LibP2P(),
		coord.WithClientLogger(logger),
		coord.WithClientMaxDigestsPerPleasePull(c.CoordMaxDigestsPerRequest),
	)

	var chairManager *chairs.Manager
	if chairStore != nil {
		chairManager = chairs.NewManager(chairs.ManagerOptions{
			Store:      chairStore,
			Cache:      chairCache,
			Self:       selfHolder,
			Rotation:   coordClient,
			Candidates: func() []chairs.Holder { return connectedChairCandidates(disco) },
			Connect: func(connectCtx context.Context, addresses []string) int {
				return disco.ConnectPeers(connectCtx, addresses)
			},
			Logger:              logger,
			LeaseDuration:       c.ChairLeaseDuration,
			RenewPeriod:         c.ChairRenewPeriod,
			RotationPeriod:      c.ChairRotationPeriod,
			RotationLead:        c.ChairRotationLead,
			StartupJitter:       c.ChairStartupJitter,
			ClaimRoundPeriod:    c.ChairClaimRoundPeriod,
			ClaimJitter:         c.ChairClaimJitter,
			ClaimInitialDivisor: uint64(c.ChairClaimInitialDivisor),
			APITimeout:          c.ChairAPITimeout,
			ClusterSizeEstimate: c.ChairClusterSizeEstimate,
		})
	}
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
			OnUnauthorizedPeer:             func(reason string) { p3.coordUnauthorizedPeer.WithLabelValues(reason).Inc() },
		}),
		coord.WithNegativeCache(negCacheAdapter{c: negCache}),
		coord.WithPullerPump(pullerPump),
		coord.WithPeerAuthz(c.CoordPeerAuthzEnforce),
		coord.WithRequireChairAssignment(c.CoordRequireChairAssignment),
		coord.WithMaxDigestsPerPleasePull(c.CoordMaxDigestsPerRequest),
	}
	if chairManager != nil {
		coordOpts = append(coordOpts,
			coord.WithChairValidator(chairManager),
			coord.WithChairSuccessor(chairManager),
		)
	}

	coordServer := coord.NewServer(cstore, nil, inflightMap, coordOpts...)
	coordServer.Bind(disco.LibP2P())

	if chairManager != nil {
		go chairManager.Run(ctx)
	}

	// Lease-chair cold-start is enabled in Kubernetes mode. Local development
	// without a chair namespace keeps the direct mirror path.
	var (
		coldStartResolver mirror.ColdStartResolver
		layerPrefetcher   mirror.LayerPrefetcher
	)

	if chairManager != nil {
		realResolver := coldstart.NewChairResolver(coldstart.ChairOptions{
			Chairs:       chairCache,
			Discovery:    disco,
			Coord:        coordClient,
			LocalPull:    coordServer,
			Inflight:     inflightMap,
			SelfPeerID:   ifaces.NodeID(disco.PeerID().String()),
			CurrentEpoch: chairManager.CurrentEpoch,
			InstallHolder: func(holder chairs.Holder) error {
				return installChairHolder(disco.LibP2P().Peerstore(), holder)
			},
			Claimer:               chairManager,
			Logger:                logger,
			APITimeout:            c.ChairAPITimeout,
			TrustedFailureClasses: configuredFailureClasses(c.OriginFailureClassesTrustedClusterWide),
		})
		coldStartResolver = coldStartAdapter{r: realResolver}
		layerPrefetcher = newLayerPrefetcher(realResolver, cstore, logger, layerProgress.observeManifest)
		logger.Info("Lease-chair cold-start orchestrator wired",
			slog.Int("chairs", chairs.Count),
			slog.Int("seeds", chairs.SeedCount),
		)
	} else {
		logger.Info("Lease-chair cold-start orchestrator disabled (no Kubernetes namespace configured)")
	}

	if layerPrefetcher == nil {
		layerPrefetcher = newLayerPrefetcher(nil, cstore, logger, layerProgress.observeManifest)
	}

	// - direct-origin-fallback direct-origin fallback controller (the design doc). Wired
	// only when the cold-start resolver is also wired; without
	// orchestration there is no `ErrColdStartExhausted` path to gate.
	var nf5Ctrl *mirror.DirectOriginFallbackController

	if coldStartResolver != nil {
		monitor := disco.Monitor()
		nf5Ctrl = mirror.NewDirectOriginFallback(mirror.DirectOriginFallbackOptions{
			Logger:           logger,
			JitterBase:       c.NF5JitterBase,
			JitterCap:        c.NF5JitterCap,
			PerNodeRateLimit: c.NF5PerNodeRateLimit,
			// No node watch exists in chair mode, so size the fallback jitter
			// from the operator's cluster estimate. The shipped 100,000-node
			// value remains conservative during partial rollouts.
			ClusterSize: func() int { return c.ChairClusterSizeEstimate },
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
		mirror.WithSelfNodeID(ifaces.NodeID(disco.PeerID().String())),
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
		// moment ListenAndServe returns - well before chair startup,
		// DHT routing-table convergence, self-announce, and cache scan complete.
		// Every startup-window
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
	// - Common case: signal-driven shutdown. ctx is canceled
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
	// Without this the lease catalog grows monotonically (we never
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

	// Readiness is API-free: the chair manager updates its local snapshot from
	// startup/claim activity and pull-time epoch refreshes. Probes never list
	// Leases, which avoids a synchronized 100,000-node API burst.
	var cacheReady atomic.Bool
	cacheReady.Store(true)

	readyCheck := func() (string, bool) {
		if !cacheReady.Load() {
			return "cache scan not complete", false
		}

		if chairManager != nil && !chairManager.Ready() {
			return "fewer than eight Lease chairs are occupied", false
		}

		if requireSelfAnnounce && !selfAnnounced.Load() {
			// Production + dynamic bootstrap: peers cannot
			// discover us until our pods/patch lands. Staying
			// 503 until then makes the rolling deploy pause and
			// surfaces an RBAC misconfiguration immediately.
			return "rolling-upgrade self-announce pending (check pods/patch RBAC)", false
		}

		if requireSelfAnnounce && noDialableP2PAddrs {
			// Patch went through but the published P2PAddrs list
			// was empty: every disco.Addrs entry was a
			// wildcard the rewrite-to-PodIP couldn't rewrite.
			// Peers see our annotation but cannot dial us, so
			// coord RPCs all fail - fail the readiness probe
			// rather than ship a silently-isolated agent. Fix:
			// align libp2p_listen with the Pod's IP family.
			return "self-announce has no dialable p2p addresses; check libp2p_listen vs Pod IP family", false
		}

		if requireSelfAnnounce && noDialableTransferAddr {
			// Same hazard, transfer-endpoint flavor. Wildcard
			// listen on the wrong family produces an undialable
			// advertised transfer address; peers' transfer pulls
			// would all connection-refused. Fix: align
			// transfer_listen with the Pod's IP family (use
			// `[::]:port` on v6-only / dual-stack clusters,
			// `0.0.0.0:port` on v4-only clusters, or `:port` to
			// let Go open a dual-stack socket on Linux).
			return "transfer listener family mismatches Pod IP; check transfer_listen vs Pod IP family", false
		}

		if chairManager != nil && c.ChairClusterSizeEstimate > 1 && disco.RoutingTableSize() < 1 {
			return "dht routing table empty", false
		}

		pingCtx, pingCancel := context.WithTimeout(ctx, time.Second)
		pingErr := cdstore.Ping(pingCtx)

		pingCancel()

		if pingErr != nil {
			return "containerd content store unavailable", false
		}

		return "", true
	}

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

// isProductionMode reports whether the caller has set any of the
// Kubernetes-Downward-API signals that imply the agent is running
// inside a real cluster (DaemonSet wiring sets all three via
// metadata.name, spec.nodeName, and a fixed Namespace env var). When
// true, a single-self membership fallback is unsafe because the
// operator believes peer coordination is active.
func isProductionMode(c *config.Config) bool {
	return c.NodeName != "" || c.PodName != "" || c.MembersNamespace != ""
}

// selfAnnounceRequiredForReadiness reports whether a successful
// pods/patch self-announce must precede the agent reporting Ready.
// True when production-mode K8s membership is wired AND the agent
// has its own pod name.
//
// Static-bootstrap peers do NOT bypass this gate. Bootstrap peers
// solve *DHT seeding* - they help kademlia discover other peers'
// addresses. They do not solve the membership-ID -> libp2p peer-ID
// mapping problem.
//
// In Kubernetes mode each pod's K8s node name (e.g. "ip-10-0-0-7")
// is its membership identity. The peer-ID (e.g. "12D3Koo…"), the
// p2p multiaddrs, and the transfer-port hostport are published on
// the agent's own pod via three annotations:
//
//	gantry.io/peer-id
//	gantry.io/p2p-addrs
//	gantry.io/transfer-addr
//
// Other agents read those annotations off the pod-informer cache to
// translate a node-name membership entry into the libp2p
// peer-ID/addr pair that Coord.PleasePull / PullIntentQuery actually
// dial. If the agent never publishes them - because pods/patch RBAC
// is broken, or the apiserver is unreachable on first attempt and
// we never retry - other agents see the K8s node name in HRW
// membership, fail to translate it, and cold-start RPCs to this
// node 503 silently. Static bootstrap peers cannot rescue that
// case: they are unrelated to per-pod annotation publication.
//
// PodName is still part of the gate because without it
// AnnounceSelf has nothing to patch - that is the dev-mode /
// docker-run scenario where K8s membership isn't expected anyway.
func selfAnnounceRequiredForReadiness(c *config.Config) bool {
	return isProductionMode(c) && c.PodName != ""
}

// hasMultiNodeMembership reports whether cold-start coordination
// should be enabled. Previously this checked Snapshot for any non-
// self entry, which deadlocked first-cluster boot: on a fresh
// cluster no peer is Ready yet, Snapshot returns just self, cold-
// start was disabled for the whole process lifetime, and the agent
// silently degraded to direct-origin pulls forever - the exact
// scenario cold-start is most needed for.
//
// Cold-start is now enabled whenever the membership view is backed
// by the real Kubernetes informer (*members.Manager). The single-
// self fake is the only mode that disables it: that mode is for
// dev/test runs with no cluster at all, where there are no peers
// to coordinate with by definition.
//
// The orchestrator itself handles an empty peer view internally
// (direct-origin-fallback / ErrColdStartExhausted fall-through), so it does not need
// a populated snapshot at construction time.
func hasMultiNodeMembership(m ifaces.Members) bool {
	_, isManager := m.(*members.Manager)
	return isManager
}

// membershipPeerIDResolver also installs the target's Pod-IP addresses before
// direct coordination RPCs dial it; DHT bootstrap does not populate every peer.
func membershipPeerIDResolver(mv ifaces.Members, ps peerstore.Peerstore, logger *slog.Logger) func(ifaces.NodeID) (peer.ID, bool) {
	return func(id ifaces.NodeID) (peer.ID, bool) {
		for _, n := range mv.Snapshot() {
			if n.ID != id || n.PeerID == "" {
				continue
			}

			pid, err := peer.Decode(n.PeerID)
			if err != nil {
				if logger != nil {
					logger.Debug("membership peer-id decode failed",
						slog.String("node_id", string(id)),
						slog.String("peer_id", n.PeerID),
						slog.Any("err", err),
					)
				}

				return "", false
			}

			var addrs []multiaddr.Multiaddr

			for _, raw := range n.P2PAddrs {
				info, err := peer.AddrInfoFromString(raw)
				if err != nil {
					if logger != nil {
						logger.Debug("membership peer address decode failed",
							slog.String("node_id", string(id)),
							slog.String("address", raw),
							slog.Any("err", err),
						)
					}

					continue
				}

				if info.ID != pid {
					if logger != nil {
						logger.Warn("membership peer address identity mismatch",
							slog.String("node_id", string(id)),
							slog.String("peer_id", pid.String()),
							slog.String("address_peer_id", info.ID.String()),
						)
					}

					continue
				}

				addrs = append(addrs, info.Addrs...)
			}

			if ps != nil && len(addrs) > 0 {
				ps.ClearAddrs(pid)
				ps.AddAddrs(pid, addrs, peerstore.AddressTTL)
			}

			return pid, true
		}

		return "", false
	}
}

// bootstrapConvergenceTarget returns the RoutingTableSize threshold
// that signals "bootstrap converged; ceasing periodic dials". It is
// the minimum of the per-cluster cap (maxSize) and the peer count
// (members snapshot size minus 1 for self).
//
// snapshotSize is the size of SnapshotForBootstrap (peers + self).
// Returns 0 when snapshotSize ≤ 1: a lone agent has nothing to dial
// and the DHT routing table will stay empty by definition, so any
// positive target would loop forever. Treating 0 as "converged"
// lets a single-node cluster exit the bootstrap loop on the first
// pass and lets /readyz flip to ready (the readiness probe applies
// the same single-node carve-out to the routing-table check).
func bootstrapConvergenceTarget(snapshotSize, maxSize int) int {
	peers := snapshotSize - 1
	if peers < 1 {
		return 0
	}

	if peers < maxSize {
		return peers
	}

	return maxSize
}

// bootstrapPeerCount returns the number of cluster members visible
// through the bootstrap view: every Running pod that has published a
// gantry.io/p2p-addrs annotation, regardless of Ready status. Falls
// back to the serving Snapshot when the Members implementation
// doesn't expose a bootstrap-specific view (e.g. the dev-mode
// single-self fake, or test stubs).
//
// Used by two places that *must not* gate on Ready: the kad-dht
// routing-table target and the readiness probe's DHT
// check. Both of them need to know "how many peers do we expect the
// routing table to learn about", and that population is the set of
// peers whose libp2p addresses are dialable - strictly larger than
// the Ready set, especially during a cold rollout where *no* pod is
// Ready yet. Using Snapshot here was a latent readiness-bypass
// bug: a fresh rollout would see snapshot size 0 or 1 across the
// whole cluster, the "snapshot > 1" guard on the DHT check would
// short-circuit to true, and every pod would flip Ready before
// libp2p/DHT had actually converged.
//
// The bootstrapper interface is matched structurally so this
// package doesn't need to import internal/members for the type
// assertion (which would create a build-time cycle with the
// fakes/test stubs used by announce_test.go).
func bootstrapPeerCount(m ifaces.Members) int {
	type bootstrapper interface {
		SnapshotForBootstrap() []ifaces.Node
	}
	if b, ok := m.(bootstrapper); ok {
		return len(b.SnapshotForBootstrap())
	}

	return len(m.Snapshot())
}

// runningMatchingPodCount returns the count of Running pods the
// informer sees (with PodIP populated), regardless of Ready or any
// announcement annotation. Falls back to len(Snapshot) when the
// Members implementation doesn't expose RunningMatchingPodCount
// (the dev-mode single-self fake; test stubs that don't model the
// informer at all). The fallback is a strict undercount on the
// dev-mode path, which is fine - the readiness gate this helper
// feeds only triggers when count > 1, and the dev-mode fake is
// always single-self.
//
// Used by /readyz to distinguish "real single-node cluster" (count
// == 1, no peers expected) from "multi-node, peers just haven't
// self-announced yet" (count > 1 but bootstrap view ≤ 1). The
// latter must keep /readyz at 503 with reason
// "peer self-announcements pending"; without this distinction the
// existing DHT check short-circuits during the first-rollout window
// where every pod is Running but none has yet published its libp2p
// multiaddrs, racing /readyz to green before any peer is dialable.
//
// Structural-typing pattern matches bootstrapPeerCount so test
// stubs (announce_test.go bootstrapStub, fakes.Members) don't drag
// internal/members into the import graph.
func runningMatchingPodCount(m ifaces.Members) int {
	type runningCounter interface {
		RunningMatchingPodCount() int
	}
	if r, ok := m.(runningCounter); ok {
		return r.RunningMatchingPodCount()
	}

	return len(m.Snapshot())
}

// routingTableTarget computes the expected steady-state kad-dht
// routing-table size given a bootstrap snapshot size and a per-
// cluster cap. The target is the number of *other* peers we expect
// the routing table to learn about - snapshotSize-1 - clamped to
// maxSize.
//
// A 2-node cluster has snapshotSize=2 but only ever populates one
// routing-table entry (the other node), so the target is 1 not 2.
// Returning the raw snapshotSize would peg the DHT health score at
// (size/snapshotSize) ≤ (snapshotSize-1)/snapshotSize even in a
// fully-converged cluster - e.g. 1/2 = 0.5 in a 2-node deploy,
// 2/3 ≈ 0.66 in a 3-node deploy - flagging healthy small clusters
// as degraded.
//
// Single-node carve-out: snapshotSize ≤ 1 -> 0 ("no peers to dial,
// any positive target would loop forever"), matching
// bootstrapConvergenceTarget's behavior so the bootstrap loop and
// the health score agree on what 'converged' means.
func routingTableTarget(snapshotSize, maxSize int) int {
	if snapshotSize <= 1 {
		return 0
	}

	peers := snapshotSize - 1
	if peers > maxSize {
		return maxSize
	}

	return peers
}

// rewriteWildcardMultiaddr returns ma with any wildcard IP component
// (/ip4/0.0.0.0 or /ip6/::) replaced by /ip4/<podIP> or /ip6/<podIP>
// of the *same family* as the wildcard. Dialable non-wildcard
// multiaddrs are returned unchanged. Returns "" when:
//
// - the multiaddr is a wildcard and no usable pod IP is available;
// - the multiaddr is a wildcard and the pod IP belongs to the
// opposite family (e.g. /ip4/0.0.0.0 with a v6 pod IP);
// - a concrete IP multiaddr is not globally unicast.
//
// The cross-family skip is critical: the wildcard family reflects
// the family the libp2p host is actually listening on. Silently
// rewriting /ip4/0.0.0.0 -> /ip6/<podIP> would publish an
// announcement pointing at an address the kernel has no socket bound
// to; peers dial it and get connection-refused. The caller drops
// empty strings from the published p2p_addrs set, so dual-stack
// pods that only have a v6 Pod IP but only listen on v4 publish no
// entry for the v4 wildcard at all (preferable to publishing a
// guaranteed-broken one). Operators on v6-only clusters must
// configure the listener for /ip6/::/ explicitly via the
// `libp2p_listen` config knob.
func rewriteWildcardMultiaddr(ma, podIP string) string {
	isWildcardV4 := strings.HasPrefix(ma, "/ip4/0.0.0.0/")

	isWildcardV6 := strings.HasPrefix(ma, "/ip6/::/")
	if !isWildcardV4 && !isWildcardV6 {
		if !isDialableMultiaddr(ma) {
			return ""
		}

		return ma
	}

	if podIP == "" {
		return ""
	}

	ip := net.ParseIP(podIP)
	if ip == nil {
		return ""
	}

	podIsV4 := ip.To4() != nil
	// Skip cross-family rewrites - they produce undialable
	// multiaddrs because the listener is bound to the *wildcard's*
	// family, not the pod IP's.
	if isWildcardV4 && !podIsV4 {
		return ""
	}

	if isWildcardV6 && podIsV4 {
		return ""
	}

	var (
		family string
		rest   string
	)
	if podIsV4 {
		family = "/ip4/" + ip.To4().String()
		rest = ma[len("/ip4/0.0.0.0"):]
	} else {
		family = "/ip6/" + ip.String()
		rest = ma[len("/ip6/::"):]
	}

	return family + rest
}

func isDialableMultiaddr(value string) bool {
	addr, err := multiaddr.NewMultiaddr(value)
	if err != nil {
		return false
	}

	for _, protocol := range []int{multiaddr.P_IP4, multiaddr.P_IP6} {
		rawIP, err := addr.ValueForProtocol(protocol)
		if err != nil {
			continue
		}

		ip := net.ParseIP(rawIP)

		return ip != nil && ip.IsGlobalUnicast()
	}

	return true
}

// advertisedTransferAddr returns the transfer endpoint to publish on
// the pod's gantry.io/transfer-addr annotation. Wildcard binds map to
// "" so members.Snapshot composes podIP:transferPort instead (the
// Snapshot fallback path); concrete binds (e.g. a NodePort override)
// are published verbatim.
//
// Family-safety: wildcard-bound listeners only listen on the family
// the wildcard names (`0.0.0.0` -> v4 only, `::` -> v6 only on Linux
// with `IPV6_V6ONLY=1`, which is the kernel default unless the Go
// runtime explicitly clears it; `net.Listen("tcp", ":port")` with an
// empty host clears `IPV6_V6ONLY` and becomes dual-stack). When the
// listen family does NOT match the pod IP family, returning a
// composed `podIP:port` annotation would point peers at an address
// the kernel has no socket bound to - a guaranteed connection
// refused. Return "" in that case so the annotation is omitted; the
// caller (readiness probe via transferAddrFamilyMismatch) is
// responsible for failing readiness so the broken pod never goes
// Ready and never appears in peers' Snapshot views. This mirrors the
// cross-family skip in rewriteWildcardMultiaddr for libp2p
// multiaddrs.
func advertisedTransferAddr(transferListen, podIP string) string {
	host, port, err := net.SplitHostPort(transferListen)
	if err != nil {
		return transferListen
	}

	switch host {
	case "":
		// Empty host -> Go listens dual-stack on Linux (IPv6
		// wildcard with IPV6_V6ONLY cleared, accepting both
		// families via v4-mapped-in-v6). Any pod-IP family is
		// dialable from a peer of either family. Outside K8s
		// podIP is empty -> return "" so members.Snapshot's
		// fallback (also empty) leaves the annotation unset.
		if podIP == "" {
			return ""
		}

		return net.JoinHostPort(podIP, port)
	case "0.0.0.0":
		if podIP == "" {
			return ""
		}

		ip := net.ParseIP(podIP)
		if ip == nil || ip.To4() == nil {
			// v4 listener, v6 pod IP -> cross-family, undialable.
			return ""
		}

		return net.JoinHostPort(podIP, port)
	case "::":
		if podIP == "" {
			return ""
		}

		ip := net.ParseIP(podIP)
		if ip == nil || ip.To4() != nil {
			// v6 listener, v4 pod IP -> cross-family, undialable.
			return ""
		}

		return net.JoinHostPort(podIP, port)
	}

	return transferListen
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
// because advertising nothing is the intended behavior there.
//
// Pairs with advertisedTransferAddr: this returns true exactly when
// the advertisedTransferAddr -> "" outcome was caused by a
// cross-family misconfiguration (not by absent podIP).
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

func chairSelfHolder(c *config.Config, disco *discovery.Host) chairs.Holder {
	peerID := disco.PeerID()

	addresses := make([]string, 0, len(disco.Addrs()))
	for _, listenAddress := range disco.Addrs() {
		address := rewriteWildcardMultiaddr(listenAddress.String(), c.PodIP)
		if address == "" {
			continue
		}

		addresses = append(addresses, address+"/p2p/"+peerID.String())
	}

	return chairs.Holder{
		PeerID:       ifaces.NodeID(peerID.String()),
		P2PAddrs:     addresses,
		TransferAddr: advertisedTransferAddr(c.TransferListen, c.PodIP),
	}
}

func announceLegacySelf(ctx context.Context, client kubernetes.Interface, namespace, podName string, holder chairs.Holder, logger *slog.Logger, announced func()) {
	announcement := members.SelfAnnouncement{
		PeerID:       string(holder.PeerID),
		P2PAddrs:     holder.P2PAddrs,
		TransferAddr: holder.TransferAddr,
	}
	backoff := time.Second

	for {
		err := members.AnnounceSelf(ctx, client, namespace, podName, announcement)
		if err == nil {
			if announced != nil {
				announced()
			}

			return
		}

		logger.Warn("legacy self-announce failed; retrying", slog.Duration("backoff", backoff), slog.Any("err", err))

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func connectedChairCandidates(disco *discovery.Host) []chairs.Holder {
	host := disco.LibP2P()
	peerIDs := host.Network().Peers()

	candidates := make([]chairs.Holder, 0, len(peerIDs))
	for _, peerID := range peerIDs {
		info := peer.AddrInfo{ID: peerID, Addrs: host.Peerstore().Addrs(peerID)}

		addresses, err := peer.AddrInfoToP2pAddrs(&info)
		if err != nil || len(addresses) == 0 {
			continue
		}

		rawAddresses := make([]string, 0, len(addresses))
		for _, address := range addresses {
			rawAddresses = append(rawAddresses, address.String())
		}

		candidates = append(candidates, chairs.Holder{PeerID: ifaces.NodeID(peerID.String()), P2PAddrs: rawAddresses})
	}

	return candidates
}

func installChairHolder(peerStore peerstore.Peerstore, holder chairs.Holder) error {
	peerID, err := peer.Decode(string(holder.PeerID))
	if err != nil {
		return fmt.Errorf("decode chair holder peer ID %q: %w", holder.PeerID, err)
	}

	addresses := make([]multiaddr.Multiaddr, 0, len(holder.P2PAddrs))
	for _, rawAddress := range holder.P2PAddrs {
		info, err := peer.AddrInfoFromString(rawAddress)
		if err != nil {
			continue
		}

		if info.ID != peerID {
			continue
		}

		addresses = append(addresses, info.Addrs...)
	}

	if len(addresses) == 0 && len(peerStore.Addrs(peerID)) == 0 {
		return fmt.Errorf("chair holder %s has no dialable libp2p addresses", holder.PeerID)
	}

	if len(addresses) > 0 {
		peerStore.ClearAddrs(peerID)
	}

	peerStore.AddAddrs(peerID, addresses, peerstore.AddressTTL)

	return nil
}

type coldStartEngine interface {
	Resolve(ctx context.Context, d digest.Digest, kind ifaces.OriginRefKind, registry, repository string, expectedSize int64) (*coldstart.Resolution, error)
}

func configuredFailureClasses(raw []string) []ifaces.FailureClass {
	out := make([]ifaces.FailureClass, 0, len(raw))
	for _, class := range raw {
		out = append(out, ifaces.FailureClass(class))
	}

	return out
}

// coldStartAdapter bridges a cold-start engine to mirror.ColdStartResolver
// without forcing the mirror package to import internal/coldstart.
type coldStartAdapter struct{ r coldStartEngine }

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
// please_pull RPCs grouped by selected chair holder.
//
// The implementation runs in a goroutine spawned by the mirror; it
// MUST NOT panic. All errors are logged at DEBUG.
type layerPrefetchAdapter struct {
	resolver   manifestPrefetchResolver
	cache      ifaces.LocalContentStore
	logger     *slog.Logger
	onManifest func(digest.Digest, []manifest.TypedChild)
}

type manifestPrefetchResolver interface {
	PrefetchManifestChildren(ctx context.Context, manifestDigest digest.Digest, children []coldstart.ChildDigest, registry, repository string) error
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
	r manifestPrefetchResolver,
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

	var cachedAdvertise sync.Map

	advertiseCached := func(d digest.Digest) {
		if markPresent == nil {
			return
		}

		key := d.String()
		if _, loaded := cachedAdvertise.LoadOrStore(key, struct{}{}); loaded {
			return
		}

		if !gate.TryAdd() {
			cachedAdvertise.Delete(key)
			return
		}

		go func() {
			defer gate.Done()
			defer cachedAdvertise.Delete(key)

			advertiseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if !markPresent(advertiseCtx, d) {
				lg.Debug("puller-pump: cached digest re-advertise failed",
					slog.String("digest", d.String()),
				)
			}
		}()
	}

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
				advertiseCached(d)
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
