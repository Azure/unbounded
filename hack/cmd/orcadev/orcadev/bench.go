// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
)

// benchOpts holds the bench flag set. Either --duration or
// --requests must be set; they are mutually exclusive.
type benchOpts struct {
	key            string
	concurrency    int
	durationStr    string
	requests       int
	rangeSizeStr   string
	full           bool
	readPattern    string
	output         string
	jsonOut        string
	label          string
	histLower      time.Duration
	histUpper      time.Duration
	histBuckets    int
	warmupRequests int
	drainTimeout   time.Duration
}

func newBenchCmd(g *globalFlags) *cobra.Command {
	o := &benchOpts{
		concurrency:    8,
		durationStr:    "30s",
		rangeSizeStr:   "1MiB",
		readPattern:    "sequential",
		output:         "text",
		histLower:      100 * time.Microsecond,
		histUpper:      10 * time.Second,
		histBuckets:    50,
		warmupRequests: 1,
		drainTimeout:   10 * time.Second,
	}

	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Parallel GET throughput and latency benchmark",
		Long: `Bench drives N concurrent workers issuing GET (or ranged GET)
requests against orca's edge for the named object. Reports total
requests, errors, bytes read, throughput, and latency percentiles
(min/p50/p90/p99/max).

Two stop conditions, mutually exclusive:

  --duration 30s     run for the wall-clock duration. When the
                     deadline fires, no new requests are issued
                     but in-flight requests are allowed to drain
                     for up to --drain-timeout (default 10s)
                     before being cancelled.
  --requests 1000    run until N requests have completed (no
                     drain phase).

Two read shapes:

  --full              fetch the whole object each iteration
  --range-size 1MiB   fetch a 1 MiB byte range; --read-pattern
                      controls placement (sequential | random)

Output:

  --output text       (default) human-friendly summary on stdout
  --output json       JSON on stdout (schema: schema_version=1)
  --json-out PATH     write JSON to PATH (in addition to --output)

The JSON includes a log-spaced latency histogram with bounds
configurable via --hist-lower / --hist-upper / --hist-buckets;
defaults are 100us..10s in 50 buckets, suitable for orca's
expected steady-state latency distribution.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBench(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.key, "key", "", "origin object to fetch (required)")
	cmd.Flags().IntVar(&o.concurrency, "concurrency", o.concurrency, "parallel workers")
	cmd.Flags().StringVar(&o.durationStr, "duration", o.durationStr, "wall-clock duration (mutually exclusive with --requests)")
	cmd.Flags().IntVar(&o.requests, "requests", 0, "stop after N requests (mutually exclusive with --duration)")
	cmd.Flags().StringVar(&o.rangeSizeStr, "range-size", o.rangeSizeStr, "per-request range size")
	cmd.Flags().BoolVar(&o.full, "full", o.full, "fetch the full object each request (ignores --range-size)")
	cmd.Flags().StringVar(&o.readPattern, "read-pattern", o.readPattern, "range placement: sequential or random")
	cmd.Flags().StringVar(&o.output, "output", o.output, "stdout format: text or json")
	cmd.Flags().StringVar(&o.jsonOut, "json-out", "", "write JSON result to this path (in addition to --output)")
	cmd.Flags().StringVar(&o.label, "label", "", "user-supplied tag baked into JSON for cross-run comparison")
	cmd.Flags().DurationVar(&o.histLower, "hist-lower", o.histLower, "latency histogram lower bound")
	cmd.Flags().DurationVar(&o.histUpper, "hist-upper", o.histUpper, "latency histogram upper bound")
	cmd.Flags().IntVar(&o.histBuckets, "hist-buckets", o.histBuckets, "latency histogram bucket count")
	cmd.Flags().IntVar(&o.warmupRequests, "warmup-requests", o.warmupRequests, "single-worker requests issued before timing starts")
	cmd.Flags().DurationVar(&o.drainTimeout, "drain-timeout", o.drainTimeout,
		"how long to let in-flight requests finish after --duration expires before cancelling them")

	return cmd
}

// benchResult is the JSON shape emitted by --output json /
// --json-out. schema_version is bumped only on breaking changes
// (additive changes preserve the value).
type benchResult struct {
	SchemaVersion int                `json:"schema_version"`
	Tool          string             `json:"tool"`
	Subcommand    string             `json:"subcommand"`
	Label         string             `json:"label,omitempty"`
	StartedAt     string             `json:"started_at"`
	FinishedAt    string             `json:"finished_at"`
	Config        benchResultConfig  `json:"config"`
	Results       benchResultPayload `json:"results"`
	ErrorsByCode  map[string]int     `json:"errors_by_code,omitempty"`
	LatencyHist   histogram          `json:"latency_histogram"`
}

type benchResultConfig struct {
	OrcaURL             string  `json:"orca_url"`
	Bucket              string  `json:"bucket"`
	Key                 string  `json:"key"`
	ObjectSizeBytes     int64   `json:"object_size_bytes"`
	ETag                string  `json:"etag,omitempty"`
	Concurrency         int     `json:"concurrency"`
	DurationSeconds     float64 `json:"duration_seconds"`
	DrainTimeoutSeconds float64 `json:"drain_timeout_seconds"`
	RequestCount        *int    `json:"request_count_target"`
	RangeSizeBytes      int64   `json:"range_size_bytes"`
	Full                bool    `json:"full"`
	ReadPattern         string  `json:"read_pattern"`
	WarmupRequests      int     `json:"warmup_requests"`
}

type benchResultPayload struct {
	Requests       int64   `json:"requests"`
	Errors         int64   `json:"errors"`
	BytesRead      int64   `json:"bytes_read"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	// GateSeconds is wall-clock time the gate was open (i.e. the
	// window during which new requests could be issued). For
	// --duration runs this equals the configured duration; for
	// --requests N runs this is the time until the Nth request
	// finished.
	GateSeconds float64 `json:"gate_seconds"`
	// DrainSeconds is wall-clock time spent after the gate closed
	// waiting for in-flight requests to finish. Zero for
	// --requests N runs. Bounded above by DrainTimeoutSeconds; if
	// the drain budget is exhausted, remaining in-flight requests
	// are cancelled and counted as errors.
	DrainSeconds      float64             `json:"drain_seconds"`
	ThroughputBytes   float64             `json:"throughput_bytes_per_second"`
	RequestsPerSecond float64             `json:"requests_per_second"`
	LatencyNs         benchLatencySummary `json:"latency_ns"`
}

