// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Count durable API operations separately from credential reviews. These tests
// use real handler/history/serialization code but fake, zero-latency Kubernetes storage.
type heartbeatAPI struct {
	client.Client
	gets, creates, updates atomic.Int64
}

func (c *heartbeatAPI) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets.Add(1)
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *heartbeatAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates.Add(1)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *heartbeatAPI) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates.Add(1)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *heartbeatAPI) reset() {
	c.gets.Store(0)
	c.creates.Store(0)
	c.updates.Store(0)
}

type heartbeatScaleFixture struct {
	s        *Server
	api      *heartbeatAPI
	roll     *rollout
	requests []*http.Request
	history  string
}

// Match the observed 1,500 slots/recipients and 62+62+57+47 old wildcards.
// Verified TLS identities isolate serialized handler work from handshake
// latency. Rollout time remains real.
func newHeartbeatScaleFixture(tb testing.TB, history bool, phase uint32) *heartbeatScaleFixture {
	tb.Helper()

	return newHeartbeatGeometryFixture(tb, history, phase, 1500)
}

func newHeartbeatGeometryFixture(tb testing.TB, history bool, phase uint32, slots uint32) *heartbeatScaleFixture {
	tb.Helper()

	ctx := context.Background()

	const participants = 1500

	nodes := make([]corev1.Node, participants)

	pods := make([]corev1.Pod, participants)
	for i := range participants {
		n, p, _ := fixtures()
		n.Name = fmt.Sprintf("node-%04d", i)
		n.UID = types.UID(n.Name)
		p.Name, p.Spec.NodeName = n.Name, n.Name
		p.UID = types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		p.Status.PodIP = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)
		nodes[i], pods[i] = *n, *p
	}

	_, _, svc := fixtures()

	g, _, err := buildCacheFixture("default", nil, nodes, pods, svc)
	if err != nil {
		tb.Fatal(err)
	}

	g.Revision = 12
	// Keep the original small history corpus; production geometry is exercised
	// separately without manufacturing oversized historical ledgers.
	g.Volume.Slots = slots
	g.Owners = g.Owners[:slots]
	api := &heartbeatAPI{Client: fakeKube()}

	store := stateStore{client: api, namespace: "state"}
	if err := store.commit(ctx, g, nil); err != nil {
		tb.Fatal(err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		tb.Fatal(err)
	}

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		tb.Fatal(err)
	}

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		tb.Fatal(err)
	}

	if phase != 1 {
		if err := s.persistPhase(ctx, "default", r, phase); err != nil {
			tb.Fatal(err)
		}
	}

	if history {
		var ds []forwardDecision

		for group, count := range []int{62, 62, 57, 47} {
			for i := range count {
				m := g.Nodes[nodes[i].Name]
				snap := index.snapshot(m.ID)
				snap.Revision = []uint64{3, 4, 6, 7}[group]

				snap.Epoch = snap.Revision
				for _, v := range snap.Volumes {
					v.Topology.Epoch = snap.Revision
				}

				data, err := marshalSnapshot(snap)
				if err != nil {
					tb.Fatal(err)
				}

				ds = append(ds, forwardDecision{Snapshot: data, PodUID: m.PodUID})
			}
		}

		if err := s.saveForwards(ctx, r, ds); err != nil {
			tb.Fatal(err)
		}
	}

	f := &heartbeatScaleFixture{s: s, api: api, roll: r, history: r.pointer.Data["forwards"]}

	for i := range participants {
		m := g.Nodes[nodes[i].Name]

		entry, err := s.current(recipient{identityBytes("universe", "default"), identityBytes("node", string(nodes[i].UID))})
		if err != nil {
			tb.Fatal(err)
		}

		digest := sha256.Sum256(entry.snapshot)
		req := httptest.NewRequest("GET", "/", nil)
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", m.ID)
		controlTLS(req, m.PodUID)
		req.Header.Set("X-Racer-Boot", fmt.Sprintf("%064x", i+1))
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Digest", hex.EncodeToString(digest[:]))
		req.Header.Set("X-Racer-Phase", fmt.Sprint(phase))
		f.requests = append(f.requests, req)
	}

	if slots == participants && len(s.source.cache) != participants {
		tb.Fatalf("fixture does not have warm snapshots: %d", len(s.source.cache))
	}

	api.reset()

	return f
}

