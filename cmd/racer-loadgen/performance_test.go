// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/gantry/config"
	gantrydigest "github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	performanceBulkLimit = 64
)

// TestRacerMixedPerformance is an opt-in measurement, not a throughput threshold.
// The runner compiles first and provides a private /run and a real Rust executable.
func TestRacerMixedPerformance(t *testing.T) {
	root := os.Getenv("RACER_PERF_ROOT")
	if root == "" {
		t.Skip("run with bash cmd/racer-loadgen/performance/run.sh")
	}

	concurrency, err := strconv.Atoi(os.Getenv("RACER_PERF_CONCURRENCY"))
	require.NoError(t, err)
	require.Greater(t, concurrency, 16)
	require.LessOrEqual(t, concurrency, 64)

	img, err := newImage(t.Context(), imageOptions{Layers: 1, LayerBytes: 3*int64(racersdk.PageSize) + 13, Seed: "mixed-v1", Repository: "perf/bulk"})
	require.NoError(t, err)
	resume, err := newImage(t.Context(), imageOptions{Layers: 1, LayerBytes: 3*int64(racersdk.PageSize) + 13, Seed: "resume-v1", Repository: "perf/resume"})
	require.NoError(t, err)

	var (
		upstreamBytes, upstreamRequests atomic.Int64
		rangeMu                         sync.Mutex
	)

	ranges := map[string]int{}
	bulkHandler, resumeHandler := img.handler(), resume.handler()
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		rangeMu.Lock()
		ranges[r.Method+" "+r.URL.Path+" "+r.Header.Get("Range")]++
		rangeMu.Unlock()

		counter := &performanceWriter{ResponseWriter: w, bytes: &upstreamBytes}
		if strings.HasPrefix(r.URL.Path, "/v2/perf/resume/") {
			resumeHandler.ServeHTTP(counter, r)
		} else {
			bulkHandler.ServeHTTP(counter, r)
		}
	}))
	t.Cleanup(upstreamServer.Close)
	cfg := &config.Config{RacerEnabled: true, UpstreamRegistries: []config.UpstreamRegistry{{Name: "loadgen.invalid", Endpoint: upstreamServer.URL}}}
	upstream, err := origin.New(cfg)
	require.NoError(t, err)

	volume := "gantry"
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	originDone := make(chan error, 1)
	originCallback := gantryracer.Origin(cfg, upstream)

	go func() {
		originDone <- racersdk.ServeOrigin(ctx, racersdk.OriginConfig{Volume: volume}, func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
			metadata, body, err := originCallback(ctx, request)
			if err != nil {
				t.Logf("origin callback failure key=%v head=%v error=%v", request.Key, request.Head, err)
			}

			return metadata, body, err
		})
	}()

	t.Cleanup(func() { cancel(); require.ErrorIs(t, <-originDone, context.Canceled) })
	require.Eventually(t, func() bool { _, e := os.Stat("/run/racer/gantry/origin/socket"); return e == nil }, 5*time.Second, 10*time.Millisecond)
	pid, diagnostics := performanceProcess(t, root)
	t.Cleanup(func() {
		if t.Failed() {
			performanceSave(t, root, "failure-rust.prom", performanceMetrics(t, diagnostics))
		}
	})

	client, err := racersdk.NewClient(racersdk.ClientConfig{Volume: volume})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	server := httptest.NewServer(gantryracer.WrapHTTP(mirror.New(cfg, nil, upstream, mirror.WithContentBackend(gantryracer.NewHandler(performanceObservedClient{Client: client, t: t}, upstream, nil))).Handler(), 60*time.Second, nil))
	t.Cleanup(server.Close)
	opts := pullTestOptions(server.URL)
	opts.Namespace, opts.Concurrency, opts.LayerConcurrency, opts.Timeout = "loadgen.invalid", concurrency, 1, 60*time.Second
	p, _ := pullTestNew(t, img, opts)
	report := map[string]any{"scope": "real Rust process + Go SDK + Gantry HTTP mirror/origin + synthetic loadgen registry; single node", "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "bulk_concurrency": concurrency, "layer_bytes": img.Layers[0].Size, "rust_pid": pid}
	report["start_spacing"] = os.Getenv("RACER_PERF_START_SPACING")
	report["manifest_bulk_control"] = os.Getenv("RACER_PERF_BULK_CONTROL") == "1"

	t.Logf("artifacts: %s", root)

	var phases []performancePhase

	for _, phase := range []struct {
		name   string
		rounds int
	}{{"cold", 1}, {"warm", 20}} {
		beforeBytes, beforeRequests := upstreamBytes.Load(), upstreamRequests.Load()
		result := performanceMixed(t, phase.name, p, pid, phase.rounds)
		result.OriginBytes, result.OriginRequests = upstreamBytes.Load()-beforeBytes, upstreamRequests.Load()-beforeRequests
		result.Amplification = float64(result.OriginBytes) / float64(result.UsefulBytes)
		phases = append(phases, result)
		phaseData, err := json.MarshalIndent(result, "", "  ")
		require.NoError(t, err)
		performanceSave(t, root, phase.name+"-results.json", phaseData)

		performanceSave(t, root, phase.name+"-rust.prom", performanceMetrics(t, diagnostics))
		t.Logf("%s: %.1f useful MiB/s; HEAD p95/p99 %.3f/%.3f ms; manifest %.3f/%.3f ms", phase.name, result.UsefulMiBPerSecond, result.Head.P95Millis, result.Head.P99Millis, result.Manifest.P95Millis, result.Manifest.P99Millis)
		require.Empty(t, result.Errors, "all bulk and metadata requests must succeed")
	}

	report["mixed"] = phases

	var resumes []map[string]any

	for _, name := range []string{"cold", "warm"} {
		beforeBytes, beforeRequests := upstreamBytes.Load(), upstreamRequests.Load()
		started := time.Now()
		n := performanceResume(t, p, resume)
		resumes = append(resumes, map[string]any{"phase": name, "useful_bytes": n, "seconds": time.Since(started).Seconds(), "origin_bytes": upstreamBytes.Load() - beforeBytes, "origin_requests": upstreamRequests.Load() - beforeRequests, "amplification": float64(upstreamBytes.Load()-beforeBytes) / float64(n)})
	}

	report["resume"] = resumes

	var pressure []map[string]any

	if os.Getenv("RACER_PERF_BULK_CONTROL") != "1" {
		for range 3 {
			pressure = append(pressure, performancePressure(t, client, img, p, pid))
		}
	}

	report["pressure"] = pressure

	require.Eventually(t, func() bool {
		metrics := string(performanceMetrics(t, diagnostics))
		return strings.Contains(metrics, "\nracer_active_requests 0\n") && strings.Contains(metrics, "\nracer_active_fills 0\n") && strings.Contains(metrics, "\nracer_pending_disk_writes 0\n")
	}, 5*time.Second, 20*time.Millisecond, "Rust requests, fills, and disk writes must drain")
	performanceSave(t, root, "final-rust.prom", performanceMetrics(t, diagnostics))
	rangeMu.Lock()
	report["upstream_requests_by_range"] = ranges
	data, err := json.MarshalIndent(report, "", "  ")
	rangeMu.Unlock()
	require.NoError(t, err)
	performanceSave(t, root, "results.json", data)
	t.Logf("measurements: %s/results.json", root)
	t.Logf("%s", data)
}

