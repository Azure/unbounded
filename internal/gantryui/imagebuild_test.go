// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestBuildRequestLimits(t *testing.T) {
	for _, request := range []BuildRequest{{1, 1}, {20, 101}, {100, 1}, {100, 1000}, {1, 100_000}} {
		if err := request.Validate(); err != nil {
			t.Fatalf("valid request %+v: %v", request, err)
		}
	}

	for _, request := range []BuildRequest{{0, 1}, {-1, 1}, {101, 1}, {1, 0}, {1, -1}, {100, 1001}, {1, 100_001}, {1, int(^uint(0) >> 1)}} {
		if err := request.Validate(); err == nil {
			t.Fatalf("accepted out-of-bounds request %+v", request)
		}
	}
}

func TestRandomLayerContainsExactPayloadAndVerifiableDigests(t *testing.T) {
	var first godigest.Digest

	for range 2 {
		var (
			output   bytes.Buffer
			reported int64
		)

		layer, diff, err := writeRandomLayer(context.Background(), &output, 2, payloadMB, rand.Reader, func(n int64) { reported = n })
		if err != nil {
			t.Fatal(err)
		}

		if godigest.FromBytes(output.Bytes()) != layer.Digest || int64(output.Len()) < payloadMB || reported != payloadMB {
			t.Fatal("compressed digest, incompressible size, or progress did not match")
		}

		gz, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
		if err != nil {
			t.Fatal(err)
		}

		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Fatal(err)
		}

		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}

		if godigest.FromBytes(raw) != diff {
			t.Fatal("uncompressed rootfs digest did not match")
		}

		archive := tar.NewReader(bytes.NewReader(raw))

		header, err := archive.Next()
		if err != nil {
			t.Fatal(err)
		}

		payload, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}

		if header.Name != "payload/layer-02.bin" || header.Size != payloadMB || int64(len(payload)) != payloadMB {
			t.Fatalf("wrong payload: %+v, %d bytes", header, len(payload))
		}

		if _, err := archive.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("unexpected additional archive entry: %v", err)
		}

		if first == layer.Digest {
			t.Fatal("successive images reused a random payload")
		}

		first = layer.Digest
	}
}

type rejectedLayerWriter struct{}

func (rejectedLayerWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRandomLayerReportsGenerationFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		out    io.Writer
		source io.Reader
		want   error
	}{
		{"canceled", ctx, io.Discard, rand.Reader, context.Canceled},
		{"short entropy", context.Background(), io.Discard, strings.NewReader("short"), io.EOF},
		{"disk full", context.Background(), rejectedLayerWriter{}, rand.Reader, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := writeRandomLayer(tc.ctx, tc.out, 1, payloadMB, tc.source, func(int64) {})
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("expected generation error %v, got %v", tc.want, err)
			}
		})
	}
}

