// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/kubelease"
	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/meshfetch"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

// cachemeshLeaveTimeout bounds the graceful drain on shutdown.
const cachemeshLeaveTimeout = 5 * time.Second

// cachemeshAgent is the optional cachemesh member of this agent: the
// routing Mesh, the meshfetch Node that moves chunks and tags, and their
// lifecycle.
type cachemeshAgent struct {
	mesh    *cachemesh.Mesh
	meshPtr atomic.Pointer[cachemesh.Mesh]
	node    *meshfetch.Node
	logger  *slog.Logger
	cancel  context.CancelFunc
	done    chan error
	tags    bool
}

// cachemeshMemberID names this agent in the mesh: the Kubernetes node name,
// else the pod name, else the hostname.
func cachemeshMemberID(c *config.Config) string {
	if c.NodeName != "" {
		return c.NodeName
	}

	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}

	host, _ := os.Hostname() //nolint:errcheck // empty host fails cachemesh validation

	return host
}

// cachemeshAddr is the member address: the pod IP and the transfer port,
// where the control and chunk endpoints are served.
func cachemeshAddr(c *config.Config) (string, error) {
	_, port, err := net.SplitHostPort(c.TransferListen)
	if err != nil {
		return "", fmt.Errorf("cachemesh: transfer_listen: %w", err)
	}

	return net.JoinHostPort(c.PodIP, port), nil
}

// newCachemeshAgent builds the cachemesh member. It does not start it.
func newCachemeshAgent(c *config.Config, origin ifaces.OriginPuller, reg *metrics.Registry, logger *slog.Logger) (*cachemeshAgent, error) {
	cm := c.Cachemesh

	rangeOrigin, ok := origin.(meshfetch.Origin)
	if !ok {
		return nil, errors.New("cachemesh: origin client does not support bounded range reads")
	}

	var tagOrigin meshfetch.TagOrigin

	if cm.CacheTags {
		if t, ok := origin.(meshfetch.TagOrigin); ok {
			tagOrigin = t
		}
	}

	addr, err := cachemeshAddr(c)
	if err != nil {
		return nil, err
	}

	client, err := chairs.NewClientset(c.MembersKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("cachemesh: %w", err)
	}

	lease := kubelease.New(client.CoordinationV1().Leases(c.CachemeshNamespace()), cm.LeaseName)
	logger = logger.With(slog.String("subsystem", "cachemesh"))
	m := newCachemeshMetrics(reg)

	mesh, err := cachemesh.New(cachemesh.Config{
		ID:            cachemeshMemberID(c),
		Addr:          addr,
		Transport:     meshfetch.NewControlTransport(),
		Lease:         lease,
		Logger:        logger,
		PartitionBits: uint8(cm.PartitionBits), //nolint:gosec // validated to 0..24
		CellSize:      cm.CellSize,
		MaxNeighbors:  cm.MaxNeighbors,
		ExpectedSize:  cm.ExpectedSize,
		Hooks: cachemesh.Hooks{
			OnControlError: func(cachemesh.Endpoint, error) { m.controlErrors.Inc() },
			OnLeaseError:   func(op string, _ error) { m.leaseErrors.WithLabelValues(op).Inc() },
		},
	})
	if err != nil {
		return nil, err
	}

	node, err := meshfetch.New(meshfetch.Config{
		Mesh:         mesh,
		Origin:       rangeOrigin,
		Tags:         tagOrigin,
		ChunkSize:    cm.ChunkSize,
		CacheBytes:   cm.ChunkCacheBytes,
		MaxFills:     cm.MaxConcurrentChunkFills,
		MaxServes:    cm.MaxConcurrentChunkServes,
		Lookahead:    cm.Lookahead,
		ChunkTimeout: cm.ChunkTimeout,
		StallTimeout: cm.ChunkStallTimeout,
		Attempts:     cm.ChunkAttempts,
		Hedge:        cm.Hedge,
		Verify:       cm.VerifyDigest,
		MetadataTTL:  cm.MetadataTTL,
		Logger:       logger,
		Hooks:        m.hooks(),
	})
	if err != nil {
		return nil, err
	}

	a := &cachemeshAgent{mesh: mesh, node: node, logger: logger, tags: tagOrigin != nil}
	a.meshPtr.Store(mesh)
	m.registerGauges(reg, a)

	logger.Info("cachemesh configured",
		slog.String("id", mesh.ID()),
		slog.String("addr", addr),
		slog.String("lease", c.CachemeshNamespace()+"/"+cm.LeaseName),
		slog.Int64("chunk_size", cm.ChunkSize),
		slog.Int64("chunk_cache_bytes", cm.ChunkCacheBytes),
		slog.Int("max_neighbors", cm.MaxNeighbors),
		slog.Int("lookahead", cm.Lookahead),
		slog.Bool("cache_tags", a.tags),
	)

	return a, nil
}

