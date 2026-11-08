// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
	mediaTypeLayerTar = "application/vnd.oci.image.layer.v1.tar"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// layerSeed derives the deterministic PRNG seed of one layer from the seed
// string and the layer index, so equal (seed, index) pairs always produce the
// same bytes and therefore the same digest.
func layerSeed(seed string, index int) [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "gantry-cluster-bench/%s/%d", seed, index))
}

// writeLayerTar writes an uncompressed tar holding one file of size bytes of
// ChaCha8 output. The data is incompressible and generated on the fly.
func writeLayerTar(w io.Writer, seed string, index int, size int64) error {
	tw := tar.NewWriter(w)

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     fmt.Sprintf("bench/layer-%03d.bin", index),
		Size:     size,
		Mode:     0o644,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	rng := rand.NewChaCha8(layerSeed(seed, index))
	buf := make([]byte, 4<<20)

	for remaining := size; remaining > 0; {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}

		if _, err := rng.Read(buf[:n]); err != nil {
			return err
		}

		if _, err := tw.Write(buf[:n]); err != nil {
			return err
		}

		remaining -= n
	}

	return tw.Close()
}

// countingHash wraps a sha256 and counts the bytes written.
type countingHash struct {
	h hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

func (c *countingHash) digest() string { return "sha256:" + hex.EncodeToString(c.h.Sum(nil)) }

type registryClient struct {
	base   *url.URL
	repo   string
	client *http.Client
}

func newRegistryClient(registry, repo string) (*registryClient, error) {
	if !strings.Contains(registry, "://") {
		registry = "http://" + registry
	}

	u, err := url.Parse(registry)
	if err != nil {
		return nil, err
	}

	return &registryClient{base: u, repo: repo, client: &http.Client{}}, nil
}

func (r *registryClient) url(path string) string {
	return r.base.ResolveReference(&url.URL{Path: "/v2/" + r.repo + path}).String()
}

func (r *registryClient) do(ctx context.Context, method, target string, body io.Reader, size int64, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.ContentLength = size
	}

	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	return r.client.Do(req)
}

// closeBody drains and closes a response body whose content is not needed.
func closeBody(resp *http.Response) {
	_, cerr := io.Copy(io.Discard, resp.Body)
	if err := errors.Join(cerr, resp.Body.Close()); err != nil {
		fmt.Fprintln(os.Stderr, "close body:", err)
	}
}

func checkStatus(resp *http.Response, want ...int) error {
	for _, w := range want {
		if resp.StatusCode == w {
			return nil
		}
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		b = []byte(err.Error())
	}

	return fmt.Errorf("%s %s: unexpected status %d: %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, strings.TrimSpace(string(b)))
}

func (r *registryClient) location(resp *http.Response) (*url.URL, error) {
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return nil, err
	}

	return r.base.ResolveReference(loc), nil
}

// blobExists reports whether the registry already has digest.
func (r *registryClient) blobExists(ctx context.Context, digest string) (bool, error) {
	resp, err := r.do(ctx, http.MethodHead, r.url("/blobs/"+digest), nil, 0, nil)
	if err != nil {
		return false, err
	}
	defer closeBody(resp)

	return resp.StatusCode == http.StatusOK, nil
}

// uploadStream streams body as one PATCH (chunked transfer) and commits it.
// The digest is only known after the body is consumed, so it is supplied by
// digestFn after the PATCH completes.
func (r *registryClient) uploadStream(ctx context.Context, body io.Reader, digestFn func() string) error {
	resp, err := r.do(ctx, http.MethodPost, r.url("/blobs/uploads/"), nil, 0, nil)
	if err != nil {
		return err
	}

	if err := resp.Body.Close(); err != nil {
		return err
	}

	if err := checkStatus(resp, http.StatusAccepted); err != nil {
		return err
	}

	loc, err := r.location(resp)
	if err != nil {
		return err
	}

	resp, err = r.do(ctx, http.MethodPatch, loc.String(), body, -1, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}

	if err := resp.Body.Close(); err != nil {
		return err
	}

	if err := checkStatus(resp, http.StatusAccepted, http.StatusNoContent); err != nil {
		return err
	}

	if loc, err = r.location(resp); err != nil {
		return err
	}

	q := loc.Query()
	q.Set("digest", digestFn())
	loc.RawQuery = q.Encode()

	resp, err = r.do(ctx, http.MethodPut, loc.String(), http.NoBody, 0, nil)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	return checkStatus(resp, http.StatusCreated)
}

func (r *registryClient) uploadBytes(ctx context.Context, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	ok, err := r.blobExists(ctx, digest)
	if err != nil || ok {
		return digest, err
	}

	return digest, r.uploadStream(ctx, bytes.NewReader(data), func() string { return digest })
}

func (r *registryClient) putManifest(ctx context.Context, tag string, data []byte) (string, error) {
	resp, err := r.do(ctx, http.MethodPut, r.url("/manifests/"+tag), bytes.NewReader(data), int64(len(data)),
		map[string]string{"Content-Type": mediaTypeManifest})
	if err != nil {
		return "", err
	}
	defer closeBody(resp)

	if err := checkStatus(resp, http.StatusCreated); err != nil {
		return "", err
	}

	return resp.Header.Get("Docker-Content-Digest"), nil
}

