// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func assertRacerDownloadHeaders(t *testing.T, h http.Header) {
	t.Helper()

	for name, want := range map[string]string{
		"Content-Disposition":    "attachment",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q; want %q", name, got, want)
		}
	}
}

func TestRacerSecurityRegistryPayloads(t *testing.T) {
	data := []byte(`<html><script>alert("registry")</script></html>`)
	d := racerDigest(data)

	for _, contentType := range []string{
		"", "text/html; charset=utf-8", "application/xhtml+xml", "image/svg+xml",
		"application/octet-stream", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json",
	} {
		for _, mode := range handlerRequestModes() {
			t.Run(contentType+"/"+mode.name, func(t *testing.T) {
				origin := racerPageOrigin(t, d, data)
				client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					metadata, body, err := origin(ctx, req)
					metadata.ContentType = contentType

					return metadata, body, err
				})
				server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
				resp := racerRequest(t, server, mode.method, "blobs", d, mode.rangeHeader, "")
				assertRacerDownloadHeaders(t, resp.Header)

				wantType := contentType
				if wantType == "" {
					wantType = "application/octet-stream"
				}

				want, status, size := data, http.StatusOK, len(data)
				if mode.name == "resume" {
					want, status, size = data[4:], http.StatusPartialContent, len(data)-4
					if got := resp.Header.Get("Content-Range"); got != "bytes 4-"+strconv.Itoa(len(data)-1)+"/"+strconv.Itoa(len(data)) {
						t.Errorf("Content-Range = %q", got)
					}
				} else if mode.method == http.MethodHead {
					want = nil
				}

				if resp.StatusCode != status || resp.Header.Get("Content-Type") != wantType || resp.ContentLength != int64(size) || resp.Header.Get("Docker-Content-Digest") != d.String() {
					t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
				}

				if got := handlerReadBody(t, resp); !bytes.Equal(got, want) {
					t.Fatalf("payload changed: %q; want %q", got, want)
				}
			})
		}
	}
}

func TestRacerSecurityWriterEntryPoints(t *testing.T) {
	const payload = `<html><script>alert("request")</script></html>`

	for _, mode := range []string{"implicit", "explicit", "flush", "readfrom", "error", "typed", "empty", "informational"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				value := r.URL.Query().Get("value")

				switch mode {
				case "empty":
					return
				case "error":
					http.Error(w, value, http.StatusBadRequest)
					return
				case "typed":
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Content-Disposition", "inline")
					w.Header().Set("X-Content-Type-Options", "")
				case "explicit":
					w.WriteHeader(http.StatusOK)
				case "informational":
					w.WriteHeader(http.StatusEarlyHints)
				case "flush":
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
					}
				case "readfrom":
					if _, err := w.(io.ReaderFrom).ReadFrom(io.LimitReader(strings.NewReader(value), int64(len(value)))); err != nil {
						t.Error(err)
					}

					return
				}

				if _, err := w.Write([]byte(value)); err != nil {
					t.Error(err)
				}
			}), 0, nil))
			defer server.Close()

			resp, err := server.Client().Get(server.URL + "?value=" + url.QueryEscape(payload))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			assertRacerDownloadHeaders(t, resp.Header)

			want, wantType, status := payload, "application/octet-stream", http.StatusOK

			switch mode {
			case "error":
				want, wantType, status = payload+"\n", "text/plain; charset=utf-8", http.StatusBadRequest
			case "typed":
				wantType = "text/html"
			case "empty":
				want = ""
			}

			if resp.StatusCode != status || resp.Header.Get("Content-Type") != wantType {
				t.Errorf("status=%d headers=%v", resp.StatusCode, resp.Header)
			}

			if got := string(handlerReadBody(t, resp)); got != want {
				t.Errorf("body = %q; want %q", got, want)
			}
		})
	}
}

func TestRacerSecurityDirectHandler(t *testing.T) {
	data := []byte(`<html><script>alert("direct")</script></html>`)
	ref := testRef()
	ref.Digest = racerDigest(data)

	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindManifest, ifaces.KindConfig} {
		for _, failed := range []bool{false, true} {
			t.Run(kind.String()+"/failed="+strconv.FormatBool(failed), func(t *testing.T) {
				ref.Kind = kind
				origin := racerPageOrigin(t, ref.Digest, data)
				client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					if failed {
						return racersdk.Metadata{}, nil, racersdk.ErrUnavailable
					}

					metadata, body, err := origin(ctx, req)
					metadata.ContentType = "text/html"

					return metadata, body, err
				})
				w := httptest.NewRecorder()
				NewHandler(client, nil, nil).ServeContent(w, httptest.NewRequest(http.MethodGet, "/", nil), ref)

				resp := w.Result()
				defer resp.Body.Close()

				assertRacerDownloadHeaders(t, resp.Header)

				want, wantType, status := data, "text/html", http.StatusOK
				if failed {
					want, wantType, status = []byte("Racer request failed\n"), "text/plain; charset=utf-8", http.StatusServiceUnavailable
				}

				if resp.StatusCode != status || resp.Header.Get("Content-Type") != wantType || !bytes.Equal(handlerReadBody(t, resp), want) {
					t.Fatalf("status=%d headers=%v body=%q", resp.StatusCode, resp.Header, w.Body.String())
				}
			})
		}
	}
}
