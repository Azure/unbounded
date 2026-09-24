// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type resolvedRegistryFixture struct {
	meta                 ifaces.OriginMetadata
	headErr, openErr     error
	heads, opens, closes int
	headRefs, openRefs   []ifaces.OriginRef
	headAuth, openAuth   []string
	offset, length, size int64
}

func TestResolvedOriginPhysicalRegistryRequests(t *testing.T) {
	for _, manifestFallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "blob", true: "blob-to-manifest"}[manifestFallback], func(t *testing.T) {
			var heads, gets atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isManifest := strings.Contains(r.URL.Path, "/manifests/")
				if r.Method == http.MethodHead {
					heads.Add(1)

					if manifestFallback && !isManifest {
						w.WriteHeader(http.StatusNotFound)
						return
					}

					w.Header().Set("Content-Length", "7")
					w.Header().Set("Content-Type", "application/octet-stream")

					if isManifest {
						w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					}

					return
				}

				gets.Add(1)

				if r.Method != http.MethodGet || isManifest != manifestFallback || r.Header.Get("Range") != "bytes=2-4" {
					t.Error("GET did not preserve resolved kind or range", r.Method, r.URL, r.Header)
				}

				w.Header().Set("Content-Range", "bytes 2-4/7")
				w.Header().Set("Content-Length", "3")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "ylo")
			}))
			defer server.Close()

			ref := testRef()
			ref.Kind = ifaces.KindBlob

			registry, err := registryorigin.New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: ref.Registry, Endpoint: server.URL}}})
			if err != nil {
				t.Fatal(err)
			}

			o, _ := sdk.NewRangeOrigin(&Origin{Registry: registry, Registries: map[string]bool{ref.Registry: true}})
			target, _ := Target(ref)
			r := httptest.NewRequest("GET", target, nil)
			r.Header.Set("Range", "bytes=2-4")

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			wantHeads := int32(1)
			if manifestFallback {
				wantHeads = 2
			}

			if w.Code != 206 || w.Body.String() != "ylo" || heads.Load() != wantHeads || gets.Load() != 1 {
				t.Fatalf("status=%d body=%q HEAD=%d GET=%d", w.Code, w.Body.String(), heads.Load(), gets.Load())
			}
		})
	}
}

func (r *resolvedRegistryFixture) HeadMetadata(ctx context.Context, ref ifaces.OriginRef) (ifaces.OriginMetadata, error) {
	r.heads++
	r.headRefs = append(r.headRefs, ref)
	r.headAuth = append(r.headAuth, registryauth.Authorization(ctx))

	return r.meta, r.headErr
}

func (r *resolvedRegistryFixture) OpenRange(ctx context.Context, ref ifaces.OriginRef, offset, length, size int64) (io.ReadCloser, error) {
	r.opens++
	r.openRefs = append(r.openRefs, ref)
	r.openAuth = append(r.openAuth, registryauth.Authorization(ctx))

	r.offset, r.length, r.size = offset, length, size
	if r.openErr != nil {
		return nil, r.openErr
	}

	return &trackedBody{Reader: strings.NewReader("payload"[offset : offset+length]), close: func() { r.closes++ }}, nil
}

type trackedBody struct {
	io.Reader
	close func()
}

func (b *trackedBody) Close() error { b.close(); return nil }