// pushLayer generates layer index and streams it to the registry, returning
// its descriptor (the digest doubles as the diff ID since it is uncompressed).
func (r *registryClient) pushLayer(ctx context.Context, seed string, index int, size int64) (descriptor, error) {
	pr, pw := io.Pipe()
	ch := &countingHash{h: sha256.New()}

	go func() {
		pw.CloseWithError(writeLayerTar(io.MultiWriter(pw, ch), seed, index, size))
	}()

	err := r.uploadStream(ctx, pr, ch.digest)
	_ = pr.CloseWithError(errors.New("upload finished"))

	if err != nil {
		return descriptor{}, fmt.Errorf("layer %d: %w", index, err)
	}

	return descriptor{MediaType: mediaTypeLayerTar, Digest: ch.digest(), Size: ch.n}, nil
}

// parseSize accepts plain bytes or a Ki/Mi/Gi (binary) or K/M/G (decimal) suffix.
func parseSize(s string) (int64, error) {
	mult := int64(1)

	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSuffix(s, u.suffix)

			break
		}
	}

	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}

	return n * mult, nil
}

func runGenImage(args []string) error {
	fs := flag.NewFlagSet("gen-image", flag.ExitOnError)
	registry := fs.String("registry", "127.0.0.1:5005", "registry host:port (plain HTTP unless a scheme is given)")
	repo := fs.String("repo", "bench/g40", "repository")
	tag := fs.String("tag", "", "tag (required)")
	seed := fs.String("seed", "", "PRNG seed (default: repo:tag)")
	layers := fs.Int("layers", 4, "number of layers")
	layerSize := fs.String("layer-size", "10Gi", "payload bytes per layer (Ki/Mi/Gi binary, K/M/G decimal)")

	parallel := fs.Int("parallel", 4, "layers uploaded concurrently")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *tag == "" {
		return errors.New("--tag is required")
	}

	if *seed == "" {
		*seed = *repo + ":" + *tag
	}

	size, err := parseSize(*layerSize)
	if err != nil {
		return err
	}

	rc, err := newRegistryClient(*registry, *repo)
	if err != nil {
		return err
	}

	ctx := context.Background()
	start := time.Now()
	descs := make([]descriptor, *layers)
	errs := make([]error, *layers)
	sem := make(chan struct{}, max(1, *parallel))

	var wg sync.WaitGroup

	for i := range *layers {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			t := time.Now()
			descs[i], errs[i] = rc.pushLayer(ctx, *seed, i, size)

			if errs[i] == nil {
				el := time.Since(t)
				fmt.Fprintf(os.Stderr, "layer %d %s %d bytes in %s (%.0f MB/s)\n", i, descs[i].Digest, descs[i].Size,
					el.Round(time.Millisecond), float64(descs[i].Size)/el.Seconds()/1e6)
			}
		})
	}

	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return err
	}

	diffIDs := make([]string, len(descs))
	for i, d := range descs {
		diffIDs[i] = d.Digest
	}

	cfg, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      "1970-01-01T00:00:00Z",
		"config":       map[string]any{"Labels": map[string]string{"gantry-cluster-bench/seed": *seed}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		return err
	}

	cfgDigest, err := rc.uploadBytes(ctx, cfg)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	m, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config:        descriptor{MediaType: mediaTypeConfig, Digest: cfgDigest, Size: int64(len(cfg))},
		Layers:        descs,
	})
	if err != nil {
		return err
	}

	digest, err := rc.putManifest(ctx, *tag, m)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	var total int64
	for _, d := range descs {
		total += d.Size
	}

	fmt.Printf("IMAGE %s/%s:%s digest=%s layers=%d total_bytes=%d elapsed=%s\n",
		rc.base.Host, *repo, *tag, digest, len(descs), total, time.Since(start).Round(time.Second))

	return nil
}

func runDeleteImage(args []string) error {
	fs := flag.NewFlagSet("delete-image", flag.ExitOnError)
	registry := fs.String("registry", "127.0.0.1:5005", "registry host:port")
	repo := fs.String("repo", "bench/g40", "repository")

	tag := fs.String("tag", "", "tag (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *tag == "" {
		return errors.New("--tag is required")
	}

	rc, err := newRegistryClient(*registry, *repo)
	if err != nil {
		return err
	}

	ctx := context.Background()

	resp, err := rc.do(ctx, http.MethodHead, rc.url("/manifests/"+*tag), nil, 0, map[string]string{"Accept": mediaTypeManifest})
	if err != nil {
		return err
	}

	if err := resp.Body.Close(); err != nil {
		return err
	}

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}

	digest := resp.Header.Get("Docker-Content-Digest")

	resp, err = rc.do(ctx, http.MethodDelete, rc.url("/manifests/"+digest), nil, 0, nil)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	if err := checkStatus(resp, http.StatusAccepted); err != nil {
		return err
	}

	fmt.Printf("deleted %s/%s:%s (%s); run registry garbage-collect to reclaim space\n", rc.base.Host, *repo, *tag, digest)

	return nil
}