type performanceWriter struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w *performanceWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(int64(n))

	return n, err
}

type performancePhase struct {
	Name                                            string
	Seconds, UsefulMiBPerSecond                     float64
	UsefulBytes, OriginBytes, OriginRequests        int64
	Amplification                                   float64
	Head, Manifest                                  performanceLatency
	PeakHeapBytes, PeakGoRSSBytes, PeakRustRSSBytes uint64
	PeakGoroutines                                  int
	Errors                                          []string
}

type performanceLatency struct {
	Samples              int
	P95Millis, P99Millis float64
}

func performanceMixed(t *testing.T, name string, p *puller, pid, rounds int) performancePhase {
	t.Helper()

	result := performancePhase{Name: name}

	var (
		active         atomic.Int64
		useful         atomic.Int64
		bulk, metadata sync.WaitGroup
	)

	start, done := make(chan struct{}), make(chan struct{})
	errors := make(chan error, p.opts.Concurrency*rounds+2)
	latencies := [2][]time.Duration{}

	for kind := range 2 {
		metadata.Go(func() {
			<-start

			for {
				select {
				case <-done:
					return
				default:
				}

				began := time.Now()
				underBulk := active.Load() > 16

				var err error
				if kind == 0 {
					err = performanceHead(t.Context(), p)
				} else {
					err = p.fetch(t.Context(), "manifest", p.img.Manifest)
				}

				if err != nil {
					errors <- err
					return
				}
				// Sample admission under load, retaining slow probes that finish
				// after the bulk tail so queue delay is never censored.
				if underBulk {
					latencies[kind] = append(latencies[kind], time.Since(began))
				}

				time.Sleep(2 * time.Millisecond)
			}
		})
	}

	sampled := make(chan struct{})

	go func() {
		defer close(sampled)

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)

				result.PeakHeapBytes = max(result.PeakHeapBytes, m.HeapAlloc)
				result.PeakGoRSSBytes = max(result.PeakGoRSSBytes, performanceRSS(os.Getpid()))
				result.PeakRustRSSBytes = max(result.PeakRustRSSBytes, performanceRSS(pid))
				result.PeakGoroutines = max(result.PeakGoroutines, runtime.NumGoroutine())
			}
		}
	}()

	imageBytes := p.img.Manifest.Size + p.img.Config.Size + p.img.Layers[0].Size
	spacing, err := time.ParseDuration(os.Getenv("RACER_PERF_START_SPACING"))
	require.NoError(t, err)

	for worker := range p.opts.Concurrency {
		bulk.Go(func() {
			<-start
			time.Sleep(time.Duration(worker) * spacing)

			active.Add(1)
			defer active.Add(-1)

			for range rounds {
				if err := p.pull(t.Context()); err != nil {
					errors <- err
					return
				}

				useful.Add(imageBytes)
			}
		})
	}

	began := time.Now()

	close(start)
	bulk.Wait()

	seconds := time.Since(began).Seconds()

	close(done)
	metadata.Wait()
	<-sampled
	close(errors)

	for err := range errors {
		result.Errors = append(result.Errors, err.Error())
	}

	result.Seconds, result.UsefulBytes = seconds, useful.Load()
	result.UsefulMiBPerSecond = float64(result.UsefulBytes) / (1024 * 1024) / seconds
	result.Head, result.Manifest = performancePercentiles(latencies[0]), performancePercentiles(latencies[1])

	if len(result.Errors) == 0 {
		require.Positive(t, result.Head.Samples)
		require.Positive(t, result.Manifest.Samples)
		require.Equal(t, int64(p.opts.Concurrency*rounds)*imageBytes, result.UsefulBytes, "all requested image bytes must be verified")
	}

	return result
}

