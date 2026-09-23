// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func rangeRef() ifaces.OriginRef {
	return ifaces.OriginRef{Registry: "reg", Repository: "repo", Digest: digestOf([]byte("0123456789"))}
}

func rangeClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	c.registries["reg"].hc = srv.Client()
	c.registries["reg"].hc.CheckRedirect = checkRedirect

	return c, srv
}

func assertRangeHeaders(t *testing.T, r *http.Request) {
	t.Helper()

	if r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("If-Match") != "" {
		t.Errorf("unexpected range headers: %v", r.Header)
	}
}

func writeRange(w http.ResponseWriter) {
	w.Header().Set("Content-Range", "bytes 2-4/10")
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.WriteString(w, "234")
}

func TestHeadMetadataResolvesManifestWithoutGET(t *testing.T) {
	var heads, gets atomic.Int32

	const mediaType = "application/vnd.oci.image.index.v1+json; charset=utf-8"

	c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)

			if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Range") != "" {
				t.Errorf("HEAD headers: %v", r.Header)
			}

			if strings.Contains(r.URL.Path, "/blobs/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			w.Header().Set("Content-Length", "10")
			w.Header().Set("Content-Type", mediaType)

			return
		}

		gets.Add(1)
		assertRangeHeaders(t, r)

		if !strings.Contains(r.URL.Path, "/manifests/") || !strings.Contains(r.Header.Get("Accept"), "index.v1+json") {
			t.Errorf("GET did not use resolved manifest: %s %v", r.URL.Path, r.Header)
		}

		writeRange(w)
	})

	metadata, err := c.HeadMetadata(context.Background(), rangeRef())
	if err != nil || metadata.Size != 10 || metadata.ContentType != mediaType || metadata.Ref.Kind != ifaces.KindManifest {
		t.Fatalf("metadata = %+v, err = %v", metadata, err)
	}

	if heads.Load() != 2 || gets.Load() != 0 {
		t.Fatalf("HEAD/GET = %d/%d", heads.Load(), gets.Load())
	}

	body, err := c.OpenRange(context.Background(), metadata.Ref, 2, 3, metadata.Size)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil || string(got) != "234" || gets.Load() != 1 {
		t.Fatalf("body = %q, err = %v, GETs = %d", got, err, gets.Load())
	}
}

