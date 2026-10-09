// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type fixtureConfig struct {
	authority.Config
	ServerConfig Config
	PeerPort     uint16
}

func testReplicationFlush(t *testing.T, f *servingFixture, unchanged, fail bool) {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, ReplicationPath, nil)
	request.TLS = f.requestState(t)
	request.Header.Set("Authorization", "Bearer "+f.token)

	want := http.StatusOK

	if unchanged {
		p, err := f.a.authority.Current()
		require.NoError(t, err)

		request.URL.RawQuery = fmt.Sprintf("after=%d", p.Sequence())
		want = http.StatusNoContent
	}

	w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true, fail: fail}

	unblock := sync.OnceFunc(func() { close(w.unblock) })
	defer unblock()

	done := make(chan any, 1)

	go func() { done <- serveRecover(f.a.Server.Handler(), w, request) }()

	select {
	case <-w.entered:
	case <-time.After(8 * time.Second):
		t.Fatal("explicit flush not reached")
	}

	require.Len(t, f.a.Server.writes, 1, "write admission released before flush")
	require.Equal(t, 1, f.a.Server.replicationPolls.count(), "poll admission released before flush")

	duplicate := httptest.NewRecorder()
	f.a.Server.Handler().ServeHTTP(duplicate, request.Clone(f.ctx))
	require.Equal(t, http.StatusTooManyRequests, duplicate.Code, "duplicate during flush")
	unblock()

	var wantAbort any
	if fail {
		wantAbort = http.ErrAbortHandler
	}

	require.Equal(t, wantAbort, <-done, "flush result")
	require.Equal(t, want, w.Code)
	require.Empty(t, f.a.Server.writes, "write admission leaked after flush")
	require.Zero(t, f.a.Server.replicationPolls.count(), "poll admission leaked after flush")
}

func (r *TopologyReconciler) observeTopology(ctx context.Context) (TopologyObservation, error) {
	cfg := r.Config

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return TopologyObservation{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := members.BuildCatalog(caches.Items)
	if err != nil {
		return TopologyObservation{}, err
	}

	ownership, err := members.ReadWorkloadIdentities(ctx, r.APIReader, cfg.Namespace, cfg.DaemonSetName)
	if err != nil {
		return TopologyObservation{}, err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods per Node.
	podsByNode := make(map[string][]corev1.Pod, len(nodes.Items))
	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return TopologyObservation{}, err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(cfg.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return TopologyObservation{}, err
		}

		podsByNode[node.Name] = list.Items
	}

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: members.Input{
		Nodes: nodes.Items, PodsByNode: podsByNode, Ownership: ownership, PeerPort: cfg.PeerPort,
	}}, nil
}

func (c fixtureConfig) authorityConfig() authority.Config { return c.Config }

type Application struct {
	authority   *authority.Authority
	Topology    *TopologyReconciler
	Keyring     *KeyringReconciler
	Server      *Server
	Lifecycle   *Lifecycle
	Replication *fixtureLeader
}

type TopologyReconciler struct {
	client.Client
	APIReader client.Reader
	Config    fixtureConfig
	authority *authority.Authority
}

func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_, err := r.authority.PublishTopology(ctx, r.observeTopology)
	return ctrl.Result{}, err
}

type KeyringReconciler struct {
	Config    fixtureConfig
	authority *authority.Authority
}

func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	delay, err := r.authority.ReconcileCredentials(ctx)
	return ctrl.Result{RequeueAfter: delay}, err
}

type fixtureLeader struct {
	Config    fixtureConfig
	Client    client.Client
	APIReader client.Reader
	authority *authority.Authority
	mu        sync.Mutex
	leader    context.Context
}

func (r *fixtureLeader) LeaderContext() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader, r.leader != nil && r.leader.Err() == nil
}

func (r *fixtureLeader) PollInterval() time.Duration {
	return min(5*time.Second, r.Config.SnapshotMaxAge/3)
}

func (r *fixtureLeader) AuthenticateReplica(ctx context.Context, req *http.Request) (string, time.Time, error) {
	i, err := r.authority.AuthenticateReplica(ctx, req)
	return i.UID(), i.Expires(), err
}

func (r *fixtureLeader) observe(ctx context.Context) { _ = r.authority.Observe(ctx) }

