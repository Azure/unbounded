// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	racer "github.com/Azure/unbounded/pkg/racer"
)

func testConfig(t testing.TB) *configuration {
	t.Helper()

	c := &configuration{Endpoint: "https://weights.blob.core.windows.net", Objects: []objectSpec{{Bucket: "models", Key: "model.safetensors", Container: "weights", Blob: "model.safetensors"}}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}

	return c
}

type memoryCloud struct {
	data   []byte
	stats  atomic.Int64
	reads  atomic.Int64
	mu     sync.Mutex
	ranges [][2]int64
}

func (m *memoryCloud) stat(context.Context, objectSpec) (blobMetadata, error) {
	m.stats.Add(1)
	return blobMetadata{int64(len(m.data)), azcore.ETag(`"azure-version"`)}, nil
}

func (m *memoryCloud) read(ctx context.Context, _ objectSpec, _ blobMetadata, p []byte, off int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.reads.Add(1)
	m.mu.Lock()
	m.ranges = append(m.ranges, [2]int64{off, int64(len(p))})
	m.mu.Unlock()
	copy(p, m.data[off:])

	return nil
}

func startFrontend(t testing.TB, c *configuration, handler http.Handler) (string, *frontend) {
	t.Helper()
	// Keep Unix paths short even when the worktree's TMPDIR is long.
	path := filepath.Join(t.TempDir(), "o")

	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}

	go func() { _ = srv.Serve(listener) }()

	t.Cleanup(func() { _ = srv.Close() })

	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	f := newFrontend(c, path, 16, 5*time.Second)
	done := make(chan error, 1)

	go func() { done <- f.serve(ctx, tcp) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	return "http://" + tcp.Addr().String(), f
}

func TestFrontendOriginRangesAndKeepAlive(t *testing.T) {
	c := testConfig(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), int(racer.PageSize+128)/16)
	cloud := &memoryCloud{data: data}

	origin, err := racer.NewOrigin(newBackend(c, cloud, 4))
	if err != nil {
		t.Fatal(err)
	}

	endpoint, frontend := startFrontend(t, c, origin)

	conn, err := net.Dial("tcp", strings.TrimPrefix(endpoint, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	var spliced int64

	for _, tc := range []struct {
		method, rangeValue    string
		start, length, status int
	}{
		{"HEAD", "", 0, 0, 200},
		{"GET", "bytes=0-7", 0, 8, 206},
		{"GET", fmt.Sprintf("bytes=%d-%d", racer.PageSize-8, racer.PageSize+7), int(racer.PageSize) - 8, 16, 206},
		{"GET", "bytes=-16", len(data) - 16, 16, 206},
		{"GET", fmt.Sprintf("bytes=%d-", racer.PageSize), int(racer.PageSize), 128, 206},
		{"GET", "", 0, len(data), 200},
	} {
		r, _ := http.NewRequest(tc.method, endpoint+"/models/model.safetensors", nil)
		if tc.rangeValue != "" {
			r.Header.Set("Range", tc.rangeValue)
		}

		if err := r.Write(conn); err != nil {
			t.Fatal(err)
		}

		response, err := http.ReadResponse(reader, r)
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(response.Body)

		_ = response.Body.Close()
		if err != nil || response.StatusCode != tc.status || !bytes.Equal(body, data[tc.start:tc.start+tc.length]) {
			t.Fatalf("%s %s: status=%d len=%d err=%v", tc.method, tc.rangeValue, response.StatusCode, len(body), err)
		}

		spliced += int64(tc.length)
	}

	if got := frontend.bytes.Load(); got != spliced {
		t.Fatalf("spliced %d, want %d", got, spliced)
	}

	if cloud.stats.Load() != 1 {
		t.Fatalf("metadata calls=%d", cloud.stats.Load())
	}

	if cloud.reads.Load() != 7 {
		t.Fatalf("page downloads=%d, want 7", cloud.reads.Load())
	}
}

func TestFrontendErrors(t *testing.T) {
	c := testConfig(t)
	origin, _ := racer.NewOrigin(newBackend(c, &memoryCloud{data: []byte("payload")}, 2))

	endpoint, _ := startFrontend(t, c, origin)
	for _, tc := range []struct {
		method, path, header, value string
		status                      int
		code                        string
	}{
		{"GET", "/models?list-type=2", "", "", 501, "NotImplemented"},
		{"GET", "/models/missing", "", "", 404, "NoSuchKey"},
		{"PUT", "/models/model.safetensors", "", "", 405, "MethodNotAllowed"},
		{"GET", "/models/model.safetensors", "Range", "bytes=90-", 416, "InvalidRange"},
		{"GET", "/models/model.safetensors", "Range", "bytes=1-2,4-5", 400, "InvalidArgument"},
		{"GET", "/models/model.safetensors", "If-Match", `"different"`, 412, "PreconditionFailed"},
	} {
		r, _ := http.NewRequest(tc.method, endpoint+tc.path, nil)
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}

		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(resp.Body)

		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(body), "<Code>"+tc.code+"</Code>") {
			t.Fatalf("%+v: %d %s", tc, resp.StatusCode, body)
		}
	}
}

