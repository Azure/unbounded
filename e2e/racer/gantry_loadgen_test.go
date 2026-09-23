//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGantryRacerContainerImage(t *testing.T) {
	binary := os.Getenv("RACER_LOADGEN_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("RACER_LOADGEN_BINARY must be an absolute workspace path")
	}

	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}

	if gantryNamespace(t) {
		return
	}

	var registry *gantryProcess

	f := newGantryFixture(t, 1, nil, func(f *gantryFixture) string {
		registry = gantryStart(t, f.dir, "image-registry", nil, binary,
			"-mode=container-image", "-role=registry", "-listen=127.0.0.1:18080", "-registry-listen=127.0.0.1:18081",
			"-footprint=128MiB", "-object-size=64MiB", "-layers-per-image=2")
		gantryAwait(t, "image registry preparation", func() bool {
			r, err := f.client.Get("http://127.0.0.1:18080/readyz")
			if err != nil {
				return false
			}

			r.Body.Close()

			return r.StatusCode == 200
		})

		return "http://127.0.0.1:18081"
	})
	puller := gantryStart(t, f.dir, "image-puller", nil, binary,
		"-mode=container-image", "-role=load", "-listen=127.0.0.1:18082",
		"-registry-url=http://127.0.0.1:18081", "-registry-namespace=fixture.test", "-gantry-endpoint=http://"+f.mirrors[0],
		"-concurrency=1", "-layer-concurrency=2", "-seed=42", "-timeout=10s")
	gantryAwait(t, "image puller readiness", func() bool {
		r, err := f.client.Get("http://127.0.0.1:18082/readyz")
		if err != nil {
			return false
		}

		r.Body.Close()

		return r.StatusCode == 200
	})

	success := `racer_loadgen_image_pulls_total{result="success"}`

	gantryAwait(t, "cold and warm simulated image pulls", func() bool {
		return f.metricAt("127.0.0.1:18082", success) >= 2
	})

	if f.metricAt("127.0.0.1:18082", `racer_loadgen_image_pulls_total{result="error"}`) != 0 {
		t.Fatal("image pulls failed")
	}

	if got := f.metricAt("127.0.0.1:18080", `racer_loadgen_registry_requests_total{kind="blob",request="range"}`); got != 3 {
		t.Fatalf("expected one range each for config and two layers, got %g", got)
	}

	if got := f.metricAt("127.0.0.1:18080", `racer_loadgen_registry_requests_total{kind="manifest",request="range"}`); got != 1 {
		t.Fatalf("expected one manifest range, got %g", got)
	}

	if f.metricAt("127.0.0.1:18080", `racer_loadgen_registry_requests_total{kind="blob",request="full"}`) != 0 {
		t.Fatal("registry payload bypassed Racer ranges")
	}

	registry.stop()

	before := f.metricAt("127.0.0.1:18082", success)

	gantryAwait(t, "warm image pulls with registry offline", func() bool {
		return f.metricAt("127.0.0.1:18082", success) >= before+2
	})

	if f.metricAt("127.0.0.1:18082", `racer_loadgen_image_pulls_total{result="error"}`) != 0 || f.metric(0, "gantry_racer_fallback_total") != 0 {
		t.Fatal("warm image pull failed or bypassed Racer")
	}

	if f.metric(0, `gantry_racer_stream_total{outcome="verified"}`) < 16 || f.metric(0, "gantry_racer_splice_bytes_total") < 128<<20 {
		t.Fatal("missing verified real Racer payload streams")
	}

	puller.stop()
}