func TestImageBuildPublishesBeforeStartingPullAndCleansTemporaryLayers(t *testing.T) {
	for _, failure := range []string{"", "push", "workload"} {
		t.Run("failure="+failure, func(t *testing.T) {
			baseLayer := []byte("existing runtime layer")
			baseConfig, _ := json.Marshal(ocispec.Image{Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"}, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []godigest.Digest{godigest.FromBytes(baseLayer)}}})
			baseManifest, _ := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: buildDescriptor(ocispec.MediaTypeImageConfig, baseConfig), Layers: []ocispec.Descriptor{buildDescriptor(ocispec.MediaTypeImageLayer, baseLayer)}})

			var mu sync.Mutex

			blobs := map[string][]byte{}

			var published []byte

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()

				w.Header().Set("Content-Type", "application/octet-stream")

				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v2/base/manifests/"+godigest.FromBytes(baseManifest).String():
					w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
					_, _ = w.Write(baseManifest)
				case r.Method == http.MethodGet && r.URL.Path == "/v2/base/blobs/"+godigest.FromBytes(baseConfig).String():
					_, _ = w.Write(baseConfig)
				case r.Method == http.MethodGet && r.URL.Path == "/v2/base/blobs/"+godigest.FromBytes(baseLayer).String():
					w.Header().Set("Content-Length", strconv.Itoa(len(baseLayer)))
					_, _ = w.Write(baseLayer)
				case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v2/generated/blobs/uploads"):
					w.Header().Set("Location", "/v2/generated/blobs/uploads/session")
					w.WriteHeader(http.StatusAccepted)
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v2/generated/blobs/uploads"):
					body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}

					digest := r.URL.Query().Get("digest")
					if godigest.FromBytes(body).String() != digest {
						http.Error(w, "digest mismatch", http.StatusBadRequest)
						return
					}

					if failure == "push" && digest != godigest.FromBytes(baseLayer).String() {
						http.Error(w, "upload rejected", http.StatusBadRequest)
						return
					}

					blobs[digest] = body
					w.Header().Set("Docker-Content-Digest", digest)
					w.Header().Set("Location", "/v2/generated/blobs/"+digest)
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v2/generated/manifests/"):
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}

					published = body
					w.Header().Set("Docker-Content-Digest", godigest.FromBytes(body).String())
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/generated/manifests/"):
					w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
					_, _ = w.Write(published)
				case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(r.URL.Path, "/v2/generated/blobs/"):
					blob, ok := blobs[strings.TrimPrefix(r.URL.Path, "/v2/generated/blobs/")]
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						return
					}

					w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
					_, _ = w.Write(blob)
				default:
					http.Error(w, fmt.Sprintf("unexpected registry request: %s %s", r.Method, r.URL), http.StatusBadRequest)
				}
			}))
			defer server.Close()

			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			registry := &Registry{host: u.Host, architecture: "amd64", client: &auth.Client{Client: server.Client()}}
			spec := BuildSpec{BuildRequest: BuildRequest{2, 1}, Name: "gantry-build-" + strings.Repeat("a", 20), Registry: u.Host, Repository: "generated", Base: u.Host + "/base@" + godigest.FromBytes(baseManifest).String(), Namespace: "demo", NodePool: "flexpool", Architecture: "amd64"}
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "daemonsets", func(k8stesting.Action) (bool, runtime.Object, error) {
				mu.Lock()
				defer mu.Unlock()

				if len(published) == 0 {
					t.Error("workload started before manifest publication")
				}

				if failure == "workload" {
					return true, nil, errors.New("workload creation denied")
				}

				return false, nil, nil
			})

			directory := t.TempDir()

			var progress []BuildProgress

			err = RunImageBuild(context.Background(), client, registry, spec, directory, func(p BuildProgress) { progress = append(progress, p) })
			if (err != nil) != (failure != "") {
				t.Fatalf("build failure=%q: %v", failure, err)
			}

			files, readErr := os.ReadDir(directory)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("temporary payloads retained: %v, %v", files, readErr)
			}

			workloads, listErr := client.AppsV1().DaemonSets("demo").List(context.Background(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatal(listErr)
			}

			if failure != "" {
				if len(workloads.Items) != 0 {
					t.Fatal("failed build launched a workload")
				}

				last := progress[len(progress)-1]
				if last.Stage != "failed" || last.Message != err.Error() || strings.Contains(last.Message, "inspect the build Job logs") {
					t.Fatalf("build failure did not report its actual cause: %+v", last)
				}

				return
			}

			if len(workloads.Items) != 1 || workloads.Items[0].Labels[buildLabel] != spec.Name || progress[len(progress)-1].Stage != "complete" {
				t.Fatal("published build did not launch its owned workload")
			}

			mu.Lock()
			defer mu.Unlock()

			var manifest ocispec.Manifest
			if err := json.Unmarshal(published, &manifest); err != nil {
				t.Fatal(err)
			}

			var config ocispec.Image
			if err := json.Unmarshal(blobs[manifest.Config.Digest.String()], &config); err != nil {
				t.Fatal(err)
			}

			if len(manifest.Layers) != 3 || len(config.RootFS.DiffIDs) != 3 || len(config.History) != 0 || config.Config.Cmd[0] != "/bin/sh" {
				t.Fatalf("invalid generated image configuration: %+v", config)
			}

			if got := workloads.Items[0].Spec.Template.Spec.Containers[0].Image; got != u.Host+"/generated@"+godigest.FromBytes(published).String() {
				t.Fatalf("pull was not pinned to published digest: %s", got)
			}
		})
	}
}