func (r *fixtureLeader) installReplica(ctx, process context.Context, p wire.Publication) error {
	return r.authority.AcceptReplica(ctx, process, p)
}

func Assemble(cfg fixtureConfig, c client.Client, reader client.Reader) *Application {
	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: c, Reader: reader})
	l := NewLifecycle(a)
	r := &fixtureLeader{Config: cfg, Client: c, APIReader: reader, authority: a}

	return &Application{authority: a, Topology: &TopologyReconciler{Client: c, APIReader: reader, Config: cfg, authority: a}, Keyring: &KeyringReconciler{Config: cfg, authority: a}, Server: New(cfg.ServerConfig, c, a, l, r), Lifecycle: l, Replication: r}
}

type cachedScaleClient struct {
	client.Client
	reader client.Reader
}

func (c cachedScaleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c cachedScaleClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

// The informer uses a synthetic list/watch HTTP source, but the cache, field
// index, deep copies, reconciler, canonical hashes, encoding and waiting are real.
// Only durable version CAS uses a fake client. This is deliberately not an HTTPS
// authentication or API-server capacity benchmark.
func TestServerScale(t *testing.T) {
	if os.Getenv("RACER_SCALE") != "1" {
		t.Skip("set RACER_SCALE=1 for 100,000-member reconciliation and waiter measurements")
	}

	for _, count := range []int{1_000, 10_000, 100_000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := initializedTopology(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reader := scaleCache(t, r, count)
			r.Client = cachedScaleClient{Client: r.Client, reader: reader}

			runtime.GC()

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			start := time.Now()
			first := reconcileTopology(t, r, ctx)
			cold := time.Since(start)

			runtime.ReadMemStats(&after)
			require.Len(t, acceptedMembers(t, r), count)
			t.Logf("members=%d cold_reconcile=%s allocated_bytes=%d publication_bytes=%d", count, cold, after.TotalAlloc-before.TotalAlloc, len(first.encoded))

			start = time.Now()

			if current := reconcileTopology(t, r, ctx); current != first {
				t.Fatal("no-op reconcile replaced publication")
			}

			t.Logf("members=%d unchanged_reconcile=%s", count, time.Since(start))

			if count == 100_000 {
				scaleFanout(t, r, ctx, count)
			}
		})
	}
}

func scaleLists(t *testing.T, r *TopologyReconciler, count int) map[string]any {
	t.Helper()

	nodes := &corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	pods := &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}

	for i := range count {
		uid := types.UID(fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))
		node := memberNode()
		node.Name, node.UID, node.ResourceVersion = fmt.Sprintf("node-%d", i), uid, "1"
		node.Annotations = map[string]string{wire.RDMANICsAnnotation: `[{"rail":0,"device":"mlx5_0","port":1,"numa_node":0},{"rail":1,"device":"mlx5_1","port":1,"numa_node":1}]`}
		pod := memberPod(uid, 1, fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255))
		pod.Spec.NodeName, pod.ResourceVersion = node.Name, "1"
		pod.OwnerReferences[0].Name = r.Config.DaemonSetName

		nodes.Items, pods.Items = append(nodes.Items, node), append(pods.Items, pod)
	}

	ds := &appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.DaemonSet{{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName, UID: testDaemonSetUID, ResourceVersion: "1"}}}}

	caches := &racerv1.ClusterCacheList{TypeMeta: metav1.TypeMeta{APIVersion: racerv1.GroupVersion.String(), Kind: "ClusterCacheList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	for i := range 16 {
		caches.Items = append(caches.Items, catalogCache(fmt.Sprintf("cache-%d", i), types.UID(fmt.Sprintf("%08x-1111-4000-8000-000000000000", i))))
		require.NoError(t, r.Create(t.Context(), &caches.Items[i]))
	}

	runKeys(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)

	return map[string]any{
		"/api/v1/nodes": nodes,
		"/api/v1/namespaces/" + r.Config.Namespace + "/pods":             pods,
		"/apis/apps/v1/namespaces/" + r.Config.Namespace + "/daemonsets": ds,
		"/apis/" + racerv1.GroupVersion.String() + "/clustercaches":      caches,
	}
}

