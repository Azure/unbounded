// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/app"
	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/cluster"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// ClusterOptions controls Harness.StartCluster.
type ClusterOptions struct {
	// Replicas is the number of in-process orca instances. Defaults
	// to 3 when zero, matching the production deploy/orca topology.
	Replicas int

	// ChunkSize is the per-chunk byte count. The orca config validator
	// enforces a 1 MiB minimum; tests typically use 1 MiB to keep test
	// blob sizes manageable while still spanning multiple chunks.
	ChunkSize int64

	// OriginID is the logical origin identifier (echoed in chunk paths).
	OriginID string

	// OriginBucket is the bucket on the origin S3 backend / Azurite.
	OriginBucket string

	// OriginDriver is "awss3" (default) or "azureblob".
	OriginDriver string

	// S3Backend is the S3-backend handle used for origin (when
	// OriginDriver=="awss3") and always for cachestore.
	S3Backend *S3Backend

	// Azurite is required when OriginDriver=="azureblob".
	Azurite *Azurite

	// AzureContainer is the Azurite container name for the origin.
	AzureContainer string

	// CachestoreBucket is the bucket on the S3 backend used as the orca
	// cachestore. If empty, a fresh bucket is allocated.
	CachestoreBucket string

	// OriginOverride, when set, replaces the constructed origin driver.
	// Used to wire CountingOrigin around the real client.
	OriginOverride origin.Origin

	// CacheStoreOverride, when set, replaces the constructed cachestore
	// driver.
	CacheStoreOverride cachestore.CacheStore

	// InternalHandlerWrap, when set, is registered with each replica's
	// app.WithInternalHandlerWrap. Tests use this to install a 409
	// counter (CountingInternalHandlerWrap.WrapFor).
	InternalHandlerWrap *CountingInternalHandlerWrap
}

// Replica represents one running *app.App in the harness.
type Replica struct {
	App          *app.App
	SelfIP       string
	InternalPort int
	PeerSource   *StaticPeerSource
	HTTP         *Client // pre-built client targeting this replica's edge
}

// Cluster is a collection of Replicas plus the harness-owned context.
type Cluster struct {
	Replicas []*Replica
}

// Get returns replica i (1-indexed).
func (c *Cluster) Get(i int) *Replica { return c.Replicas[i-1] }

// Len returns the replica count.
func (c *Cluster) Len() int { return len(c.Replicas) }

// FindBySelfIPPort returns the replica whose (SelfIP, InternalPort)
// matches the given peer; nil if none.
func (c *Cluster) FindBySelfIPPort(ip string, port int) *Replica {
	for _, r := range c.Replicas {
		if r.SelfIP == ip && r.InternalPort == port {
			return r
		}
	}

	return nil
}

