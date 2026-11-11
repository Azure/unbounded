// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// digestCacheOrigin models Racer's digest-only object identity: the first fill
// of a key fixes its metadata and bytes, and later requests for the same key
// reuse them whatever route (kind) they name.
type digestCacheOrigin struct {
	inner racersdk.Origin
	mu    sync.Mutex
	fills atomic.Int32
	cache map[string]cachedObject
}

type cachedObject struct {
	metadata racersdk.Metadata
	data     []byte
}

func (c *digestCacheOrigin) origin(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := req.Key.String()

	object, ok := c.cache[key]
	if !ok {
		fill := req
		fill.Head, fill.Offset, fill.Length = false, 0, racersdk.PageSize

		metadata, body, err := c.inner(ctx, fill)
		if err != nil {
			return metadata, nil, err
		}

		var data []byte
		if body != nil {
			data, err = io.ReadAll(body)
			_ = body.Close()

			if err != nil {
				return racersdk.Metadata{}, nil, err
			}
		}

		c.fills.Add(1)

		object = cachedObject{metadata: metadata, data: data}
		c.cache[key] = object
	}

	if req.Head || req.Offset >= object.metadata.Size {
		return object.metadata, nil, nil
	}

	end := min(req.Offset+req.Length, object.metadata.Size)

	return object.metadata, io.NopCloser(bytes.NewReader(object.data[req.Offset:end])), nil
}

// Racer keys objects by digest alone, so the same digest may be filled through
// the blob route and then read through the manifest route, or the reverse. The
// manifest route must still answer with the manifest's media type, and the
// blob route with application/octet-stream, from either source.
func TestRacerManifestMediaTypeRouteIndependent(t *testing.T) {
	const indexType = "application/vnd.oci.image.index.v1+json"

	data := []byte(`{"schemaVersion":2,"mediaType":"` + indexType + `","manifests":[]}`)

	registrySource := func(t *testing.T) (racersdk.Origin, ifaces.OriginRef) {
		t.Helper()

		d := racerDigest(data)
		registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Each route reports a different type, as registries do.
			switch r.URL.Path {
			case "/v2/library/image/blobs/" + d.String():
				w.Header().Set("Content-Type", "application/octet-stream")
			case "/v2/library/image/manifests/" + d.String():
				w.Header().Set("Content-Type", indexType+"; charset=utf-8")
			default:
				http.NotFound(w, r)

				return
			}

			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("Docker-Content-Digest", d.String())

			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		}))
		t.Cleanup(registry.Close)

		cfg := racerConfig()
		cfg.UpstreamRegistries[0].Endpoint = registry.URL

		upstream, err := registryorigin.New(cfg)
		if err != nil {
			t.Fatal(err)
		}

		ref := testRef()
		ref.Digest = d

		return Origin(cfg, upstream), ref
	}

	localSource := func(t *testing.T) (racersdk.Origin, ifaces.OriginRef) {
		t.Helper()

		store, ref := localFixture(t, data, ifaces.KindManifest)

		return localOrigin(t, store, &testUpstream{}), ref
	}

	sources := map[string]func(*testing.T) (racersdk.Origin, ifaces.OriginRef){
		"registry": registrySource, "local": localSource,
	}

	for name, source := range sources {
		for _, order := range [][]ifaces.OriginRefKind{
			{ifaces.KindBlob, ifaces.KindManifest},
			{ifaces.KindManifest, ifaces.KindBlob},
		} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(name+"/"+order[0].String()+"-then-"+order[1].String()+"/"+method, func(t *testing.T) {
					origin, ref := source(t)
					cached := &digestCacheOrigin{inner: origin, cache: map[string]cachedObject{}}
					handler := NewHandler(racerFakeClient(t, cached.origin), nil, nil)

					for _, kind := range order {
						ref.Kind = kind
						w := httptest.NewRecorder()
						handler.ServeContent(w, httptest.NewRequest(method, "/", nil), ref)

						wantType, wantBody := "application/octet-stream", data
						if kind == ifaces.KindManifest {
							wantType = indexType
						}

						if method == http.MethodHead {
							wantBody = nil
						}

						if w.Code != http.StatusOK || w.Header().Get("Content-Type") != wantType || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) || !bytes.Equal(w.Body.Bytes(), wantBody) {
							t.Fatalf("%v: status=%d headers=%v body=%q", kind, w.Code, w.Header(), w.Body.String())
						}
					}

					if cached.fills.Load() != 1 {
						t.Fatalf("fills = %d; want one shared cache entry", cached.fills.Load())
					}
				})
			}
		}
	}

	// Both sources fill the same version metadata for every route.
	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindManifest} {
		var metadata []racersdk.Metadata

		for _, name := range []string{"registry", "local"} {
			origin, ref := sources[name](t)
			ref.Kind = kind

			got, body, err := origin(t.Context(), racersdk.OriginRequest{Request: requestFor(t, ref, ""), Length: racersdk.PageSize})
			if err != nil {
				t.Fatalf("%s %v: %v", name, kind, err)
			}

			_ = body.Close()
			got.ExpiresAt = metadata0(metadata).ExpiresAt
			metadata = append(metadata, got)
		}

		if metadata[0] != metadata[1] || metadata[0].ContentType != objectContentType {
			t.Fatalf("%v: registry %+v, local %+v", kind, metadata[0], metadata[1])
		}
	}
}

