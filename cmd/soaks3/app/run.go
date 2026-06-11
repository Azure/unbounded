// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// runOptions holds the parsed flags for the run subcommand.
type runOptions struct {
	endpoint    string
	bucket      string
	keyPrefix   string
	manifest    string
	count       int64
	objectSize  string
	concurrency int
	duration    time.Duration
	rate        float64
	rangeRead   bool
	rangeSize   string
	metricsAddr string
	reportEvery time.Duration
	timeout     time.Duration
	zipf        zipfConfig
}

// newRunCommand builds the "run" subcommand.
func newRunCommand() *cobra.Command {
	opts := &runOptions{
		endpoint:    "http://127.0.0.1:9000",
		keyPrefix:   "soaks3/",
		objectSize:  "4MiB",
		concurrency: 32,
		metricsAddr: ":9300",
		reportEvery: 5 * time.Second,
		timeout:     30 * time.Second,
		zipf:        defaultZipfConfig(),
	}

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive read load against an unbounded-storage S3 frontend",
		Long: "run issues GET requests for keys selected by a Zipf (or uniform)\n" +
			"distribution so that hot-key and cache-churn behavior emerges from the\n" +
			"distribution tunables.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLoad(cmd.Context(), opts)
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&opts.endpoint, "endpoint", opts.endpoint, "Base URL of the S3 frontend")
	fs.StringVar(&opts.bucket, "bucket", "", "Bucket name for path-style requests")
	fs.StringVar(&opts.keyPrefix, "key-prefix", opts.keyPrefix, "Key prefix for objects")
	fs.StringVar(&opts.manifest, "manifest", "", "Path to a seed manifest.json to auto-configure count, object-size and key-prefix")
	fs.Int64Var(&opts.count, "count", 0, "Number of objects in the data set (ignored when --manifest is set)")
	fs.StringVar(&opts.objectSize, "object-size", opts.objectSize, "Object size, used for range-read math (ignored when --manifest is set)")
	fs.IntVar(&opts.concurrency, "concurrency", opts.concurrency, "Number of concurrent workers")
	fs.DurationVar(&opts.duration, "duration", 0, "How long to run (0 = until interrupted)")
	fs.Float64Var(&opts.rate, "rate", 0, "Target aggregate request rate in req/s (0 = unthrottled)")
	fs.BoolVar(&opts.rangeRead, "range-read", false, "Issue ranged GETs instead of full-object GETs")
	fs.StringVar(&opts.rangeSize, "range-size", "64KiB", "Range length for ranged GETs")
	fs.StringVar(&opts.metricsAddr, "metrics-addr", opts.metricsAddr, "Address for the Prometheus /metrics endpoint (empty to disable)")
	fs.DurationVar(&opts.reportEvery, "report-interval", opts.reportEvery, "Interval between stdout progress reports (0 to disable)")
	fs.DurationVar(&opts.timeout, "request-timeout", opts.timeout, "Per-request timeout")
	registerZipfFlags(fs, &opts.zipf)

	return cmd
}

// runLoad executes the run subcommand.
func runLoad(ctx context.Context, opts *runOptions) error {
	count, objectSize, keyPrefix, err := resolveRunConfig(opts)
	if err != nil {
		return err
	}

	if opts.concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1, got %d", opts.concurrency)
	}

	if opts.rate < 0 {
		return fmt.Errorf("--rate must not be negative, got %g", opts.rate)
	}

	var rangeSize int64
	if opts.rangeRead {
		rangeSize, err = parseSize(opts.rangeSize)
		if err != nil {
			return fmt.Errorf("--range-size: %w", err)
		}

		if rangeSize <= 0 {
			return fmt.Errorf("--range-size must be positive, got %d", rangeSize)
		}

		if rangeSize > objectSize {
			rangeSize = objectSize
		}
	}

	km, err := newKeyModel(keyPrefix, count)
	if err != nil {
		return err
	}

	if err := opts.zipf.validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if opts.duration > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.duration)
		defer cancel()
	}

	m := newMetrics()
	srv := m.serve(opts.metricsAddr)

	if srv != nil {
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			_ = srv.Shutdown(shutCtx) //nolint:errcheck // best-effort shutdown on exit.
		}()
	}

	fmt.Printf("[soaks3] running load: %d objects, %s each, concurrency=%d, endpoint=%s\n",
		count, humanize.IBytes(uint64(objectSize)), opts.concurrency, opts.endpoint)

	start := time.Now()

	reportCtx, reportCancel := context.WithCancel(ctx)
	go m.runReporter(reportCtx, opts.reportEvery)

	client := &http.Client{Timeout: opts.timeout}

	g := &loadGen{
		opts:       opts,
		km:         km,
		objectSize: objectSize,
		rangeSize:  rangeSize,
		client:     client,
		metrics:    m,
	}

	g.run(ctx)

	reportCancel()
	m.summary(start)

	return nil
}

