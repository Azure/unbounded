//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	sdk "github.com/Azure/unbounded/pkg/racer"
)

// Roll the actual agent processes one node at a time while retaining containerd,
// the node identity, YAML configuration and Racer slabs. The operator/API test
// separately checks annotation selection and the pod template that drives these
// restarts; this fixture checks normal containerd pulls during mixed backends.
func TestGantryRacerRollingSwitch(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	f := newGantryFixture(t, 2, nil)
	backends := []string{"racer", "racer"}
	restart := func(node int, backend, stage string) {
		t.Helper()
		f.agents[node].stop()
		// No chair API is used by this native fixture. Bound empty-DHT waits so
		// direct origin reads fit its external deadline; chair RBAC is covered
		// by the real-API operator transition test.
		f.agents[node] = gantryStart(t, f.dir, fmt.Sprintf("gantry-%s-%d", stage, node),
			[]string{"SSL_CERT_FILE=" + filepath.Join(f.dir, fmt.Sprintf("registry-%d.pem", node))},
			os.Getenv("GANTRY_BINARY"), "agent", "--config", f.agentConfigs[node],
			fmt.Sprintf("--node-name=node%d", node),
			"--content-backend="+backend, fmt.Sprintf("--racer-cache-uid=node%d", node),
			"--peer-rediscover-budget=0", "--bootstrap-window=1ms", "--nf5-jitter-base=1ms", "--nf5-jitter-cap=1ms",
			fmt.Sprintf("--transfer-listen=127.0.0.1:%d", 18000+node),
			fmt.Sprintf("--chair-listen=127.0.0.1:%d", 19000+node))
		t.Cleanup(func() {
			if !t.Failed() || backend != "racer" {
				return
			}

			for _, socket := range []string{"origin", "cache"} {
				transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/run/racer/node%d/%s", node, socket))
				}}
				probe := &http.Client{Transport: transport, Timeout: time.Second}

				method, target := http.MethodHead, fmt.Sprintf("http://localhost/gantry-readiness?node=node%d", node)
				if socket == "cache" {
					method, target = http.MethodOptions, "http://localhost/"
				}

				req, err := http.NewRequestWithContext(context.Background(), method, target, nil)
				if err != nil {
					t.Error(err)
					continue
				}

				resp, err := probe.Do(req)
				if err != nil {
					t.Logf("node%d %s readiness probe: %v", node, socket, err)
				} else {
					t.Logf("node%d %s readiness probe: %s", node, socket, resp.Status)
					resp.Body.Close()
				}

				transport.CloseIdleConnections()
			}
		})
		gantryAwait(t, "restarted "+backend+" agent", func() bool {
			resp, err := f.client.Get("http://" + f.metrics[node] + "/readyz")
			if err != nil {
				return false
			}

			resp.Body.Close()

			return resp.StatusCode == http.StatusOK
		})
		// Direct mode releases its mirror startup gate on a separate ticker
		// after /readyz succeeds. Wait for that gate rather than retrying Pull.
		gantryAwait(t, "restarted mirror startup gate", func() bool {
			resp, _, err := f.request(node, http.MethodGet, "/v2/", "", "")
			return err == nil && resp.StatusCode == http.StatusOK
		})

		backends[node] = backend
	}
	verify := func(stage string, allRacer bool) {
		t.Helper()

		for node := range 2 {
			// A distinct digest at every stage forces an actual downstream fetch;
			// an image already committed in containerd would hide a broken switch.
			data := bytes.Repeat([]byte(fmt.Sprintf("%s-node%d-", stage, node)), int(sdk.PageSize)/len(stage)+123)
			layer := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: ocidigest.FromBytes(data), Size: int64(len(data))}
			config := fmt.Appendf(nil, `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]}}`, layer.Digest)
			manifest := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"size":%d,"digest":%q},"layers":[{"mediaType":%q,"size":%d,"digest":%q}]}`,
				ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageConfig, len(config), ocidigest.FromBytes(config), layer.MediaType, layer.Size, layer.Digest)
			objects := map[string]gantryObject{
				gantryPath("public", "blobs", data):         {data: data, mediaType: layer.MediaType},
				gantryPath("public", "blobs", config):       {data: config, mediaType: ocispec.MediaTypeImageConfig},
				gantryPath("public", "manifests", manifest): {data: manifest, mediaType: ocispec.MediaTypeImageManifest},
			}

			f.mu.Lock()
			for path, object := range objects {
				f.objects[path] = object
			}
			f.mu.Unlock()

			before := f.originRequestCount("GET", gantryPath("public", "blobs", data))
			fallback := f.metric(node, "gantry_racer_fallback_total")

			ctx, release, err := f.containerd.WithLease(namespaces.WithNamespace(t.Context(), fmt.Sprintf("switch-%s-%d", stage, node)))
			if err != nil {
				t.Fatal(err)
			}

			resolver := docker.NewResolver(docker.ResolverOptions{Hosts: func(host string) ([]docker.RegistryHost, error) {
				if host != "fixture.test" {
					return nil, fmt.Errorf("unexpected registry %q", host)
				}

				return []docker.RegistryHost{{Client: f.client, Host: f.mirrors[node], Scheme: "http", Path: "/v2", Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve}}, nil
			}})
			ref := "fixture.test/public@" + ocidigest.FromBytes(manifest).String()

			_, err = f.containerd.Pull(ctx, ref, containerd.WithResolver(resolver))
			if err != nil {
				release(ctx)
				t.Fatalf("%s node%d (%s) normal pull: %v", stage, node, backends[node], err)
			}

			for _, payload := range [][]byte{manifest, config, data} {
				stored, err := content.ReadBlob(ctx, f.containerd.ContentStore(), ocispec.Descriptor{Digest: ocidigest.FromBytes(payload), Size: int64(len(payload))})
				if err != nil || !bytes.Equal(stored, payload) {
					t.Fatalf("%s node%d committed content mismatch: %v", stage, node, err)
				}
			}

			release(ctx)

			if f.originRequestCount("GET", gantryPath("public", "blobs", data)) <= before {
				t.Fatal("unique image pull did not fetch the layer")
			}

			if allRacer {
				t.Logf("node%d rollout pull registry fallbacks=%g", node, f.metric(node, "gantry_racer_fallback_total")-fallback)
				// A rolling origin replacement can leave transient failed pooled
				// connections or open breakers. Require the layer to recover to
				// actual Racer serving within a deadline, not permanent fallback.
				gantryAwait(t, "verified Racer layer without registry fallback", func() bool {
					completed := f.metric(node, `gantry_racer_stream_total{outcome="completed"}`)
					fallback := f.metric(node, "gantry_racer_fallback_total")

					resp, body, err := f.request(node, http.MethodGet, gantryPath("public", "blobs", data), "", "")
					if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
						t.Fatalf("post-rollout layer verification failed: response=%v err=%v bytes=%d", resp, err, len(body))
					}

					return f.metric(node, "gantry_racer_fallback_total") == fallback &&
						f.metric(node, `gantry_racer_stream_total{outcome="completed"}`) > completed
				})
			}

			t.Logf("%s node%d backend=%s: verified normal pull and all three committed OCI objects", stage, node, backends[node])
		}
	}

	restart(0, "direct", "initial")
	restart(1, "direct", "initial")
	verify("direct", false)
	restart(0, "racer", "enable")
	verify("mixed-enable", false)
	restart(1, "racer", "enable")
	verify("racer", true)
	restart(0, "direct", "rollback")
	verify("mixed-rollback", false)
	restart(1, "direct", "rollback")
	// Stop both dataplanes to prove the completed rollback no longer needs UDS.
	for _, process := range f.racers {
		process.stop()
	}

	verify("direct-restored", false)
}
