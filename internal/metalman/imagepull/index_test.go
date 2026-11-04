// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

func threeLayerImage(t *testing.T, reg *ipt.Registry) (string, []string) {
	t.Helper()

	ref, dgst := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{"disk/bottom": []byte("bottom")}},
		{Files: map[string][]byte{"disk/middle": []byte("middle")}},
		{Files: map[string][]byte{"disk/top": []byte("top")}},
	}})

	return ref, reg.LayerDigests(dgst)
}

func TestMissingFileServedFromIndex(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := threeLayerImage(t, reg)
	p := &imagepull.Puller{}
	img := resolve(t, p, imagepull.Source{Ref: ref})

	if _, err := img.Open(t.Context(), "pxelinux.cfg/01-aa-bb", "missing"); !errors.Is(err, imagepull.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if n := reg.Count(ipt.KindBlob); n != 3 {
		t.Fatalf("blob requests for first miss = %d, want 3 (one index build per layer)", n)
	}

	reg.ResetRequests()

	// A second Image from the same Puller shares the indexes.
	img2 := resolve(t, p, imagepull.Source{Ref: ref})
	for range 5 {
		if _, err := img2.Open(t.Context(), "other/missing"); !errors.Is(err, imagepull.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}

	if n := reg.Count(ipt.KindBlob); n != 0 {
		t.Fatalf("blob requests for indexed misses = %d, want 0", n)
	}

	if n := imagepull.IndexedLayers(p); n != 3 {
		t.Fatalf("indexed layers = %d, want 3", n)
	}
}

func TestOpenSkipsLayersWithoutName(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, layers := threeLayerImage(t, reg)
	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	// The top file only needs the top layer indexed.
	if data, _ := readFile(t, img, "disk/top"); string(data) != "top" {
		t.Fatalf("top = %q", data)
	}

	if n := len(reg.BlobRequests(layers[0])) + len(reg.BlobRequests(layers[1])); n != 0 {
		t.Fatalf("lower layer requests = %d, want 0", n)
	}

	readFile(t, img, "disk/bottom")
	reg.ResetRequests()

	if data, _ := readFile(t, img, "disk/bottom"); string(data) != "bottom" {
		t.Fatalf("bottom = %q", data)
	}

	if n := len(reg.BlobRequests(layers[0])); n != 1 {
		t.Fatalf("bottom layer requests = %d, want 1", n)
	}

	if n := len(reg.BlobRequests(layers[1])) + len(reg.BlobRequests(layers[2])); n != 0 {
		t.Fatalf("upper layer requests = %d, want 0", n)
	}
}

func TestIndexCacheBounded(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := threeLayerImage(t, reg)
	p := &imagepull.Puller{MaxIndexedLayers: 2}
	img := resolve(t, p, imagepull.Source{Ref: ref})

	if _, err := img.Open(t.Context(), "missing"); !errors.Is(err, imagepull.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	if n := imagepull.IndexedLayers(p); n != 2 {
		t.Fatalf("indexed layers = %d, want 2", n)
	}

	// Evicted indexes are rebuilt on demand.
	if data, _ := readFile(t, img, "disk/bottom"); string(data) != "bottom" {
		t.Fatalf("bottom = %q", data)
	}
}

func TestIndexBuildSingleflight(t *testing.T) {
	tracker := &concurrencyTracker{delay: 50 * time.Millisecond}
	reg := ipt.NewRegistry(t, ipt.WithMiddleware(tracker.middleware))
	ref, layers := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/a": []byte("a")})
	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			if _, err := img.Open(t.Context(), "missing"); !errors.Is(err, imagepull.ErrNotFound) {
				t.Errorf("err = %v", err)
			}
		})
	}

	wg.Wait()

	if n := len(reg.BlobRequests(reg.LayerDigests(layers)[0])); n != 1 {
		t.Fatalf("blob requests = %d, want 1 shared index build", n)
	}
}