func TestHeadMetadataValidation(t *testing.T) {
	for _, tc := range []struct {
		name, length, encoding string
		status                 int
		unavailable, fail      bool
	}{
		{name: "empty", length: "0"},
		{name: "size", length: "10"},
		{name: "missing length", unavailable: true, fail: true},
		{name: "encoded", length: "10", encoding: "gzip", fail: true},
		{name: "unsupported", status: 405, unavailable: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("unexpected %s", r.Method)
				}

				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}

				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}

				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
			})
			_, err := c.HeadMetadata(context.Background(), rangeRef())

			var unavailable *ifaces.OriginMetadataUnavailableError
			if (err != nil) != tc.fail || errors.As(err, &unavailable) != tc.unavailable {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOpenRangeResponses(t *testing.T) {
	for _, tc := range []struct {
		name, contentRange, body, length, encoding string
		status                                     int
		offset, count                              int64
		chunked, openFail, readFail, unsupported   bool
	}{
		{name: "partial", status: 206, contentRange: "bytes 2-4/10", body: "234", offset: 2, count: 3},
		{name: "last byte", status: 206, contentRange: "bytes 9-9/10", body: "9", offset: 9, count: 1},
		{name: "whole 200", status: 200, body: "0123456789", count: 10},
		{name: "whole 206", status: 206, contentRange: "bytes 0-9/10", body: "0123456789", count: 10},
		{name: "ignored range", status: 200, body: "0123456789", offset: 2, count: 3, openFail: true, unsupported: true},
		{name: "prefix 200", status: 200, body: "012", count: 3, openFail: true, unsupported: true},
		{name: "416", status: 416, offset: 2, count: 3, openFail: true, unsupported: true},
		{name: "wrong start", status: 206, contentRange: "bytes 1-3/10", offset: 2, count: 3, openFail: true},
		{name: "wrong end", status: 206, contentRange: "bytes 2-5/10", offset: 2, count: 3, openFail: true},
		{name: "wrong total", status: 206, contentRange: "bytes 2-4/11", offset: 2, count: 3, openFail: true},
		{name: "unknown total", status: 206, contentRange: "bytes 2-4/*", offset: 2, count: 3, openFail: true},
		{name: "malformed", status: 206, contentRange: "bytes +2-4/10", offset: 2, count: 3, openFail: true},
		{name: "multipart", status: 206, contentRange: "bytes 2-4/10, 5-6/10", offset: 2, count: 3, openFail: true},
		{name: "missing content range", status: 206, offset: 2, count: 3, openFail: true},
		{name: "wrong length", status: 206, contentRange: "bytes 2-4/10", length: "4", offset: 2, count: 3, openFail: true},
		{name: "encoded", status: 206, contentRange: "bytes 2-4/10", encoding: "gzip", offset: 2, count: 3, openFail: true},
		{name: "truncated framed", status: 206, contentRange: "bytes 2-4/10", length: "3", body: "23", offset: 2, count: 3, readFail: true},
		{name: "truncated chunked", status: 206, contentRange: "bytes 2-4/10", body: "23", offset: 2, count: 3, chunked: true, readFail: true},
		{name: "oversized chunked", status: 206, contentRange: "bytes 2-4/10", body: "2345", offset: 2, count: 3, chunked: true, readFail: true},
		{name: "valid chunked", status: 206, contentRange: "bytes 2-4/10", body: "234", offset: 2, count: 3, chunked: true},
		{name: "404 no fallback", status: 404, offset: 2, count: 3, openFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32

			c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				if r.Method != http.MethodGet || r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", tc.offset, tc.offset+tc.count-1) || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("If-Match") != "" {
					t.Errorf("request = %s %v", r.Method, r.Header)
				}

				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}

				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}

				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}

				w.WriteHeader(tc.status)

				if tc.chunked {
					w.(http.Flusher).Flush()
				}

				_, _ = io.WriteString(w, tc.body)
			})
			ref := rangeRef()
			ref.Offset = 999 // Explicit bounds take precedence.
			body, err := c.OpenRange(context.Background(), ref, tc.offset, tc.count, 10)

			var unsupported *ifaces.OriginRangeUnsupportedError
			if (err != nil) != tc.openFail || errors.As(err, &unsupported) != tc.unsupported {
				t.Fatalf("open err = %v", err)
			}

			if body != nil {
				defer body.Close()

				got, readErr := io.ReadAll(body)
				if (readErr != nil) != tc.readFail || int64(len(got)) > tc.count || (!tc.readFail && string(got) != tc.body) {
					t.Fatalf("body = %q, err = %v", got, readErr)
				}

				if tc.readFail {
					var oe *ifaces.OriginError
					if !errors.As(readErr, &oe) {
						t.Fatalf("unclassified read error: %v", readErr)
					}
				}
			}

			if requests.Load() != 1 {
				t.Fatalf("requests = %d", requests.Load())
			}
		})
	}
}

func TestOpenRangeInvalidBounds(t *testing.T) {
	var requests atomic.Int32

	c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	for _, bounds := range [][3]int64{{-1, 1, 10}, {0, 0, 10}, {0, -1, 10}, {0, 1, -1}, {10, 1, 10}, {9, 2, 10}, {0, 0, 0}, {math.MaxInt64, 2, math.MaxInt64}} {
		if _, err := c.OpenRange(context.Background(), rangeRef(), bounds[0], bounds[1], bounds[2]); err == nil {
			t.Fatalf("accepted bounds %v", bounds)
		}
	}

	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestOpenRangeAuthenticationRetries(t *testing.T) {
	for _, basic := range []bool{false, true} {
		t.Run(fmt.Sprintf("basic=%t", basic), func(t *testing.T) {
			var (
				dataRequests, tokenRequests atomic.Int32
				tokenURL                    string
			)

			c, srv := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenRequests.Add(1)

					if r.Header.Get("Range") != "" {
						t.Error("Range sent to token endpoint")
					}

					_, _ = io.WriteString(w, `{"token":"fresh","expires_in":300}`)

					return
				}

				dataRequests.Add(1)
				assertRangeHeaders(t, r)

				if !strings.Contains(r.Header.Get("Accept"), "index.v1+json") {
					t.Error("Accept lost on retry")
				}

				authorized := r.Header.Get("Authorization") == "Bearer fresh"
				if basic {
					user, pass, ok := r.BasicAuth()
					authorized = ok && user == "node" && pass == "password"
				}

				if !authorized {
					challenge := `Bearer realm="` + tokenURL + `"`
					if basic {
						challenge = `Basic realm="registry"`
					}

					w.Header().Set("WWW-Authenticate", challenge)
					w.WriteHeader(http.StatusUnauthorized)

					return
				}

				writeRange(w)
			})
			tokenURL = srv.URL + "/token"
			c.registries["reg"].username = "node"
			c.registries["reg"].password = "password"
			c.registries["reg"].setToken("stale", time.Minute)

			ref := rangeRef()
			ref.Kind = ifaces.KindManifest

			body, err := c.OpenRange(context.Background(), ref, 2, 3, 10)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()

			got, err := io.ReadAll(body)

			wantTokens := int32(1)
			if basic {
				wantTokens = 0
			}

			if err != nil || string(got) != "234" || dataRequests.Load() != 2 || tokenRequests.Load() != wantTokens {
				t.Fatalf("body=%q err=%v data/token=%d/%d", got, err, dataRequests.Load(), tokenRequests.Load())
			}
		})
	}
}