func TestResolvedOriginRemoteRequests(t *testing.T) {
	ref := testRef()
	ref.Kind = ifaces.KindBlob
	resolved := ref
	resolved.Kind = ifaces.KindManifest
	target, _ := Target(ref)

	tag := `"` + ref.Digest.Hex() + `"`
	for _, tc := range []struct {
		name, method, field, value string
		size                       int64
		status, opens              int
		headErr, openErr           error
	}{
		{name: "manifest-full", method: "GET", size: 7, status: 200, opens: 1},
		{name: "manifest-range", method: "GET", field: "Range", value: "bytes=2-4", size: 7, status: 206, opens: 1},
		{name: "head", method: "HEAD", size: 7, status: 200},
		{name: "conditional", method: "GET", field: "If-None-Match", value: tag, size: 7, status: 304},
		{name: "precondition", method: "GET", field: "If-Match", value: `"old"`, size: 7, status: 412},
		{name: "range-rejected", method: "GET", field: "Range", value: "bytes=7-", size: 7, status: 416},
		{name: "empty", method: "GET", size: 0, status: 200},
		{name: "empty-range", method: "GET", field: "Range", value: "bytes=0-", size: 0, status: 416},
		{name: "head-auth-error", method: "GET", size: 7, status: 401, headErr: &ifaces.OriginError{StatusCode: 401, Challenge: "Bearer registry"}},
		{name: "get-rate-limit", method: "GET", size: 7, status: 429, opens: 1, openErr: &ifaces.OriginError{StatusCode: 429, RetryAfterHeader: "10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := &resolvedRegistryFixture{meta: ifaces.OriginMetadata{Ref: resolved, Size: tc.size, ContentType: "application/vnd.oci.image.manifest.v1+json"}, headErr: tc.headErr, openErr: tc.openErr}
			o, _ := sdk.NewRangeOrigin(&Origin{Registry: registry, Registries: map[string]bool{ref.Registry: true}})
			r := httptest.NewRequest(tc.method, target, nil)
			r.Header.Set("Racer-Origin-Data", base64.StdEncoding.EncodeToString([]byte("Bearer request")))

			if tc.field != "" {
				r.Header.Set(tc.field, tc.value)
			}

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			if w.Code != tc.status || registry.heads != 1 || registry.opens != tc.opens || registry.headRefs[0] != ref || registry.headAuth[0] != "Bearer request" {
				t.Fatalf("status=%d heads=%d opens=%d refs=%v auth=%v", w.Code, registry.heads, registry.opens, registry.headRefs, registry.headAuth)
			}

			if tc.opens > 0 && (registry.openRefs[0] != resolved || registry.openAuth[0] != "Bearer request" || registry.size != tc.size) {
				t.Fatal("resolved manifest reference, credentials, or size lost")
			}

			if tc.opens > 0 && tc.openErr == nil && registry.closes != 1 {
				t.Fatal("registry body not closed")
			}

			if tc.name == "manifest-range" && (w.Body.String() != "ylo" || registry.offset != 2 || registry.length != 3 || w.Header().Get("ETag") != tag || w.Header().Get("Content-Type") != registry.meta.ContentType) {
				t.Fatal(w.Header(), w.Body.String())
			}

			if tc.name == "head-auth-error" && w.Header().Get("WWW-Authenticate") != "Bearer registry" {
				t.Fatal(w.Header())
			}

			if tc.name == "get-rate-limit" && w.Header().Get("Retry-After") != "10" {
				t.Fatal(w.Header())
			}

			if tc.opens == 0 && w.Body.Len() != 0 {
				t.Fatal("metadata-only response has payload")
			}
		})
	}
}

type racingLocalFixture struct {
	desc             ocispec.Descriptor
	descErr, openErr error
	body             string
	seekable         bool
	opens, closes    int
}

func (l *racingLocalFixture) Descriptor(context.Context, digest.Digest) (ocispec.Descriptor, error) {
	return l.desc, l.descErr
}

func (l *racingLocalFixture) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	l.opens++
	if l.openErr != nil {
		return nil, 0, l.openErr
	}

	b := &trackedBody{Reader: bytes.NewReader([]byte(l.body)), close: func() { l.closes++ }}
	if l.seekable {
		return &trackedSeekBody{b, b.Reader.(io.Seeker)}, int64(len(l.body)), nil
	}

	return b, int64(len(l.body)), nil
}

type trackedSeekBody struct {
	*trackedBody
	io.Seeker
}