func corruptFile(t *testing.T, reg *ipt.Registry, layer, marker string) []byte {
	t.Helper()

	resp, err := http.Get(reg.URL() + "/v2/img/blobs/" + layer) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	orig, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	blob := bytes.Clone(orig)

	i := bytes.Index(blob, []byte(marker))
	if i < 0 {
		t.Fatalf("marker %q not found", marker)
	}

	blob[i] ^= 0x20
	reg.SetBlob(layer, blob)

	return orig
}

func TestFileContentVerified(t *testing.T) {
	for name, opts := range map[string][]ipt.Option{"ranged": nil, "sequential": {ipt.WithoutRange()}} {
		t.Run(name, func(t *testing.T) {
			reg := ipt.NewRegistry(t, opts...)
			ref, dgst := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
				Entries:     []ipt.Entry{ipt.File("disk/file", []byte("good content")), ipt.File("disk/z", randomBytes(4096, 5))},
				Compression: ipt.Uncompressed,
			}}})
			layer := reg.LayerDigests(dgst)[0]
			img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})

			// Build the index from the intact layer, then corrupt the file.
			f, err := img.Open(t.Context(), "disk/file")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck // test

			corruptFile(t, reg, layer, "good content")

			if _, _, err := img.ReadFile(t.Context(), "disk/file"); !errors.Is(err, imagepull.ErrDigestMismatch) {
				t.Fatalf("ReadFile err = %v, want ErrDigestMismatch", err)
			}

			// A sequential read must never end with io.EOF.
			buf := make([]byte, 64)

			var lastErr error
			for lastErr == nil {
				_, lastErr = f.Read(buf)
			}

			if !errors.Is(lastErr, imagepull.ErrDigestMismatch) {
				t.Fatalf("Read err = %v, want ErrDigestMismatch", lastErr)
			}

			// Ranged reads from a non-zero offset cannot be verified.
			g, err := img.Open(t.Context(), "disk/file")
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close() //nolint:errcheck // test

			if _, err := g.Seek(5, io.SeekStart); err != nil {
				t.Fatal(err)
			}

			if data, err := io.ReadAll(g); err != nil || string(data) != "content" {
				t.Fatalf("partial read = %q, %v", data, err)
			}
		})
	}
}

func TestFileWithholdsTailOnDigestMismatch(t *testing.T) {
	reg := ipt.NewRegistry(t)

	content := append(randomBytes(4096, 9), []byte("TAILMARK")...)
	ref, dgst := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
		Entries:     []ipt.Entry{ipt.File("disk/file", content), ipt.File("disk/z", []byte("z"))},
		Compression: ipt.Uncompressed,
	}}})
	layer := reg.LayerDigests(dgst)[0]
	img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/file")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test

	corruptFile(t, reg, layer, "TAILMARK")

	var (
		got     []byte
		lastErr error
	)

	buf := make([]byte, 1000)

	for lastErr == nil {
		var n int

		n, lastErr = f.Read(buf)
		got = append(got, buf[:n]...)
	}

	if !errors.Is(lastErr, imagepull.ErrDigestMismatch) {
		t.Fatalf("Read err = %v, want ErrDigestMismatch", lastErr)
	}

	if len(got) >= len(content) {
		t.Fatalf("delivered %d of %d bytes, want the final read withheld", len(got), len(content))
	}

	if bytes.Contains(got, []byte("tAILMARK")) {
		t.Fatal("corrupted tail bytes were delivered")
	}

	if !bytes.Equal(got, content[:len(got)]) {
		t.Fatal("delivered prefix differs from content")
	}

	// The error is sticky.
	if _, err := f.Read(buf); !errors.Is(err, imagepull.ErrDigestMismatch) {
		t.Fatalf("second Read err = %v, want ErrDigestMismatch", err)
	}
}

