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

// cachemeshContent returns the local content source: the content root
// read directly. It returns nil (serve peers from memory only) when the root
// is unset or unreadable.
func cachemeshContent(root string, logger *slog.Logger) meshfetch.ContentSource {
	if root == "" {
		return nil
	}

	dc, err := meshfetch.NewDirContent(root, logger)
	if err != nil {
		logger.Warn("cachemesh content root unreadable; serving peers from memory only",
			slog.String("content_root", root), slog.Any("err", err))

		return nil
	}

	return dc
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

	lease := kubelease.New(client.CoordinationV1().Leases(c.CachemeshNamespace()), config.CachemeshLeaseName)
	logger = logger.With(slog.String("subsystem", "cachemesh"))
	m := newCachemeshMetrics(reg)

	mesh, err := cachemesh.New(cachemesh.Config{
		ID:           cachemeshMemberID(c),
		Addr:         addr,
		Transport:    meshfetch.NewControlTransport(),
		Lease:        lease,
		Logger:       logger,
		MaxNeighbors: cm.MaxNeighbors,
		ExpectedSize: cm.ExpectedSize,
		Hooks: cachemesh.Hooks{
			OnControlError: func(cachemesh.Endpoint, error) { m.controlErrors.Inc() },
			OnLeaseError:   func(op string, _ error) { m.leaseErrors.WithLabelValues(op).Inc() },
		},
	})
	if err != nil {
		return nil, err
	}

	content := cachemeshContent(cm.ContentRoot, logger)

	node, err := meshfetch.New(meshfetch.Config{
		Mesh:          mesh,
		Origin:        rangeOrigin,
		Tags:          tagOrigin,
		ChunkSize:     cm.ChunkSize,
		Content:       content,
		InflightBytes: cm.InflightBytes,
		MaxFills:      cm.MaxConcurrentChunkFills,
		Verify:        cm.VerifyDigest,
		Logger:        logger,
		Hooks:         m.hooks(),
	})
	if err != nil {
		return nil, err
	}

	a := &cachemeshAgent{mesh: mesh, node: node, logger: logger, tags: tagOrigin != nil}
	a.meshPtr.Store(mesh)
	registerCachemeshState(reg, a)

	logger.Info("cachemesh configured",
		slog.String("id", mesh.ID()),
		slog.String("addr", addr),
		slog.String("lease", c.CachemeshNamespace()+"/"+config.CachemeshLeaseName),
		slog.Int64("chunk_size", cm.ChunkSize),
		slog.String("content_root", cm.ContentRoot),
		slog.Bool("content_readable", content != nil),
		slog.Int("max_neighbors", cm.MaxNeighbors),
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
	controlErrors prometheus.Counter
	leaseErrors   *prometheus.CounterVec
}

const cachemeshSubsystem = "cachemesh"

func newCachemeshMetrics(reg *metrics.Registry) *cachemeshMetrics {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return reg.NewCounterVec(cachemeshSubsystem, prometheus.CounterOpts{Name: "gantry_cachemesh_" + name, Help: help}, labels)
	}
	single := func(name, help string) prometheus.Counter {
		return reg.NewCounter(cachemeshSubsystem, prometheus.CounterOpts{Name: "gantry_cachemesh_" + name, Help: help})
	}
	histogram := func(name, help string, start float64, n int, labels ...string) *prometheus.HistogramVec {
		return reg.NewHistogramVec(cachemeshSubsystem, prometheus.HistogramOpts{
			Name: "gantry_cachemesh_" + name, Help: help, Buckets: prometheus.ExponentialBuckets(start, 2, n),
		}, labels)
	}

	return &cachemeshMetrics{
		lookups: counter("chunk_lookups_total",
			"Chunk lookups by requester (local mirror request or peer) and result: hit (complete in memory), joined (an in-progress fill), fill (a new fill), or local (a peek served from the local content store).",
			"requester", "result"),
		fills: counter("chunk_fills_total",
			"Chunk fill attempts by source (origin, peer, prev_owner, owner, local, holder) and outcome (ok, error).",
			"source", "outcome"),
		fillBytes: counter("chunk_fill_bytes_total", "Bytes read into chunk fills, by source.", "source"),
		fillSeconds: histogram("chunk_fill_duration_seconds", "Duration of chunk fill attempts, by source and outcome.",
			0.005, 14, "source", "outcome"),
		peerRequests: counter("peer_requests_total",
			"Data-plane requests to peers by result as reported to the mesh health tracker (success, failure, timeout, overload).",
			"result"),
		peerSeconds: histogram("peer_request_seconds",
			"Time from sending a data-plane request to a peer until its response headers, by result.", 0.0005, 16, "result"),
		hedges: counter("hedges_total",
			"Hedged chunk requests: sent, won (the hedge answered first), or denied by the hedge budget.", "outcome"),
		serves: counter("chunk_serves_total",
			"Chunk requests from peers by outcome (ok, error, shed, loop, ttl, no_route, not_found, bad_request).", "outcome"),
		serveBytes: single("chunk_serve_bytes_total", "Chunk bytes sent to peers."),
		blobs: counter("blob_requests_total",
			"Mirror blob and manifest requests offered to cachemesh, by outcome (served, declined, aborted, verify_failed).",
			"outcome"),
		blobBytes: single("blob_bytes_total", "Bytes served to containerd through cachemesh."),
		blobSeconds: histogram("blob_duration_seconds", "Duration of mirror requests offered to cachemesh, by outcome.",
			0.005, 16, "outcome"),
		tags: counter("tag_lookups_total",
			"Tag resolutions by outcome: hit (fresh local copy), origin (resolved here as owner), peer, error.", "outcome"),
		controlErrors: single("control_errors_total", "Failed cachemesh control-plane calls to other members."),
		leaseErrors:   counter("lease_errors_total", "Failed cachemesh Lease operations, by operation.", "op"),
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
		OnMeta: func(outcome string) { m.tags.WithLabelValues(outcome).Inc() },
	}
}