// A recipient that already has the configuration must not reconstruct it after
// payload eviction. Use the actual fixed slot geometry and all 1,500 recipients.
func TestHeartbeatProductionGeometry(t *testing.T) {
	f := newHeartbeatGeometryFixture(t, false, 1, defaultSlots)

	var builds atomic.Int64

	f.s.buildSnapshot = func(index *topologyIndex, key recipient) (*entry, error) {
		builds.Add(1)
		return buildSnapshotEntry(index, key)
	}

	cache := make(map[recipient]*entry, len(f.s.source.cache))
	for key, el := range f.s.source.cache {
		cache[key] = el.Value.(cachedEntry).entry
	}

	if len(cache) == 0 || len(cache) >= len(f.requests) || f.s.source.bytes > snapshotCacheBytes {
		t.Fatalf("fixture did not exercise bounded payload eviction: entries=%d bytes=%d", len(cache), f.s.source.bytes)
	}

	t.Logf("production geometry: slots=%d recipients=%d retained payloads=%d cache bytes=%d", defaultSlots, len(f.requests), len(cache), f.s.source.bytes)
	// Barrier correctness is independent of the measured wall-clock sweep.
	for _, req := range f.requests {
		f.roll.acks[req.PathValue("node")] = rolloutAck{boot: req.Header.Get("X-Racer-Boot"), phase: 1, seen: time.Now()}
	}

	delete(f.roll.acks, f.requests[0].PathValue("node"))

	if c := f.call(t, 0); c.Phase != 2 || f.api.gets.Load() != 1 || f.api.updates.Load() != 1 {
		t.Fatal("config-free acknowledgment failed to persist prepare barrier")
	}

	f.api.reset()

	start := make(chan struct{})
	durations := make([]time.Duration, len(f.requests))

	var wg sync.WaitGroup
	for i := range f.requests {
		wg.Go(func() {
			<-start

			now := time.Now()
			c := f.call(t, i)

			durations[i] = time.Since(now)
			if c.Configuration != nil || hex.EncodeToString(c.SnapshotDigest) != f.requests[i].Header.Get("X-Racer-Digest") || c.Revision != 12 {
				t.Errorf("recipient %d received inconsistent config-free command", i)
			}
		})
	}

	close(start)
	wg.Wait()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	late := 0

	for _, d := range durations {
		if d > 2*time.Second {
			late++
		}
	}

	t.Logf("production heartbeat wave: p50=%s p99=%s max=%s beyond-2s=%d phase=%d", durations[750], durations[1484], durations[1499], late, f.roll.phase)
	// Structural regression assertion, independent of host speed: config-free
	// traffic must neither rebuild nor displace serialized payloads.
	for key, want := range cache {
		el := f.s.source.cache[key]
		if el == nil || el.Value.(cachedEntry).entry != want {
			t.Fatal("config-free heartbeats rebuilt/displaced cached payloads")
		}
	}

	if builds.Load() != 0 || len(f.roll.acks) != len(f.requests) {
		t.Fatalf("config-free wave: builds=%d acknowledgments=%d", builds.Load(), len(f.roll.acks))
	}

	if f.api.gets.Load()+f.api.creates.Load()+f.api.updates.Load() != 0 {
		t.Fatal("steady config-free wave touched durable state")
	}
	// A real resend after eviction must still carry the exact deterministic
	// snapshot, while subsequent acknowledgments return to the digest-only path.
	req := f.requests[0]
	req.Header.Set("X-Racer-Needs-Config", "1")

	c := f.call(t, 0)
	raw := configurationSnapshot(t, c.Configuration)

	digest := sha256.Sum256(raw)
	if builds.Load() != 1 || hex.EncodeToString(digest[:]) != req.Header.Get("X-Racer-Digest") {
		t.Fatal("evicted configuration resend did not reproduce the exact snapshot")
	}

	t.Logf("resend: snapshot=%d bytes envelope=%d bytes remote-slot-neighbors=%d", len(raw), proto.Size(c.Configuration), len(c.Configuration.GetSnapshot().Volumes[0].Topology.Neighbors))
	req.Header.Del("X-Racer-Needs-Config")

	if c := f.call(t, 0); c.Configuration != nil || builds.Load() != 1 {
		t.Fatal("resend did not return to config-free heartbeats")
	}
}

func (f *heartbeatScaleFixture) call(tb testing.TB, i int) *pb.ControlCommand {
	tb.Helper()

	w := httptest.NewRecorder()
	f.s.control(w, f.requests[i])

	var command pb.ControlCommand

	if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &command) != nil {
		tb.Fatalf("heartbeat %d: HTTP=%d body=%q", i, w.Code, w.Body.String())
	}

	return &command
}

