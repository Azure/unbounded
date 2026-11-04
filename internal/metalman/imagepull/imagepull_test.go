// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1)) //nolint:gosec // test data
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

func resolve(t *testing.T, p *imagepull.Puller, src imagepull.Source) *imagepull.Image {
	t.Helper()

	img, err := p.Resolve(t.Context(), src)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", src.Ref, err)
	}

	return img
}

func readFile(t *testing.T, img *imagepull.Image, names ...string) ([]byte, string) {
	t.Helper()

	data, name, err := img.ReadFile(t.Context(), names...)
	if err != nil {
		t.Fatalf("ReadFile(%v): %v", names, err)
	}

	return data, name
}

func TestResolveTagAndDigest(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, dgst := reg.PushFiles(t, "netboot", "v1", map[string][]byte{"disk/vmlinuz": []byte("kernel")})

	p := &imagepull.Puller{}

	img := resolve(t, p, imagepull.Source{Ref: ref})
	if img.Digest() != dgst {
		t.Fatalf("Digest = %s, want %s", img.Digest(), dgst)
	}

	if img.Source().Ref != ref {
		t.Fatalf("Source().Ref = %s, want %s", img.Source().Ref, ref)
	}

	byDigest := resolve(t, p, imagepull.Source{Ref: reg.Host() + "/netboot@" + dgst})
	if byDigest.Digest() != dgst {
		t.Fatalf("Digest by digest ref = %s, want %s", byDigest.Digest(), dgst)
	}

	for _, im := range []*imagepull.Image{img, byDigest} {
		data, name := readFile(t, im, "/disk/vmlinuz")
		if string(data) != "kernel" || name != "disk/vmlinuz" {
			t.Fatalf("ReadFile = %q (%s)", data, name)
		}
	}
}

func TestResolveMissingImage(t *testing.T) {
	reg := ipt.NewRegistry(t)

	_, err := (&imagepull.Puller{}).Resolve(t.Context(), imagepull.Source{Ref: reg.Host() + "/nope:v1"})
	if !errors.Is(err, imagepull.ErrImageNotFound) {
		t.Fatalf("err = %v, want ErrImageNotFound", err)
	}

	if _, err := (&imagepull.Puller{}).Resolve(t.Context(), imagepull.Source{Ref: "not a ref"}); err == nil {
		t.Fatal("expected error for invalid reference")
	}
}

func TestMultiArchSelection(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, indexDigest := reg.PushMultiArch(t, "netboot", "v1", map[string]map[string][]byte{
		"amd64": {"disk/arch": []byte("amd64")},
		"arm64": {"disk/arch": []byte("arm64")},
	})

	p := &imagepull.Puller{}

	for arch, want := range map[string]string{"": "amd64", "amd64": "amd64", "arm64": "arm64", "linux/arm64": "arm64", "aarch64": "arm64"} {
		img := resolve(t, p, imagepull.Source{Ref: ref, Architecture: arch})
		if img.Digest() == indexDigest {
			t.Fatalf("arch %q: Digest is the index digest", arch)
		}

		data, _ := readFile(t, img, "disk/arch")
		if string(data) != want {
			t.Fatalf("arch %q: got %q, want %q", arch, data, want)
		}
	}

	_, err := p.Resolve(t.Context(), imagepull.Source{Ref: ref, Architecture: "riscv64"})
	if !errors.Is(err, imagepull.ErrPlatformNotFound) {
		t.Fatalf("err = %v, want ErrPlatformNotFound", err)
	}
}

func TestDockerMediaTypes(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "netboot", "v1", ipt.ImageSpec{
		Docker: true,
		Platforms: map[string][]ipt.Layer{
			"amd64": {
				{Files: map[string][]byte{"disk/a": []byte("gz")}},
				{Files: map[string][]byte{"disk/b": []byte("tar")}, Compression: ipt.Uncompressed},
			},
		},
	})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	for name, want := range map[string]string{"disk/a": "gz", "disk/b": "tar"} {
		if data, _ := readFile(t, img, name); string(data) != want {
			t.Fatalf("%s = %q, want %q", name, data, want)
		}
	}
}

