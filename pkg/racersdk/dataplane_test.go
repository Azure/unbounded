// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in real-daemon interoperability, with no Rust/toolchain dependency in the
// normal Go test suite. A configured but unusable binary is a failure, not a skip.
func TestDataplaneInterop(t *testing.T) {
	binary := os.Getenv("RACER_DATAPLANE_BINARY")
	if binary == "" {
		t.Skip("set RACER_DATAPLANE_BINARY to test the Rust dataplane")
	}

	data := payload(int(2*PageSize + 123))

	originData := make([]byte, MaxOriginDataBytes)
	for i := range originData {
		originData[i] = byte(i)
	}

	store := &memoryStore{data: data, meta: Metadata{Size: int64(len(data)), ETag: checksumTag(data), TTL: durationPointer(time.Hour)}}
	origin, _ := NewOrigin(conformanceStore{sdk: store})

	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/sdk%2Fblob?b=2&a=1&a=3" {
			got, status := decodeOriginData(r.Header)
			if status != 0 || !bytes.Equal(got, originData) {
				t.Error("binary origin data changed across dataplane")
			}
		}

		if r.Header.Get("Accept-Encoding") != "identity" || len(r.Header.Values("X-Racer-Target")) != 0 {
			// Direct conformance deliberately supplies a conflicting legacy header.
			// Only the SDK object's requests exclusively originate at the dataplane.
			if r.RequestURI == "/sdk%2Fblob?b=2&a=1&a=3" {
				t.Error("invalid upstream headers")
			}
		}

		switch r.RequestURI {
		case "/encoded":
			w.Header().Set("Content-Encoding", "gzip")
		case "/duplicate-encoding":
			w.Header()["Content-Encoding"] = []string{"identity", "identity"}
		case "/identity":
			w.Header().Set("Content-Encoding", "identity")
		}

		origin.ServeHTTP(w, r)
	}))
	defer server.Close()

	metrics, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	metricsAddress := metrics.Addr().String()

	metrics.Close()

	dir := t.TempDir()
	cacheSocket := filepath.Join(socketDirectory(t), "cache")

	tlsEnv := dataplaneEnrollment(t, dir)

	config := map[string]any{"snapshot": map[string]any{
		"universe": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		"node":     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		"revision": "1", "epoch": "1",
		"memberCatalogs": []any{map[string]any{"members": []any{map[string]any{"node": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), "podUid": "sdk"}}}},
		"volumes": []any{map[string]any{
			"id": "sdk", "cacheSocket": cacheSocket, "originSocket": server.Listener.Addr().String(), "cacheGeneration": "1",
			"memberCatalog": 0, "maxCandidateAttempts": 1,
			"peerEndpoints": map[string]any{}, "topology": map[string]any{
				"epoch": "1", "slotCount": 1, "localSlots": []int{0}, "routingAlgorithm": 1,
				"product": map[string]any{"leftFactor": 1, "rightFactor": 1, "members": []string{hex.EncodeToString(bytes.Repeat([]byte{2}, 32))}, "roles": []int{0}, "localMember": 0, "candidateWidth": 1, "candidates": []int{0}},
			},
		}},
	}}

	wire, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, wire, 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "dataplane.log")

	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	cmd := exec.Command(binary)

	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "RACER_") {
			cmd.Env = append(cmd.Env, env)
		}
	}

	// Each distinct conformance object consumes a payload extent, even when its
	// body is short. Leave room beyond the three-page SDK object so protocol
	// assertions do not depend on eviction and checkpoint reclamation timing.
	cmd.Env = append(cmd.Env,
		"RACER_UNIVERSE="+strings.Repeat("01", 32), "RACER_NODE="+strings.Repeat("02", 32),
		"RACER_SLAB_PATH="+filepath.Join(dir, "cache.slab"), "RACER_SLAB_SIZE=2147483648", "RACER_SHARDS=1",
		"RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8",
		"RACER_METRICS_ADDR="+metricsAddress, "RACER_RDMA_MODE=disabled")
	cmd.Env = append(cmd.Env, tlsEnv...)
	cmd.Stdout = log

	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	exit := make(chan error, 1)

	go func() { exit <- cmd.Wait() }()

	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)

		select {
		case <-exit:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()

			<-exit
		}

		if t.Failed() {
			contents, _ := os.ReadFile(logPath)
			t.Logf("dataplane log:\n%s", contents)
		}
	}()

	deadline := time.Now().Add(20 * time.Second)

	readiness := &http.Client{Timeout: time.Second}
	defer readiness.CloseIdleConnections()

	for {
		response, err := readiness.Get("http://" + metricsAddress + "/readyz")
		if err == nil {
			response.Body.Close()

			if response.StatusCode == http.StatusOK {
				break
			}
		}

		select {
		case err := <-exit:
			exit <- err

			t.Fatalf("dataplane exited: %v", err)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatal("dataplane startup timed out")
		}

		time.Sleep(50 * time.Millisecond)
	}

	c, err := NewClient(cacheSocket, ClientOptions{Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	target := "/sdk%2Fblob?b=2&a=1&a=3"

	view, err := c.WithOriginData(originData)
	if err != nil {
		t.Fatal(err)
	}

	object, err := view.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	if store.stats.Load() != 1 || store.opens.Load() != 0 {
		t.Fatal("HEAD faulted pages")
	}

	for i := 0; i < 2; i++ {
		out := make(sliceWriter, len(data))

		n, err := object.Download(ctx, out)
		if err != nil || n != int64(len(data)) || !bytes.Equal(out, data) {
			t.Fatalf("transfer %d: n=%d err=%v", i, n, err)
		}
	}

	if store.opens.Load() != 3 {
		t.Fatalf("cold/warm page fetches: got %d want 3", store.opens.Load())
	}

	stats := store.stats.Load()
	// Cached metadata and pages are reusable without the miss's origin data.
	object, err = c.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]byte, 17)

	n, err := object.ReadAt(ctx, out, PageSize-5)
	if err != nil || n != len(out) || !bytes.Equal(out, data[PageSize-5:PageSize+12]) {
		t.Fatal("cross-page read", n, err)
	}

	if store.stats.Load() != stats || store.opens.Load() != 3 {
		t.Fatal("origin data changed persistent cache identity")
	}

	t.Logf("verified HEAD + 3 cold pages + warm reuse + cross-page ReadAt (%d bytes)", len(data))
	t.Run("direct", func(t *testing.T) { runReadConformance(t, server.Listener.Addr().String()) })
	t.Run("cached", func(t *testing.T) { runReadConformance(t, cacheSocket) })

	for _, target := range []string{"/encoded", "/duplicate-encoding", "/identity"} {
		want := 502
		if target == "/identity" {
			want = 200
		}

		deadline := time.Now().Add(10 * time.Second)

		for {
			r, _ := http.NewRequestWithContext(ctx, "HEAD", "http://localhost"+target, nil)

			resp, err := c.http.Do(r)
			if err != nil {
				t.Fatal(err)
			}

			resp.Body.Close()
			// A rejected representation opens the backend breaker. Allow its
			// cooldown before testing the next independent representation.
			if resp.StatusCode == 503 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if resp.StatusCode != want {
				t.Fatalf("metadata encoding %s: %d want %d", target, resp.StatusCode, want)
			}

			break
		}
	}
}