func TestHeartbeatScalePrepareFreshness(t *testing.T) {
	f := newHeartbeatScaleFixture(t, true, 1)
	start := time.Now()

	for i := range f.requests {
		f.call(t, i)
	}

	t.Logf("real 1500-recipient prepare sweep: %s phase=%d, ledger=%d bytes, snapshot cache=%d bytes", time.Since(start), f.roll.phase, len(f.history), f.s.source.bytes)
	// Runtime is measured, never a pass/fail threshold. Freshness assertions use
	// explicitly stale/fresh reports, so a slow CI machine does not fail the test.
	if f.roll.phase != 1 && f.roll.phase != 2 {
		t.Fatalf("unexpected phase %d", f.roll.phase)
	}

	if f.roll.pointer.Data["forwards"] != f.history {
		t.Fatal("current acknowledgments collected unknown-boot history")
	}

	// Use a fresh phase-1 fixture for a deterministic missing/stale/fresh barrier.
	f = newHeartbeatScaleFixture(t, true, 1)
	refresh := func() {
		for _, req := range f.requests {
			f.roll.acks[req.PathValue("node")] = rolloutAck{boot: req.Header.Get("X-Racer-Boot"), phase: 1, seen: time.Now()}
		}
	}
	refresh()

	staleNode := f.requests[0].PathValue("node")
	ack := f.roll.acks[staleNode]
	ack.seen = time.Now().Add(-time.Hour)

	f.roll.acks[staleNode] = ack
	if c := f.call(t, 1); c.Phase != 1 || f.api.updates.Load() != 0 {
		t.Fatal("stale recipient bypassed prepare barrier")
	}

	delete(f.roll.acks, staleNode)

	if c := f.call(t, 1); c.Phase != 1 {
		t.Fatal("missing recipient bypassed prepare barrier")
	}

	refresh()
	delete(f.roll.acks, staleNode)

	if c := f.call(t, 0); c.Phase != 2 || f.api.gets.Load() != 1 || f.api.updates.Load() != 1 || f.api.creates.Load() != 0 {
		t.Fatalf("fresh prepared recipients did not persist exactly one barrier: phase=%d gets=%d updates=%d", c.Phase, f.api.gets.Load(), f.api.updates.Load())
	}

	if f.roll.pointer.Data["forwards"] != f.history {
		t.Fatal("barrier changed unresolved history")
	}
}

// A synchronized wave exposes queue latency without imposing machine-dependent
// performance assertions. Requests remain valid, even if the measured delay is
// longer than a real Rust client's two-second first-byte deadline.
func TestHeartbeatScaleConcurrentWave(t *testing.T) {
	f := newHeartbeatScaleFixture(t, true, 1)
	start := make(chan struct{})
	durations := make([]time.Duration, len(f.requests))

	var wg sync.WaitGroup
	for i := range f.requests {
		wg.Go(func() {
			<-start

			now := time.Now()

			f.call(t, i)
			durations[i] = time.Since(now)
		})
	}

	close(start)
	wg.Wait()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	late := 0

	for _, d := range durations {
		if d > 2*time.Second {
			late++
		}
	}

	t.Logf("1500-recipient wave: p50=%s p99=%s max=%s beyond-first-byte-deadline=%d phase=%d gets=%d updates=%d", durations[750], durations[1484], durations[1499], late, f.roll.phase, f.api.gets.Load(), f.api.updates.Load())

	if f.roll.pointer.Data["forwards"] != f.history {
		t.Fatal("concurrent acknowledgments changed unresolved history")
	}
}