func performancePercentiles(values []time.Duration) performanceLatency {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })

	if len(values) == 0 {
		return performanceLatency{}
	}

	at := func(q float64) float64 {
		return float64(values[int(math.Ceil(q*float64(len(values))))-1]) / float64(time.Millisecond)
	}

	return performanceLatency{Samples: len(values), P95Millis: at(.95), P99Millis: at(.99)}
}

func performanceHead(ctx context.Context, p *puller) error {
	u := p.opts.Target + "/v2/" + p.img.repository + "/blobs/" + p.img.Layers[0].Digest.String() + "?ns=" + p.opts.Namespace

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || resp.ContentLength != p.img.Layers[0].Size || resp.Header.Get("Gantry-Mirrored") != "1" {
		return fmt.Errorf("unexpected HEAD: %s length=%d", resp.Status, resp.ContentLength)
	}

	return nil
}

func performanceResume(t *testing.T, p *puller, img *syntheticImage) int64 {
	t.Helper()

	layer := img.Layers[0]
	offset := int64(racersdk.PageSize) + 7
	u := p.opts.Target + "/v2/" + img.repository + "/blobs/" + layer.Digest.String() + "?ns=" + p.opts.Namespace
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	require.NoError(t, err)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	resp, err := p.client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Gantry-Mirrored"))
	require.Equal(t, fmt.Sprintf("bytes %d-%d/%d", offset, layer.Size-1, layer.Size), resp.Header.Get("Content-Range"))

	hash := sha256.New()
	_, err = io.Copy(hash, io.NewSectionReader(img.blobs[layer.Digest].data, 0, offset))
	require.NoError(t, err)
	n, err := io.Copy(hash, resp.Body)
	require.NoError(t, err)
	require.Equal(t, layer.Size-offset, n)
	require.Equal(t, layer.Digest.String(), fmt.Sprintf("sha256:%x", hash.Sum(nil)))

	return n
}

