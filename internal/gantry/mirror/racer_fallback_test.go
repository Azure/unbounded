// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

// Keep the former fallback regressions, but enforce fail-closed responses.
func TestRacerFailuresNeverBypass(t *testing.T) {
	for _, mode := range []string{
		"503", "404", "401", "403", "429", "500", "502", "504", "416",
		"412", "timeout", "disconnect", "missing-length", "wrong-etag",
		"missing-etag", "invalid-range", "oversized-error-header",
	} {
		t.Run(mode, func(t *testing.T) {
			for _, phase := range []string{"metadata", "prepare"} {
				t.Run(phase, func(t *testing.T) {
					for _, method := range []string{http.MethodHead, http.MethodGet} {
						if method == http.MethodHead && phase == "prepare" {
							continue
						}

						for _, kind := range []string{"manifests", "config", "layer"} {
							t.Run(method+"/"+kind, func(t *testing.T) {
								data := []byte("cached " + kind)
								d := digestOf(data)

								var requests atomic.Int64

								client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									requests.Add(1)

									auth, err := base64.StdEncoding.DecodeString(r.Header.Get("Racer-Origin-Data"))
									if err != nil || string(auth) != "Bearer request-secret" || r.Header.Get("Authorization") != "" {
										t.Error("incorrect credential delegation", err)
									}

									w.Header().Set("ETag", `"`+d.Hex()+`"`)

									if phase == "prepare" && r.Method == http.MethodHead {
										w.Header().Set("Content-Length", fmt.Sprint(len(data)))
										return
									}
									// These untrusted fields must never become error headers/body.
									w.Header().Set("Racer-Origin-Data", "request-secret")
									w.Header().Set("Authorization", "Bearer request-secret")
									w.Header().Set("Content-Type", "application/secret")
									w.Header().Set("Content-Range", "bytes 0-1/2")

									switch mode {
									case "timeout":
										<-r.Context().Done()
										return
									case "disconnect":
										conn, _, err := http.NewResponseController(w).Hijack()
										if err == nil {
											_ = conn.Close()
										}

										return
									case "missing-length":
										w.WriteHeader(200)
										w.(http.Flusher).Flush()

										return
									case "wrong-etag":
										w.Header().Set("ETag", `"wrong"`)
									case "missing-etag":
										w.Header().Del("ETag")
									case "oversized-error-header":
										w.Header().Set("WWW-Authenticate", strings.Repeat("x", 2048))
										w.WriteHeader(http.StatusUnauthorized)

										return
									case "invalid-range":
										w.Header().Set("Content-Length", fmt.Sprint(len(data)))
										w.WriteHeader(206)

										return
									default:
										var status int

										_, _ = fmt.Sscan(mode, &status)

										w.Header().Set("WWW-Authenticate", `Bearer realm="https://registry/token"`)
										w.Header().Set("Retry-After", "7")
										w.WriteHeader(status)
										_, _ = io.WriteString(w, "request-secret")

										return
									}

									w.Header().Set("Content-Length", fmt.Sprint(len(data)))
									_, _ = w.Write(data)
								}))
								up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 16)}
								// No direct store dependency exists to bypass Racer on failure.

								cfg := reviewConfig()
								cfg.RacerMetadataTimeout = 100 * time.Millisecond
								cfg.PeerFetchTimeout = 200 * time.Millisecond

								var fallbacks, completed int

								server := mirror.NewRacer(cfg, up, &gantryracer.Backend{Client: client},
									mirror.WithRacerMetrics(nil, func() { fallbacks++ }),
									mirror.WithLiveStreamCompletedHook(func(digest.Digest) { completed++ }))

								pathKind := "blobs"
								if kind == "manifests" {
									pathKind = kind
								}

								r := httptest.NewRequest(method, "/v2/repo/"+pathKind+"/"+d.String(), nil)
								r.Header.Set("Authorization", "Bearer request-secret")

								w := httptest.NewRecorder()
								server.Handler().ServeHTTP(w, r)

								want := 503

								switch mode {
								case "401", "403", "404", "429":
									_, _ = fmt.Sscan(mode, &want)
								}

								if w.Code != want || requests.Load() == 0 || len(up.seen) != 0 || fallbacks != 0 || completed != 0 {
									t.Fatal("failure bypassed Racer or reported completion", w.Code, requests.Load(), len(up.seen), fallbacks, completed)
								}

								for _, h := range []string{"ETag", "Content-Range", "Docker-Content-Digest", "Accept-Ranges", "Racer-Origin-Data", "Authorization"} {
									if w.Header().Get(h) != "" {
										t.Error("leaked header", h)
									}
								}

								if strings.Contains(w.Body.String(), "request-secret") || w.Header().Get("Content-Type") == "application/secret" {
									t.Fatal("upstream error content leaked")
								}

								if len(mode) == 3 && w.Header().Get("Retry-After") != "7" {
									t.Fatal("lost retry hint", w.Header())
								}

								if mode == "401" || mode == "403" {
									if w.Header().Get("WWW-Authenticate") != `Bearer realm="https://registry/token"` || w.Header().Get("Retry-After") != "7" {
										t.Fatal("lost authentication metadata", w.Header())
									}
								} else if w.Header().Get("WWW-Authenticate") != "" {
									t.Fatal("unrelated challenge leaked")
								}
							})
						}
					}
				})
			}
		})
	}
}

