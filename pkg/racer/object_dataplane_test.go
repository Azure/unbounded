// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestObjectDataplaneInterop(t *testing.T) {
	dataplane, object := os.Getenv("RACER_DATAPLANE_BINARY"), os.Getenv("RACER_OBJECT_BINARY")
	if dataplane == "" || object == "" {
		t.Skip("set RACER_DATAPLANE_BINARY and RACER_OBJECT_BINARY")
	}

	data := payload(int(PageSize) + 123)

	var heads, gets atomic.Int64

	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/weights/model.safetensors" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("ETag", `"azure-version"`)
		w.Header().Set("x-ms-blob-type", "BlockBlob")

		if r.Method == "HEAD" {
			heads.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))

			return
		}

		gets.Add(1)

		if r.Header.Get("If-Match") != `"azure-version"` {
			t.Error("missing Azure version pin")
			w.WriteHeader(412)

			return
		}

		var first, last int
		if _, err := fmt.Sscanf(r.Header.Get("x-ms-range"), "bytes=%d-%d", &first, &last); err != nil {
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); err != nil {
				t.Error(err)
				w.WriteHeader(400)

				return
			}
		}

		if first < 0 || last < first || last >= len(data) {
			w.WriteHeader(416)
			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(last-first+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(data)))
		w.WriteHeader(206)
		_, _ = w.Write(data[first : last+1])
	}))
	defer azure.Close()

	dir := t.TempDir()
	sockets := socketDirectory(t)
	cache, origin := filepath.Join(sockets, "cache"), filepath.Join(sockets, "origin")
	mapping := map[string]any{"azure_endpoint": azure.URL, "objects": []any{map[string]string{"bucket": "models", "key": "model.safetensors", "container": "weights", "blob": "model.safetensors"}}}
	writeJSON := func(name string, value any) string {
		t.Helper()

		wire, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, wire, 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}
	config := writeJSON("objects.json", mapping)
	start := func(name string, args, env []string) {
		t.Helper()

		log, err := os.Create(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		cmd.Stdout = log

		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}

		exit := make(chan error, 1)

		go func() { exit <- cmd.Wait() }()

		t.Cleanup(func() {
			_ = cmd.Process.Signal(os.Interrupt)

			select {
			case <-exit:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()

				<-exit
			}

			log.Close()

			if t.Failed() {
				body, _ := os.ReadFile(log.Name())
				t.Logf("%s:\n%s", name, body)
			}
		})
	}
	start("origin", []string{object, "backend", "--config", config, "--socket", origin, "--azure-auth", "anonymous"}, os.Environ())

	waitSocket := func(path string) {
		t.Helper()

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return
			}

			time.Sleep(50 * time.Millisecond)
		}

		t.Fatalf("socket unavailable: %s", path)
	}
	waitSocket(origin)
	writeJSON("config.json", map[string]any{"snapshot": map[string]any{
		"universe": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), "node": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), "revision": "1", "epoch": "1",
		"volumes": []any{map[string]any{"id": "object", "cacheSocket": cache, "originSocket": origin, "cacheGeneration": "1", "peerEndpoints": map[string]any{}, "topology": map[string]any{"epoch": "1", "slotCount": 1, "localSlots": []int{0}}}},
	}})

	var env []string

	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "RACER_") {
			env = append(env, v)
		}
	}

	env = append(env, dataplaneEnrollment(t, dir)...)
	env = append(env, "RACER_UNIVERSE="+strings.Repeat("01", 32), "RACER_NODE="+strings.Repeat("02", 32), "RACER_SLAB_PATH="+filepath.Join(dir, "cache.slab"), "RACER_SLAB_SIZE=536870912", "RACER_SHARDS=1", "RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8", "RACER_METRICS_ADDR=127.0.0.1:0", "RACER_RDMA_MODE=disabled")
	start("dataplane", []string{dataplane}, env)
	waitSocket(cache)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	listener.Close()
	start("frontend", []string{object, "frontend", "--config", config, "--socket", cache, "--listen", address}, os.Environ())

	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()

	endpoint := "http://" + address + "/models/model.safetensors"
	deadline := time.Now().Add(30 * time.Second)

	for {
		r, err := client.Head(endpoint)
		if err == nil {
			r.Body.Close()

			if r.StatusCode == 200 {
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatal("frontend not ready")
		}

		time.Sleep(100 * time.Millisecond)
	}

	for pass := range 2 {
		r, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(r.Body)
		r.Body.Close()

		if err != nil || r.StatusCode != 200 || !bytes.Equal(body, data) {
			t.Fatalf("pass=%d status=%d bytes=%d err=%v", pass, r.StatusCode, len(body), err)
		}

		if heads.Load() != 1 || gets.Load() != 2 {
			t.Fatalf("pass=%d Azure HEAD=%d GET=%d; expected 1 HEAD and 2 cold pages, no warm fetches", pass, heads.Load(), gets.Load())
		}
	}
}