func TestHeaderNeverReadsPayload(t *testing.T) {
	for _, header := range []string{"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n", "GET / HTTP/1.1\r\nHost: x\r\n\r\n"} {
		for split := 1; split <= len(header); split++ {
			r := &headerOnlyReader{data: []byte(header), chunk: split}

			got, err := readHeader(r)
			if err != nil || string(got) != header {
				t.Fatalf("chunk %d: %q %v", split, got, err)
			}
		}
	}

	if _, err := readHeader(strings.NewReader(strings.Repeat("x", maxHeaderBytes))); err == nil {
		t.Fatal("accepted oversized header")
	}
}

type headerOnlyReader struct {
	data  []byte
	chunk int
}

func (r *headerOnlyReader) Read(p []byte) (int, error) {
	if len(p) > len(r.data) {
		panic("reader requested payload bytes")
	}

	n := min(len(p), r.chunk)
	copy(p, r.data[:n])
	r.data = r.data[n:]

	return n, nil
}

func TestBackendReadAheadAndAdmission(t *testing.T) {
	c := testConfig(t)
	cloud := &memoryCloud{data: bytes.Repeat([]byte{42}, int(racer.PageSize)+10)}
	b := newBackend(c, cloud, 1)
	o := c.Objects[0]

	source, err := b.Open(context.Background(), o.target, o.etag)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := b.Open(ctx, o.target, o.etag); err == nil {
		t.Fatal("admission ignored cancellation")
	}

	p := make([]byte, 32768)
	for off := int64(0); off < racer.PageSize; off += int64(len(p)) {
		if _, err := source.ReadAt(p, off); err != nil {
			t.Fatal(err)
		}
	}

	if cloud.reads.Load() != 1 {
		t.Fatalf("small reads amplified into %d downloads", cloud.reads.Load())
	}

	n, err := source.ReadAt(p, racer.PageSize)
	if n != 10 || err != io.EOF {
		t.Fatalf("EOF: %d %v", n, err)
	}

	_ = source.Close()
	_ = source.Close()

	if len(b.slots) != 0 || len(b.buffers) != 1 {
		t.Fatal("source leaked admission or buffer")
	}

	if _, err := b.Open(context.Background(), o.target, `"wrong"`); err != racer.ErrVersionChanged {
		t.Fatal(err)
	}
}

func TestAzureRangesAndMetadata(t *testing.T) {
	for _, scenario := range []string{"ok", "truncated", "version", "range", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/weights/model.safetensors" {
					t.Errorf("path %q", r.URL.Path)
				}

				w.Header().Set("ETag", `"azure-version"`)
				w.Header().Set("Content-Length", "7")

				if r.Method == "HEAD" {
					return
				}

				if r.Header.Get("If-Match") != `"azure-version"` || r.Header.Get("x-ms-range") != "bytes=0-6" {
					t.Errorf("headers: %v", r.Header)
				}

				if scenario == "missing" {
					w.WriteHeader(404)
					return
				}

				w.Header().Set("Content-Range", "bytes 0-6/7")

				if scenario == "version" {
					w.Header().Set("ETag", `"changed"`)
				}

				if scenario == "range" {
					w.Header().Set("Content-Range", "bytes 1-7/8")
				}

				w.WriteHeader(206)

				if scenario != "truncated" {
					_, _ = io.WriteString(w, "payload")
				}
			}))
			defer server.Close()

			a, err := azureClient(server.URL, "anonymous", 2)
			if err != nil {
				t.Fatal(err)
			}

			o := testConfig(t).Objects[0]

			m, err := a.stat(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			p := make([]byte, 7)

			err = a.read(ctx, o, m, p, 0)
			if scenario == "ok" {
				if err != nil || string(p) != "payload" {
					t.Fatalf("%q %v", p, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestWorkloadIdentityConfiguration(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "test-client")
	t.Setenv("AZURE_TENANT_ID", "test-tenant")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", filepath.Join(t.TempDir(), "token"))

	if _, err := azureClient("https://weights.blob.core.windows.net", "workload-identity", 2); err != nil {
		t.Fatal(err)
	}

	if err := os.Unsetenv("AZURE_FEDERATED_TOKEN_FILE"); err != nil {
		t.Fatal(err)
	}

	if _, err := azureClient("https://weights.blob.core.windows.net", "workload-identity", 2); err == nil {
		t.Fatal("missing token config must fail, not fall back")
	}
}

func BenchmarkFrontendSplice(b *testing.B) {
	c := testConfig(b)
	cloud := &memoryCloud{data: bytes.Repeat([]byte{7}, 16<<20)}
	origin, _ := racer.NewOrigin(newBackend(c, cloud, 16))
	endpoint, _ := startFrontend(b, c, origin)
	b.SetBytes(int64(len(cloud.data)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r, err := http.Get(endpoint + "/models/model.safetensors")
			if err != nil {
				b.Error(err)
				return
			}

			n, err := io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()

			if err != nil || n != int64(len(cloud.data)) {
				b.Errorf("body %d %v", n, err)
				return
			}
		}
	})
}