func metadata0(m []racersdk.Metadata) racersdk.Metadata {
	if len(m) == 0 {
		return racersdk.Metadata{}
	}

	return m[0]
}

func TestManifestBodyMediaType(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"field", `{"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`, "application/vnd.docker.distribution.manifest.v2+json"},
		{"normalized field", `{"mediaType":" Application/VND.oci.image.index.v1+json; charset=utf-8","manifests":[]}`, ociIndexMediaType},
		{"field wins over structure", `{"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","layers":[]}`, "application/vnd.docker.distribution.manifest.list.v2+json"},
		{"inferred index", `{"schemaVersion":2,"manifests":[]}`, ociIndexMediaType},
		{"inferred manifest from config", `{"schemaVersion":2,"config":{}}`, ociManifestMediaType},
		{"inferred manifest from layers", `{"schemaVersion":2,"layers":[]}`, ociManifestMediaType},
		{"parameter-only field inferred", `{"mediaType":";charset=utf-8","layers":[]}`, ociManifestMediaType},
		{"schema 1", `{"schemaVersion":1,"fsLayers":[]}`, ""},
		{"empty object", `{}`, ""},
		{"null manifests", `{"manifests":null}`, ""},
		{"not JSON", `<html></html>`, ""},
		{"empty", ``, ""},
		{"array", `[]`, ""},
		{"trailing garbage", `{"layers":[]} x`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := manifestBodyMediaType([]byte(tc.body))
			if tc.want == "" {
				if !errors.Is(err, errUnknownManifestType) {
					t.Fatalf("got %q err=%v; want errUnknownManifestType", got, err)
				}

				return
			}

			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v; want %q", got, err, tc.want)
			}
		})
	}
}

// A manifest Gantry cannot type is a 503 so containerd falls through to the
// registry, which owns the authoritative Content-Type. Nothing is committed.
func TestRacerManifestUnknownMediaTypeFallsThrough(t *testing.T) {
	for _, data := range [][]byte{[]byte("not json"), []byte(`{"schemaVersion":1,"fsLayers":[]}`)} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			ref := testRef()
			ref.Kind, ref.Digest = ifaces.KindManifest, racerDigest(data)
			w := httptest.NewRecorder()
			NewHandler(racerFakeClient(t, racerPageOrigin(t, ref.Digest, data)), nil, nil).ServeContent(w, httptest.NewRequest(method, "/", nil), ref)

			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Gantry-Mirrored") != "" || w.Header().Get("Docker-Content-Digest") != "" || bytes.Contains(w.Body.Bytes(), data) {
				t.Fatalf("%s %q: status=%d headers=%v body=%q", method, data, w.Code, w.Header(), w.Body.String())
			}
		}
	}
}

// A manifest whose bytes do not match its digest is rejected before any
// header is committed and its cached version is invalidated.
func TestRacerManifestDigestMismatchBeforeHeaders(t *testing.T) {
	want := []byte(`{"schemaVersion":2,"layers":[]}`)
	served := []byte(`{"schemaVersion":2,"layers":{}}`)
	ref := testRef()
	ref.Kind, ref.Digest = ifaces.KindManifest, racerDigest(want)

	inv := &recordingInvalidator{}

	var mismatches atomic.Int32

	handler := NewHandler(racerFakeClient(t, racerPageOrigin(t, ref.Digest, served)), nil, nil,
		WithInvalidator(inv), WithDigestMismatchObserver(func() { mismatches.Add(1) }))

	w := httptest.NewRecorder()
	handler.ServeContent(w, httptest.NewRequest(http.MethodGet, "/", nil), ref)

	if w.Code != http.StatusBadGateway || w.Header().Get("Gantry-Mirrored") != "" || bytes.Contains(w.Body.Bytes(), served) {
		t.Fatalf("status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}

	if mismatches.Load() != 1 || len(inv.recorded()) != 1 {
		t.Fatalf("mismatches=%d invalidations=%d; want 1 1", mismatches.Load(), len(inv.recorded()))
	}
}