func TestLayerCompressions(t *testing.T) {
	for name, c := range map[string]ipt.Compression{"gzip": ipt.Gzip, "uncompressed": ipt.Uncompressed, "zstd": ipt.Zstd} {
		t.Run(name, func(t *testing.T) {
			reg := ipt.NewRegistry(t)
			content := randomBytes(5000, 1)
			ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
				Files:       map[string][]byte{"disk/blob": content, "disk/other": []byte("x")},
				Compression: c,
			}}})

			img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})
			if data, _ := readFile(t, img, "disk/blob"); !bytes.Equal(data, content) {
				t.Fatal("content mismatch")
			}

			if _, _, err := img.ReadFile(t.Context(), "disk/missing"); !errors.Is(err, imagepull.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestLayerPrecedenceAndWhiteouts(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{
			"disk/a":      []byte("old a"),
			"disk/b":      []byte("b"),
			"disk/dir/c":  []byte("c"),
			"disk/gone/x": []byte("x"),
			"disk/keep":   []byte("keep"),
			"root":        []byte("root"),
		}},
		{Entries: []ipt.Entry{
			ipt.Whiteout("disk/b"),
			ipt.Opaque("disk/dir"),
			ipt.Whiteout("disk/gone"),
		}, Files: map[string][]byte{
			"disk/a":     []byte("new a"),
			"disk/dir/e": []byte("e"),
		}},
		{Files: map[string][]byte{"disk/top": []byte("top")}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	for name, want := range map[string]string{
		"disk/a": "new a", "disk/dir/e": "e", "disk/keep": "keep", "disk/top": "top", "root": "root",
	} {
		if data, _ := readFile(t, img, name); string(data) != want {
			t.Errorf("%s = %q, want %q", name, data, want)
		}
	}

	for _, name := range []string{"disk/b", "disk/dir/c", "disk/gone/x"} {
		if _, _, err := img.ReadFile(t.Context(), name); !errors.Is(err, imagepull.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestOpaqueRoot(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{"disk/a": []byte("a")}},
		{Entries: []ipt.Entry{ipt.Opaque("")}, Files: map[string][]byte{"disk/b": []byte("b")}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})
	if _, _, err := img.ReadFile(t.Context(), "disk/a"); !errors.Is(err, imagepull.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenNamePrecedence(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Files: map[string][]byte{"low/first": []byte("lower first")}},
		{Entries: []ipt.Entry{
			ipt.File("disk/second", []byte("second")),
			ipt.File("disk/first", []byte("first")),
			ipt.File("up/second", []byte("upper second")),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	tests := []struct {
		names      []string
		want, name string
	}{
		{[]string{"missing", "disk/first"}, "first", "disk/first"},
		// Within a layer, the first name argument wins, regardless of tar
		// entry order.
		{[]string{"disk/first", "disk/second"}, "first", "disk/first"},
		{[]string{"disk/second", "disk/first"}, "second", "disk/second"},
		// An upper layer wins over a lower layer regardless of name order.
		{[]string{"low/first", "up/second"}, "upper second", "up/second"},
	}
	for _, tt := range tests {
		data, name := readFile(t, img, tt.names...)
		if string(data) != tt.want || name != tt.name {
			t.Errorf("ReadFile(%v) = %q (%s), want %q (%s)", tt.names, data, name, tt.want, tt.name)
		}
	}

	_, err := img.Open(t.Context(), "a", "b")
	if !errors.Is(err, imagepull.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if _, err := img.Open(t.Context()); err == nil {
		t.Fatal("Open with no names should fail")
	}
}

func TestLinks(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{
		{Entries: []ipt.Entry{
			ipt.Dir("disk/"),
			ipt.File("disk/vmlinuz-6.1", []byte("kernel")),
			ipt.Symlink("disk/vmlinuz", "vmlinuz-6.1"),
			ipt.Symlink("disk/abs", "/disk/vmlinuz-6.1"),
			ipt.Hardlink("disk/hard", "disk/vmlinuz-6.1"),
			ipt.Symlink("disk/chain", "vmlinuz"),
			ipt.Symlink("disk/loop", "loop"),
			ipt.Symlink("disk/dangling", "nothing"),
		}},
	}})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	for _, n := range []string{"disk/vmlinuz", "disk/abs", "disk/hard", "disk/chain"} {
		data, name := readFile(t, img, n)
		if string(data) != "kernel" || name != n {
			t.Errorf("ReadFile(%s) = %q (%s)", n, data, name)
		}
	}

	if _, _, err := img.ReadFile(t.Context(), "disk/loop"); err == nil || errors.Is(err, imagepull.ErrNotFound) {
		t.Errorf("loop: err = %v, want link depth error", err)
	}

	if _, _, err := img.ReadFile(t.Context(), "disk/dangling"); !errors.Is(err, imagepull.ErrNotFound) {
		t.Errorf("dangling: err = %v, want ErrNotFound", err)
	}

	if _, _, err := img.ReadFile(t.Context(), "disk"); err == nil || errors.Is(err, imagepull.ErrNotFound) {
		t.Errorf("dir: err = %v, want not-a-regular-file error", err)
	}
}

// concurrencyTracker records the maximum number of concurrent blob requests.
type concurrencyTracker struct {
	cur, max atomic.Int32
	delay    time.Duration
}

func (c *concurrencyTracker) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			next.ServeHTTP(w, r)
			return
		}

		n := c.cur.Add(1)
		defer c.cur.Add(-1)

		for {
			m := c.max.Load()
			if n <= m || c.max.CompareAndSwap(m, n) {
				break
			}
		}

		time.Sleep(c.delay)
		next.ServeHTTP(w, r)
	})
}

