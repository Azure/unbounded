// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

func reviewConfig() *config.Config {
	cfg := config.NewDefault()
	cfg.ContentBackend = "racer"
	cfg.PeerFetchTimeout = 10 * time.Second
	cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "registry.example"}}

	return cfg
}

func TestRacerColdPagePreparationExceedsMetadataBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}

	data := []byte("cold page delivered after bounded preparation")
	d := digestOf(data)

	var progress atomic.Int64

	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)

		if r.Method == "GET" {
			// Model a page owner making progress while filling its cold page.
			for range 7 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(500 * time.Millisecond):
					progress.Add(1)
				}
			}
		}

		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	cfg := reviewConfig()
	cfg.RacerMetadataTimeout = 100 * time.Millisecond

	var fallback atomic.Int64

	up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 1)}

	m := httptest.NewServer(mirror.NewRacer(cfg, fakes.NewCache(), up, &gantryracer.Backend{Client: client}, mirror.WithRacerMetrics(nil, func() { fallback.Add(1) })).Handler())
	defer m.Close()

	resp, err := m.Client().Get(m.URL + "/v2/repo/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil || !bytes.Equal(body, data) || progress.Load() != 7 || fallback.Load() != 0 {
		t.Fatal(err, progress.Load(), fallback.Load())
	}
}