func TestResolvedOriginLocalFallback(t *testing.T) {
	ref := testRef()
	ref.Kind = ifaces.KindBlob
	resolved := ref
	resolved.Kind = ifaces.KindManifest
	target, _ := Target(ref)

	const mediaType = "application/vnd.oci.image.index.v1+json"
	for _, tc := range []struct {
		name                        string
		remoteSize                  int64
		remoteType                  string
		openErr, headErr            error
		seekable                    bool
		localBody                   string
		status, heads, gets, closes int
	}{
		{name: "disappeared", remoteSize: 7, remoteType: mediaType, openErr: &ifaces.ErrNotFound{}, status: 206, heads: 1, gets: 1},
		{name: "nonseekable", remoteSize: 7, remoteType: mediaType + "; charset=utf-8", localBody: "payload", status: 206, heads: 1, gets: 1, closes: 1},
		{name: "size-mismatch", remoteSize: 8, remoteType: mediaType, openErr: &ifaces.ErrNotFound{}, status: 412, heads: 1},
		{name: "type-mismatch", remoteSize: 7, remoteType: "application/vnd.oci.image.manifest.v1+json", openErr: &ifaces.ErrNotFound{}, status: 412, heads: 1},
		{name: "local-size-mismatch", remoteSize: 7, remoteType: mediaType, seekable: true, localBody: "payload!", status: 412, closes: 1},
		{name: "local-error", openErr: fs.ErrPermission, status: 403},
		{name: "fallback-error", openErr: &ifaces.ErrNotFound{}, headErr: &ifaces.OriginError{StatusCode: 503}, status: 503, heads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := &racingLocalFixture{desc: ocispec.Descriptor{Size: 7, MediaType: mediaType}, openErr: tc.openErr, body: tc.localBody, seekable: tc.seekable}
			registry := &resolvedRegistryFixture{meta: ifaces.OriginMetadata{Ref: resolved, Size: tc.remoteSize, ContentType: tc.remoteType}, headErr: tc.headErr}
			o, _ := sdk.NewRangeOrigin(&Origin{Local: local, Registry: registry, Registries: map[string]bool{ref.Registry: true}})
			r := httptest.NewRequest("GET", target, nil)
			r.Header.Set("Range", "bytes=2-4")
			r.Header.Set("Racer-Origin-Data", base64.StdEncoding.EncodeToString([]byte("Bearer fallback")))

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			if w.Code != tc.status || local.opens != 1 || local.closes != tc.closes || registry.heads != tc.heads || registry.opens != tc.gets {
				t.Fatalf("status=%d local=%d/%d registry=%d/%d", w.Code, local.opens, local.closes, registry.heads, registry.opens)
			}

			if tc.gets > 0 && (w.Body.String() != "ylo" || registry.openRefs[0] != resolved || registry.headAuth[0] != "Bearer fallback" || registry.openAuth[0] != "Bearer fallback" || registry.closes != 1) {
				t.Fatal("fallback changed reference, bytes, auth, or cleanup")
			}

			if tc.status >= 400 && w.Body.Len() != 0 {
				t.Fatal("mismatched representation was served")
			}
		})
	}
}

func TestResolvedOriginRequestIsolationAndClose(t *testing.T) {
	ref := testRef()
	target, _ := Target(ref)
	registry := &resolvedRegistryFixture{meta: ifaces.OriginMetadata{Ref: ref, Size: 7, ContentType: "application/vnd.oci.image.manifest.v1+json"}}
	o := &Origin{Registry: registry, Registries: map[string]bool{ref.Registry: true}}

	first, err := o.ResolveRange(t.Context(), target, []byte("Bearer first"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := o.ResolveRange(t.Context(), target, []byte("Bearer second"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := first.OpenRange(ctx, 0, 7); !errors.Is(err, context.Canceled) || registry.opens != 0 {
		t.Fatal("canceled open reached registry", err)
	}

	for _, h := range []sdk.ResolvedRange{second, first} {
		body, err := h.OpenRange(t.Context(), 0, 7)
		if err != nil {
			t.Fatal(err)
		}

		_ = body.Close()
		_ = h.Close()

		state := h.(*resolvedRange)
		if state.originData != nil || state.origin != nil {
			t.Fatal("handle retained request state")
		}

		if _, err := h.OpenRange(t.Context(), 0, 7); !errors.Is(err, fs.ErrClosed) {
			t.Fatal("closed handle opened payload", err)
		}
	}

	if registry.heads != 2 || registry.opens != 2 || registry.openAuth[0] != "Bearer second" || registry.openAuth[1] != "Bearer first" || registry.closes != 2 {
		t.Fatal("request-scoped credentials were shared or metadata was resolved again")
	}
}

func TestResolvedOriginRemoteToLocalRace(t *testing.T) {
	ref := testRef()
	target, _ := Target(ref)

	const mediaType = "application/vnd.oci.image.manifest.v1+json"
	for _, tc := range []struct {
		name, localType string
		wantErr         bool
		gets            int
	}{
		{"appeared", mediaType, false, 0},
		{"unknown-type", "", false, 1},
		{"changed-type", "application/vnd.oci.image.index.v1+json", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := &racingLocalFixture{descErr: &ifaces.ErrNotFound{}, body: "payload", seekable: true}
			registry := &resolvedRegistryFixture{meta: ifaces.OriginMetadata{Ref: ref, Size: 7, ContentType: mediaType}}
			o := &Origin{Local: local, Registry: registry, Registries: map[string]bool{ref.Registry: true}}

			h, err := o.ResolveRange(t.Context(), target, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()

			local.descErr = nil
			local.desc = ocispec.Descriptor{Size: 7, MediaType: tc.localType}

			body, err := h.OpenRange(t.Context(), 2, 3)
			if tc.wantErr {
				if !errors.Is(err, sdk.ErrVersionChanged) {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}

				data, err := io.ReadAll(body)
				_ = body.Close()

				if err != nil || string(data) != "ylo" {
					t.Fatal(string(data), err)
				}
			}

			if registry.heads != 1 || registry.opens != tc.gets || local.closes != 1 {
				t.Fatal("race caused repeated resolution or leaked local body")
			}
		})
	}
}