// routes registers the control, chunk, and metadata endpoints on the
// transfer listener.
func (a *cachemeshAgent) routes(mux *http.ServeMux) {
	mux.Handle(meshfetch.ControlPath, meshfetch.ControlHandler(&a.meshPtr))
	a.node.Register(mux)
}

// cachemeshMirrorOption plugs the mesh into the mirror. A nil agent
// (cachemesh disabled) leaves the mirror unchanged.
func cachemeshMirrorOption(a *cachemeshAgent) mirror.Option {
	return func(s *mirror.Server) {
		if a == nil {
			return
		}

		mirror.WithBlobFetcher(a.node)(s)

		if a.tags {
			mirror.WithTagResolver(a.node)(s)
		}
	}
}

// start runs the mesh in the background. The mirror path declines until
// the member is Ready, so agent readiness never waits for the mesh.
func (a *cachemeshAgent) start() {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.done = make(chan error, 1)

	go func() {
		err := a.mesh.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			a.logger.Error("cachemesh stopped", slog.Any("err", err))
		}

		a.done <- err
	}()
}

// stop drains the member (its partitions move to others) and stops it.
func (a *cachemeshAgent) stop() {
	if a.cancel == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cachemeshLeaveTimeout)
	defer cancel()

	if err := a.mesh.Leave(ctx); err != nil {
		a.logger.Warn("cachemesh leave", slog.Any("err", err))
	}

	a.cancel()

	select {
	case <-a.done:
	case <-ctx.Done():
	}

	a.node.Close()
}

type cachemeshMetrics struct {
	lookups       *prometheus.CounterVec
	fills         *prometheus.CounterVec
	fillBytes     *prometheus.CounterVec
	fillSeconds   *prometheus.HistogramVec
	peerRequests  *prometheus.CounterVec
	peerSeconds   *prometheus.HistogramVec
	hedges        *prometheus.CounterVec
	serves        *prometheus.CounterVec
	serveBytes    prometheus.Counter
	blobs         *prometheus.CounterVec
	blobBytes     prometheus.Counter
	blobSeconds   *prometheus.HistogramVec
	tags          *prometheus.CounterVec
	evicted       prometheus.Counter
	controlErrors prometheus.Counter
	leaseErrors   *prometheus.CounterVec
}

func newCachemeshMetrics(reg *metrics.Registry) *cachemeshMetrics {
	const sub = "cachemesh"

	return &cachemeshMetrics{
		lookups: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_lookups_total",
			Help: "Chunk lookups by requester (local mirror request or peer) and result: hit (complete in the chunk cache), joined (an in-progress fill), or fill (a new fill).",
		}, []string{"requester", "result"}),
		fills: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_fills_total",
			Help: "Chunk fill attempts by source (origin, peer, prev_owner, owner) and outcome (ok, error).",
		}, []string{"source", "outcome"}),
		fillBytes: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_fill_bytes_total",
			Help: "Bytes read into the chunk cache, by source.",
		}, []string{"source"}),
		fillSeconds: reg.NewHistogramVec(sub, prometheus.HistogramOpts{
			Name:    "gantry_cachemesh_chunk_fill_duration_seconds",
			Help:    "Duration of chunk fill attempts, by source and outcome.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"source", "outcome"}),
		peerRequests: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_peer_requests_total",
			Help: "Data-plane requests to peers by result as reported to the mesh health tracker (success, failure, timeout, overload).",
		}, []string{"result"}),
		peerSeconds: reg.NewHistogramVec(sub, prometheus.HistogramOpts{
			Name:    "gantry_cachemesh_peer_request_seconds",
			Help:    "Time from sending a data-plane request to a peer until its response headers, by result.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 16),
		}, []string{"result"}),
		hedges: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_hedges_total",
			Help: "Hedged chunk requests: sent, won (the hedge answered first), or denied by the hedge budget.",
		}, []string{"outcome"}),
		serves: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_serves_total",
			Help: "Chunk requests from peers by outcome (ok, error, shed, loop, ttl, no_route, not_found, bad_request).",
		}, []string{"outcome"}),
		serveBytes: reg.NewCounter(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_serve_bytes_total",
			Help: "Chunk bytes sent to peers.",
		}),
		blobs: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_blob_requests_total",
			Help: "Mirror blob and manifest requests offered to cachemesh, by outcome (served, declined, aborted, verify_failed).",
		}, []string{"outcome"}),
		blobBytes: reg.NewCounter(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_blob_bytes_total",
			Help: "Bytes served to containerd through cachemesh.",
		}),
		blobSeconds: reg.NewHistogramVec(sub, prometheus.HistogramOpts{
			Name:    "gantry_cachemesh_blob_duration_seconds",
			Help:    "Duration of mirror requests offered to cachemesh, by outcome.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 16),
		}, []string{"outcome"}),
		tags: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_tag_lookups_total",
			Help: "Tag resolutions by outcome: hit (fresh local copy), origin (resolved here as owner), peer, error.",
		}, []string{"outcome"}),
		evicted: reg.NewCounter(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_chunk_evicted_bytes_total",
			Help: "Bytes evicted from the in-memory chunk cache.",
		}),
		controlErrors: reg.NewCounter(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_control_errors_total",
			Help: "Failed cachemesh control-plane calls to other members.",
		}),
		leaseErrors: reg.NewCounterVec(sub, prometheus.CounterOpts{
			Name: "gantry_cachemesh_lease_errors_total",
			Help: "Failed cachemesh Lease operations, by operation.",
		}, []string{"op"}),
	}
}