func TestRacerRefusedSocketNeverBypasses(t *testing.T) {
	// Unavailable sockets must fail for every digest kind and method.
	for _, mode := range []string{"HEAD/manifests", "GET/manifests", "HEAD/config", "GET/config", "HEAD/layer", "GET/layer"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp(".", ".racer-refused-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)

			folder, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer folder.Close()

			path := fmt.Sprintf("/proc/self/fd/%d/client", folder.Fd())

			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}

			listener.(*net.UnixListener).SetUnlinkOnClose(false)
			_ = listener.Close()

			client, err := sdk.NewClient(path, sdk.ClientOptions{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()

			up := &authorizationCapturingOrigin{seen: make(chan string, 4)}
			server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client})

			method, kind, _ := strings.Cut(mode, "/")
			if kind != "manifests" {
				kind = "blobs"
			}

			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, httptest.NewRequest(method, "/v2/repo/"+kind+"/"+digestOf(nil).String(), nil))

			if w.Code != 503 || len(up.seen) != 0 {
				t.Fatal("refused socket bypassed Racer", w.Code)
			}
		})
	}
}

func TestRacerUnavailableRejectsCloseDelimitedGET(t *testing.T) {
	up := &metadataOnlyRegistry{authorizationCapturingOrigin{seen: make(chan string, 1)}}
	server := mirror.NewRacer(reviewConfig(), up, nil)
	req := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+digestOf(nil).String(), nil)
	req.Proto, req.ProtoMinor = "HTTP/1.0", 0
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable || len(up.seen) != 0 {
		t.Fatal("close-delimited fallback was allowed", w.Code)
	}
}

func TestRacerInvalidClientRangeDoesNotFetch(t *testing.T) {
	data := []byte("Racer range content")
	d := digestOf(data)

	var gets atomic.Int64

	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}

		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	up := &authorizationCapturingOrigin{seen: make(chan string, 4)}
	server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+d.String(), nil)
	r.Header.Set("Range", "bytes=999-1000")
	server.Handler().ServeHTTP(w, r)

	if w.Code != 416 || w.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", len(data)) || gets.Load() != 0 || len(up.seen) != 0 {
		t.Fatal("invalid client range fetched content", w.Code, w.Header(), gets.Load())
	}
}

func TestRacerTruncatedBodyAbortsAfterHeaders(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 128<<10)
	d := digestOf(data)
	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))

		if r.Method == http.MethodHead {
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[:len(data)/2])
	}))
	up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 4)}

	var completed, fallbacks int

	result := make(chan error, 1)
	server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client},
		mirror.WithRacerMetrics(func(_ sdk.TransferStats, _ bool, err error) { result <- err }, func() { fallbacks++ }),
		mirror.WithLiveStreamCompletedHook(func(digest.Digest) { completed++ }))
	finished := make(chan struct{})

	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)

		server.Handler().ServeHTTP(w, r)
	}))
	defer m.Close()

	resp, err := m.Client().Get(m.URL + "/v2/repo/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	<-finished

	if resp.StatusCode != 200 || readErr == nil || !bytes.Equal(body, data[:len(data)/2]) || <-result == nil || completed != 0 || fallbacks != 0 || len(up.seen) != 0 {
		t.Fatal("truncated Racer response was completed or substituted", resp.Status, readErr, len(body), completed, fallbacks)
	}
}

func TestRacerAndDirectBackendDigestSuccess(t *testing.T) {
	for _, backend := range []string{"racer", "direct"} {
		for _, kind := range []string{"manifests", "config", "layer"} {
			for _, method := range []string{http.MethodHead, http.MethodGet} {
				t.Run(backend+"/"+kind+"/"+method, func(t *testing.T) {
					data := []byte("content for " + kind)
					d := digestOf(data)

					var requests atomic.Int64

					client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						w.Header().Set("ETag", `"`+d.Hex()+`"`)
						http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
					}))
					up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 4)}
					cfg := reviewConfig()
					cfg.ContentBackend = backend
					store := fakes.NewCache()

					var server *mirror.Server

					if backend == "racer" {
						server = mirror.NewRacer(cfg, up, &gantryracer.Backend{Client: client})
					} else {
						server = mirror.New(cfg, store, up)
					}

					m := httptest.NewServer(server.Handler())
					defer m.Close()

					pathKind := "blobs"
					if kind == "manifests" {
						pathKind = kind
					}

					r, err := http.NewRequestWithContext(t.Context(), method, m.URL+"/v2/repo/"+pathKind+"/"+d.String(), nil)
					if err != nil {
						t.Fatal(err)
					}

					resp, err := m.Client().Do(r)
					if err != nil {
						t.Fatal(err)
					}

					body, err := io.ReadAll(resp.Body)

					_ = resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || resp.ContentLength != int64(len(data)) || method == http.MethodGet && !bytes.Equal(body, data) || method == http.MethodHead && len(body) != 0 {
						t.Fatal("digest response failed", resp.Status, resp.ContentLength, len(body), err)
					}

					if backend == "racer" && (requests.Load() == 0 || len(up.seen) != 0) || backend == "direct" && (requests.Load() != 0 || len(up.seen) != 1) {
						t.Fatal("incorrect content backend", requests.Load(), len(up.seen))
					}
				})
			}
		}
	}
}