func scaleSource(lists map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if req.URL.Query().Get("sendInitialEvents") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonBadRequest, Code: 400, Message: "synthetic source supports ordinary list/watch"})

			return
		}

		if req.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			<-req.Context().Done()

			return
		}

		list, ok := lists[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}

		json.NewEncoder(w).Encode(list)
	})
}

func scaleCache(t *testing.T, r *TopologyReconciler, count int) cache.Cache {
	t.Helper()
	source := httptest.NewServer(scaleSource(scaleLists(t, r, count)))
	t.Cleanup(source.Close)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, appsv1.SchemeGroupVersion, racerv1.GroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("PodList"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("DaemonSet"), meta.RESTScopeNamespace)
	mapper.Add(racerv1.GroupVersion.WithKind("ClusterCache"), meta.RESTScopeRoot)

	options := cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:       {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
		&corev1.Secret{}:    {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}, Field: fields.OneTermEqualSelector("metadata.name", r.Config.CredentialsSecretName)},
		&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
		&appsv1.DaemonSet{}: {Namespaces: map[string]cache.Config{r.Config.Namespace: {}}},
	}}
	options.Scheme, options.Mapper = r.Scheme(), mapper
	reader, err := cache.New(&rest.Config{Host: source.URL, QPS: 1000, Burst: 1000}, options)
	require.NoError(t, err)
	require.NoError(t, reader.IndexField(t.Context(), &corev1.Pod{}, podNodeIndex, podNodeKeys))

	for _, obj := range []client.Object{&corev1.Node{}, &appsv1.DaemonSet{}, &racerv1.ClusterCache{}} {
		_, err := reader.GetInformer(t.Context(), obj)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- reader.Start(ctx) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	syncCtx, stopSync := context.WithTimeout(ctx, time.Minute)
	defer stopSync()

	require.True(t, reader.WaitForCacheSync(syncCtx), "scale informer did not synchronize")

	return reader
}

func scaleFanout(t *testing.T, r *TopologyReconciler, ctx context.Context, count int) {
	t.Helper()

	current, err := r.authority.Current()
	require.NoError(t, err)
	// Keep both realistic full-size encodings alive. Prepare before admission so
	// fanout measures Install plus delivery, independent of canonical encoding.
	members := make(AcceptedMembers, count)

	for id, member := range acceptedMembers(t, r) {
		member.Shares++
		members[id] = member
	}

	sequence := current.Sequence()
	server := New(r.Config.ServerConfig, nil, nil, nil, nil)

	waiting, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan *authority.PublicationHandle, count)
	failures := make(chan error, count)

	var wg sync.WaitGroup

	runtime.GC()
	runtime.GC() // Clear temporary encoding sync.Pools before the waiter baseline.

	var before, parked runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()

	for id := range members {
		wg.Go(func() {
			if !server.polls.acquire(id) {
				results <- nil

				failures <- wire.Overloaded

				return
			}
			defer server.polls.release(id)

			p, err := waitFixturePublication(waiting, r.authority, sequence)
			results <- p

			failures <- err
		})
	}

	defer wg.Wait()
	defer cancel()

	eventually(t, "100000 admitted waiters", func() bool { return server.polls.count() == count })

	admit := time.Since(start)

	runtime.GC()
	runtime.ReadMemStats(&parked)
	require.False(t, server.polls.acquire(testOtherUID), "global bound failed")

	for id := range members {
		require.False(t, server.polls.acquire(id), "duplicate bound failed")
		break
	}

	start = time.Now()
	next := replicationSmokePublish(t, ctx, r, members)
	install := time.Since(start)

	wg.Wait()

	fanout := time.Since(start)

	for range count {
		require.NoError(t, <-failures)

		got := <-results
		require.NotNil(t, got)
		require.Equal(t, next.record.Sequence, got.Sequence(), "waiter missed or copied full publication")
	}

	awaitServerPolls(t, server, 0)
	t.Logf("waiters=%d GOMAXPROCS=%d admission=%s install=%s all_delivered=%s heap_delta=%d stack_delta=%d next_bytes=%d", count, runtime.GOMAXPROCS(0), admit, install, fanout, int64(parked.HeapAlloc)-int64(before.HeapAlloc), int64(parked.StackInuse)-int64(before.StackInuse), len(next.encoded))
}