type benchLatencySummary struct {
	Min int64 `json:"min"`
	P50 int64 `json:"p50"`
	P90 int64 `json:"p90"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

func runBench(ctx context.Context, g *globalFlags, o *benchOpts) error {
	if o.key == "" {
		return fmt.Errorf("--key is required")
	}

	if o.durationStr != "" && o.requests > 0 {
		return fmt.Errorf("--duration and --requests are mutually exclusive")
	}

	if o.concurrency < 1 {
		return fmt.Errorf("--concurrency must be >= 1")
	}

	if o.readPattern != "sequential" && o.readPattern != "random" {
		return fmt.Errorf("--read-pattern must be 'sequential' or 'random'")
	}

	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("--output must be 'text' or 'json'")
	}

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	// Spin up an auto-managed kubectl port-forward to svc/orca if
	// --orca-url is the dev default and the port is unreachable.
	// Cleanup runs on subcommand return; if the operator already
	// has their own port-forward this is a no-op.
	cleanup, err := ensureEdgeReachable(ctx, g)
	if err != nil {
		return err
	}

	defer cleanup()

	edge := newEdgeClient(g.orcaURL, g.timeout)

	// Resolve object metadata up front; we need size to plan ranges.
	info, err := oc.Head(ctx, o.key)
	if err != nil {
		return fmt.Errorf("origin head: %w", err)
	}

	rangeSize := info.Size
	if !o.full {
		rs, err := parseSize(o.rangeSizeStr)
		if err != nil {
			return fmt.Errorf("--range-size: %w", err)
		}

		if rs > info.Size {
			rs = info.Size
		}

		rangeSize = rs
	}

	var (
		duration time.Duration
		reqLimit int
	)

	if o.durationStr != "" {
		d, err := time.ParseDuration(o.durationStr)
		if err != nil {
			return fmt.Errorf("--duration: %w", err)
		}

		duration = d
	}

	if o.requests > 0 {
		reqLimit = o.requests
	}

	if duration == 0 && reqLimit == 0 {
		duration = 30 * time.Second
	}

	fmt.Fprintf(os.Stderr, "bench: key=%s size=%s range=%s concurrency=%d pattern=%s\n",
		o.key, formatSize(info.Size), formatSize(rangeSize), o.concurrency, o.readPattern)

	// Warmup: prime the metadata cache so the first timed request
	// doesn't pay HEAD latency on top of GET latency.
	for i := 0; i < o.warmupRequests; i++ {
		resp, err := edge.Head(ctx, oc.Bucket(), o.key)
		if err != nil {
			return fmt.Errorf("warmup: %w", err)
		}
		_ = resp //nolint:wsl // warmup output is intentionally discarded
	}

	startedAt := time.Now()

	results, gateClosedAt := runBenchLoop(ctx, edge, oc.Bucket(), o.key, info.Size, rangeSize, o, duration, reqLimit)

	finishedAt := time.Now()
	elapsed := finishedAt.Sub(startedAt)
	gateDuration := gateClosedAt.Sub(startedAt)
	if gateDuration < 0 {
		gateDuration = 0
	}

	drainDuration := finishedAt.Sub(gateClosedAt)
	if drainDuration < 0 {
		drainDuration = 0
	}

	stats := computeLatencyStats(results.latencies)
	hist := buildHistogram(results.latencies, o.histLower, o.histUpper, o.histBuckets)

	br := benchResult{
		SchemaVersion: 1,
		Tool:          "orcadev",
		Subcommand:    "bench",
		Label:         o.label,
		StartedAt:     startedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:    finishedAt.UTC().Format(time.RFC3339Nano),
		Config: benchResultConfig{
			OrcaURL:             g.orcaURL,
			Bucket:              oc.Bucket(),
			Key:                 o.key,
			ObjectSizeBytes:     info.Size,
			ETag:                info.ETag,
			Concurrency:         o.concurrency,
			DurationSeconds:     duration.Seconds(),
			DrainTimeoutSeconds: o.drainTimeout.Seconds(),
			RangeSizeBytes:      rangeSize,
			Full:                o.full,
			ReadPattern:         o.readPattern,
			WarmupRequests:      o.warmupRequests,
		},
		Results: benchResultPayload{
			Requests:          results.requests,
			Errors:            results.errors,
			BytesRead:         results.bytes,
			ElapsedSeconds:    elapsed.Seconds(),
			GateSeconds:       gateDuration.Seconds(),
			DrainSeconds:      drainDuration.Seconds(),
			ThroughputBytes:   ratePerSec(results.bytes, elapsed),
			RequestsPerSecond: ratePerSec(results.requests, elapsed),
			LatencyNs: benchLatencySummary{
				Min: int64(stats.Min),
				P50: int64(stats.P50),
				P90: int64(stats.P90),
				P99: int64(stats.P99),
				Max: int64(stats.Max),
			},
		},
		ErrorsByCode: results.errorsByCode,
		LatencyHist:  hist,
	}

	if reqLimit > 0 {
		n := reqLimit
		br.Config.RequestCount = &n
		br.Config.DurationSeconds = 0
	}

	if err := emitBenchResult(br, o); err != nil {
		return err
	}

	if results.errors > 0 {
		return fmt.Errorf("%d errors during benchmark", results.errors)
	}

	return nil
}

// benchAcc accumulates per-request results in a thread-safe manner.
type benchAcc struct {
	requests     int64
	errors       int64
	bytes        int64
	latencies    []time.Duration
	errorsByCode map[string]int
	mu           sync.Mutex
}

func (a *benchAcc) record(elapsed time.Duration, n int64, err error, code string) {
	atomic.AddInt64(&a.requests, 1)
	atomic.AddInt64(&a.bytes, n)

	if err != nil {
		atomic.AddInt64(&a.errors, 1)
	}

	a.mu.Lock()
	a.latencies = append(a.latencies, elapsed)
	if code != "" {
		if a.errorsByCode == nil {
			a.errorsByCode = make(map[string]int)
		}

		a.errorsByCode[code]++
	}
	a.mu.Unlock()
}

// runBenchLoop launches o.concurrency workers and runs until either
// the deadline elapses or reqLimit requests have completed. The
// shared benchAcc carries per-request results out; the returned
// gateClosedAt is the wall-clock time the gate stopped admitting
// new work (zero in --requests mode where the gate closes only
// after the Nth request completes; non-zero in --duration mode
// where the deadline fired).
//
// In --duration mode the two contexts are split: gateCtx bounds
// new-work admission to `duration`, while reqCtx bounds the
// underlying HTTP calls to `duration + drainTimeout`. After the
// gate closes, in-flight requests are given drainTimeout to
// finish; any still pending past that get reqCtx-cancelled and
// counted as errors.
func runBenchLoop(
	ctx context.Context,
	edge *edgeClient,
	bucket, key string,
	objectSize, rangeSize int64,
	o *benchOpts,
	duration time.Duration,
	reqLimit int,
) (*benchAcc, time.Time) {
	acc := &benchAcc{latencies: make([]time.Duration, 0, 1024)}

	// gateCtx controls "may a worker start another request?". In
	// --duration mode it expires at `duration`; in --requests mode
	// it inherits ctx and is never deadline-cancelled (the issued
	// counter does the gating).
	gateCtx := ctx

	var gateCancel context.CancelFunc

	if duration > 0 {
		gateCtx, gateCancel = context.WithTimeout(ctx, duration)
		defer gateCancel()
	}

	// reqCtx is what the HTTP calls actually use. It is deliberately
	// NOT bound by `duration`: after the gate closes, in-flight
	// requests continue against reqCtx until they finish or until
	// the drain timer (below) cancels them. Ctrl-C on the parent
	// ctx still propagates to reqCtx for prompt teardown.
	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()

	// gateClosedAt is set the instant the gate stops admitting
	// new work. We capture it in two places: (a) when the gate's
	// deadline fires (via context.AfterFunc), (b) when all workers
	// exit voluntarily in --requests mode (set below in the
	// post-Wait fallback). gateClosedCh provides a one-shot signal
	// so the drain timer doesn't double-fire.
	var (
		gateClosedAt     time.Time
		gateClosedMu     sync.Mutex
		gateClosedOnce   sync.Once
		drainCancelTimer *time.Timer
	)

	setGateClosed := func() {
		gateClosedOnce.Do(func() {
			gateClosedMu.Lock()
			gateClosedAt = time.Now()
			gateClosedMu.Unlock()
			// Arm the drain cap: after o.drainTimeout, force-cancel
			// any still-in-flight requests via reqCtx.
			if o.drainTimeout > 0 {
				drainCancelTimer = time.AfterFunc(o.drainTimeout, reqCancel)
			}
		})
	}

	// In --duration mode, wire the gate's expiration to setGateClosed.
	// In --requests mode, the workers themselves call it after the
	// reqLimit is reached.
	if duration > 0 {
		context.AfterFunc(gateCtx, setGateClosed)
	}

	var (
		nextOffset atomic.Int64 // for sequential pattern
		issued     atomic.Int64 // for --requests gate
	)

	var wg sync.WaitGroup

	worker := func(workerID int) {
		defer wg.Done()

		rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano() + int64(workerID))) //nolint:gosec // benchmark RNG

		for {
			select {
			case <-gateCtx.Done():
				return
			default:
			}

			if reqLimit > 0 {
				next := issued.Add(1)
				if next > int64(reqLimit) {
					// Last worker through closes the gate so the
					// --requests run reports gate_seconds equal to
					// elapsed (no separate drain phase).
					setGateClosed()
					return
				}
			}

			// Pick a range.
			var (
				start, end int64
			)

			if o.full || rangeSize >= objectSize {
				start, end = 0, objectSize-1
			} else if o.readPattern == "random" {
				maxStart := objectSize - rangeSize
				if maxStart <= 0 {
					start = 0
				} else {
					start = rng.Int63n(maxStart + 1)
				}

				end = start + rangeSize - 1
			} else {
				// sequential: each worker takes the next stride
				start = nextOffset.Add(rangeSize) - rangeSize
				start %= objectSize

				end = start + rangeSize - 1
				if end >= objectSize {
					end = objectSize - 1
				}
			}

			reqStart := time.Now()

			var (
				resp edgeResponse
				err  error
				code string
				n    int64
			)

			if o.full || (start == 0 && end == objectSize-1) {
				resp, err = edge.Get(reqCtx, bucket, key)
			} else {
				resp, err = edge.GetRange(reqCtx, bucket, key, start, end)
			}

			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					code = "context_canceled"
				} else {
					code = "transport_error"
				}

				acc.record(time.Since(reqStart), 0, err, code)

				continue
			}

			n, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close() //nolint:errcheck // benchmark body close best-effort

			elapsed := time.Since(reqStart)

			if err != nil {
				acc.record(elapsed, n, err, "body_read_error")
				continue
			}

			if resp.Status != 200 && resp.Status != 206 {
				acc.record(elapsed, n, fmt.Errorf("status %d", resp.Status), fmt.Sprintf("http_%d", resp.Status))
				continue
			}

			acc.record(elapsed, n, nil, "")
		}
	}

	for i := 0; i < o.concurrency; i++ {
		wg.Add(1)
		go worker(i)
	}

	wg.Wait()

	// All workers exited. Stop the drain-cancel timer if it's
	// still pending so we don't fire reqCancel after Wait
	// returned (harmless either way; tidier this way).
	if drainCancelTimer != nil {
		drainCancelTimer.Stop()
	}

	// In --requests mode, if no worker exited via the reqLimit
	// path (e.g. parent ctx was cancelled before the limit was
	// reached), gateClosedAt may still be zero. Treat the Wait
	// return as the gate close so the caller can still compute a
	// meaningful gate_seconds.
	gateClosedMu.Lock()
	if gateClosedAt.IsZero() {
		gateClosedAt = time.Now()
	}

	out := gateClosedAt
	gateClosedMu.Unlock()

	return acc, out
}

// emitBenchResult writes the human + JSON outputs per the --output
// and --json-out flags.
func emitBenchResult(br benchResult, o *benchOpts) error {
	switch o.output {
	case "text":
		writeBenchHuman(os.Stdout, br)
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(br); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
	}

	if o.jsonOut != "" {
		f, err := os.Create(o.jsonOut)
		if err != nil {
			return fmt.Errorf("create --json-out: %w", err)
		}

		defer f.Close() //nolint:errcheck

		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")

		if err := enc.Encode(br); err != nil {
			return fmt.Errorf("write --json-out: %w", err)
		}
	}

	return nil
}

func writeBenchHuman(w io.Writer, br benchResult) {
	fprintln(w, "bench results:")
	fprintf(w, "  label:       %s\n", labelOrEmpty(br.Label))
	fprintf(w, "  key:         %s\n", br.Config.Key)
	fprintf(w, "  size:        %s\n", formatSize(br.Config.ObjectSizeBytes))
	fprintf(w, "  range:       %s\n", rangeDescription(br.Config))
	fprintf(w, "  concurrency: %d\n", br.Config.Concurrency)
	fprintf(w, "  elapsed:     %s\n", time.Duration(br.Results.ElapsedSeconds*float64(time.Second)).Round(time.Millisecond))
	fprintf(w, "  gate open:   %s\n", time.Duration(br.Results.GateSeconds*float64(time.Second)).Round(time.Millisecond))
	fprintf(w, "  drain:       %s\n", time.Duration(br.Results.DrainSeconds*float64(time.Second)).Round(time.Millisecond))
	fprintln(w)
	fprintf(w, "  requests:    %d\n", br.Results.Requests)
	fprintf(w, "  errors:      %d\n", br.Results.Errors)
	fprintf(w, "  bytes read:  %s\n", formatSize(br.Results.BytesRead))
	fprintf(w, "  throughput:  %s\n",
		formatRate(br.Results.BytesRead, time.Duration(br.Results.ElapsedSeconds*float64(time.Second))))
	fprintf(w, "  req rate:    %.1f req/s\n", br.Results.RequestsPerSecond)
	fprintln(w)
	fprintln(w, "  latency:")
	fprintf(w, "    min:    %s\n", time.Duration(br.Results.LatencyNs.Min).Round(time.Microsecond))
	fprintf(w, "    p50:    %s\n", time.Duration(br.Results.LatencyNs.P50).Round(time.Microsecond))
	fprintf(w, "    p90:    %s\n", time.Duration(br.Results.LatencyNs.P90).Round(time.Microsecond))
	fprintf(w, "    p99:    %s\n", time.Duration(br.Results.LatencyNs.P99).Round(time.Microsecond))
	fprintf(w, "    max:    %s\n", time.Duration(br.Results.LatencyNs.Max).Round(time.Microsecond))

	if len(br.ErrorsByCode) > 0 {
		fprintln(w)
		fprintln(w, "  errors by code:")

		for code, n := range br.ErrorsByCode {
			fprintf(w, "    %-20s %d\n", code, n)
		}
	}
}

func rangeDescription(c benchResultConfig) string {
	if c.Full {
		return "full object"
	}

	return fmt.Sprintf("%s (%s pattern)", formatSize(c.RangeSizeBytes), c.ReadPattern)
}

func labelOrEmpty(s string) string {
	if s == "" {
		return "(none)"
	}

	return s
}

func ratePerSec(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}

	return float64(n) / d.Seconds()
}
