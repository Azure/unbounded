// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type pullOptions struct {
	Backend           string
	Volume            string
	Profile           string
	ZipfExponent      float64
	Target            string
	Namespace         string
	Concurrency       int
	ConcurrencyFile   string
	NodeCapsFile      string
	NodeName          string
	BlobConcurrency   int
	Timeout           time.Duration
	RetryDelay        time.Duration
	Interval          time.Duration
	Verify            bool
	DiagnoseIntegrity bool
}

type puller struct {
	batches      []blobBatch
	acquire      func(context.Context, string, ocispec.Descriptor) (blobResponse, error)
	closeBackend func() error
	repository   string
	opts         pullOptions
	target       *url.URL
	metrics      *loadMetrics
	client       *http.Client
	transport    *http.Transport
	devNull      *os.File
	buffers      sync.Pool
	failureLogs  failureLogs
	expected     map[digest.Digest]blobSource
	// Shared by workers; production uses the concurrency-safe package RNG.
	randomFloat64 func() float64
}

func newPuller(repository string, opts pullOptions, metrics *loadMetrics) (*puller, error) {
	if opts.Backend == "" {
		opts.Backend = "gantry"
	}

	if opts.Backend != "gantry" && opts.Backend != "uds" && opts.Backend != "s3" {
		return nil, errors.New("backend must be gantry, uds, or s3")
	}

	if opts.DiagnoseIntegrity && !opts.Verify {
		return nil, errors.New("diagnose-integrity requires verify")
	}

	if repository == "" || metrics == nil {
		return nil, errors.New("repository and metrics are required")
	}

	if opts.Profile == "" {
		opts.Profile = profileShuffle
	}

	if err := validateProfile(opts.Profile, opts.ZipfExponent); err != nil {
		return nil, err
	}

	if opts.Concurrency < 0 || opts.BlobConcurrency < 1 || opts.Timeout <= 0 || opts.RetryDelay <= 0 || opts.Interval < 0 {
		return nil, errors.New("concurrency and interval must be nonnegative; blob concurrency, timeout, and retry delay must be positive")
	}

	capacity := opts.Concurrency
	if err := validateNodeCapsOptions(opts); err != nil {
		return nil, err
	}

	if opts.ConcurrencyFile != "" {
		if opts.Concurrency > maxLiveConcurrency {
			return nil, fmt.Errorf("concurrency must be in [0, %d] when concurrency-file is enabled", maxLiveConcurrency)
		}

		capacity = maxLiveConcurrency
	}

	if capacity > int(^uint(0)>>1)/opts.BlobConcurrency {
		return nil, errors.New("combined batch and blob concurrency is too large")
	}

	if opts.Backend == "uds" {
		// HTTP configuration is irrelevant in direct mode, including target validation.
		opts.Target = "http://unused.invalid"
	}

	target, err := url.Parse(opts.Target)
	if err != nil {
		return nil, fmt.Errorf("parse pull target: %w", err)
	}

	if (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" || target.User != nil ||
		target.RawQuery != "" || target.ForceQuery || strings.Contains(opts.Target, "#") || target.Opaque != "" {
		return nil, errors.New("pull target must be an http or https URL with a host and no credentials, query, or fragment")
	}

	if port := target.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid pull target port")
		}
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport must be an *http.Transport")
	}

	transport := base.Clone()
	transport.DisableCompression = true
	transport.MaxIdleConnsPerHost = max(2, capacity*opts.BlobConcurrency)
	transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	p := &puller{
		randomFloat64: rand.Float64,
		repository:    repository, opts: opts, target: target, metrics: metrics, transport: transport,
		client: &http.Client{
			Transport: transport,
			// Read redirect responses as failures so every received body is accounted for.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		buffers: sync.Pool{New: func() any {
			buffer := make([]byte, 32*1024)
			return &buffer
		}},
	}

	p.acquire = p.acquireHTTP
	if opts.Backend == "uds" {
		if err := p.configureUDS(capacity); err != nil {
			transport.CloseIdleConnections()
			return nil, err
		}
	}

	p.devNull, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("open drain destination: %w", err)
	}

	return p, nil
}