// These are real HTTPS protocol clients, not Rust processes or in-memory Wait
// calls. Kubernetes authority is fake. Never interpret this as API capacity.
func TestReplicatedServingSmoke(t *testing.T) { replicatedServingSmoke(t, 12) }

func TestReplicatedServingCapacity(t *testing.T) {
	value := os.Getenv("RACER_REPLICATION_CLIENTS")
	if value == "" {
		t.Skip("set RACER_REPLICATION_CLIENTS to an exact count in [1,10000]")
	}

	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 10_000 {
		t.Fatal("RACER_REPLICATION_CLIENTS must be in [1,10000]; 100k requires distributed validation, not this loopback harness")
	}

	replicatedServingSmoke(t, count)
}

type replicationSmokeListener struct {
	net.Listener
	accepted atomic.Int64
	live     atomic.Int64
}

type replicationSmokeConn struct {
	net.Conn
	once  sync.Once
	owner *replicationSmokeListener
}

func (l *replicationSmokeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	l.accepted.Add(1)
	l.live.Add(1)

	return &replicationSmokeConn{Conn: c, owner: l}, nil
}

func (c *replicationSmokeConn) Close() error {
	c.once.Do(func() { c.owner.live.Add(-1) })
	return c.Conn.Close()
}

type replicationSmokeReplica struct {
	a        *Application
	ctx      context.Context
	cancel   context.CancelFunc
	endpoint string
	listener *replicationSmokeListener
	done     chan error
}

type replicationSmokePeer struct {
	client  *http.Client
	replica int
}

type replicationSmokeResult struct {
	index   int
	bytes   int64
	digest  [32]byte
	elapsed time.Duration
	err     error
}

type replicationSmoke struct {
	ctx         context.Context
	fixture     *servingFixture
	peers       []replicationSmokePeer
	replicas    []*replicationSmokeReplica
	internal    *http.Client
	reviews     atomic.Int64
	overloaded  atomic.Int64
	reconnected atomic.Int64
	workers     sync.WaitGroup
}

func replicatedServingSmoke(t *testing.T, count int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	setup := time.Now()
	f := newServingFixture(t)
	smoke := &replicationSmoke{ctx: ctx, fixture: f}
	accepted := smoke.createPeers(t, count)
	base := replicationSmokePublish(t, ctx, f.a.Topology, accepted)
	smoke.startReplicas(t)
	smoke.authorizeReplicas(t)
	smoke.install(t, base)
	smoke.internal = f.client(t, nil)
	smoke.internal.Timeout = 30 * time.Second
	smoke.replicate(t, base)

	leader := smoke.replicas[0]
	for _, path := range []string{wire.SnapshotPath, ReplicationPath} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+path, nil)
		require.NoError(t, err)
		response, err := smoke.internal.Do(request)
		responseBody(t, response, err, http.StatusUnauthorized)
	}

	observed := make(chan struct{})

	go func() { defer close(observed); smoke.observe() }()

	defer func() { cancel(); <-observed }()
	defer func() { cancel(); smoke.workers.Wait() }()

	t.Logf("clients=%d replicas=3 GOMAXPROCS=%d setup=%s full_bytes=%d max_writes=%d max_auth=%d", count, runtime.GOMAXPROCS(0), time.Since(setup), len(base.encoded), leader.a.Server.config.Limits.MaxConcurrentWrites, leader.a.Server.config.Limits.MaxConcurrentBootstrap)
	replicationSmokeStats(t, "baseline", smoke.replicas)

	start := time.Now()
	smoke.collect(t, "cold", start, smoke.run(0, true), base)

	for phase := range 2 {
		start = time.Now()
		results := smoke.run(base.record.Sequence, false)
		replicationSmokePark(t, ctx, smoke.replicas, count, results)
		replicationSmokeStats(t, "parked", smoke.replicas)

		if phase == 1 {
			smoke.replicas[2].cancel()
			replicationSmokePark(t, ctx, smoke.replicas[:2], count, results)
			t.Logf("failure_repark=%s", time.Since(start))
			replicationSmokeStats(t, "failure-parked", smoke.replicas)
		}

		for id, member := range accepted {
			member.Shares++
			accepted[id] = member
		}

		start = time.Now()
		base = replicationSmokePublish(t, ctx, f.a.Topology, accepted)
		smoke.install(t, base)
		smoke.replicate(t, base)
		smoke.collect(t, []string{"update", "replica-failure-update"}[phase], start, results, base)
	}

	require.Equal(t, int64(count/3), smoke.reconnected.Load(), "reconnected clients")
}