func TestRacerFallbackAuthoritativeGETContentType(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, payload string
		blob                       bool
	}{
		{"late-index-type", "application/vnd.oci.image.index.v1+json", `{"padding":"` + strings.Repeat(" ", 600) + `","mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, false},
		{"absent-json-field", "application/vnd.oci.image.index.v1+json", `{"schemaVersion":2,"manifests":[]}`, true},
		{"absent-http-header", "", `{"schemaVersion":2,"manifests":[]}`, false},
		{"absent-header-typed-json", "", `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, false},
		{"opaque-get-type", "application/custom; version=1", `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, false},
		{"empty-object-no-type", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var heads atomic.Int64

			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" {
					heads.Add(1)
					w.Header().Set("Content-Type", "wrong/head")

					return
				}

				if tc.blob && strings.Contains(r.URL.Path, "/blobs/") {
					w.WriteHeader(404)
					return
				}

				w.Header()["Content-Type"] = nil
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}

				_, _ = io.WriteString(w, tc.payload)
			}))
			defer up.Close()

			cfg := reviewConfig()
			cfg.UpstreamRegistries[0].Endpoint = up.URL

			registry, err := origin.New(cfg)
			if err != nil {
				t.Fatal(err)
			}

			m := httptest.NewServer(mirror.NewRacer(cfg, fakes.NewCache(), registry, nil).Handler())
			defer m.Close()

			kind := "manifests"
			if tc.blob {
				kind = "blobs"
			}

			resp, err := m.Client().Get(m.URL + "/v2/repo/" + kind + "/" + digestOf([]byte(tc.payload)).String())
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(resp.Body)

			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || string(body) != tc.payload || resp.Header.Get("Content-Type") != tc.contentType || heads.Load() != 0 {
				t.Fatal(resp.Status, resp.Header, err)
			}

			if tc.contentType == "" && len(resp.Header.Values("Content-Type")) != 0 {
				t.Fatal("absent upstream Content-Type was synthesized", resp.Header)
			}
		})
	}
}

type stalledFallbackOrigin struct {
	opened, closed chan struct{}
	once           sync.Once
}

func (o *stalledFallbackOrigin) PullWithMetadata(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	body, size, err := o.Pull(ctx, ref)
	return body, size, "application/octet-stream", err
}

func (o *stalledFallbackOrigin) HeadMetadata(ctx context.Context, ref ifaces.OriginRef) (ifaces.OriginMetadata, error) {
	size, contentType, err := o.Head(ctx, ref)
	return ifaces.OriginMetadata{Ref: ref, Size: size, ContentType: contentType}, err
}

func (*stalledFallbackOrigin) OpenRange(context.Context, ifaces.OriginRef, int64, int64, int64) (io.ReadCloser, error) {
	return nil, &ifaces.OriginRangeUnsupportedError{Reason: "fixture only serves full objects"}
}

func (o *stalledFallbackOrigin) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return -1, "application/octet-stream", nil
}

func (o *stalledFallbackOrigin) Pull(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
	o.once.Do(func() { close(o.opened) })
	return &endlessBody{closed: o.closed}, -1, nil
}

type endlessBody struct{ closed chan struct{} }

func (*endlessBody) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func (b *endlessBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}

	return nil
}

func TestRacerFallbackStalledDownstreamDeadlineAndAdmission(t *testing.T) {
	cfg := reviewConfig()
	cfg.PeerFetchTimeout = 500 * time.Millisecond
	cfg.RacerMaxConcurrentTransfers = 1
	up := &stalledFallbackOrigin{opened: make(chan struct{}), closed: make(chan struct{})}
	server := mirror.NewRacer(cfg, fakes.NewCache(), up, nil)
	finished := make(chan struct{}, 4)
	handler := server.Handler()

	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()

		handler.ServeHTTP(w, r)
	}))
	defer m.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(m.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}

	path := "/v2/repo/blobs/" + digestOf([]byte("never completes")).String()

	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: mirror\r\n\r\n", path)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-up.opened:
	case <-time.After(time.Second):
		t.Fatal("fallback did not open")
	}

	resp, err := m.Client().Get(m.URL + path)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" {
		t.Fatal("admission did not reject", resp.Status)
	}

	select {
	case <-up.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled downstream retained origin body")
	}
	// The rejected request and the timed-out request both leave their handlers.
	for range 2 {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("handler did not release")
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), "HEAD", m.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err = m.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("admission not released", resp.Status)
	}
}

func TestRacerDisconnectCancelsLaterPage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}
	// First page is real 64 MiB so this exercises nextPage after raw splice,
	// not just cancellation while preparing the first response.
	page := make([]byte, sdk.PageSize)
	hash := sha256.New()
	_, _ = hash.Write(page)
	_, _ = hash.Write([]byte{0})
	d := digest.MustParse(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
	second := make(chan struct{})
	canceled := make(chan struct{})
	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprint(sdk.PageSize+1))
			return
		}

		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", sdk.PageSize, sdk.PageSize) {
			close(second)
			<-r.Context().Done()
			close(canceled)

			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(sdk.PageSize))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", sdk.PageSize-1, sdk.PageSize+1))
		w.WriteHeader(206)
		_, _ = w.Write(page)
	}))
	cfg := reviewConfig()
	cfg.RacerMaxConcurrentTransfers = 1
	up := &authorizationCapturingOrigin{seen: make(chan string, 1)}
	stats := make(chan sdk.TransferStats, 1)
	server := mirror.NewRacer(cfg, fakes.NewCache(), up, &gantryracer.Backend{Client: client}, mirror.WithRacerMetrics(func(s sdk.TransferStats, _ bool, _ error) { stats <- s }, nil))

	m := httptest.NewServer(server.Handler())
	defer m.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(m.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	// A buffered pipelined request is discarded under Connection: close; it
	// must not cancel the active response before the actual disconnect.
	_, err = fmt.Fprintf(conn, "GET /v2/repo/blobs/%s HTTP/1.1\r\nHost: mirror\r\n\r\nGET /v2/ HTTP/1.1\r\nHost: mirror\r\n\r\n", d.String())
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = io.CopyN(io.Discard, resp.Body, sdk.PageSize); err != nil {
		t.Fatal(err)
	}

	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second page not requested")
	}

	_ = conn.Close()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel stalled next page")
	}

	select {
	case result := <-stats:
		if result.SpliceBytes == 0 || result.TeeCalls != 0 || result.TeeBytes != 0 {
			t.Fatal("lost splice forwarding without verification", result)
		}
	case <-time.After(time.Second):
		t.Fatal("mirror did not release")
	}

	if len(up.seen) != 0 {
		t.Fatal("fell back after headers")
	}
}
