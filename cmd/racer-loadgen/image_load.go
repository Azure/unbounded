// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

const (
	maxImageMetadata = 1 << 20
	maxImageCatalog  = 32 << 20
)

type imageMetrics struct {
	pulls, blobs                    *prometheus.CounterVec
	duration                        *prometheus.HistogramVec
	bytes                           prometheus.Counter
	registryRequests, registryBytes *prometheus.CounterVec
}

func newImageMetrics(reg prometheus.Registerer) *imageMetrics {
	m := &imageMetrics{
		pulls:            prometheus.NewCounterVec(prometheus.CounterOpts{Name: "racer_loadgen_image_pulls_total", Help: "Completed image pull attempts."}, []string{"result"}),
		blobs:            prometheus.NewCounterVec(prometheus.CounterOpts{Name: "racer_loadgen_image_objects_total", Help: "Manifest, config, and layer download attempts."}, []string{"kind", "result"}),
		duration:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "racer_loadgen_image_pull_duration_seconds", Help: "Full verified image pull latency, excluding catalog preparation.", Buckets: []float64{.001, .01, .1, .5, 1, 5, 10, 30, 60, 120, 300, 600}}, []string{"result"}),
		bytes:            prometheus.NewCounter(prometheus.CounterOpts{Name: "racer_loadgen_image_received_bytes_total", Help: "Manifest, config, and layer bytes consumed, including failed attempts; excludes catalog."}),
		registryRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "racer_loadgen_registry_requests_total", Help: "Known registry object requests by kind and request type."}, []string{"kind", "request"}),
		registryBytes:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "racer_loadgen_registry_sent_bytes_total", Help: "Registry response body bytes by kind and request type."}, []string{"kind", "request"}),
	}
	reg.MustRegister(m.pulls, m.blobs, m.duration, m.bytes, m.registryRequests, m.registryBytes)

	for _, result := range []string{"success", "error"} {
		m.pulls.WithLabelValues(result)
		m.duration.WithLabelValues(result)
	}

	return m
}

// Redirects could turn a successful benchmark into a direct upstream pull.
func imageHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck // The standard transport is never replaced by this binary.
	t.DisableCompression = true
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 256

	return &http.Client{Transport: t, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func fetchCatalog(ctx context.Context, client *http.Client, c config) (imageCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.registryURL, "/")+"/loadgen/catalog", nil)
	if err != nil {
		return imageCatalog{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return imageCatalog{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // Response cleanup.

	if resp.StatusCode != http.StatusOK {
		return imageCatalog{}, fmt.Errorf("catalog: %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageCatalog+1))
	if err != nil || len(data) > maxImageCatalog {
		return imageCatalog{}, fmt.Errorf("catalog body too large or unreadable: %v", err)
	}

	var catalog imageCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return catalog, err
	}

	if catalog.Version != 1 || catalog.Repository != imageRepository || len(catalog.Images) == 0 || len(catalog.Images) > 100_000 {
		return catalog, fmt.Errorf("invalid image catalog")
	}

	for _, desc := range catalog.Images {
		if err := validateImageDescriptor(desc, oci.MediaTypeImageManifest); err != nil {
			return catalog, err
		}
	}

	return catalog, nil
}

func validateImageDescriptor(d oci.Descriptor, mediaType string) error {
	if d.Digest.Validate() != nil || d.Digest.Algorithm().String() != "sha256" || d.Size <= 0 || d.MediaType != mediaType || len(d.URLs) != 0 {
		return fmt.Errorf("invalid %s descriptor", mediaType)
	}

	if mediaType != oci.MediaTypeImageLayer && d.Size > maxImageMetadata {
		return fmt.Errorf("image metadata exceeds %d bytes", maxImageMetadata)
	}

	return nil
}

func fetchImageObject(ctx context.Context, client *http.Client, c config, desc oci.Descriptor, kind string, m *imageMetrics) (data []byte, err error) {
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}

		m.blobs.WithLabelValues(kind, result).Inc()
	}()

	pathKind := "blobs"
	if kind == "manifest" {
		pathKind = "manifests"
	}

	target := strings.TrimRight(c.gantryEndpoint, "/") + "/v2/" + imageRepository + "/" + pathKind + "/" + desc.Digest.String() + "?ns=" + url.QueryEscape(c.registryNamespace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", desc.MediaType)
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // Response cleanup.

	if resp.StatusCode != http.StatusOK || (resp.ContentLength >= 0 && resp.ContentLength != desc.Size) || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		return nil, fmt.Errorf("%s: unexpected response %s, length %d", kind, resp.Status, resp.ContentLength)
	}

	h := sha256.New()

	var (
		body bytes.Buffer
		sink io.Writer = h
	)

	if kind != "layer" {
		sink = io.MultiWriter(h, &body)
	}
	// Bound malformed responses, including chunked bodies, to the descriptor size
	// plus one byte. Layers are never retained in memory.
	n, err := io.Copy(sink, io.LimitReader(resp.Body, desc.Size))
	m.bytes.Add(float64(n))

	if err != nil {
		return nil, err
	}

	var extra [1]byte

	extraN, extraErr := io.ReadFull(resp.Body, extra[:])
	m.bytes.Add(float64(extraN))

	if n != desc.Size || extraN != 0 || extraErr != io.EOF || fmt.Sprintf("sha256:%x", h.Sum(nil)) != desc.Digest.String() {
		return nil, fmt.Errorf("%s: size or SHA-256 mismatch", kind)
	}

	return body.Bytes(), nil
}

func pullImage(ctx context.Context, client *http.Client, c config, desc oci.Descriptor, m *imageMetrics) (err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()

	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}

		m.pulls.WithLabelValues(result).Inc()
		m.duration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	}()

	if err := validateImageDescriptor(desc, oci.MediaTypeImageManifest); err != nil {
		return err
	}

	data, err := fetchImageObject(ctx, client, c, desc, "manifest", m)
	if err != nil {
		return err
	}

	var manifest oci.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}

	if manifest.SchemaVersion != 2 || manifest.MediaType != oci.MediaTypeImageManifest || len(manifest.Layers) == 0 || len(manifest.Layers) > 1024 {
		return fmt.Errorf("invalid image manifest")
	}

	if err := validateImageDescriptor(manifest.Config, oci.MediaTypeImageConfig); err != nil {
		return err
	}

	for _, layer := range manifest.Layers {
		if err := validateImageDescriptor(layer, oci.MediaTypeImageLayer); err != nil {
			return err
		}
	}

	if _, err := fetchImageObject(ctx, client, c, manifest.Config, "config", m); err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(c.layerConcurrency)

	for _, layer := range manifest.Layers {
		g.Go(func() error {
			_, err := fetchImageObject(ctx, client, c, layer, "layer", m)
			return err
		})
	}

	return g.Wait()
}

func runImageLoad(ctx context.Context, client *http.Client, c config, catalog imageCatalog, m *imageMetrics) {
	z := newZipf(len(catalog.Images), c.exponent)

	var wg sync.WaitGroup
	for i := 0; i < c.concurrency; i++ {
		wg.Go(func() {
			r := rand.New(rand.NewSource(int64(mix(uint64(c.seed) + uint64(i)))))
			for ctx.Err() == nil {
				desc := catalog.Images[z.sample(r)]
				if err := pullImage(ctx, client, c, desc, m); err != nil {
					if ctx.Err() != nil {
						return
					}

					slog.Warn("image pull failed", "worker", i, "digest", desc.Digest, "error", err)

					timer := time.NewTimer(time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		})
	}

	wg.Wait()
}