func performancePressure(t *testing.T, client *racersdk.Client, img *syntheticImage, p *puller, pid int) map[string]any {
	t.Helper()

	digest, err := gantrydigest.Parse(img.Layers[0].Digest.String())
	require.NoError(t, err)
	request, err := gantryracer.Request(ifaces.OriginRef{Registry: "loadgen.invalid", Repository: img.repository, Digest: digest, Kind: ifaces.KindBlob}, "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var values []*racersdk.Object

	for range performanceBulkLimit {
		value, err := client.Get(ctx, request)
		require.NoError(t, err)

		values = append(values, value)
	}

	var (
		waiters  sync.WaitGroup
		failures atomic.Int64
	)

	for range 32 {
		waiters.Go(func() {
			value, err := client.Get(ctx, request)
			if err != nil {
				failures.Add(1)
			} else {
				_ = value.Close()
			}
		})
	}

	// Hold saturated admission long enough for bounded-resource observations.
	time.Sleep(250 * time.Millisecond)

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	pressureRustRSS := performanceRSS(pid)

	started := time.Now()
	_, err = client.Stat(t.Context(), request)
	require.NoError(t, err, "metadata must progress while bulk reads are held")

	headMillis := float64(time.Since(started)) / float64(time.Millisecond)
	started = time.Now()

	require.NoError(t, p.fetch(t.Context(), "manifest", img.Manifest), "verified manifest must progress while bulk admission is saturated")

	manifestMillis := float64(time.Since(started)) / float64(time.Millisecond)

	manifestDigest, err := gantrydigest.Parse(img.Manifest.Digest.String())
	require.NoError(t, err)
	manifestRequest, err := gantryracer.Request(ifaces.OriginRef{Registry: "loadgen.invalid", Repository: img.repository, Digest: manifestDigest, Kind: ifaces.KindManifest}, "")
	require.NoError(t, err)

	value, err := client.Get(ctx, manifestRequest, racersdk.ReadOptions{SmallObject: true})
	require.NoError(t, err, "small objects must progress while bulk reads are held")
	_, err = io.Copy(io.Discard, value)
	require.NoError(t, err)
	require.NoError(t, value.Close())

	cancel()

	for _, value := range values {
		_ = value.Close()
	}

	waiters.Wait()
	require.Equal(t, int64(32), failures.Load())

	recovery := time.Now()

	require.NoError(t, p.pull(t.Context()))

	return map[string]any{"manifest_at_bulk_saturation_ms": manifestMillis, "heap_bytes_at_pressure": m.HeapAlloc, "go_rss_after_recovery": performanceRSS(os.Getpid()), "rust_rss_at_pressure": pressureRustRSS, "rust_rss_after_recovery": performanceRSS(pid), "head_at_saturation_ms": headMillis, "verified_recovery_pull_ms": float64(time.Since(recovery)) / float64(time.Millisecond), "failed_waiters": failures.Load()}
}

// Observe only failures at the production SDK boundary. This preserves the body
// and admission paths while distinguishing local admission from Rust HTTP errors.
type performanceObservedClient struct {
	*racersdk.Client
	t *testing.T
}

func (c performanceObservedClient) Get(ctx context.Context, request racersdk.Request, options ...racersdk.ReadOptions) (*racersdk.Object, error) {
	// Explicit A/B control: keep the current SDK and identical paced workload,
	// but route mirror-selected small objects through the bulk pool.
	if os.Getenv("RACER_PERF_BULK_CONTROL") == "1" && len(options) == 1 && options[0].SmallObject {
		option := options[0]
		option.SmallObject = false
		options = []racersdk.ReadOptions{option}
	}

	value, err := c.Client.Get(ctx, request, options...)
	c.failure(request, err)

	return value, err
}

func (c performanceObservedClient) Stat(ctx context.Context, request racersdk.Request) (racersdk.Metadata, error) {
	metadata, err := c.Client.Stat(ctx, request)
	c.failure(request, err)

	return metadata, err
}

func (c performanceObservedClient) failure(request racersdk.Request, err error) {
	if err == nil {
		return
	}

	c.t.Logf("SDK failure key=%v error=%v", request.Key, err)
}

func performanceRSS(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}

	pages, _ := strconv.ParseUint(fields[1], 10, 64)

	return pages * uint64(os.Getpagesize())
}

