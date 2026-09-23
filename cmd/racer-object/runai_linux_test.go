// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	racer "github.com/Azure/unbounded/pkg/racer"
)

// Opt-in because the pinned CPU vLLM image is large. All container lifetimes are
// bounded by the test context; no cloud credentials or external model are needed.
func TestRunaiExplicitWeights(t *testing.T) {
	image := os.Getenv("RACER_OBJECT_RUNAI_IMAGE")
	if image == "" {
		t.Skip("set RACER_OBJECT_RUNAI_IMAGE to a vLLM 0.29.0 / Run:ai 0.16.1 image")
	}

	slog.SetLogLoggerLevel(slog.LevelDebug)
	defer slog.SetLogLoggerLevel(slog.LevelInfo)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	generate := `import sys, torch
from safetensors.torch import save
sys.stdout.buffer.write(save({"weight": (torch.arange(1024*1024)%97).float().reshape(1024,1024)/16, "bias": torch.arange(1024).float()/16}))`

	data, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=host", "--entrypoint=python3", image, "-c", generate).Output()
	if err != nil {
		t.Fatal(err)
	}

	c := testConfig(t)
	cloud := &memoryCloud{data: data}

	origin, err := racer.NewOrigin(newBackend(c, cloud, 4))
	if err != nil {
		t.Fatal(err)
	}

	endpoint, _ := startFrontend(t, c, origin)

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	for _, script := range []string{"/repo/e2e/racer/vllm/load.py", "/repo/cmd/racer-object/vllm/test_loader.py"} {
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=host",
			"-v", root+":/repo:ro", "-e", "PYTHONDONTWRITEBYTECODE=1", "-e", "AWS_ENDPOINT_URL="+endpoint, "-e", "RUNAI_STREAMER_S3_ENDPOINT="+endpoint,
			"-e", "AWS_ACCESS_KEY_ID=local", "-e", "AWS_SECRET_ACCESS_KEY=local", "-e", "AWS_DEFAULT_REGION=us-east-1", "-e", "AWS_EC2_METADATA_DISABLED=true",
			"--entrypoint=python3", image, script)

		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, output)
		}

		t.Logf("%s", output)
	}

	if cloud.stats.Load() != 1 || cloud.reads.Load() == 0 {
		t.Fatalf("metadata=%d ranges=%d", cloud.stats.Load(), cloud.reads.Load())
	}
}

// Point at a running real Racer cache configured for the same mapping as the
// production frontend. This gate also works with a live AKS Workload Identity
// backend. Verify byte identity and warm reads without any listing operation.
func TestExternalCache(t *testing.T) {
	socket, config := os.Getenv("RACER_OBJECT_CACHE_SOCKET"), os.Getenv("RACER_OBJECT_CONFIG")
	if socket == "" || config == "" {
		t.Skip("set RACER_OBJECT_CACHE_SOCKET and RACER_OBJECT_CONFIG")
	}

	c, err := loadConfiguration(config)
	if err != nil {
		t.Fatal(err)
	}

	tl, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	f := newFrontend(c, socket, 8, time.Minute)
	done := make(chan error, 1)

	go func() { done <- f.serve(ctx, tl) }()

	defer func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	testExternalObject(t, "http://"+tl.Addr().String(), c.Objects[0])

	if f.bytes.Load() == 0 {
		t.Fatal("no payload spliced")
	}
}

// Keep the external test's URL construction identical to a path-style S3 client.
func objectURL(endpoint string, o objectSpec) string {
	u, _ := url.Parse(endpoint)
	u.Path = "/" + o.Bucket + "/" + o.Key

	return u.String()
}

func testExternalObject(t *testing.T, endpoint string, o objectSpec) {
	t.Helper()

	client := &http.Client{Timeout: time.Minute}
	defer client.CloseIdleConnections()

	var first string

	for pass := range 2 {
		resp, err := client.Get(objectURL(endpoint, o))
		if err != nil {
			t.Fatal(err)
		}

		h := sha256.New()
		n, err := io.Copy(h, resp.Body)
		resp.Body.Close()

		if err != nil || resp.StatusCode != 200 || n != resp.ContentLength {
			t.Fatalf("pass=%d status=%d bytes=%d err=%v", pass, resp.StatusCode, n, err)
		}

		digest := fmt.Sprintf("%x", h.Sum(nil))
		if pass == 0 {
			first = digest
		} else if digest != first {
			t.Fatal("cold/warm bytes differ")
		}

		if expected := os.Getenv("RACER_OBJECT_SHA256"); expected != "" && digest != expected {
			t.Fatalf("unexpected digest %s", digest)
		}

		t.Logf("pass=%d bytes=%d sha256=%s", pass, n, digest)
	}
}