func (s *replicationSmoke) createPeers(t *testing.T, count int) AcceptedMembers {
	t.Helper()

	f := s.fixture
	accepted := make(AcceptedMembers, count)
	s.peers = make([]replicationSmokePeer, count)
	handshakes := make(chan struct{}, 24)
	_, _, rotation, material := keyState(t, f.a.Keyring)
	ca, signingKey, err := parseSigning(material.Keys[rotation.ActiveIssuer])
	require.NoError(t, err)

	for i := range s.peers {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		accepted[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: fmt.Sprintf("10.%d.%d.%d:8082", i>>16, (i>>8)&255, i&255), RDMANICs: []wire.RDMANIC{}}
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		template := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "spiffe", Host: string(f.request.Cluster), Path: "/node/" + string(id)}}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, signingKey)
		require.NoError(t, err)

		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}}}, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second}
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			select {
			case handshakes <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			defer func() { <-handshakes }()

			bounded, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()

			return (&tls.Dialer{Config: transport.TLSClientConfig}).DialContext(bounded, network, address)
		}
		t.Cleanup(transport.CloseIdleConnections)
		s.peers[i] = replicationSmokePeer{client: &http.Client{Transport: transport, Timeout: 45 * time.Second}, replica: i % 3}
	}

	return accepted
}

func (s *replicationSmoke) startReplicas(t *testing.T) {
	t.Helper()

	f := s.fixture
	for range 3 {
		a := assembleFixture(f.a.Topology.Config, f.a.Topology.Client, f.a.Topology.APIReader)
		process, stop := context.WithCancel(s.ctx)
		a.authority.BindProcess(process)
		replicationSmokeLifecycle(a.Lifecycle, process)
		a.Replication.observe(process)
		listener, err := (&net.ListenConfig{}).Listen(process, "tcp", "127.0.0.1:0")
		require.NoError(t, err)

		r := &replicationSmokeReplica{a: a, ctx: process, cancel: stop, endpoint: "https://" + listener.Addr().String(), listener: &replicationSmokeListener{Listener: listener}, done: make(chan error, 1)}
		s.replicas = append(s.replicas, r)
		config := a.Server.tlsConfig(process, f.serverCertificate)

		go func() { r.done <- a.Server.serve(process, r.listener, config) }()

		t.Cleanup(func() {
			stop()

			select {
			case err := <-r.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(12 * time.Second):
				t.Error("replica shutdown exceeded bound")
			}
		})
	}
}

