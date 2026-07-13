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
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/discovery"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
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
	peerClient := transfer.NewClient()
	transferOpts := []transfer.Option{
		transfer.WithLogger(logger),
		transfer.WithDescriber(cdstore),
		transfer.WithMetrics(
			func() { p2.peerServe.Inc() },
			func() { p2.peerMiss.Inc() },
		),
	}
	transferSrv := transfer.New(cstore, transferOpts...)

	transferStop, err := transferSrv.ListenAndServe(c.TransferListen)
	if err != nil {
		return fmt.Errorf("transfer listen: %w", err)
	}

	logger.Info("transfer endpoint listening", slog.String("addr", c.TransferListen))

	// - membership view + cold-start orchestrator. Members
	// requires Kubernetes credentials (in-cluster or explicit
	// kubeconfig); when neither is available we fall back to a
	// single-self membership view that disables cold-start so the
	// mirror keeps behaviour for local development. When
	// production K8s env vars are set (GANTRY_NODE_NAME etc.)
	// failure to start the informer is fatal - silently degrading
	// to single-node mode in production would advertise a healthy
	// agent that is in fact running with no peer coordination.
	memberView, membersStop, err := buildMembers(ctx, c, disco, logger)
	if err != nil {
		return fmt.Errorf("members: %w", err)
	}
	defer membersStop()

	// (cont.) - self-announce: write libp2p peer.ID, listen
	// multiaddrs, and the transfer endpoint into our own Pod's
	// annotations so peer agents can discover this node without
	// operator-supplied bootstrap_peers. Loops with capped backoff
	// for the lifetime of ctx - when self-announce is the only path
	// to peer discovery (prod + dynamic bootstrap), the readiness
	// probe below gates traffic on the first successful patch so
	// missing `pods/patch` RBAC surfaces as a stuck deploy rather
	// than a silently-isolated agent.
	var selfAnnounced atomic.Bool
	// noDialableP2PAddrs is set when announceSelfAndBootstrap
	// reports a successful patch but with zero published P2PAddrs -
	// every disco.Addrs entry was either a wildcard the
	// rewrite-to-PodIP path couldn't rewrite (mismatched IP family,
	// no Pod IP exposed) or otherwise unsuitable. Surfacing this in
	// /readyz is the only way to fail the rollout instead of
	// silently shipping a libp2p-unreachable agent.
	var noDialableP2PAddrs atomic.Bool
	// noDialableTransferAddr is set when c.TransferListen is
	// wildcard-bound to a single IP family that does not match the
	// pod's IP family - e.g. transfer_listen=0.0.0.0:5001 on an
	// IPv6-only cluster, or transfer_listen=[::]:5001 on a v4-only
	// cluster. In that state, peers reading our self-announce
	// annotation (or composing podIP:transferPort via the Snapshot
	// fallback) would see an address the kernel has no socket bound
	// to, and every peer transfer attempt would connection-refused
	// - duplicating the libp2p cross-family mode for the transfer
	// endpoint. Computed once at startup since c.TransferListen and
	// c.PodIP do not change after boot.
	var noDialableTransferAddr atomic.Bool
	noDialableTransferAddr.Store(transferAddrFamilyMismatch(c.TransferListen, c.PodIP))

	if noDialableTransferAddr.Load() {
		// Loud diagnostic so the readiness probe's terse message
		// has something concrete in the logs. Mirrors the libp2p
		// cross-family warning emitted from announceSelfAndBootstrap.
		logger.Error("transfer: listener family mismatches Pod IP; advertised transfer address will be empty and peers cannot dial this node for blob fetches",
			slog.String("transfer_listen", c.TransferListen),
			slog.String("pod_ip", c.PodIP),
		)
	}
	// A successful self-announce is required for readiness iff the
	// agent is running in production K8s mode with its own pod name
	// set. The self-announce publishes the gantry.io/peer-id,
	// gantry.io/p2p-addrs, and gantry.io/transfer-addr annotations
	// on this pod so other agents can translate a K8s-node-name
	// membership entry (the cluster's HRW key) into the libp2p
	// peer-ID + multiaddrs they actually dial.
	//
	// Static bootstrap peers do NOT bypass this gate. Bootstrap
	// peers solve *DHT seeding* - they help kademlia discover other
	// peers' addresses - but they do not solve the membership-ID ->
	// libp2p peer-ID mapping problem, which is what the per-pod
	// annotations carry. An agent that DHT-bootstrapped successfully
	// but never published its annotations still 503s every inbound
	// please_pull / pull_intent_query because other agents fail to
	// translate its node name. The full rationale (and the test that
	// pins this contract) lives at selfAnnounceRequiredForReadiness
	// below.
	requireSelfAnnounce := false
	if mgr, ok := memberView.(*members.Manager); ok && c.PodName != "" {
		requireSelfAnnounce = selfAnnounceRequiredForReadiness(c)
		go announceSelfAndBootstrap(ctx, mgr, disco, c, logger, func(addrCount int) {
			selfAnnounced.Store(true)
			noDialableP2PAddrs.Store(addrCount == 0)
		})
	}

	// - wire the routing-table target now that memberView is
	// online. the design doc defines target = min(informer_node_count,
	// kademlia_max_routing_table_size); the constant cap of 256 is
	// derived from kad-dht's bucket-size 20 × log2(10000) ≈ 266 and
	// rounded down. Read live on every score call.
	//
	// Sizing uses bootstrapPeerCount (= SnapshotForBootstrap when
	// available) instead of Snapshot. During a fresh rollout no peer
	// is Ready yet, so Snapshot reports 0–1 and the DHT health score
	// computed downstream from this target looks artificially good
	// ("target=0, current=0, score=1.0"). The bootstrap view counts
	// every Running pod that has published a p2p-addrs annotation,
	// which is the set of peers we actually expect kad-dht to learn
	// about, so the score reflects real convergence pressure.
	//
	// IMPORTANT: the target is "other peers we expect to see in the
	// routing table" - i.e. snapshot-1 to exclude self, NOT the raw
	// snapshot count. A 2-node cluster has bootstrapPeerCount=2 but
	// can only ever populate 1 routing-table entry (the other node);
	// returning 2 would make health score capped at 0.5 even in a
	// fully-converged 2-node cluster. Single-node carve-out returns
	// 0 so the lone-agent health score is well-defined (matches
	// bootstrapConvergenceTarget's behaviour).
	const kademliaMaxRoutingTable = 256

	if monitor := disco.Monitor(); monitor != nil {
		monitor.SetRoutingTableTarget(func() int {
			return routingTableTarget(bootstrapPeerCount(memberView), kademliaMaxRoutingTable)
		})
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
		// Resolve NodeID -> peer.ID via the live membership snapshot:
		// each peer publishes its libp2p peer.ID into a pod
		// annotation (the design doc) which Members reads in Snapshot. This
		// lets the cluster use stable K8s node names as NodeIDs
		// while still dialing libp2p RPCs to the right peer.
		coord.WithPeerIDResolver(membershipPeerIDResolver(memberView, logger)),
	)
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
		coord.WithMaxDigestsPerPleasePull(c.CoordMaxDigestsPerRequest),
	}
	coordServer := coord.NewServer(cstore, memberView, inflightMap, coordOpts...)
	coordServer.Bind(disco.LibP2P())

	// cold-start orchestrator. Enabled whenever the real
	// Kubernetes membership informer is in use; disabled only for
	// the dev-mode single-self fake (where there are no peers to
	// coordinate with by definition). The previous "Snapshot has
	// non-self entry" gate broke first-cluster boot - see
	// hasMultiNodeMembership for the full rationale.
	var (
		coldStartResolver mirror.ColdStartResolver
		layerPrefetcher   mirror.LayerPrefetcher
	)

	if hasMultiNodeMembership(memberView) {
		selfZone := lookupSelfZone(memberView)
		realResolver := coldstart.New(coldstart.Options{
			Members:               memberView,
			Discovery:             disco,
			Coord:                 coordClient,
			Inflight:              inflightMap,
			Logger:                logger,
			HrwK:                  c.HRWK,
			HrwScope:              hrw.ParseScope(c.HRWTopologyScope),
			SelfZone:              selfZone,
			LocalIntent:           coordServer,
			LocalPull:             coordServer,
			TransientCooldownCap:  c.OriginFailureHonorWindowCap,
			TopKExpansionFactor:   c.TopKExpansionFactorDegraded,
			TrustedFailureClasses: parseTrustedFailureClasses(c.OriginFailureClassesTrustedClusterWide, logger),
			Metrics: coldstart.MetricsHooks{
				OnRankMismatch: func(kindLabel string, _ ifaces.NodeID) {
					p3.hrwRankMismatch.WithLabelValues(kindLabel).Inc()
				},
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
			},
		})
		coldStartResolver = coldStartAdapter{r: realResolver}
		layerPrefetcher = newLayerPrefetcher(realResolver, cstore, logger)
		logger.Info("cold-start orchestrator wired",
			slog.Int("hrw_k", c.HRWK),
			slog.String("hrw_scope", c.HRWTopologyScope),
		)
	} else {
		logger.Info("cold-start orchestrator disabled (single-self membership; no Kubernetes informer)")
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
			ClusterSize: func() int { return bootstrapPeerCount(memberView) },
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
		mirror.WithOriginStreamMetrics(
			func(kind string) { p9.originStreamStarted.WithLabelValues(kind).Inc() },
			func(kind string) { p9.originStreamCompleted.WithLabelValues(kind).Inc() },
			func(kind string) { p9.originStreamFailed.WithLabelValues(kind).Inc() },
		),
		mirror.WithLiveStreamCompletedHook(func(d digest.Digest) {
			streamCommitTracker.RecordCompleted(d)
		}),
		mirror.WithDiscovery(disco, peerClient),
		mirror.WithSelfNodeID(memberView.Self()),
		mirror.WithSelfPeerID(ifaces.NodeID(disco.PeerID().String())),
		mirror.WithPeerMetrics(
			func(outcome string) { p2.peerFetch.WithLabelValues(outcome).Inc() },
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

	// - readiness state. /readyz waits for three signals:
	// (1) members informer initial sync, (2) DHT routing table
	// non-empty, (3) cache scan complete. (3) is implicit because
	// cache.Open runs synchronously above, but we set a flag here
	// so the relationship is explicit in the probe logic.
	var (
		membersReady atomic.Bool
		cacheReady   atomic.Bool
	)
	cacheReady.Store(true)

	go func() {
		if err := memberView.WaitForSync(ctx); err == nil {
			membersReady.Store(true)
		}
	}()

	readyCheck := func() (string, bool) {
		if !cacheReady.Load() {
			return "cache scan not complete", false
		}

		if !membersReady.Load() {
			return "members informer not synced", false
		}

		if requireSelfAnnounce && !selfAnnounced.Load() {
			// Production + dynamic bootstrap: peers cannot
			// discover us until our pods/patch lands. Staying
			// 503 until then makes the rolling deploy pause and
			// surfaces an RBAC misconfiguration immediately.
			return "members self-announce pending (check pods/patch RBAC)", false
		}

		if requireSelfAnnounce && noDialableP2PAddrs.Load() {
			// Patch went through but the published P2PAddrs list
			// was empty: every disco.Addrs entry was a
			// wildcard the rewrite-to-PodIP couldn't rewrite.
			// Peers see our annotation but cannot dial us, so
			// coord RPCs all fail - fail the readiness probe
			// rather than ship a silently-isolated agent. Fix:
			// align libp2p_listen with the Pod's IP family.
			return "members self-announce has no dialable p2p addresses; check libp2p_listen vs Pod IP family", false
		}

		if requireSelfAnnounce && noDialableTransferAddr.Load() {
			// Same hazard, transfer-endpoint flavour. Wildcard
			// listen on the wrong family produces an undialable
			// advertised transfer address; peers' transfer pulls
			// would all connection-refused. Fix: align
			// transfer_listen with the Pod's IP family (use
			// `[::]:port` on v6-only / dual-stack clusters,
			// `0.0.0.0:port` on v4-only clusters, or `:port` to
			// let Go open a dual-stack socket on Linux).
			return "transfer listener family mismatches Pod IP; check transfer_listen vs Pod IP family", false
		}
		// Multi-node rollout, no peer has self-announced yet:
		// every Gantry pod the informer sees is Running (so the
		// "running" count > 1) but only this pod has published
		// p2p-addrs (so the bootstrap view ≤ 1, typically == 1
		// because we annotated ourselves earlier in startup).
		// Without this gate the existing DHT-empty check below
		// short-circuits to true (because bootstrap count ≤ 1
		// trips the single-node carve-out) and /readyz races to
		// green before any peer is actually dialable - pods flip
		// Ready, mirror traffic starts, every coord/transfer dial
		// gets connection-refused, and the cluster thunders the
		// origin. Staying 503 with this specific reason tells
		// operators the real cause (peers aren't announced yet);
		// "dht routing table empty" would misattribute it.
		//
		// Must run BEFORE the DHT check below so the reason
		// string is correct on the first-rollout path.
		if runningMatchingPodCount(memberView) > 1 && bootstrapPeerCount(memberView) <= 1 {
			return "peer self-announcements pending", false
		}
		// Single-node cluster carve-out: with only self in the
		// members view there are no peers to dial, so the kad-dht
		// routing table will stay empty by definition. Without this
		// check /readyz would hang forever on a legitimate
		// single-node deploy (an operator running one agent in a
		// dev cluster, a one-node staging environment, or the
		// transient state during an initial rollout where the first
		// pod's informer has synced but the second hasn't started).
		// The bootstrap loop applies the matching carve-out via
		// bootstrapConvergenceTarget returning 0.
		//
		// IMPORTANT: this uses bootstrapPeerCount (= Running pods
		// with a p2p-addrs annotation, regardless of Ready),
		// NOT Snapshot which is the Ready-only serving view.
		// During a fresh rollout the current pod is not Ready yet
		// and peer pods may also not be Ready yet, so Snapshot
		// returns 0–1 and this guard would skip the DHT check
		// entirely - letting pods become Ready before libp2p/DHT
		// has converged and starting their mirror traffic into a
		// non-functional cluster, which thunders the origin. The
		// bootstrap view is the right scope because the DHT can
		// only converge through peers that have at least announced
		// libp2p addresses, which is exactly what the bootstrap
		// view filters for.
		if bootstrapPeerCount(memberView) > 1 && disco.RoutingTableSize() < 1 {
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

	// Block until signal or metrics-server crash.
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

// buildMembers tries to construct a k8s-informer-backed Members
// Manager. Behaviour depends on whether production-mode env vars
// signal that K8s membership is expected:
//
// - Dev mode (NodeName, PodName, and MembersNamespace all empty):
// fall back silently to a single-self stub. Cold-start is
// disabled downstream via hasMultiNodeMembership; the agent
// serves the direct-mirror path. This is the path local
// `go run` invocations take.
//
// - Production mode (any of NodeName / PodName / MembersNamespace
// non-empty): an informer construction failure, OR a sync that
// does not complete within memberSyncTimeout, is fatal.
// Returning a single-self stub here would advertise a healthy
// agent that is silently running with no peer coordination at
// all - worse than crash-looping, because the operator sees no
// signal. A WaitForSync deadline in particular is the canonical
// symptom of broken RBAC / API egress / service-account perms;
// the previous implementation called Manager.Start(ctx) with
// the long-lived app context which blocked indefinitely on
// those failures, never reaching the 10s deadline branch.
//
// - Dev-mode WaitForSync timeout: warn and fall back to the
// single-self stub so local `go run` against a missing or
// misconfigured kubeconfig still boots.
func buildMembers(ctx context.Context, c *config.Config, disco *discovery.Host, logger *slog.Logger) (ifaces.Members, func(), error) {
	prodMode := isProductionMode(c)
	// Required inputs for the real informer path.
	if c.NodeName == "" || c.MembersLabelSelector == "" {
		if prodMode {
			return nil, nil, fmt.Errorf("production mode (NodeName/PodName/Namespace set) but NodeName or LabelSelector missing: refusing to silently fall back to single-self stub")
		}

		logger.Info("members: using single-self stub (NodeName/LabelSelector unset)")

		return singleSelfMembers(c, disco), func() {}, nil
	}

	mgr, err := members.New(members.Options{
		NodeName:      c.NodeName,
		Namespace:     c.MembersNamespace,
		LabelSelector: c.MembersLabelSelector,
		ZoneLabelKey:  c.ZoneLabelKey,
		Kubeconfig:    c.MembersKubeconfig,
		TransferPort:  transferPortFromListen(c.TransferListen),
	})
	if err != nil {
		if prodMode {
			return nil, nil, fmt.Errorf("members.New: %w", err)
		}

		logger.Warn("members.New failed; falling back to single-self stub (dev mode)", slog.Any("err", err))

		return singleSelfMembers(c, disco), func() {}, nil
	}
	// Start kicks off the informer goroutines without blocking. The
	// sync deadline below is *the* policy knob: production mode
	// treats a timeout as a fatal startup failure (the canonical
	// symptom of broken RBAC / API egress / service-account perms);
	// dev mode warns and falls back to the single-self stub.
	mgr.Start()

	syncTimeout := memberSyncDefaultTimeout
	if c.MembersSyncTimeout > 0 {
		syncTimeout = c.MembersSyncTimeout
	}

	syncCtx, syncCancel := context.WithTimeout(ctx, syncTimeout)
	syncErr := mgr.WaitForSync(syncCtx)

	syncCancel()

	if syncErr != nil {
		if prodMode {
			mgr.Stop()
			return nil, nil, fmt.Errorf("members initial sync (timeout=%s): %w", syncTimeout, syncErr)
		}

		logger.Warn("members initial sync failed; falling back to single-self stub (dev mode)",
			slog.Duration("timeout", syncTimeout),
			slog.Any("err", syncErr),
		)
		mgr.Stop()

		return singleSelfMembers(c, disco), func() {}, nil
	}

	logger.Info("members informer ready",
		slog.String("node_name", c.NodeName),
		slog.Int("peers", len(mgr.Snapshot())),
	)

	return mgr, mgr.Stop, nil
}

// memberSyncDefaultTimeout is the built-in default for how long buildMembers
// waits for the initial list+watch on the pod and node informers before
// failing (prod) or degrading to the single-self stub (dev). Operators on
// clusters with a slow API server or large-scale simultaneous DaemonSet
// rollouts can override this via config.MembersSyncTimeout /
// GANTRY_MEMBERS_SYNC_TIMEOUT / --members-sync-timeout.
//
// 30s is generous for a healthy apiserver - a real timeout almost always
// means RBAC, API egress, or service-account permissions are broken; failing
// fast surfaces that as an immediate deploy-time signal rather than a silent
// "why isn't dedup working?" mystery.
const memberSyncDefaultTimeout = 30 * time.Second

// singleSelfMembers returns a single-entry Members view for dev/test
// runs that have no Kubernetes cluster behind them.
func singleSelfMembers(c *config.Config, disco *discovery.Host) ifaces.Members {
	id := c.NodeName
	if id == "" {
		id = disco.PeerID().String()
	}

	return fakes.NewMembers(ifaces.NodeID(id), ifaces.Node{
		ID:   ifaces.NodeID(id),
		Addr: c.TransferListen,
	})
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

// lookupSelfZone returns the zone label of this node from the members
// snapshot, or "" if absent. Used to seed coldstart.Options.SelfZone
// under HrwScope = "zone".
func lookupSelfZone(m ifaces.Members) string {
	self := m.Self()
	for _, n := range m.Snapshot() {
		if n.ID == self {
			return n.Zone
		}
	}

	return ""
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

// transferPortFromListen parses the port number out of a `host:port`
// listen spec such as "0.0.0.0:5001" or ":5001". Returns 0 when the
// spec is empty or malformed; members.Snapshot then falls back to a
// bare pod-IP address.
func transferPortFromListen(listen string) int {
	if listen == "" {
		return 0
	}

	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}

	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}

	return n
}

// membershipPeerIDResolver returns a coord.WithPeerIDResolver callback
// that consults the live members snapshot. NodeID -> Node.PeerID is the
// fast path; on miss the resolver returns (_, false) so coord.Client
// falls through to its static teach-cache and finally to
// peer.Decode(NodeID). The membership view is read on every call (cheap
// in-memory copy) so newly-joined peers are picked up without restart.
func membershipPeerIDResolver(mv ifaces.Members, logger *slog.Logger) func(ifaces.NodeID) (peer.ID, bool) {
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

			return pid, true
		}

		return "", false
	}
}

// announceSelfAndBootstrap publishes this agent's libp2p identity into
// its own Pod's annotations, then dials every peer announcement in the
// membership snapshot to seed the kad-dht routing table. The
// announcement is retried with capped exponential backoff for the
// lifetime of ctx - there is no permanent-failure exit; if `pods/patch`
// RBAC is eventually fixed, the patch succeeds on the next attempt and
// onAnnounced fires.
//
// onAnnounced is invoked exactly once, the first time AnnounceSelf
// succeeds. Callers that gate readiness on it (production deploys with
// dynamic bootstrap and no static peers) keep returning 503 until then,
// which is the canonical "your pods/patch RBAC is wrong" signal.
//
// The bootstrap snapshot intentionally includes NotReady pods
// (SnapshotForBootstrap) because readiness depends on RoutingTableSize
// being > 0 - a deadlock if every peer is waiting for every other
// peer to be Ready first.
func announceSelfAndBootstrap(ctx context.Context, mgr *members.Manager, disco *discovery.Host, c *config.Config, logger *slog.Logger, onAnnounced func(addrCount int)) {
	// Build the announcement. Wildcard listen addresses (0.0.0.0,
	// ::) are not dialable from other pods; substitute the agent's
	// Pod IP so the published p2p-addrs are usable.
	listenAddrs := disco.Addrs()
	peerID := disco.PeerID()

	multiaddrs := make([]string, 0, len(listenAddrs))
	for _, la := range listenAddrs {
		ma := rewriteWildcardMultiaddr(la.String(), c.PodIP)
		if ma == "" {
			// Skip wildcards we can't rewrite - better no entry
			// than an undialable one.
			continue
		}
		// Format /ip4/.../tcp/.../p2p/<peerID> so peers can dial
		// directly without a separate ID resolution step.
		multiaddrs = append(multiaddrs, ma+"/p2p/"+peerID.String())
	}

	if len(multiaddrs) == 0 {
		// Loud diagnostic so the readiness probe's terse message has
		// something to point at in the logs. The peer is in a
		// broken-but-not-crashed state: cdsub still works, the
		// transfer endpoint still serves, but no other agent can
		// dial us over libp2p so coord RPCs (please_pull,
		// pull_intent) will all fail. readyCheck stays 503 on this
		// condition; fix is to align libp2p_listen with the Pod's
		// IP family (typical cause: pod is v4-only but libp2p_listen
		// specifies a v6 wildcard, or vice versa).
		logger.Error("members: self-announce will produce zero dialable p2p addresses; check libp2p_listen vs Pod IP family",
			slog.String("pod_ip", c.PodIP),
			slog.Int("listen_addrs", len(listenAddrs)),
		)
	}

	ann := members.SelfAnnouncement{
		PeerID:       peerID.String(),
		P2PAddrs:     multiaddrs,
		TransferAddr: advertisedTransferAddr(c.TransferListen, c.PodIP),
	}

	// Retry the patch with capped exponential backoff. Loops until
	// success or ctx cancellation - the previous 5-attempt cap
	// silently exited into the bootstrap loop on a permanent RBAC
	// failure, leaving the cluster's annotation pool missing this
	// pod forever. Now an eventual RBAC fix self-heals on the next
	// attempt and onAnnounced flips the readiness gate.
	backoff := 1 * time.Second

	const maxBackoff = 30 * time.Second

	for {
		err := mgr.AnnounceSelf(ctx, c.PodName, ann)
		if err == nil {
			logger.Info("members: self-announce ok",
				slog.String("pod", c.PodName),
				slog.String("peer_id", peerID.String()),
				slog.Int("p2p_addrs", len(multiaddrs)),
			)

			if onAnnounced != nil {
				// Pass addr count so readiness can distinguish
				// "patch ok and we're dialable" (>0) from "patch
				// ok but we are silently isolated" (==0).
				onAnnounced(len(multiaddrs))
			}

			break
		}

		logger.Warn("members: self-announce failed; will retry",
			slog.Duration("backoff", backoff),
			slog.Any("err", err),
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	// Periodic bootstrap loop. A single ConnectPeers call at startup
	// can miss peers whose AnnounceSelf hasn't completed yet (cold
	// cluster boot, rolling deploys). We poll the bootstrap snapshot
	// every 5s for the first minute, then back off to every 30s
	// while RoutingTableSize is still below a healthy threshold, and
	// stop entirely once the table is populated. kad-dht handles
	// ongoing refresh from there.
	//
	// The convergence target is cluster-size aware: on a 2-node
	// cluster the routing table can never reach 5, so a fixed
	// threshold of 5 would loop forever dialing the same single
	// peer. We cap target at min(maxHealthyRTSize, peer_count) with
	// a floor of 1 so single-node deployments (membership has only
	// self) exit immediately after the first pass.
	const (
		aggressiveInterval = 5 * time.Second
		relaxedInterval    = 30 * time.Second
		aggressiveBudget   = 60 * time.Second
		maxHealthyRTSize   = 5
	)

	bootstrapStart := time.Now()

	for {
		peerAddrs := bootstrapPeerAddrs(mgr)
		if len(peerAddrs) > 0 {
			connected := disco.ConnectPeers(ctx, peerAddrs)
			logger.Debug("members: bootstrap dial pass",
				slog.Int("connected", connected),
				slog.Int("candidates", len(peerAddrs)),
				slog.Int("routing_table", disco.RoutingTableSize()),
			)
		}

		target := bootstrapConvergenceTarget(len(mgr.SnapshotForBootstrap()), maxHealthyRTSize)
		if disco.RoutingTableSize() >= target {
			// Cold-start race carve-out: when `target` is 0 the
			// membership snapshot has only self in it. That can mean
			// either (a) this is a genuine single-node deployment, or
			// (b) DaemonSet siblings are starting in parallel and the
			// apiserver has not yet observed them. We cannot tell the
			// difference at the first pass, so we KEEP DIALING until
			// aggressiveBudget elapses. Without this carve-out the
			// bootstrap loop exits at t<1s with target=0, /readyz
			// permanently flips to "dht routing table empty" once the
			// informer observes peers, and no further dials happen.
			if target > 0 || time.Since(bootstrapStart) > aggressiveBudget {
				logger.Info("members: bootstrap converged; ceasing periodic dials",
					slog.Int("routing_table", disco.RoutingTableSize()),
					slog.Int("target", target),
					slog.Duration("elapsed", time.Since(bootstrapStart)),
				)

				return
			}
		}

		interval := aggressiveInterval
		if time.Since(bootstrapStart) > aggressiveBudget {
			interval = relaxedInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
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
// bootstrapConvergenceTarget's behaviour so the bootstrap loop and
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

// bootstrapPeerAddrs collects every published p2p multiaddr across all
// peers in the bootstrap-view snapshot, excluding self.
func bootstrapPeerAddrs(mgr *members.Manager) []string {
	peers := mgr.SnapshotForBootstrap()

	out := make([]string, 0, len(peers))
	for _, n := range peers {
		if n.ID == mgr.Self() || len(n.P2PAddrs) == 0 {
			continue
		}

		out = append(out, n.P2PAddrs...)
	}

	return out
}

// rewriteWildcardMultiaddr returns ma with any wildcard IP component
// (/ip4/0.0.0.0 or /ip6/::) replaced by /ip4/<podIP> or /ip6/<podIP>
// of the *same family* as the wildcard. Non-wildcard multiaddrs are
// returned unchanged. Returns "" when:
//
// - the multiaddr is a wildcard and no usable pod IP is available;
// - the multiaddr is a wildcard and the pod IP belongs to the
// opposite family (e.g. /ip4/0.0.0.0 with a v6 pod IP).
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
// because advertising nothing is the intended behaviour there.
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
// please_pull RPCs grouped by HRW rank-0 puller.
//
// The implementation runs in a goroutine spawned by the mirror; it
// MUST NOT panic. All errors are logged at DEBUG.
type layerPrefetchAdapter struct {
	resolver *coldstart.Resolver
	cache    ifaces.LocalContentStore
	logger   *slog.Logger
}

// maxManifestBytes caps the size of a manifest body the prefetcher
// is willing to parse. OCI Distribution recommends manifests stay
// well under 4 MiB; a body larger than that almost certainly indicates
// a misconfigured upstream (or attack), and we'd rather skip prefetch
// than allocate a multi-MB buffer per manifest serve.
const maxManifestBytes int64 = 4 * 1024 * 1024

func newLayerPrefetcher(r *coldstart.Resolver, cache ifaces.LocalContentStore, logger *slog.Logger) mirror.LayerPrefetcher {
	return &layerPrefetchAdapter{
		resolver: r,
		cache:    cache,
		logger:   logger.With(slog.String("subsystem", "prefetch")),
	}
}

func (p *layerPrefetchAdapter) OnManifestServed(ctx context.Context, registry, repository string, manifestDigest digest.Digest) {
	if p.resolver == nil {
		return
	}
	// Use a fresh deadline so the prefetch survives the request
	// context that just finished; cap at 30s so a stuck prefetch
	// can't pin a goroutine forever.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rc, _, err := p.cache.Open(ctx, manifestDigest)
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

	if len(children) == 0 {
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

	if err := p.resolver.PrefetchChildren(ctx, pending, registry, repository); err != nil {
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