func pushBigUncompressed(t *testing.T, reg *ipt.Registry, size int) (string, []byte, string) {
	t.Helper()

	content := randomBytes(size, 7)
	ref, dgst := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
		Files:       map[string][]byte{"disk/big": content, "disk/z-after": []byte("after")},
		Compression: ipt.Uncompressed,
	}}})

	layers := reg.LayerDigests(dgst)
	if len(layers) != 1 {
		t.Fatalf("layers = %v", layers)
	}

	return ref, content, layers[0]
}

func TestRangedConcurrentDownload(t *testing.T) {
	tracker := &concurrencyTracker{delay: 20 * time.Millisecond}
	reg := ipt.NewRegistry(t, ipt.WithMiddleware(tracker.middleware))
	ref, content, layer := pushBigUncompressed(t, reg, 10*1024)

	p := &imagepull.Puller{ChunkSize: 1024, Concurrency: 3}
	img := resolve(t, p, imagepull.Source{Ref: ref})

	// "disk/z-after" is the last entry, so the whole blob is read.
	if data, _ := readFile(t, img, "disk/z-after"); string(data) != "after" {
		t.Fatalf("z-after = %q", data)
	}

	reqs := reg.BlobRequests(layer)
	if len(reqs) < 10 {
		t.Fatalf("blob requests = %d, want one per 1 KiB chunk", len(reqs))
	}

	for _, r := range reqs {
		if r.Range == "" {
			t.Fatalf("blob request without Range: %+v", r)
		}
	}

	if got := tracker.max.Load(); got < 2 || got > 3 {
		t.Fatalf("max concurrent blob requests = %d, want 2..3", got)
	}

	reg.ResetRequests()

	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}
}

func TestSmallBlobSingleRequest(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, dgst := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/a": []byte("a")})

	img := resolve(t, &imagepull.Puller{ChunkSize: 1 << 20}, imagepull.Source{Ref: ref})
	readFile(t, img, "disk/a")

	// One plain GET builds the layer index, one serves the file.
	reqs := reg.BlobRequests(reg.LayerDigests(dgst)[0])
	if len(reqs) != 2 || reqs[0].Range != "" || reqs[1].Range != "" {
		t.Fatalf("blob requests = %+v, want two plain GETs", reqs)
	}
}

func TestNoRangeFallback(t *testing.T) {
	reg := ipt.NewRegistry(t, ipt.WithoutRange())
	ref, content, layer := pushBigUncompressed(t, reg, 10*1024)

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024, Concurrency: 4}, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if _, _, err := img.ReadFile(t.Context(), "missing"); !errors.Is(err, imagepull.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	// One GET builds the index and one serves the file; the missing file
	// is answered from the index.
	if n := len(reg.BlobRequests(layer)); n != 2 {
		t.Fatalf("blob requests = %d, want 2", n)
	}
}

func TestBlobRedirectKeepsRange(t *testing.T) {
	reg := ipt.NewRegistry(t, ipt.WithBlobRedirect(), ipt.WithBasicAuth("u", "p"))
	ref, content, layer := pushBigUncompressed(t, reg, 6*1024)

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{
		Ref: ref, Credential: auth.Credential{Username: "u", Password: "p"},
	})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	cdn := 0

	for _, r := range reg.BlobRequests(layer) {
		if r.CDN {
			cdn++

			if r.Range == "" {
				t.Fatalf("CDN request lost Range: %+v", r)
			}
		}
	}

	if cdn < 6 {
		t.Fatalf("CDN requests = %d, want >= 6", cdn)
	}
}

