// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

// corruptingMirror proxies to upstream like Gantry (ignoring Range) and,
// while corrupt is set, flips one byte of file content in every blob
// response, keeping the length intact.
func corruptingMirror(t *testing.T, upstream *ipt.Registry) (*httptest.Server, *atomic.Bool, *atomic.Int32) {
	t.Helper()

	var (
		corrupt atomic.Bool
		served  atomic.Int32
	)

	corrupt.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.URL()+r.URL.Path, nil)
		if err != nil {
			t.Error(err)
			return
		}

		req.Header.Set("Accept", r.Header.Get("Accept"))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test

		body, _ := io.ReadAll(resp.Body)

		for k, v := range resp.Header {
			w.Header()[k] = v
		}

		if strings.Contains(r.URL.Path, "/blobs/") && resp.StatusCode == http.StatusOK && corrupt.Load() && len(body) > 1024 {
			served.Add(1)

			body[600] ^= 0xff // inside the first file's content
		}

		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv, &corrupt, &served
}

func upstreamBlobReads(reg *ipt.Registry, layer string) int {
	var n int

	for _, r := range reg.BlobRequests(layer) {
		if strings.Contains(r.UserAgent, "unbounded-metalman-imagepull") {
			n++
		}
	}

	return n
}

func TestCorruptMirrorLayerRetriedFromUpstream(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, layer := pushBigUncompressed(t, upstream, 8*1024)
	mirror, _, served := corruptingMirror(t, upstream)

	img := resolve(t, &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024}, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if served.Load() == 0 {
		t.Fatal("mirror never served a corrupted blob")
	}

	if upstreamBlobReads(upstream, layer) == 0 {
		t.Fatal("upstream received no blob request")
	}
}

func TestCorruptMirrorFileRetriedFromUpstream(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, layer := pushBigUncompressed(t, upstream, 8*1024)
	mirror, corrupt, served := corruptingMirror(t, upstream)
	corrupt.Store(false)

	img := resolve(t, &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024}, imagepull.Source{Ref: ref})

	// Build the index from a healthy mirror, then corrupt file reads.
	f, err := img.Open(t.Context(), "disk/big")
	if err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	upstream.ResetRequests()
	corrupt.Store(true)

	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if served.Load() == 0 {
		t.Fatal("mirror never served a corrupted blob")
	}

	if upstreamBlobReads(upstream, layer) == 0 {
		t.Fatal("upstream received no blob request")
	}
}

func TestCorruptMirrorAndUpstream(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, _, layer := pushBigUncompressed(t, upstream, 8*1024)
	mirror, _, _ := corruptingMirror(t, upstream)

	img := resolve(t, &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024}, imagepull.Source{Ref: ref})

	// Corrupt upstream too, at a different byte than the mirror flips.
	data := upstreamBlob(t, upstream, layer)
	data[700] ^= 0xff
	upstream.SetBlob(layer, data)

	_, _, err := img.ReadFile(t.Context(), "disk/big")
	if !errors.Is(err, imagepull.ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}

	if upstreamBlobReads(upstream, layer) == 0 {
		t.Fatal("upstream received no blob request")
	}
}

func upstreamBlob(t *testing.T, reg *ipt.Registry, d string) []byte {
	t.Helper()

	resp, err := http.Get(reg.URL() + "/v2/img/blobs/" + d) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestCorruptMirrorRememberedAcrossResolve(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, layer := pushBigUncompressed(t, upstream, 8*1024)
	mirror, _, served := corruptingMirror(t, upstream)

	p := &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024}

	img := resolve(t, p, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if served.Load() == 0 {
		t.Fatal("mirror never served a corrupted blob")
	}

	if imagepull.QuarantineLen(p) != 1 {
		t.Fatalf("quarantine len = %d, want 1", imagepull.QuarantineLen(p))
	}

	before := served.Load()

	upstream.ResetRequests()

	img2 := resolve(t, p, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img2, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch on second request")
	}

	if got := served.Load(); got != before {
		t.Fatalf("mirror served %d more blob requests, want 0", got-before)
	}

	if upstreamBlobReads(upstream, layer) == 0 {
		t.Fatal("upstream received no blob request")
	}
}

func TestCorruptMirrorQuarantineExpires(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, _ := pushBigUncompressed(t, upstream, 8*1024)
	mirror, _, served := corruptingMirror(t, upstream)

	p := &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024, MirrorQuarantine: time.Millisecond}

	if data, _ := readFile(t, resolve(t, p, imagepull.Source{Ref: ref}), "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	before := served.Load()

	time.Sleep(10 * time.Millisecond)

	_, _ = readFile(t, resolve(t, p, imagepull.Source{Ref: ref}), "disk/big")

	if served.Load() == before {
		t.Fatal("mirror not retried after quarantine expired")
	}
}

func TestCorruptMirrorQuarantineDisabled(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, _ := pushBigUncompressed(t, upstream, 8*1024)
	mirror, _, served := corruptingMirror(t, upstream)

	p := &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024, MirrorQuarantine: -1}

	if data, _ := readFile(t, resolve(t, p, imagepull.Source{Ref: ref}), "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if imagepull.QuarantineLen(p) != 0 {
		t.Fatal("quarantine recorded while disabled")
	}

	before := served.Load()

	_, _ = readFile(t, resolve(t, p, imagepull.Source{Ref: ref}), "disk/big")

	if served.Load() == before {
		t.Fatal("mirror not tried with quarantine disabled")
	}
}

func TestQuarantineEvictsOldest(t *testing.T) {
	p := &imagepull.Puller{}

	for i := range imagepull.MaxQuarantined + 1 {
		imagepull.QuarantineAdd(p, "m", fmt.Sprint(i))
	}

	if got := imagepull.QuarantineLen(p); got != imagepull.MaxQuarantined {
		t.Fatalf("len = %d, want %d", got, imagepull.MaxQuarantined)
	}

	if imagepull.QuarantineHas(p, "m", "0") {
		t.Fatal("oldest entry not evicted")
	}

	if !imagepull.QuarantineHas(p, "m", fmt.Sprint(imagepull.MaxQuarantined)) {
		t.Fatal("newest entry missing")
	}

	if imagepull.QuarantineHas(p, "other", "1") {
		t.Fatal("entry matched a different mirror")
	}
}