func (p *puller) close() {
	p.transport.CloseIdleConnections()

	if p.devNull != nil {
		if err := p.devNull.Close(); err != nil {
			slog.Warn("close drain destination", "error", err)
		}
	}

	if p.closeBackend != nil {
		if err := p.closeBackend(); err != nil {
			slog.Warn("close blob backend", "error", err)
		}
	}
}

// run owns the worker lifetime, including workers sleeping between pulls.
func (p *puller) run(ctx context.Context) {
	defer p.transport.CloseIdleConnections()
	defer p.metrics.appliedConcurrency.Set(0)

	if p.opts.ConcurrencyFile != "" {
		p.runLive(ctx, livePollInterval)
		return
	}

	p.metrics.appliedConcurrency.Set(float64(p.opts.Concurrency))

	initialTraversal := p.newTraversal()

	var workers sync.WaitGroup
	for range p.opts.Concurrency {
		workers.Go(func() {
			traversal := initialTraversal

			for ctx.Err() == nil {
				delay := p.opts.Interval
				if err := p.pullBatch(ctx, p.nextBatch(&traversal)); err != nil {
					delay = p.opts.RetryDelay
				}

				if !waitPullDelay(ctx, delay) {
					return
				}
			}
		})
	}

	// In origin-only mode there are no workers, but run still blocks until shutdown.
	<-ctx.Done()
	workers.Wait()
}

func waitPullDelay(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (p *puller) pullBatch(ctx context.Context, batch blobBatch) (err error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	start := time.Now()

	p.metrics.inFlight.Inc()

	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		result := pullResult(err)
		if err == nil && p.opts.Verify {
			// Credit only a complete verified operation, including any OCI prefix.
			var bytes float64
			for _, blob := range batch.prefix {
				bytes += float64(blob.descriptor.Size)
			}

			for _, blob := range batch.blobs {
				bytes += float64(blob.descriptor.Size)
			}

			p.metrics.verifiedBytes.Add(bytes)
		}

		p.metrics.inFlight.Dec()
		p.metrics.pulls.WithLabelValues(result).Inc()
		p.metrics.pullDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
		p.reportPullFailure(err, time.Now(), slog.Default())
	}()

	for _, blob := range batch.prefix {
		if err := p.fetch(ctx, blob.kind, blob.descriptor); err != nil {
			return err
		}
	}

	group, blobCtx := errgroup.WithContext(ctx)
	count := min(p.opts.BlobConcurrency, len(batch.blobs))
	// A fixed worker pool avoids allocating a goroutine or queued job per blob.
	for worker := range count {
		group.Go(func() error {
			for index := worker; index < len(batch.blobs); index += count {
				if err := blobCtx.Err(); err != nil {
					return err
				}

				blob := batch.blobs[index]
				if err := p.fetch(blobCtx, blob.kind, blob.descriptor); err != nil {
					return err
				}
			}

			return nil
		})
	}

	return group.Wait()
}

func pullResult(err error) string {
	if err == nil {
		return "success"
	}

	if errors.Is(err, context.Canceled) {
		return "canceled"
	}

	return "error"
}