// StartCluster brings up `opts.Replicas` orca instances (default 3)
// pointed at the origin/cachestore described in opts. Every replica
// binds to 127.0.0.1 with an OS-assigned distinct internal port; one
// StaticPeerSource per replica is initialized with the full peer set
// (with explicit ports). Tests can mutate any replica's PeerSource
// independently.
//
// Cleanup (Shutdown of each app) is registered with t.Cleanup.
func StartCluster(ctx context.Context, t *testing.T, opts ClusterOptions) *Cluster {
	t.Helper()

	if opts.Replicas == 0 {
		opts.Replicas = 3
	}

	if opts.Replicas < 1 {
		t.Fatalf("StartCluster: Replicas must be >= 1, got %d", opts.Replicas)
	}

	if opts.ChunkSize == 0 {
		opts.ChunkSize = 1024 * 1024
	}

	if opts.OriginDriver == "" {
		opts.OriginDriver = "awss3"
	}

	if opts.OriginID == "" {
		opts.OriginID = "inttest-origin"
	}

	if opts.S3Backend == nil {
		t.Fatal("StartCluster: S3Backend handle required")
	}

	if opts.OriginDriver == "azureblob" {
		if opts.Azurite == nil {
			t.Fatal("StartCluster: Azurite handle required for azureblob driver")
		}

		if opts.AzureContainer == "" {
			t.Fatal("StartCluster: AzureContainer required for azureblob driver")
		}
	}

	if opts.OriginBucket == "" && opts.OriginDriver == "awss3" {
		t.Fatal("StartCluster: OriginBucket required for awss3 driver")
	}

	cacheBucket := opts.CachestoreBucket
	if cacheBucket == "" {
		cacheBucket = opts.S3Backend.NewBucket(ctx, t, "orca-cache")
	}

	// Allocate per-replica internal listeners up front (open) so each
	// replica's peer source can advertise the full set with explicit
	// ports from t=0. We hand the open listeners to app.Start via
	// WithInternalListener/WithEdgeListener/WithOpsListener so there
	// is no close-and-rebind window for races with concurrent tests.
	internalListeners := make([]net.Listener, opts.Replicas)
	internalPorts := make([]int, opts.Replicas)
	edgeListeners := make([]net.Listener, opts.Replicas)
	opsListeners := make([]net.Listener, opts.Replicas)

	for i := range internalListeners {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			closeListeners(internalListeners)
			closeListeners(edgeListeners)
			closeListeners(opsListeners)
			t.Fatalf("alloc internal port for replica %d: %v", i+1, err)
		}

		internalListeners[i] = ln
		internalPorts[i] = ln.Addr().(*net.TCPAddr).Port //nolint:errcheck // *net.TCPAddr from net.Listen

		eln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			closeListeners(internalListeners)
			closeListeners(edgeListeners)
			closeListeners(opsListeners)
			t.Fatalf("alloc edge port for replica %d: %v", i+1, err)
		}

		edgeListeners[i] = eln

		oln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			closeListeners(internalListeners)
			closeListeners(edgeListeners)
			closeListeners(opsListeners)
			t.Fatalf("alloc ops port for replica %d: %v", i+1, err)
		}

		opsListeners[i] = oln
	}

	allPeers := make([]cluster.Peer, opts.Replicas)
	for i := range allPeers {
		allPeers[i] = cluster.Peer{
			IP:   "127.0.0.1",
			Port: internalPorts[i],
		}
	}

	cl := &Cluster{}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 0; i < opts.Replicas; i++ {
		selfIP := "127.0.0.1"
		selfPort := internalPorts[i]
		ps := NewStaticPeerSource(selfIP, selfPort, allPeers)

		cfg := buildConfig(opts, cacheBucket)
		cfg.Cluster.SelfPodIP = selfIP
		cfg.Cluster.InternalListen = net.JoinHostPort(selfIP, strconv.Itoa(selfPort))
		cfg.Server.Listen = edgeListeners[i].Addr().String()

		appOpts := []app.Option{
			app.WithLogger(logger),
			app.WithPeerSource(ps),
			app.WithEdgeListener(edgeListeners[i]),
			app.WithInternalListener(internalListeners[i]),
			app.WithOpsListener(opsListeners[i]),
		}

		if opts.OriginOverride != nil {
			appOpts = append(appOpts, app.WithOrigin(opts.OriginOverride))
		}

		if opts.CacheStoreOverride != nil {
			appOpts = append(appOpts, app.WithCacheStore(opts.CacheStoreOverride))
		}

		if opts.InternalHandlerWrap != nil {
			appOpts = append(appOpts, app.WithInternalHandlerWrap(opts.InternalHandlerWrap.WrapFor(selfIP+":"+strconv.Itoa(selfPort))))
		}

		a, err := app.Start(ctx, cfg, appOpts...)
		if err != nil {
			t.Fatalf("app.Start replica %d: %v", i+1, err)
		}

		r := &Replica{
			App:          a,
			SelfIP:       selfIP,
			InternalPort: selfPort,
			PeerSource:   ps,
			HTTP:         NewClient("http://" + a.EdgeAddr),
		}
		cl.Replicas = append(cl.Replicas, r)

		t.Cleanup(func() {
			ctxShut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_ = a.Shutdown(ctxShut) //nolint:errcheck // shutdown logs already emitted
		})
	}
	// Wait for every replica's Cluster.Peers() to converge to the
	// full set.
	if err := waitForPeers(ctx, cl, opts.Replicas, 2*time.Second); err != nil {
		t.Fatalf("waitForPeers: %v", err)
	}

	return cl
}