// Three finite polls per recipient through real HTTP, with the Rust first-byte
// budget approximated by a two-second whole-request deadline for tiny commands.
// This isolates CP response liveness: no worker execution, TokenReview latency,
// reconciliation, API stalls, or Rust retry jitter is simulated. No throughput
// threshold is asserted. Every transport is closed and all handlers are drained.
// Opt in separately from race suites: instrumented canceled handlers can take
// longer to drain than this investigation's external 90-second command budget.
func TestHeartbeatScaleHTTPPolling(t *testing.T) {
	if os.Getenv("RACER_TEST_HTTP_POLLING") != "1" {
		t.Skip("set RACER_TEST_HTTP_POLLING=1 for the bounded HTTP load reproduction")
	}

	f := newHeartbeatScaleFixture(t, true, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		for _, template := range f.requests {
			if template.PathValue("node") == req.PathValue("node") {
				req.TLS = template.TLS
				break
			}
		}

		f.s.control(w, req)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	httpClient := &http.Client{Transport: transport}

	var ok, deadline, unavailable, unexpected atomic.Int64

	start := make(chan struct{})

	var wg sync.WaitGroup
	for _, template := range f.requests {
		wg.Go(func() {
			<-start

			for range 3 {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

				req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/v3/"+template.PathValue("universe")+"/"+template.PathValue("node"), nil)
				if err != nil {
					cancel()
					t.Error(err)

					return
				}

				req.Header = template.Header.Clone()

				response, err := httpClient.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}

				switch {
				case errors.Is(err, context.DeadlineExceeded):
					deadline.Add(1)
				case err != nil:
					unexpected.Add(1)
					t.Errorf("unexpected transport error: %v", err)
				case response.StatusCode == http.StatusOK:
					ok.Add(1)
				case response.StatusCode == http.StatusServiceUnavailable:
					unavailable.Add(1)
				default:
					unexpected.Add(1)
					t.Errorf("unexpected status %d", response.StatusCode)
				}

				cancel()
				time.Sleep(250 * time.Millisecond)
			}
		})
	}

	begin := time.Now()

	close(start)
	wg.Wait()

	clientsDone := time.Since(begin)

	server.Close()
	t.Logf("4500 HTTP polls: clients=%s drained=%s success=%d deadline=%d unavailable=%d unexpected=%d phase=%d gets=%d updates=%d", clientsDone, time.Since(begin), ok.Load(), deadline.Load(), unavailable.Load(), unexpected.Load(), f.roll.phase, f.api.gets.Load(), f.api.updates.Load())

	if f.roll.pointer.Data["forwards"] != f.history {
		t.Fatal("HTTP polling changed unresolved history")
	}
}

func BenchmarkHeartbeatScale(b *testing.B) {
	for _, history := range []bool{false, true} {
		b.Run(fmt.Sprintf("history=%t", history), func(b *testing.B) {
			f := newHeartbeatScaleFixture(b, history, 4)
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				f.s.control(w, f.requests[i%len(f.requests)])

				if w.Code != 200 {
					b.Fatal(w.Code, w.Body.String())
				}
			}

			b.StopTimer()
			b.ReportMetric(float64(f.api.gets.Load()+f.api.creates.Load()+f.api.updates.Load())/float64(b.N), "state-API/op")

			if f.api.gets.Load()+f.api.creates.Load()+f.api.updates.Load() != 0 || f.roll.pointer.Data["forwards"] != f.history {
				b.Fatal("steady-state heartbeat touched durable state")
			}
		})
	}
}

func BenchmarkHeartbeatForwardDecode(b *testing.B) {
	f := newHeartbeatScaleFixture(b, true, 4)
	b.ReportAllocs()
	b.SetBytes(int64(len(f.history)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if ds, err := forwardHistory(f.history, "default", 12); err != nil || len(ds) != 228 {
			b.Fatal(len(ds), err)
		}
	}
}

// Cancellation while waiting for the subscription lock must not refresh an
// acknowledgment or consume history/serialization work after the client disconnects.
func TestHeartbeatCanceledWaiter(t *testing.T) {
	f := newHeartbeatScaleFixture(t, true, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := f.requests[0].WithContext(ctx)
	started := make(chan struct{})

	f.s.mu.Lock()
	w := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		defer close(done)

		close(started)
		f.s.control(w, req)
	}()
	// The server lock remains held until after cancellation, so no rollout work
	// can occur while the request is still live.
	<-started
	cancel()
	f.s.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled heartbeat did not return")
	}

	_, acknowledged := f.roll.acks[req.PathValue("node")]
	t.Logf("canceled request: HTTP=%d response-bytes=%d acknowledgment-recorded=%t state-API=%d", w.Code, w.Body.Len(), acknowledged, f.api.gets.Load()+f.api.updates.Load()+f.api.creates.Load())

	if acknowledged || w.Code == http.StatusOK && strings.Contains(w.Header().Get("Content-Type"), "protobuf") {
		t.Fatal("canceled waiter recorded a fresh acknowledgment and/or produced a command")
	}

	if w.Body.Len() != 0 || f.api.gets.Load()+f.api.updates.Load()+f.api.creates.Load() != 0 || f.roll.pointer.Data["forwards"] != f.history {
		t.Fatal("canceled waiter performed response or durable work")
	}
}