func (p *puller) fetch(ctx context.Context, kind string, desc ocispec.Descriptor) (err error) {
	start := time.Now()
	reason, status := failureTransport, 0

	var integrity *integrityEvidence

	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		if err != nil {
			err = &pullFailure{err: err, reason: reason, kind: kind, status: status, integrity: integrity}
		}

		result := pullResult(err)
		p.metrics.requests.WithLabelValues(kind, result).Inc()
		p.metrics.requestDuration.WithLabelValues(kind, result).Observe(time.Since(start).Seconds())
	}()

	response, err := p.acquire(ctx, kind, desc)
	if err != nil {
		switch p.opts.Backend {
		case "uds":
			if code := sdkErrorStatus(err); code != 0 {
				reason, status = failureStatus, code
			}
		case "s3":
			if code := s3ErrorStatus(err); code != 0 {
				reason, status = failureStatus, code
			}
		}

		return fmt.Errorf("request %s: %w", kind, err)
	}

	status = response.status

	defer func() {
		if closeErr := response.body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close %s body: %w", kind, closeErr)
		}
	}()

	var (
		n      int64
		actual string
		pages  *pageEvidence
	)

	if p.opts.DiagnoseIntegrity && response.success {
		expected, ok := p.expected[desc.Digest]
		if !ok || expected.descriptor.Size != desc.Size {
			reason = failureOther
			return errors.New("diagnostic expected object missing or inconsistent")
		}

		n, actual, pages, err = p.readBodyDiagnostic(response.body, expected)
		if errors.Is(err, errDiagnosticOracle) {
			reason = failureOther
		}
	} else {
		n, actual, err = p.readBody(response.body)
	}

	if err != nil {
		return fmt.Errorf("read %s %s: %w", kind, desc.Digest, err)
	}

	if !response.success {
		reason = failureStatus
		return fmt.Errorf("request %s %s: status %d", kind, desc.Digest, status)
	}

	if response.totalSize != nil && *response.totalSize != desc.Size {
		reason = failureSize
		return fmt.Errorf("%s %s: metadata size %d, expected %d", kind, desc.Digest, *response.totalSize, desc.Size)
	}

	if response.etag != nil && *response.etag != `"`+desc.Digest.String()+`"` {
		reason = failureOther
		return fmt.Errorf("%s %s: metadata ETag mismatch", kind, desc.Digest)
	}

	if n != desc.Size {
		reason = failureSize
		if n < desc.Size {
			reason = failureIncomplete
		}

		return fmt.Errorf("%s %s: size %d, expected %d", kind, desc.Digest, n, desc.Size)
	}

	if p.opts.Verify && actual != desc.Digest.String() {
		reason = failureDigest
		integrity = &integrityEvidence{
			expectedDigest: desc.Digest.String(), actualDigest: actual,
			expectedSize: desc.Size, receivedSize: n,
			pages: pages,
		}

		return fmt.Errorf("%s %s: digest mismatch (received %s)", kind, desc.Digest, actual)
	}

	return nil
}

// readBody counts bytes even when Read returns data together with an error.
func (p *puller) readBody(body io.Reader) (int64, string, error) {
	if !p.opts.Verify && !p.opts.DiagnoseIntegrity {
		// Keep both the *os.File destination and Object source unwrapped so the
		// SDK can splice. The file is opened once per puller and shared by workers.
		n, err := io.Copy(p.devNull, body)
		p.metrics.receivedBytes.Add(float64(n))

		return n, "", err
	}

	buffer, ok := p.buffers.Get().(*[]byte)
	if !ok {
		return 0, "", errors.New("invalid pull buffer")
	}
	defer p.buffers.Put(buffer)

	var hasher hash.Hash
	if p.opts.Verify {
		hasher = sha256.New()
	}

	var total int64

	for {
		n, err := body.Read(*buffer)
		if n > 0 {
			total += int64(n)
			p.metrics.receivedBytes.Add(float64(n))

			if hasher != nil {
				if _, hashErr := hasher.Write((*buffer)[:n]); hashErr != nil {
					return total, "", hashErr
				}
			}
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return total, "", err
		}
	}

	if hasher != nil {
		return total, fmt.Sprintf("sha256:%x", hasher.Sum(nil)), nil
	}

	return total, "", nil
}

// sdkErrorStatus maps SDK sentinels to HTTP-style reporting codes. Context and
// closed errors remain transport failures; unknown SDK failures are bad gateways.
func sdkErrorStatus(err error) int {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, net.ErrClosed):
		return 0
	case errors.Is(err, racersdk.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, racersdk.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, racersdk.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, racersdk.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, racersdk.ErrVersionMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, racersdk.ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable
	case errors.Is(err, racersdk.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}