func (s *replicationSmoke) authorizeReplicas(t *testing.T) {
	t.Helper()
	// Test-local leader selection and TokenReview, but the production TLS route,
	// controller Pod/SA checks, bounded decoder and durable install are exercised.
	f, leader := s.fixture, s.replicas[0]
	leader.a.Replication.leader = leader.ctx
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: f.a.Topology.Config.Namespace, Name: f.a.Topology.Config.ControllerServiceAccount, UID: "smoke-controller-sa"}}
	require.NoError(t, f.a.Topology.Create(s.ctx, sa))

	tokens := map[string]*corev1.Pod{}

	for i := 1; i < len(s.replicas); i++ {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sa.Namespace, Name: fmt.Sprintf("controller-%d", i), UID: types.UID(fmt.Sprintf("controller-uid-%d", i))}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name}}
		require.NoError(t, f.a.Topology.Create(s.ctx, pod))
		tokens[f.token+strconv.Itoa(i)] = pod
	}

	leader.a.Replication.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return fmt.Errorf("unexpected API create %T", obj)
		}

		s.reviews.Add(1)

		pod := tokens[review.Spec.Token]
		if pod == nil || !slices.Equal(review.Spec.Audiences, []string{ReplicationAudience}) {
			return nil
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{ReplicationAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:" + sa.Namespace + ":" + sa.Name, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})
	fixtureDependencies[leader.a.authority].Client = leader.a.Replication.Client
}

func (s *replicationSmoke) install(t *testing.T, publication *CommittedPublication) {
	t.Helper()

	image, err := wire.DecodePublication(strings.NewReader(publication.encoded))
	require.NoError(t, err)

	leader := s.replicas[0]
	require.NoError(t, leader.a.Replication.installReplica(s.ctx, leader.ctx, image))
}

func (s *replicationSmoke) replicate(t *testing.T, publication *CommittedPublication) {
	t.Helper()

	start := time.Now()

	for i, r := range s.replicas {
		if i == 0 || r.ctx.Err() != nil {
			continue
		}

		request, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.replicas[0].endpoint+ReplicationPath, nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+s.fixture.token+strconv.Itoa(i))
		response, err := s.internal.Do(request)
		require.NoError(t, err)

		image, decodeErr := wire.DecodePublication(response.Body)
		closeErr := response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, decodeErr)
		require.NoError(t, closeErr)
		require.NoError(t, r.a.Replication.installReplica(s.ctx, r.ctx, image))
		_, err = r.a.authority.Current()
		require.NoError(t, err)
		require.Equal(t, publication.encoded, capturePublication(t, r.a.authority).encoded, "replica diverged")
	}

	t.Logf("replication sequence=%d elapsed=%s mock_token_reviews=%d", publication.record.Sequence, time.Since(start), s.reviews.Load())
}

func (s *replicationSmoke) observe() {
	// Keep production freshness bounds; observations use fake Kubernetes reads.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, r := range s.replicas {
				if r.ctx.Err() == nil {
					r.a.Replication.observe(r.ctx)
				}
			}
		}
	}
}

func (s *replicationSmoke) run(after wire.Sequence, cold bool) <-chan replicationSmokeResult {
	results := make(chan replicationSmokeResult, len(s.peers))
	slots := make(chan struct{}, 24)

	for i := range s.peers {
		s.workers.Go(func() {
			if cold {
				select {
				case slots <- struct{}{}:
				case <-s.ctx.Done():
					results <- replicationSmokeResult{index: i, err: s.ctx.Err()}
					return
				}

				defer func() { <-slots }()
			}

			start := time.Now()
			result := s.peers[i].poll(s.ctx, s.replicas, i, after, &s.overloaded, &s.reconnected)

			result.elapsed = time.Since(start)
			results <- result
		})
	}

	return results
}

func (s *replicationSmoke) collect(t *testing.T, phase string, started time.Time, results <-chan replicationSmokeResult, want *CommittedPublication) {
	t.Helper()

	count := len(s.peers)
	digest := sha256.Sum256([]byte(want.encoded))
	latencies := make([]time.Duration, 0, count)

	var total int64

	for range s.peers {
		select {
		case result := <-results:
			if result.err != nil || result.digest != digest || result.bytes != int64(len(want.encoded)) {
				t.Fatalf("%s client=%d bytes=%d err=%v digest_match=%v", phase, result.index, result.bytes, result.err, result.digest == digest)
			}

			total += result.bytes
			latencies = append(latencies, result.elapsed)
		case <-s.ctx.Done():
			t.Fatal(s.ctx.Err())
		}
	}

	slices.Sort(latencies)
	t.Logf("phase=%s clients=%d all_delivered=%s request_p50=%s request_p99=%s bytes=%d cumulative_429=%d reconnects=%d", phase, count, time.Since(started), latencies[len(latencies)/2], latencies[(len(latencies)-1)*99/100], total, s.overloaded.Load(), s.reconnected.Load())
	replicationSmokeStats(t, phase, s.replicas)
}

func replicationSmokeLifecycle(l *Lifecycle, ctx context.Context) { l.process, l.synced = ctx, true }

func (p *replicationSmokePeer) poll(ctx context.Context, replicas []*replicationSmokeReplica, index int, after wire.Sequence, overloaded, reconnected *atomic.Int64) replicationSmokeResult {
	result := replicationSmokeResult{index: index}

	for attempt := range 6 {
		r := replicas[p.replica]

		status, reconnect, err := p.readSnapshot(ctx, r, after, &result)
		if reconnect {
			p.replica = index % 2

			reconnected.Add(1)

			continue
		}

		if err != nil {
			result.err = err
			return result
		}

		if status != http.StatusTooManyRequests {
			return result
		}

		overloaded.Add(1)

		if !replicationSleep(ctx, time.Second+time.Duration((index*31+attempt*97)%900)*time.Millisecond) {
			break
		}
	}

	result.err = fmt.Errorf("six-attempt request budget exhausted")

	return result
}