// rewriteContentRange replaces the Content-Range of 206 responses for which
// match returns true.
func rewriteContentRange(match func(rng string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if match(r.Header.Get("Range")) {
				w = &crWriter{ResponseWriter: w}
			}

			next.ServeHTTP(w, r)
		})
	}
}

type crWriter struct{ http.ResponseWriter }

func (c *crWriter) WriteHeader(code int) {
	if code == http.StatusPartialContent {
		c.Header().Set("Content-Range", "bytes 1-2/3")
	}

	c.ResponseWriter.WriteHeader(code)
}

func TestBadContentRangeOnFirstChunkFallsBack(t *testing.T) {
	reg := ipt.NewRegistry(t, ipt.WithMiddleware(rewriteContentRange(func(rng string) bool {
		return strings.HasPrefix(rng, "bytes=0-")
	})))
	ref, content, layer := pushBigUncompressed(t, reg, 4*1024)

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	// Index build and file read each issue a ranged then a plain request.
	reqs := reg.BlobRequests(layer)
	if len(reqs) != 4 || reqs[0].Range == "" || reqs[1].Range != "" || reqs[2].Range == "" || reqs[3].Range != "" {
		t.Fatalf("blob requests = %+v, want ranged then plain, twice", reqs)
	}
}

func TestBadContentRangeOnLaterChunkFails(t *testing.T) {
	reg := ipt.NewRegistry(t, ipt.WithMiddleware(rewriteContentRange(func(rng string) bool {
		return rng != "" && !strings.HasPrefix(rng, "bytes=0-")
	})))
	ref, _, _ := pushBigUncompressed(t, reg, 4*1024)

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})

	_, _, err := img.ReadFile(t.Context(), "disk/big")
	if err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("err = %v, want Content-Range error", err)
	}
}

func TestDigestMismatch(t *testing.T) {
	for name, opts := range map[string][]ipt.Option{"ranged": nil, "sequential": {ipt.WithoutRange()}} {
		t.Run(name, func(t *testing.T) {
			reg := ipt.NewRegistry(t, opts...)
			ref, _, layer := pushBigUncompressed(t, reg, 4*1024)

			var blob []byte

			for _, req := range []string{layer} {
				resp, err := http.Get(reg.URL() + "/v2/img/blobs/" + req) //nolint:noctx // test
				if err != nil {
					t.Fatal(err)
				}

				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)
				_ = resp.Body.Close()
				blob = buf.Bytes()
			}

			// Flip a byte in the file content; the tar stays well formed.
			i := bytes.LastIndex(blob, []byte("after"))
			blob[i] = 'A'
			reg.SetBlob(layer, blob)

			img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})

			_, _, err := img.ReadFile(t.Context(), "missing")
			if !errors.Is(err, imagepull.ErrDigestMismatch) {
				t.Fatalf("err = %v, want ErrDigestMismatch", err)
			}
		})
	}
}

func blobReaderGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	return strings.Count(string(buf[:n]), "imagepull.(*blobReader)")
}

func TestEarlyCloseCancelsInFlight(t *testing.T) {
	tracker := &concurrencyTracker{delay: 200 * time.Millisecond}
	reg := ipt.NewRegistry(t, ipt.WithMiddleware(tracker.middleware))
	content := randomBytes(64*1024, 3)
	ref, _ := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
		Entries:     []ipt.Entry{ipt.File("disk/first", []byte("first")), ipt.File("disk/big", content)},
		Compression: ipt.Uncompressed,
	}}})

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024, Concurrency: 4}, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/big")
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4096)
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}

	if blobReaderGoroutines() == 0 {
		t.Fatal("expected in-flight chunk goroutines before Close")
	}

	start := time.Now()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("Close took %v, want prompt cancellation", d)
	}

	// Canceled readers unwind asynchronously (for example a background layer
	// index scan), so allow a short grace period before declaring a leak.
	deadline := time.Now().Add(2 * time.Second)
	for blobReaderGoroutines() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if n := blobReaderGoroutines(); n != 0 {
		t.Fatalf("%d blob reader goroutines left after Close", n)
	}

	if _, err := f.Read(buf); err == nil {
		t.Fatal("Read after Close should fail")
	}
}