func TestBoundedAPIDelegatedAuthorization(t *testing.T) {
	for _, head := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			for _, authorization := range []string{"Basic cmVxdWVzdGVyOnNlY3JldA==", "Bearer " + strings.Repeat("x", registryauth.MaxAuthorizationBytes-len("Bearer "))} {
				t.Run(fmt.Sprintf("head=%t/reject=%t/basic=%t", head, rejected, strings.HasPrefix(authorization, "Basic")), func(t *testing.T) {
					var requests atomic.Int32

					c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)

						if r.Header.Get("Authorization") != authorization {
							t.Error("delegated credential changed")
						}

						if rejected {
							w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token"`)
							w.Header().Set("Retry-After", "17")
							w.WriteHeader(http.StatusUnauthorized)

							return
						}

						if head {
							w.Header().Set("Content-Length", "10")
							return
						}

						writeRange(w)
					})
					c.registries["reg"].username = "node"
					c.registries["reg"].password = "password"
					c.registries["reg"].setToken("node-token", time.Minute)

					ctx := registryauth.WithAuthorization(context.Background(), authorization)

					var err error
					if head {
						_, err = c.HeadMetadata(ctx, rangeRef())
					} else {
						var body io.ReadCloser

						body, err = c.OpenRange(ctx, rangeRef(), 2, 3, 10)
						if body != nil {
							_, err = io.ReadAll(body)
							_ = body.Close()
						}
					}

					if (err != nil) != rejected {
						t.Fatalf("err=%v", err)
					}

					if rejected {
						var oe *ifaces.OriginError
						if !errors.As(err, &oe) || oe.Class != ifaces.FailureAuth || oe.Challenge == "" || oe.StatusCode != 401 || oe.RetryAfter != 17*time.Second {
							t.Fatalf("missing failure details: %+v", oe)
						}
					}

					if requests.Load() != 1 || c.registries["reg"].cachedToken() != "node-token" {
						t.Fatal("delegated auth retried or changed shared token")
					}
				})
			}
		}
	}
}

func TestBoundedAPIDelegatedHTTPRejected(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer delegated")
	_, headErr := c.HeadMetadata(ctx, rangeRef())

	_, rangeErr := c.OpenRange(ctx, rangeRef(), 2, 3, 10)
	if headErr == nil || rangeErr == nil || requests.Load() != 0 {
		t.Fatalf("HEAD=%v range=%v requests=%d", headErr, rangeErr, requests.Load())
	}
}

func TestOpenRangeRedirectHeaders(t *testing.T) {
	for _, target := range []string{"same", "cross-host", "downgrade"} {
		t.Run(target, func(t *testing.T) {
			var (
				destination string
				finals      atomic.Int32
			)

			final := func(w http.ResponseWriter, r *http.Request) {
				finals.Add(1)
				assertRangeHeaders(t, r)

				wantAuth := ""
				if target == "same" {
					wantAuth = "Bearer delegated"
				}

				if r.Header.Get("Authorization") != wantAuth {
					t.Errorf("redirect leaked/lost authorization")
				}

				writeRange(w)
			}
			c, srv := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/final" {
					final(w, r)
					return
				}

				assertRangeHeaders(t, r)
				http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
			})
			destination = srv.URL + "/final"

			if target == "cross-host" {
				other := httptest.NewTLSServer(http.HandlerFunc(final))
				defer other.Close()

				destination = strings.Replace(other.URL, "127.0.0.1", "localhost", 1) + "/final"
				// httptest's trusted certificate covers example.com, not localhost.
				transport := c.registries["reg"].hc.Transport.(*http.Transport).Clone()
				transport.TLSClientConfig.ServerName = "example.com"

				c.registries["reg"].hc.Transport = transport
				defer transport.CloseIdleConnections()
			}

			if target == "downgrade" {
				other := httptest.NewServer(http.HandlerFunc(final))
				defer other.Close()

				destination = other.URL + "/final"
			}

			ctx := registryauth.WithAuthorization(context.Background(), "Bearer delegated")

			body, err := c.OpenRange(ctx, rangeRef(), 2, 3, 10)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()

			got, err := io.ReadAll(body)
			if err != nil || string(got) != "234" || finals.Load() != 1 {
				t.Fatalf("body=%q err=%v finals=%d", got, err, finals.Load())
			}
		})
	}
}