// resolveRunConfig determines the count, object size and key prefix from the
// manifest (when set) or the explicit flags.
func resolveRunConfig(opts *runOptions) (count, objectSize int64, keyPrefix string, err error) {
	if opts.manifest != "" {
		man, err := readManifest(opts.manifest)
		if err != nil {
			return 0, 0, "", err
		}

		if man.Count <= 0 {
			return 0, 0, "", fmt.Errorf("manifest %s has non-positive count %d", opts.manifest, man.Count)
		}

		if man.ObjectSize <= 0 {
			return 0, 0, "", fmt.Errorf("manifest %s has non-positive object size %d", opts.manifest, man.ObjectSize)
		}

		return man.Count, man.ObjectSize, man.KeyPrefix, nil
	}

	if opts.count <= 0 {
		return 0, 0, "", errors.New("one of --count or --manifest must be set")
	}

	objectSize, err = parseSize(opts.objectSize)
	if err != nil {
		return 0, 0, "", fmt.Errorf("--object-size: %w", err)
	}

	if objectSize <= 0 {
		return 0, 0, "", fmt.Errorf("--object-size must be positive, got %d", objectSize)
	}

	return opts.count, objectSize, opts.keyPrefix, nil
}

// loadGen drives concurrent read workers.
type loadGen struct {
	opts       *runOptions
	km         keyModel
	objectSize int64
	rangeSize  int64
	client     *http.Client
	metrics    *metrics
}

// run starts the workers and blocks until ctx is done.
func (g *loadGen) run(ctx context.Context) {
	var wg sync.WaitGroup

	// perWorkerInterval paces each worker to approximate the aggregate rate.
	var perWorkerInterval time.Duration
	if g.opts.rate > 0 {
		perWorkerInterval = time.Duration(float64(time.Second) * float64(g.opts.concurrency) / g.opts.rate)
	}

	for w := 0; w < g.opts.concurrency; w++ {
		sel, err := g.opts.zipf.newSelector(g.km.count, w)
		if err != nil {
			fmt.Printf("[soaks3] worker %d: %v\n", w, err)
			continue
		}

		wg.Add(1)

		go func(sel *selector) {
			defer wg.Done()

			g.worker(ctx, sel, perWorkerInterval)
		}(sel)
	}

	wg.Wait()
}

// worker issues requests until ctx is done.
func (g *loadGen) worker(ctx context.Context, sel *selector, interval time.Duration) {
	next := time.Now()

	for {
		if ctx.Err() != nil {
			return
		}

		if interval > 0 {
			now := time.Now()
			if next.After(now) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(next.Sub(now)):
				}

				next = next.Add(interval)
			} else {
				// The worker has fallen behind schedule (a request took
				// longer than the pacing interval). Resync to now instead
				// of accumulating credit, so improving latency cannot cause
				// a burst that overshoots the target rate.
				next = now.Add(interval)
			}
		}

		idx := sel.pick()
		g.doRequest(ctx, sel, idx)
	}
}

// doRequest performs a single GET (optionally ranged) and records metrics.
func (g *loadGen) doRequest(ctx context.Context, sel *selector, idx int64) {
	key := g.km.keyForIndex(idx)
	url := g.objectURL(key)
	op := "GET"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		g.metrics.observe(op, "error", 0, 0, true)
		return
	}

	wantStatus := http.StatusOK

	if g.opts.rangeRead {
		op = "GET-range"
		start := g.rangeStart(sel)
		end := start + g.rangeSize - 1
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		wantStatus = http.StatusPartialContent
	}

	g.metrics.inflight.Inc()

	began := time.Now()
	resp, err := g.client.Do(req)
	g.metrics.inflight.Dec()

	if err != nil {
		if ctx.Err() != nil {
			return
		}

		g.metrics.observe(op, "error", time.Since(began), 0, true)

		return
	}

	n, copyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close() //nolint:errcheck // best-effort close after draining.
	elapsed := time.Since(began)

	failed := resp.StatusCode != wantStatus || copyErr != nil
	g.metrics.observe(op, strconv.Itoa(resp.StatusCode), elapsed, n, failed)
}

// objectURL builds the path-style object URL.
func (g *loadGen) objectURL(key string) string {
	base := g.opts.endpoint
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}

	if g.opts.bucket != "" {
		return base + "/" + g.opts.bucket + "/" + key
	}

	return base + "/" + key
}

// rangeStart chooses a random in-bounds start offset for a ranged read.
func (g *loadGen) rangeStart(sel *selector) int64 {
	span := g.objectSize - g.rangeSize
	if span <= 0 {
		return 0
	}

	return sel.rng.Int63n(span + 1)
}
