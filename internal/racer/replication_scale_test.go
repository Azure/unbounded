// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
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

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

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

func replicatedServingSmoke(t *testing.T, count int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	f := newServingFixture(t)
	members := make(AcceptedMembers, count)
	peers := make([]replicationSmokePeer, count)
	handshakes := make(chan struct{}, 24)
	setup := time.Now()
	_, _, rotation, material := keyState(t, f.a.Keyring)

	ca, signingKey, err := parseSigning(material.Keys[rotation.ActiveIssuer])
	if err != nil {
		t.Fatal(err)
	}

	for i := range peers {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		members[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: fmt.Sprintf("10.%d.%d.%d:8082", i>>16, (i>>8)&255, i&255), RDMANICs: []wire.RDMANIC{}}

		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		template := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "spiffe", Host: string(f.request.Cluster), Path: "/node/" + string(id)}}}

		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, signingKey)
		if err != nil {
			t.Fatal(err)
		}

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
		peers[i] = replicationSmokePeer{client: &http.Client{Transport: transport, Timeout: 45 * time.Second}, replica: i % 3}
	}

	base := replicationSmokePublish(t, ctx, f.a.Topology, members)

	var replicas []*replicationSmokeReplica

	for range 3 {
		a := Assemble(f.a.Server.Config, f.a.Topology.Client, f.a.Topology.APIReader)
		process, stop := context.WithCancel(ctx)
		a.Server.Publications.bindProcess(process)
		replicationSmokeLifecycle(a.Lifecycle, process)
		a.Replication.observe(process)

		listener, err := (&net.ListenConfig{}).Listen(process, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		r := &replicationSmokeReplica{a: a, ctx: process, cancel: stop, endpoint: "https://" + listener.Addr().String(), listener: &replicationSmokeListener{Listener: listener}, done: make(chan error, 1)}
		replicas = append(replicas, r)

		config := a.Server.tlsConfig(process, f.serverCertificate)

		go func() {
			r.done <- a.Server.serve(process, r.listener, config)
		}()

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

	// Test-local leader selection and TokenReview, but the production TLS route,
	// controller Pod/SA checks, bounded decoder and durable install are exercised.
	leader := replicas[0]
	leader.a.Replication.leader = leader.ctx

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: f.a.Server.Config.Namespace, Name: f.a.Server.Config.ControllerServiceAccount, UID: "smoke-controller-sa"}}
	if err := f.a.Topology.Create(ctx, sa); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]*corev1.Pod{}

	for i := 1; i < len(replicas); i++ {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sa.Namespace, Name: fmt.Sprintf("controller-%d", i), UID: types.UID(fmt.Sprintf("controller-uid-%d", i))}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name}}
		if err := f.a.Topology.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}

		tokens[f.token+strconv.Itoa(i)] = pod
	}

	var reviews atomic.Int64

	leader.a.Replication.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return fmt.Errorf("unexpected API create %T", obj)
		}

		reviews.Add(1)

		pod := tokens[review.Spec.Token]
		if pod == nil || !slices.Equal(review.Spec.Audiences, []string{ReplicationAudience}) {
			return nil
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{ReplicationAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:" + sa.Namespace + ":" + sa.Name, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})

	// Initial publisher installation uses the same canonical/durable validation.
	image, err := wire.DecodePublication(strings.NewReader(base.encoded))
	if err != nil {
		t.Fatal(err)
	}

	if err := leader.a.Replication.installReplica(ctx, leader.ctx, image); err != nil {
		t.Fatal(err)
	}

	internal := f.client(t, nil)
	internal.Timeout = 30 * time.Second
	replicate := func(publication *CommittedPublication) {
		t.Helper()

		start := time.Now()

		for i, r := range replicas {
			if i == 0 || r.ctx.Err() != nil {
				continue
			}

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+replicationPath, nil)
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Authorization", "Bearer "+f.token+strconv.Itoa(i))

			response, err := internal.Do(request)
			if err != nil {
				t.Fatal(err)
			}

			image, decodeErr := wire.DecodePublication(response.Body)

			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil || closeErr != nil {
				t.Fatalf("replication status=%d decode=%v close=%v", response.StatusCode, decodeErr, closeErr)
			}

			if err := r.a.Replication.installReplica(ctx, r.ctx, image); err != nil {
				t.Fatal(err)
			}

			current, err := r.a.Server.Publications.Current()
			if err != nil || current.encoded != publication.encoded {
				t.Fatal("replica diverged", err)
			}
		}

		t.Logf("replication sequence=%d elapsed=%s mock_token_reviews=%d", publication.record.Sequence, time.Since(start), reviews.Load())
	}
	replicate(base)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+wire.SnapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := internal.Do(request)
	responseBody(t, response, err, http.StatusUnauthorized)

	request, err = http.NewRequestWithContext(ctx, http.MethodGet, leader.endpoint+replicationPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err = internal.Do(request)
	responseBody(t, response, err, http.StatusUnauthorized)

	// Keep production freshness bounds; periodic authority observations are fake
	// Kubernetes reads, never fake dataplane polls or extended freshness windows.
	observed := make(chan struct{})

	go func() {
		defer close(observed)

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, r := range replicas {
					if r.ctx.Err() == nil {
						r.a.Replication.observe(r.ctx)
					}
				}
			}
		}
	}()

	defer func() { cancel(); <-observed }()

	t.Logf("clients=%d replicas=3 GOMAXPROCS=%d setup=%s full_bytes=%d max_writes=%d max_auth=%d", count, runtime.GOMAXPROCS(0), time.Since(setup), len(base.encoded), leader.a.Server.Config.Limits.MaxConcurrentWrites, leader.a.Server.Config.Limits.MaxConcurrentBootstrap)
	replicationSmokeStats(t, "baseline", replicas)

	var (
		overloaded, reconnected atomic.Int64
		workers                 sync.WaitGroup
	)

	defer func() { cancel(); workers.Wait() }()

	run := func(after wire.Sequence, cold bool) <-chan replicationSmokeResult {
		results := make(chan replicationSmokeResult, count)
		slots := make(chan struct{}, 24)

		for i := range peers {
			workers.Go(func() {
				if cold {
					select {
					case slots <- struct{}{}:
					case <-ctx.Done():
						results <- replicationSmokeResult{index: i, err: ctx.Err()}
						return
					}

					defer func() { <-slots }()
				}

				start := time.Now()
				result := replicationSmokeResult{index: i}

				defer func() { result.elapsed = time.Since(start); results <- result }()

				for attempt := range 6 {
					peer := &peers[i]
					r := replicas[peer.replica]

					path := r.endpoint + wire.SnapshotPath
					if after != 0 {
						path += fmt.Sprintf("?after=%d", after)
					}

					request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
					if err != nil {
						result.err = err
						return
					}

					response, err := peer.client.Do(request)
					if err != nil {
						if r.ctx.Err() != nil && ctx.Err() == nil {
							peer.replica = i % 2

							reconnected.Add(1)

							continue
						}

						result.err = err

						return
					}

					hash := sha256.New()
					result.bytes, err = io.Copy(hash, io.LimitReader(response.Body, wire.MaxPublicationBytes+1))

					closeErr := response.Body.Close()
					if r.ctx.Err() != nil && ctx.Err() == nil && (err != nil || response.StatusCode == http.StatusServiceUnavailable) {
						peer.replica = i % 2

						reconnected.Add(1)

						continue
					}

					if err != nil || closeErr != nil {
						result.err = fmt.Errorf("body read=%v close=%v", err, closeErr)
						return
					}

					if response.StatusCode == http.StatusTooManyRequests {
						overloaded.Add(1)

						if !replicationSleep(ctx, time.Second+time.Duration((i*31+attempt*97)%900)*time.Millisecond) {
							break
						}

						continue
					}

					if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
						result.err = fmt.Errorf("HTTP status %d or missing TLS 1.3", response.StatusCode)
						return
					}

					copy(result.digest[:], hash.Sum(nil))

					return
				}

				result.err = fmt.Errorf("six-attempt request budget exhausted")
			})
		}

		return results
	}

	collect := func(phase string, started time.Time, results <-chan replicationSmokeResult, want *CommittedPublication) {
		t.Helper()

		digest := sha256.Sum256([]byte(want.encoded))
		latencies := make([]time.Duration, 0, count)

		var total int64

		for range peers {
			select {
			case result := <-results:
				if result.err != nil || result.digest != digest || result.bytes != int64(len(want.encoded)) {
					t.Fatalf("%s client=%d bytes=%d err=%v digest_match=%v", phase, result.index, result.bytes, result.err, result.digest == digest)
				}

				total += result.bytes
				latencies = append(latencies, result.elapsed)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}

		slices.Sort(latencies)
		t.Logf("phase=%s clients=%d all_delivered=%s request_p50=%s request_p99=%s bytes=%d cumulative_429=%d reconnects=%d", phase, count, time.Since(started), latencies[len(latencies)/2], latencies[(len(latencies)-1)*99/100], total, overloaded.Load(), reconnected.Load())
		replicationSmokeStats(t, phase, replicas)
	}
	start := time.Now()
	collect("cold", start, run(0, true), base)

	for phase := range 2 {
		start = time.Now()
		results := run(base.record.Sequence, false)
		replicationSmokePark(t, ctx, replicas, count, results)
		replicationSmokeStats(t, "parked", replicas)

		if phase == 1 {
			replicas[2].cancel()
			replicationSmokePark(t, ctx, replicas[:2], count, results)
			t.Logf("failure_repark=%s", time.Since(start))
			replicationSmokeStats(t, "failure-parked", replicas)
		}

		for id, member := range members {
			member.Shares++
			members[id] = member
		}

		start = time.Now()
		base = replicationSmokePublish(t, ctx, f.a.Topology, members)

		image, err := wire.DecodePublication(strings.NewReader(base.encoded))
		if err != nil {
			t.Fatal(err)
		}

		if err := leader.a.Replication.installReplica(ctx, leader.ctx, image); err != nil {
			t.Fatal(err)
		}

		replicate(base)
		collect([]string{"update", "replica-failure-update"}[phase], start, results, base)
	}

	if reconnected.Load() != int64(count/3) {
		t.Fatalf("reconnected=%d want=%d", reconnected.Load(), count/3)
	}
}

func replicationSmokeLifecycle(l *Lifecycle, ctx context.Context) {
	l.process, l.synced = ctx, true
}

func replicationSmokePublish(t *testing.T, ctx context.Context, r *TopologyReconciler, members AcceptedMembers) *CommittedPublication {
	t.Helper()

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, members, nil)
	if err != nil {
		t.Fatal(err)
	}

	publication, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Publications.Install(publication); err != nil {
		t.Fatal(err)
	}

	return publication
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