func TestOpenRangeTokenFailureDetails(t *testing.T) {
	var tokenURL string

	c, srv := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Retry-After", "23")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenURL+`"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	tokenURL = srv.URL + "/token"
	_, err := c.OpenRange(context.Background(), rangeRef(), 2, 3, 10)

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.StatusCode != 429 || oe.Class != ifaces.FailureRateLimited || oe.RetryAfter != 23*time.Second || oe.Ref != rangeRef() {
		t.Fatalf("missing token failure details: %+v, err=%v", oe, err)
	}
}

func TestRangeReadCloserReadFullDetectsOversize(t *testing.T) {
	body := &rangeReadCloser{ReadCloser: io.NopCloser(strings.NewReader("2345")), ref: rangeRef(), remaining: 3}

	_, err := io.ReadFull(body, make([]byte, 3))
	if err == nil {
		t.Fatal("ReadFull hid oversized body")
	}
}

func TestOpenRangeCancellationAndStreaming(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "2")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	body, err := c.OpenRange(ctx, rangeRef(), 2, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()

	var first [1]byte
	if _, err := io.ReadFull(body, first[:]); err != nil || first[0] != '2' {
		t.Fatalf("first byte = %q, err=%v", first, err)
	}

	cancel()

	if _, err := io.ReadAll(body); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestBoundedAPIInvalidReference(t *testing.T) {
	var requests atomic.Int32

	c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })

	for _, invalid := range []string{"repository", "registry", "digest"} {
		ref := rangeRef()

		switch invalid {
		case "repository":
			ref.Repository = "../../etc"
		case "registry":
			ref.Registry = "unknown"
		case "digest":
			ref = ifaces.OriginRef{Registry: "reg", Repository: "repo"}
		}

		_, headErr := c.HeadMetadata(context.Background(), ref)

		_, rangeErr := c.OpenRange(context.Background(), ref, 2, 3, 10)
		if headErr == nil || rangeErr == nil {
			t.Fatalf("%s HEAD=%v range=%v", invalid, headErr, rangeErr)
		}
	}

	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestRangeResponseMalformedHeaders(t *testing.T) {
	for _, headers := range []http.Header{
		{"Content-Range": {"bytes 2-4/10", "bytes 2-4/11"}},
		{"Content-Range": {"bytes 2-4/10"}, "Content-Length": {"+3"}},
		{"Content-Range": {"bytes 2-4/10"}, "Content-Encoding": {"identity", "gzip"}},
		{"Content-Range": {"bytes 2-4/9223372036854775808"}},
	} {
		resp := &http.Response{StatusCode: 206, Header: headers}
		if err := validateRangeResponse(resp, 2, 3, 10); err == nil {
			t.Fatalf("accepted headers %v", headers)
		}
	}
}

func TestOpenRangeHTTPFailureDetails(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		for _, retry := range []string{"19", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), "invalid", "-1"} {
			t.Run(fmt.Sprintf("%d/%s", status, retry), func(t *testing.T) {
				c, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", retry)
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					w.WriteHeader(status)
				})
				_, err := c.OpenRange(context.Background(), rangeRef(), 2, 3, 10)

				var oe *ifaces.OriginError
				if !errors.As(err, &oe) || oe.StatusCode != status || oe.RetryAfterHeader != retry {
					t.Fatalf("failure=%+v err=%v", oe, err)
				}

				if retry == "19" && oe.RetryAfter != 19*time.Second {
					t.Fatalf("delay=%v", oe.RetryAfter)
				}

				if strings.Contains(retry, "GMT") && (oe.RetryAfter < 59*time.Minute || oe.RetryAfter > time.Hour) {
					t.Fatalf("date delay=%v", oe.RetryAfter)
				}

				if (retry == "invalid" || retry == "-1") && oe.RetryAfter != 0 {
					t.Fatalf("invalid delay=%v", oe.RetryAfter)
				}

				if status == 403 && (oe.Class != ifaces.FailureAuth || oe.Challenge == "") {
					t.Fatalf("auth=%+v", oe)
				}
			})
		}
	}
}