func buildConfig(opts ClusterOptions, cacheBucket string) *config.Config {
	cfg := &config.Config{
		Server: config.Server{
			Listen: "127.0.0.1:0",
			Auth:   config.ServerAuth{Enabled: false},
		},
		Origin: config.Origin{
			ID:           opts.OriginID,
			Driver:       opts.OriginDriver,
			TargetGlobal: 32,
			QueueTimeout: 5 * time.Second,
			Retry: config.OriginRetry{
				Attempts:         2,
				BackoffInitial:   10 * time.Millisecond,
				BackoffMax:       50 * time.Millisecond,
				MaxTotalDuration: 2 * time.Second,
			},
		},
		Cachestore: config.Cachestore{
			Driver: "s3",
			S3: config.CachestoreS3{
				Endpoint:     opts.S3Backend.Endpoint(),
				Bucket:       cacheBucket,
				Region:       opts.S3Backend.Region(),
				AccessKey:    opts.S3Backend.AccessKey(),
				SecretKey:    opts.S3Backend.SecretKey(),
				UsePathStyle: true,
			},
		},
		Cluster: config.Cluster{
			Service:           "orca-peers.test.svc.cluster.local",
			MembershipRefresh: 250 * time.Millisecond,
			InternalListen:    "127.0.0.1:0", // overridden per replica
			InternalTLS:       config.InternalTLS{Enabled: false},
			TargetReplicas:    opts.Replicas,
			SelfPodIP:         "127.0.0.1", // overridden per replica
		},
		ChunkCatalog: config.ChunkCatalog{MaxEntries: 1024},
		Metadata: config.Metadata{
			TTL:         5 * time.Minute,
			NegativeTTL: 5 * time.Second,
			MaxEntries:  1024,
		},
		Chunking: config.Chunking{Size: config.ByteSize(opts.ChunkSize)},
	}

	switch opts.OriginDriver {
	case "awss3":
		cfg.Origin.AWSS3 = config.AWSS3{
			Endpoint:     opts.S3Backend.Endpoint(),
			Region:       opts.S3Backend.Region(),
			Bucket:       opts.OriginBucket,
			AccessKey:    opts.S3Backend.AccessKey(),
			SecretKey:    opts.S3Backend.SecretKey(),
			UsePathStyle: true,
		}
	case "azureblob":
		cfg.Origin.Azureblob = config.Azureblob{
			Account:    opts.Azurite.AccountName(),
			AccountKey: opts.Azurite.AccountKey(),
			Container:  opts.AzureContainer,
			Endpoint:   opts.Azurite.Endpoint(),
		}
	}

	return cfg
}

// waitForPeers polls each replica's cluster.Peers() until every
// replica has at least the expected count or the deadline elapses.
func waitForPeers(ctx context.Context, cl *Cluster, want int, dl time.Duration) error {
	deadline := time.Now().Add(dl)

	for time.Now().Before(deadline) {
		ok := true

		for _, r := range cl.Replicas {
			if len(r.App.Cluster.Peers()) < want {
				ok = false
				break
			}
		}

		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return fmt.Errorf("peer-set did not converge to %d on all %d replicas within %s",
		want, len(cl.Replicas), dl)
}

func closeListeners(lns []net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close() //nolint:errcheck // best-effort cleanup
		}
	}
}
