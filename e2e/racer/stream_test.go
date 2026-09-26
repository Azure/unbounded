//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRetainedKindStream uses only an explicitly selected local kind cluster.
// It updates the fixture origin and restarts the test Gantry DaemonSet.
func TestRetainedKindStream(t *testing.T) {
	kubeconfig := os.Getenv("RACER_E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set RACER_E2E_KUBECONFIG to a retained racer e2e kind kubeconfig")
	}

	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	artifacts, err := os.MkdirTemp(filepath.Join(root, "tmp"), "racer-stream-")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, root: root, artifacts: artifacts, kubeconfig: kubeconfig}
	current := strings.TrimSpace(h.kubectl("config", "current-context"))
	require.True(t, strings.HasPrefix(current, "kind-racer-e2e-"), "requires a local racer e2e kind context: %s", current)
	t.Logf("artifacts: %s", artifacts)
	t.Cleanup(h.diagnostics)
	fixture := newImage(t)

	var networks []struct {
		IPAM struct{ Config []struct{ Gateway string } }
	}
	require.NoError(t, json.Unmarshal([]byte(h.run("docker", "network", "inspect", "kind")), &networks))

	var gateway string

	for _, config := range networks[0].IPAM.Config {
		if net.ParseIP(config.Gateway).To4() != nil {
			gateway = config.Gateway
			break
		}
	}

	require.NotEmpty(t, gateway)
	origin := "http://" + net.JoinHostPort(gateway, h.serve(fixture.handler()))
	h.apply(fmt.Sprintf(gantryManifest, origin))
	h.kubectl("rollout", "restart", "daemonset/gantry-racer-e2e", "-n", namespace)
	h.kubectl("rollout", "status", "daemonset/gantry-racer-e2e", "-n", namespace, "--timeout=90s")
	pod := strings.TrimSpace(h.kubectl("get", "pod", "-n", namespace, "-l", "app=gantry-racer-e2e", "-o", "jsonpath={.items[0].metadata.name}"))
	mirror := h.forward(pod, "5000", "/v2/")
	client := &http.Client{Timeout: 90 * time.Second}

	for attempt := range 3 {
		for id, blob := range fixture.blobs {
			kind := "blobs"
			if id == fixture.manifest {
				kind = "manifests"
			}

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, mirror+"/v2/fixture/image/"+kind+"/"+id+"?ns="+registry, nil)
			require.NoError(t, err)
			response, err := client.Do(request)
			require.NoError(t, err)

			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			t.Logf("attempt=%d status=%d expected=%d actual=%d digest=%s error=%v", attempt+1, response.StatusCode, len(blob.body), len(body), digest(body), readErr)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Equal(t, "1", response.Header.Get("Gantry-Mirrored"))
			require.NoError(t, readErr)
			require.Len(t, body, len(blob.body))
			require.Equal(t, id, digest(body))
		}
	}

	concurrency := 4
	if value := os.Getenv("RACER_E2E_STREAM_CONCURRENCY"); value != "" {
		concurrency, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.Greater(t, concurrency, 0)
		require.LessOrEqual(t, concurrency, 64)
	}

	for id, blob := range fixture.blobs {
		if len(blob.body) <= 64<<20 {
			continue
		}

		t.Run("concurrent-layer", func(t *testing.T) {
			for reader := range concurrency {
				t.Run(strconv.Itoa(reader), func(t *testing.T) {
					t.Parallel()

					request, err := http.NewRequestWithContext(ctx, http.MethodGet, mirror+"/v2/fixture/image/blobs/"+id+"?ns="+registry, nil)
					require.NoError(t, err)
					response, err := client.Do(request)
					require.NoError(t, err)

					defer response.Body.Close()

					require.Equal(t, http.StatusOK, response.StatusCode)
					require.Equal(t, "1", response.Header.Get("Gantry-Mirrored"))

					hash := sha256.New()
					n, err := io.Copy(hash, response.Body)
					require.NoError(t, err, "received %d of %d bytes", n, len(blob.body))
					require.Equal(t, int64(len(blob.body)), n)
					require.Equal(t, id, fmt.Sprintf("sha256:%x", hash.Sum(nil)))
				})
			}
		})
	}
}