func TestFileEmptyReadPrimesStream(t *testing.T) {
	reg := ipt.NewRegistry(t)
	content := randomBytes(3000, 23)
	ref, _ := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/f": content})
	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test

	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	if n, err := f.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty Read = %d, %v", n, err)
	}

	reg.ResetRequests()

	data, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(data, content[100:]) {
		t.Fatalf("ReadAll = %d bytes, %v", len(data), err)
	}

	if n := reg.Count(ipt.KindBlob); n != 0 {
		t.Fatalf("blob requests after priming = %d, want 0", n)
	}
}

func TestFileVerifiedAfterRewind(t *testing.T) {
	reg := ipt.NewRegistry(t)
	content := randomBytes(3000, 21)
	ref, _ := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/f": content, "disk/empty": nil})
	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test

	// Skip around, then read from the start: the full read is verified and
	// ends with io.EOF.
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	if data, err := io.ReadAll(f); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("full read: err=%v equal=%v", err, bytes.Equal(data, content))
	}

	if data, _ := readFile(t, img, "disk/empty"); len(data) != 0 {
		t.Fatalf("empty = %q", data)
	}
}

func TestIndexNotStoredWhenLayerUnverified(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, dgst := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/a": []byte("payload")})
	layer := reg.LayerDigests(dgst)[0]
	p := &imagepull.Puller{}
	img := resolve(t, p, imagepull.Source{Ref: ref})

	// Gzip compresses the payload, so corrupt the gzip trailer CRC instead
	// of searching for content: the last 8 bytes are CRC32 and size.
	resp, err := http.Get(reg.URL() + "/v2/img/blobs/" + layer) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	orig, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	bad := bytes.Clone(orig)
	bad[len(bad)-8] ^= 0xff
	reg.SetBlob(layer, bad)

	if _, _, err := img.ReadFile(t.Context(), "disk/a"); err == nil {
		t.Fatal("expected error for corrupt layer")
	}

	if n := imagepull.IndexedLayers(p); n != 0 {
		t.Fatalf("indexed layers = %d, want 0 after failed verification", n)
	}

	reg.SetBlob(layer, orig)

	if data, _ := readFile(t, img, "disk/a"); string(data) != "payload" {
		t.Fatalf("data = %q", data)
	}
}

func TestBufferBudget(t *testing.T) {
	for name, maxBuf := range map[string]int64{"shared": 3 * 1024, "smaller than a chunk": 512} {
		t.Run(name, func(t *testing.T) {
			tracker := &concurrencyTracker{delay: 2 * time.Millisecond}
			reg := ipt.NewRegistry(t, ipt.WithMiddleware(tracker.middleware))
			ref, content, _ := pushBigUncompressed(t, reg, 32*1024)

			p := &imagepull.Puller{ChunkSize: 1024, Concurrency: 4, MaxBufferedBytes: maxBuf}
			img := resolve(t, p, imagepull.Source{Ref: ref})

			var wg sync.WaitGroup

			for range 8 {
				wg.Go(func() {
					data, _, err := img.ReadFile(t.Context(), "disk/big")
					if err != nil || !bytes.Equal(data, content) {
						t.Errorf("ReadFile: err=%v equal=%v", err, bytes.Equal(data, content))
					}
				})
			}

			wg.Wait()

			used, peak := imagepull.BufferStats(p)
			if peak > maxBuf {
				t.Fatalf("peak buffered = %d, want <= %d", peak, maxBuf)
			}

			if used != 0 {
				t.Fatalf("buffered after completion = %d, want 0", used)
			}

			if maxBuf >= 1024 && peak == 0 {
				t.Fatal("expected some prefetching within the budget")
			}
		})
	}
}

func TestBufferBudgetReleasedOnClose(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _, _ := pushBigUncompressed(t, reg, 32*1024)

	p := &imagepull.Puller{ChunkSize: 1024, Concurrency: 4}
	img := resolve(t, p, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/big")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if used, _ := imagepull.BufferStats(p); used != 0 {
		t.Fatalf("buffered after Close = %d, want 0", used)
	}
}