func (m *cachemeshMetrics) hooks() meshfetch.Hooks {
	return meshfetch.Hooks{
		OnLookup: func(requester, result string) { m.lookups.WithLabelValues(requester, result).Inc() },
		OnFill: func(source, outcome string, bytes int64, d time.Duration) {
			m.fills.WithLabelValues(source, outcome).Inc()
			m.fillBytes.WithLabelValues(source).Add(float64(bytes))
			m.fillSeconds.WithLabelValues(source, outcome).Observe(d.Seconds())
		},
		OnPeerResult: func(result string, d time.Duration) {
			m.peerRequests.WithLabelValues(result).Inc()
			m.peerSeconds.WithLabelValues(result).Observe(d.Seconds())
		},
		OnHedge: func(outcome string) { m.hedges.WithLabelValues(outcome).Inc() },
		OnServe: func(outcome string, bytes int64) {
			m.serves.WithLabelValues(outcome).Inc()
			m.serveBytes.Add(float64(bytes))
		},
		OnBlob: func(outcome string, bytes int64, d time.Duration) {
			m.blobs.WithLabelValues(outcome).Inc()
			m.blobBytes.Add(float64(bytes))
			m.blobSeconds.WithLabelValues(outcome).Observe(d.Seconds())
		},
		OnMeta:  func(outcome string) { m.tags.WithLabelValues(outcome).Inc() },
		OnEvict: func(bytes int64) { m.evicted.Add(float64(bytes)) },
	}
}

func (m *cachemeshMetrics) registerGauges(reg *metrics.Registry, a *cachemeshAgent) {
	const sub = "cachemesh"

	boolGauge := func(b bool) float64 {
		if b {
			return 1
		}

		return 0
	}

	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_members",
		Help: "Members in this agent's cachemesh view.",
	}, func() float64 { return float64(a.mesh.Members()) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_view_term",
		Help: "Term of this agent's cachemesh view.",
	}, func() float64 { return float64(a.mesh.Version().Term) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_view_seq",
		Help: "Sequence number of this agent's cachemesh view.",
	}, func() float64 { return float64(a.mesh.Version().Seq) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_leading",
		Help: "1 while this agent is the cachemesh leader.",
	}, func() float64 { return boolGauge(a.mesh.Leading()) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_ready",
		Help: "1 while this agent is an active cachemesh member (the mirror uses cachemesh only then).",
	}, func() float64 { return boolGauge(a.mesh.Ready()) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_neighbors",
		Help: "Data-plane neighbors of this agent in the cachemesh grid.",
	}, func() float64 { return float64(len(a.mesh.Neighbors())) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_chunk_cache_bytes",
		Help: "Bytes charged to the chunk cache budget: cached chunks plus fills in progress.",
	}, func() float64 { return float64(a.node.Stats().Bytes) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_chunk_cache_chunks",
		Help: "Chunks in the chunk cache, complete or in progress.",
	}, func() float64 { return float64(a.node.Stats().Chunks) })
	reg.NewGaugeFunc(sub, prometheus.GaugeOpts{
		Name: "gantry_cachemesh_chunk_fills_inflight",
		Help: "Chunk fills in progress.",
	}, func() float64 { return float64(a.node.Stats().Inflight) })
}
