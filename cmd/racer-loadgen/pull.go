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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

type pullOptions struct {
	Target           string
	Namespace        string
	Concurrency      int
	LayerConcurrency int
	Timeout          time.Duration
	RetryDelay       time.Duration
	Interval         time.Duration
	Verify           bool
}

type puller struct {
	img       *syntheticImage
	opts      pullOptions
	target    *url.URL
	metrics   *loadMetrics
	client    *http.Client
	transport *http.Transport
	buffers   sync.Pool
}

func newPuller(img *syntheticImage, opts pullOptions, metrics *loadMetrics) (*puller, error) {
	if img == nil || metrics == nil {
		return nil, errors.New("image and metrics are required")
	}

	if opts.Concurrency < 0 || opts.LayerConcurrency < 1 || opts.Timeout <= 0 || opts.RetryDelay <= 0 || opts.Interval < 0 {
		return nil, errors.New("concurrency and interval must be nonnegative; layer concurrency, timeout, and retry delay must be positive")
	}

	if opts.Concurrency > int(^uint(0)>>1)/opts.LayerConcurrency {
		return nil, errors.New("combined image and layer concurrency is too large")
	}

	target, err := url.Parse(opts.Target)
	if err != nil {
		return nil, fmt.Errorf("parse pull target: %w", err)
	}

	if (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" || target.User != nil ||
		target.RawQuery != "" || target.ForceQuery || strings.Contains(opts.Target, "#") || target.Opaque != "" {
		return nil, errors.New("pull target must be an http or https URL with a host and no credentials, query, or fragment")
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport must be an *http.Transport")
	}

	transport := base.Clone()
	transport.DisableCompression = true
	transport.MaxIdleConnsPerHost = max(2, opts.Concurrency*opts.LayerConcurrency)
	transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	p := &puller{
		img: img, opts: opts, target: target, metrics: metrics, transport: transport,
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

	return p, nil
}

// run owns the worker lifetime, including workers sleeping between pulls.
func (p *puller) run(ctx context.Context) {
	defer p.transport.CloseIdleConnections()

	var workers sync.WaitGroup
	for range p.opts.Concurrency {
		workers.Go(func() {
			for ctx.Err() == nil {
				delay := p.opts.Interval
				if err := p.pull(ctx); err != nil {
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

func (p *puller) pull(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	start := time.Now()

	p.metrics.inFlight.Inc()

	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		result := pullResult(err)

		p.metrics.inFlight.Dec()
		p.metrics.pulls.WithLabelValues(result).Inc()
		p.metrics.pullDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	}()

	if err := p.fetch(ctx, "manifest", p.img.Manifest); err != nil {
		return err
	}

	if err := p.fetch(ctx, "config", p.img.Config); err != nil {
		return err
	}

	group, layerCtx := errgroup.WithContext(ctx)
	count := min(p.opts.LayerConcurrency, len(p.img.Layers))
	// A fixed worker pool avoids allocating a goroutine or queued job per layer.
	for worker := range count {
		group.Go(func() error {
			for index := worker; index < len(p.img.Layers); index += count {
				if err := layerCtx.Err(); err != nil {
					return err
				}

				if err := p.fetch(layerCtx, "layer", p.img.Layers[index]); err != nil {
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

	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		result := pullResult(err)
		p.metrics.requests.WithLabelValues(kind, result).Inc()
		p.metrics.requestDuration.WithLabelValues(kind, result).Observe(time.Since(start).Seconds())
	}()

	endpoint := *p.target

	resource := "blobs"
	if kind == "manifest" {
		resource = "manifests"
	}

	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v2/" + p.img.repository + "/" + resource + "/" + desc.Digest.String()

	endpoint.RawPath = ""
	if p.opts.Namespace != "" {
		endpoint.RawQuery = url.Values{"ns": {p.opts.Namespace}}.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create %s request: %w", kind, err)
	}

	req.Header.Set("Accept", desc.MediaType)

	response, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", kind, err)
	}

	defer func() {
		if closeErr := response.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close %s body: %w", kind, closeErr)
		}
	}()

	n, actual, err := p.readBody(response.Body)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", kind, desc.Digest, err)
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("request %s %s: HTTP status %d", kind, desc.Digest, response.StatusCode)
	}

	if n != desc.Size {
		return fmt.Errorf("%s %s: size %d, expected %d", kind, desc.Digest, n, desc.Size)
	}

	if p.opts.Verify && actual != desc.Digest.String() {
		return fmt.Errorf("%s %s: digest mismatch (received %s)", kind, desc.Digest, actual)
	}

	return nil
}

// readBody counts bytes even when Read returns data together with an error.
func (p *puller) readBody(body io.Reader) (int64, string, error) {
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

		if errors.Is(err, io.EOF) {
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
