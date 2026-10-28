// Command genimage pushes an OCI image made of random, incompressible layers
// to a plain HTTP registry. Every run uses a different seed so each benchmark
// round starts from content that no cache has seen.
//
// It depends only on the standard library so it can be cross-compiled and
// copied into any pod next to the origin registry.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type layerResult struct {
	desc   descriptor
	diffID string
}

func main() {
	registry := flag.String("registry", "http://127.0.0.1:5000", "registry base URL")
	repo := flag.String("repo", "bench/img", "repository name")
	tag := flag.String("tag", "latest", "tag to push")
	layers := flag.Int("layers", 8, "number of layers")
	layerBytes := flag.Int64("layer-bytes", 1<<30, "random payload bytes per layer")
	seed := flag.Uint64("seed", uint64(time.Now().UnixNano()), "PRNG seed")
	parallel := flag.Int("parallel", 8, "layers generated and pushed in parallel")

	flag.Parse()

	start := time.Now()
	results := make([]layerResult, *layers)
	errs := make([]error, *layers)
	sem := make(chan struct{}, *parallel)

	var wg sync.WaitGroup

	for i := range *layers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			results[i], errs[i] = pushLayer(*registry, *repo, i, *layerBytes, *seed)
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			log.Fatal(err)
		}
	}

	diffIDs := make([]string, 0, len(results))
	layerDescs := make([]descriptor, 0, len(results))

	for _, r := range results {
		diffIDs = append(diffIDs, r.diffID)
		layerDescs = append(layerDescs, r.desc)
	}

	config := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      time.Now().UTC().Format(time.RFC3339),
		"config":       map[string]any{"Cmd": []string{"/data/none"}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}

	configDigest, err := pushBlob(*registry, *repo, bytes.NewReader(configBytes), nil)
	if err != nil {
		log.Fatal(err)
	}

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		"layers": layerDescs,
	}

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		log.Fatal(err)
	}

	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])

	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/%s", *registry, *repo, *tag), bytes.NewReader(manifestBytes))
	if err != nil {
		log.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")

	if err := expect(http.DefaultClient.Do(req)); err != nil {
		log.Fatalf("put manifest: %v", err)
	}

	var total int64
	for _, d := range layerDescs {
		total += d.Size
	}

	fmt.Fprintf(os.Stderr, "pushed %d layers, %d bytes in %s\n", len(layerDescs), total, time.Since(start).Round(time.Millisecond))
	fmt.Printf("%s@%s\n", *repo, manifestDigest)
}

// pushLayer streams one gzip(tar(random file)) layer to the registry and
// returns its descriptor and uncompressed diff ID. Random data does not
// compress, so gzip at NoCompression produces the same stored-block stream
// a real compressor would and decompression cost stays realistic.
func pushLayer(registry, repo string, index int, size int64, seed uint64) (layerResult, error) {
	pr, pw := io.Pipe()
	diff := sha256.New()

	go func() {
		pw.CloseWithError(writeLayer(pw, diff, index, size, seed))
	}()

	counter := &countingReader{r: pr}

	digest, err := pushBlob(registry, repo, counter, nil)
	if err != nil {
		return layerResult{}, fmt.Errorf("layer %d: %w", index, err)
	}

	return layerResult{
		desc: descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest,
			Size:      counter.n,
		},
		diffID: "sha256:" + hex.EncodeToString(diff.Sum(nil)),
	}, nil
}

func writeLayer(w io.Writer, diff hash.Hash, index int, size int64, seed uint64) error {
	gz, err := gzip.NewWriterLevel(w, gzip.NoCompression)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(io.MultiWriter(gz, diff))

	hdr := &tar.Header{
		Name:     fmt.Sprintf("data/layer-%02d.bin", index),
		Mode:     0o644,
		Size:     size,
		ModTime:  time.Unix(0, 0),
		Typeflag: tar.TypeReg,
	}

	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	var key [32]byte
	for i := range key {
		key[i] = byte(seed >> (8 * (i % 8)))
	}

	key[31] = byte(index)
	key[30] = byte(index >> 8)

	rng := rand.NewChaCha8(key)
	buf := make([]byte, 1<<20)

	for remaining := size; remaining > 0; {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}

		_, _ = rng.Read(buf[:n])

		if _, err := tw.Write(buf[:n]); err != nil {
			return err
		}

		remaining -= n
	}

	if err := tw.Close(); err != nil {
		return err
	}

	return gz.Close()
}

// pushBlob uploads body with a streaming PATCH and commits it by digest.
func pushBlob(registry, repo string, body io.Reader, _ http.Header) (string, error) {
	resp, err := http.Post(fmt.Sprintf("%s/v2/%s/blobs/uploads/", registry, repo), "application/octet-stream", nil)
	if err != nil {
		return "", err
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("start upload: %s", resp.Status)
	}

	location, err := resolve(registry, resp.Header.Get("Location"))
	if err != nil {
		return "", err
	}

	h := sha256.New()

	req, err := http.NewRequest(http.MethodPatch, location, io.TeeReader(body, h))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("patch upload: %s", resp.Status)
	}

	location, err = resolve(registry, resp.Header.Get("Location"))
	if err != nil {
		return "", err
	}

	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))

	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}

	req, err = http.NewRequest(http.MethodPut, location+sep+"digest="+url.QueryEscape(digest), nil)
	if err != nil {
		return "", err
	}

	if err := expect(http.DefaultClient.Do(req)); err != nil {
		return "", fmt.Errorf("commit upload: %w", err)
	}

	return digest, nil
}

func resolve(base, location string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	l, err := url.Parse(location)
	if err != nil {
		return "", err
	}

	return b.ResolveReference(l).String(), nil
}

func expect(resp *http.Response, err error) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}

	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}