func (p *replicationSmokePeer) readSnapshot(ctx context.Context, replica *replicationSmokeReplica, after wire.Sequence, result *replicationSmokeResult) (int, bool, error) {
	path := replica.endpoint + wire.SnapshotPath
	if after != 0 {
		path += fmt.Sprintf("?after=%d", after)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, false, err
	}

	response, err := p.client.Do(request)
	if err != nil {
		return 0, replica.ctx.Err() != nil && ctx.Err() == nil, err
	}

	hash := sha256.New()
	result.bytes, err = io.Copy(hash, io.LimitReader(response.Body, wire.MaxPublicationBytes+1))

	closeErr := response.Body.Close()
	if replica.ctx.Err() != nil && ctx.Err() == nil && (err != nil || response.StatusCode == http.StatusServiceUnavailable) {
		return response.StatusCode, true, nil
	}

	if err != nil || closeErr != nil {
		return response.StatusCode, false, fmt.Errorf("body read=%v close=%v", err, closeErr)
	}

	if response.StatusCode == http.StatusTooManyRequests {
		return response.StatusCode, false, nil
	}

	if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		return response.StatusCode, false, fmt.Errorf("HTTP status %d or missing TLS 1.3", response.StatusCode)
	}

	copy(result.digest[:], hash.Sum(nil))

	return response.StatusCode, false, nil
}

func replicationSmokePublish(t *testing.T, ctx context.Context, r *TopologyReconciler, accepted AcceptedMembers) *CommittedPublication {
	t.Helper()

	_, err := r.authority.PublishTopology(ctx, func(context.Context) (TopologyObservation, error) {
		nodes := corev1.NodeList{}

		for id, member := range accepted {
			encoded, err := json.Marshal(member)
			if err != nil {
				return TopologyObservation{}, err
			}

			nodes.Items = append(nodes.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: string(id), UID: types.UID(id), Annotations: map[string]string{admittedMemberAnnotation: string(encoded), wire.SharesAnnotation: strconv.FormatUint(uint64(member.Shares), 10)}}})
		}

		return TopologyObservation{Nodes: nodes, Input: members.Input{Nodes: nodes.Items, PeerPort: r.Config.PeerPort}}, nil
	})
	require.NoError(t, err)

	return capturePublication(t, r.authority)
}

func replicationSmokePark(t *testing.T, ctx context.Context, replicas []*replicationSmokeReplica, count int, results <-chan replicationSmokeResult) {
	t.Helper()

	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for {
		select {
		case result := <-results:
			t.Fatalf("client %d completed before publication: %v", result.index, result.err)
		default:
		}

		total := 0
		for _, r := range replicas {
			total += r.a.Server.polls.count()
		}

		if total == count {
			return
		}

		if !replicationSleep(deadline, 10*time.Millisecond) {
			t.Fatalf("parked %d/%d real HTTPS requests before deadline", total, count)
		}
	}
}

func replicationSmokeStats(t *testing.T, phase string, replicas []*replicationSmokeReplica) {
	t.Helper()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var polls, live, accepted []int64
	for _, r := range replicas {
		polls = append(polls, int64(r.a.Server.polls.count()))
		live = append(live, r.listener.live.Load())
		accepted = append(accepted, r.listener.accepted.Load())
	}

	status, _ := os.ReadFile("/proc/self/status")

	var resource []string

	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "Threads:") {
			resource = append(resource, strings.TrimSpace(line))
		}
	}

	fds, _ := os.ReadDir("/proc/self/fd")
	t.Logf("resources phase=%s polls=%v live_tcp=%v accepted_tcp=%v heap=%d stack=%d total_alloc=%d goroutines=%d fd=%d process=%v", phase, polls, live, accepted, mem.HeapAlloc, mem.StackInuse, mem.TotalAlloc, runtime.NumGoroutine(), len(fds), resource)
}
