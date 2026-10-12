// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestRacerRegistryContentType(t *testing.T) {
	for _, tc := range []struct {
		name, route, contentType, data string
	}{
		{"OCI manifest", "manifests", "application/vnd.oci.image.manifest.v1+json", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`},
		{"OCI index", "manifests", "application/vnd.oci.image.index.v1+json", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`},
		{"Docker manifest", "manifests", "application/vnd.docker.distribution.manifest.v2+json", `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`},
		{"Docker list", "manifests", "application/vnd.docker.distribution.manifest.list.v2+json", `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`},
		{"parameterized manifest", "manifests", "application/vnd.oci.image.manifest.v1+json; charset=utf-8", `{"schemaVersion":2,"layers":[]}`},
		{"empty manifest", "manifests", "application/vnd.oci.image.manifest.v1+json", ""},
		{"blob", "blobs", "application/custom", "layer bytes"},
		{"empty blob", "blobs", "application/octet-stream", ""},
	} {
		for _, supplied := range []bool{false, true} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(tc.name+"/supplied="+strconv.FormatBool(supplied)+"/"+method, func(t *testing.T) {
					d := racerDigest([]byte(tc.data))

					var heads, gets atomic.Int32

					registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/v2/library/image/"+tc.route+"/"+d.String() {
							t.Errorf("unexpected registry request: %s", r.URL)
							http.NotFound(w, r)

							return
						}
						// A nil header suppresses net/http's automatic content sniffing.
						w.Header()["Content-Type"] = nil
						if supplied {
							w.Header().Set("Content-Type", tc.contentType)
						}

						w.Header().Set("Content-Length", strconv.Itoa(len(tc.data)))
						w.Header().Set("Docker-Content-Digest", d.String())

						if r.Method == http.MethodHead {
							heads.Add(1)
							return
						}

						gets.Add(1)

						if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=0-"+strconv.FormatInt(racersdk.PageSize-1, 10) {
							t.Errorf("unexpected registry read: method=%s range=%q", r.Method, r.Header.Get("Range"))
						}

						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, tc.data)
					}))
					defer registry.Close()

					cfg := racerConfig()
					cfg.UpstreamRegistries[0].Endpoint = registry.URL

					upstream, err := registryorigin.New(cfg)
					if err != nil {
						t.Fatal(err)
					}

					origin := Origin(cfg, upstream)
					client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
						metadata, body, err := origin(ctx, req)

						wantType := ""
						if supplied {
							wantType = tc.contentType
						}

						if err != nil || metadata.ContentType != wantType || metadata.Size != int64(len(tc.data)) {
							t.Errorf("registry metadata=%+v error=%v; want type=%q size=%d", metadata, err, wantType, len(tc.data))
						}

						return metadata, body, err
					})
					server := racerServer(t, cfg, client, &racerLegacyTrap{})
					resp := racerRequest(t, server, method, tc.route, d, "", "")
					body := handlerReadBody(t, resp)

					wantHeads, wantGets := int32(0), int32(1)
					if method == http.MethodHead {
						wantHeads, wantGets = 1, 0
					}

					if heads.Load() != wantHeads || gets.Load() != wantGets {
						t.Fatalf("registry calls: heads=%d gets=%d; want %d %d", heads.Load(), gets.Load(), wantHeads, wantGets)
					}

					if !supplied && tc.route == "manifests" {
						if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" || resp.Header.Get("Docker-Content-Digest") != "" {
							t.Fatalf("unknown manifest type accepted: status=%d headers=%v", resp.StatusCode, resp.Header)
						}

						wantBody := "invalid Racer metadata\n"
						if method == http.MethodHead {
							wantBody = ""
						}

						if string(body) != wantBody || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
							t.Fatalf("unexpected error response: body=%q headers=%v", body, resp.Header)
						}

						return
					}

					wantType := tc.contentType
					if !supplied {
						wantType = "application/octet-stream"
					}

					if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != wantType || resp.Header.Get("Gantry-Mirrored") != "1" || resp.Header.Get("Docker-Content-Digest") != d.String() || resp.ContentLength != int64(len(tc.data)) {
						t.Fatalf("unexpected success response: status=%d headers=%v", resp.StatusCode, resp.Header)
					}

					wantBody := tc.data
					if method == http.MethodHead {
						wantBody = ""
					}

					if string(body) != wantBody {
						t.Fatalf("body=%q; want %q", body, wantBody)
					}
				})
			}
		}
	}
}