func TestFileServeContent(t *testing.T) {
	reg := ipt.NewRegistry(t)
	content := randomBytes(5000, 9)
	ref, dgst := reg.Push(t, "img", "v1", ipt.ImageSpec{Layers: []ipt.Layer{{
		Files: map[string][]byte{"disk/file": content},
	}}})
	layer := reg.LayerDigests(dgst)[0]

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

	f, err := img.Open(t.Context(), "disk/file")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test

	if f.Size() != 5000 || f.Name() != "disk/file" || f.ModTime().IsZero() {
		t.Fatalf("Size=%d Name=%s ModTime=%v", f.Size(), f.Name(), f.ModTime())
	}

	serve := func(rng string) []byte {
		req := httptest.NewRequest(http.MethodGet, "/file", nil)
		req.Header.Set("Range", rng)

		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(rec, req, f.Name(), f.ModTime(), f)

		if rec.Code != http.StatusPartialContent {
			t.Fatalf("Range %s: status %d", rng, rec.Code)
		}

		return rec.Body.Bytes()
	}

	if got := serve("bytes=10-19"); !bytes.Equal(got, content[10:20]) {
		t.Fatal("bytes=10-19 mismatch")
	}

	reg.ResetRequests()

	if got := serve("bytes=4000-4009"); !bytes.Equal(got, content[4000:4010]) {
		t.Fatal("bytes=4000-4009 mismatch")
	}

	if got := serve("bytes=-10"); !bytes.Equal(got, content[4990:]) {
		t.Fatal("suffix mismatch")
	}

	// Forward reads reuse the open stream.
	if n := len(reg.BlobRequests(layer)); n != 0 {
		t.Fatalf("blob requests after forward reads = %d, want 0", n)
	}

	// A backwards read re-opens the layer.
	if got := serve("bytes=0-9"); !bytes.Equal(got, content[:10]) {
		t.Fatal("bytes=0-9 mismatch")
	}

	if n := len(reg.BlobRequests(layer)); n != 1 {
		t.Fatalf("blob requests after backwards read = %d, want 1", n)
	}

	// Full read through io.Copy-style reads after a seek.
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	all := new(bytes.Buffer)
	if _, err := all.ReadFrom(f); err != nil || !bytes.Equal(all.Bytes(), content) {
		t.Fatalf("full read: err=%v equal=%v", err, bytes.Equal(all.Bytes(), content))
	}

	if _, err := f.Seek(-1, 0); err == nil {
		t.Fatal("negative seek should fail")
	}
}

func TestBasicAuth(t *testing.T) {
	reg := ipt.NewRegistry(t, ipt.WithBasicAuth("user", "secret"))
	ref, _ := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/a": []byte("a")})

	img := resolve(t, &imagepull.Puller{}, imagepull.Source{
		Ref: ref, Credential: auth.Credential{Username: "user", Password: "secret"},
	})
	if got := img.Source(); got.Credential != auth.EmptyCredential || got.Ref != ref {
		t.Fatalf("Source() = %+v, want Ref %s and no credential", got, ref)
	}

	// Reads after Resolve, including a re-open, still authenticate.
	for range 2 {
		if data, _ := readFile(t, img, "disk/a"); string(data) != "a" {
			t.Fatalf("data = %q", data)
		}
	}

	_, err := (&imagepull.Puller{}).Resolve(t.Context(), imagepull.Source{Ref: ref})
	if err == nil || errors.Is(err, imagepull.ErrImageNotFound) {
		t.Fatalf("anonymous: err = %v, want unauthorized", err)
	}

	// oras reports the 401 basic challenge it cannot answer.
	if !strings.Contains(err.Error(), "basic credential not found") {
		t.Fatalf("anonymous: err = %v, want missing credential error", err)
	}

	_, err = (&imagepull.Puller{}).Resolve(t.Context(), imagepull.Source{
		Ref: ref, Credential: auth.Credential{Username: "user", Password: "wrong"},
	})
	if err == nil {
		t.Fatal("wrong password: expected error")
	}
}

func TestContextCanceled(t *testing.T) {
	reg := ipt.NewRegistry(t)
	ref, _ := reg.PushFiles(t, "img", "v1", map[string][]byte{"disk/a": []byte("a")})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := (&imagepull.Puller{}).Resolve(ctx, imagepull.Source{Ref: ref}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestInvalidMirror(t *testing.T) {
	for _, m := range []string{"ftp://x", "http://", "http://host/path", "::"} {
		p := &imagepull.Puller{Mirror: m}
		if _, err := p.Resolve(t.Context(), imagepull.Source{Ref: "example.com/a:b"}); err == nil {
			t.Errorf("mirror %q: expected error", m)
		}
	}
}