// cachemeshStateMetric is a gauge or counter read from the mesh or the
// chunk store at scrape time.
type cachemeshStateMetric struct {
	name    string
	help    string
	counter bool
	value   func(a *cachemeshAgent, st meshfetch.Stats) float64
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

var cachemeshStateMetrics = []cachemeshStateMetric{
	{
		"members", "Members in this agent's cachemesh view.", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return float64(a.mesh.Members()) },
	},
	{
		"view_term", "Term of this agent's cachemesh view.", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return float64(a.mesh.Version().Term) },
	},
	{
		"view_seq", "Sequence number of this agent's cachemesh view.", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return float64(a.mesh.Version().Seq) },
	},
	{
		"leading", "1 while this agent is the cachemesh leader.", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return boolGauge(a.mesh.Leading()) },
	},
	{
		"ready", "1 while this agent is an active cachemesh member (the mirror uses cachemesh only then).", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return boolGauge(a.mesh.Ready()) },
	},
	{
		"neighbors", "Data-plane neighbors of this agent in the cachemesh grid.", false,
		func(a *cachemeshAgent, _ meshfetch.Stats) float64 { return float64(len(a.mesh.Neighbors())) },
	},
	{
		"chunk_store_chunks", "Chunks in the chunk store, complete or in progress.", false,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.Chunks) },
	},
	{
		"chunk_fills_inflight", "Chunk fills in progress.", false,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.Inflight) },
	},
	{
		"page_memory_bytes", "Memory in chunk page buffers in use: in-flight fill pages, pages being sent, lingering chunks, and recently read content store pages.", false,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.PageBytes) },
	},
	{
		"inflight_borrowed_bytes", "Memory fills borrowed from the in-flight budget for lagging readers.", false,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.ExtraPageBytes) },
	},
	{
		"memory_chunks", "Complete chunks peers read that linger in borrowed in-flight pages.", false,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.MemoryChunks) },
	},
	{
		"local_page_reads_total", "Chunk pages read from the local containerd content store for readers behind a fill's window.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.LocalReads) },
	},
	{
		"shared_page_reads_total", "Page reads served by a concurrent or recent reader's read instead of the content store.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.PageReadsShared) },
	},
	{
		"local_errors_total", "Failed reads of the local containerd content store; the affected readers retry elsewhere.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.LocalErrors) },
	},
	{
		"local_fills_total", "Chunk fills read from the local containerd content store.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.LocalFills) },
	},
	{
		"holder_peeks_total", "Owner reads of a chunk from a recent requester's content store that succeeded.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.HolderPeeks) },
	},
	{
		"holder_misses_total", "Owner reads of a chunk from a recent requester's content store that failed.", true,
		func(_ *cachemeshAgent, st meshfetch.Stats) float64 { return float64(st.HolderMisses) },
	},
}

// registerCachemeshState registers the scrape-time gauges and counters of a.
func registerCachemeshState(reg *metrics.Registry, a *cachemeshAgent) {
	for _, sm := range cachemeshStateMetrics {
		f := func() float64 { return sm.value(a, a.node.Stats()) }
		name := "gantry_cachemesh_" + sm.name

		if sm.counter {
			reg.NewCounterFunc(cachemeshSubsystem, prometheus.CounterOpts{Name: name, Help: sm.help}, f)
		} else {
			reg.NewGaugeFunc(cachemeshSubsystem, prometheus.GaugeOpts{Name: name, Help: sm.help}, f)
		}
	}
}