func performanceSave(t *testing.T, root, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), data, 0o600))
}

func performanceMetrics(t *testing.T, address string) []byte {
	t.Helper()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + address + "/metrics")
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return data
}

func performanceProcess(t *testing.T, root string) (int, string) {
	t.Helper()

	control := exec.Command(os.Getenv("RACER_PERF_CONTROL"), root)
	stdin, err := control.StdinPipe()
	require.NoError(t, err)
	stdout, err := control.StdoutPipe()
	require.NoError(t, err)

	control.Stderr = os.Stderr
	require.NoError(t, control.Start())
	t.Cleanup(func() { _ = stdin.Close(); require.NoError(t, control.Wait()) })

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan())
	endpoint := scanner.Text()
	port := func() string {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, err)

		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		return address
	}
	diagnostics := port()
	cmd := exec.Command(os.Getenv("RACER_PERF_BIN"))

	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "RACER_") {
			cmd.Env = append(cmd.Env, env)
		}
	}

	settings := map[string]string{
		"CLUSTER_ID": "11111111-1111-4111-8111-111111111111", "CONTROL_ENDPOINT": endpoint,
		"PEER_LISTEN": port(), "DIAGNOSTICS_LISTEN": diagnostics, "MAX_THREADS": "2", "ENABLE_RDMA": "false",
		"PLAINTEXT_BYTES": "268435456", "CIPHERTEXT_BYTES": "536870912", "DIRTY_BYTES": "134217728", "REGISTERED_BYTES": "1",
		"REQUEST_CONTEXT_BYTES": "16777216", "SLAB_BYTES": "1073741824", "SEGMENT_BYTES": "67108864", "FREE_SEGMENT_RESERVE": "2",
		"QUEUE_ENTRIES": "256", "CLIENT_CONNECTIONS": "128", "ORIGIN_CONNECTIONS_PER_CACHE": "8", "METADATA_ENTRIES": "128", "FLIGHTS": "64", "PIPES": "72", "RANGE_WINDOW_PAGES": "2",
		"REQUEST_TIMEOUT_MS": "60000", "READER_STALL_TIMEOUT_MS": "30000", "SHUTDOWN_TIMEOUT_MS": "10000",
	}
	// Use production resource defaults unless a run explicitly records an override.
	// In particular, do not hide pipe contention by budgeting one per SDK slot.
	for name, fallback := range map[string]string{
		"PIPES": "16", "CIPHERTEXT_BYTES": "268435456",
		"REQUEST_TIMEOUT_MS": "30000", "READER_STALL_TIMEOUT_MS": "10000",
		"MAX_THREADS": "2", "CLIENT_CONNECTIONS": "128",
	} {
		settings[name] = fallback
		if value := os.Getenv("RACER_PERF_" + name); value != "" {
			settings[name] = value
		}
	}

	for name, path := range map[string]string{"TRUST_BUNDLE": "trust.pem", "SERVICE_ACCOUNT_TOKEN": "token", "SECRET_DIRECTORY": "secrets", "IDENTITY_DIRECTORY": "identity", "SLAB_DIRECTORY": "slabs"} {
		settings[name] = filepath.Join(root, path)
	}

	for name, value := range settings {
		cmd.Env = append(cmd.Env, "RACER_"+name+"="+value)
	}

	configData, err := json.MarshalIndent(settings, "", "  ")
	require.NoError(t, err)
	performanceSave(t, root, "dataplane-config.json", configData)
	log, err := os.Create(filepath.Join(root, "dataplane.log"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, log.Close()) })

	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)

		done := make(chan error, 1)

		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()

			<-done
			t.Error("dataplane shutdown timeout")
		}
	})

	httpClient := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get("http://" + diagnostics + "/readyz")
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 20*time.Millisecond, "dataplane readiness; see %s/dataplane.log", root)

	return cmd.Process.Pid, diagnostics
}
