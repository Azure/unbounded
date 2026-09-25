// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type pipelineResult struct {
	stats   sdk.TransferStats
	partial bool
	err     error
}

func pipelineReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not make bounded progress")

		var zero T

		return zero
	}
}

// A current-page body cannot arrive until the next GET starts. This checks the
// actual mirror/backend/SDK socket path, rather than emulating its scheduler.
func TestRacerPipelineBoundaries(t *testing.T) {
	for _, mode := range []string{"success", "retry", "forbidden", "version", "truncated", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			d := digestOf([]byte("pipeline fixture"))

			var (
				next atomic.Int64
				gate atomic.Pointer[chan struct{}]
			)

			started := make(chan struct{})
			gate.Store(&started)

			canceled := make(chan struct{}, 32)
			client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Racer-Origin-Data") != base64.StdEncoding.EncodeToString([]byte("Bearer pipeline")) || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("lost pinned origin credentials/encoding")
				}

				w.Header().Set("ETag", `"`+d.Hex()+`"`)
				w.Header().Set("Content-Type", "application/octet-stream")

				if r.Method == "HEAD" {
					w.Header().Set("Content-Length", fmt.Sprint(sdk.PageSize+8))
					return
				}

				if r.Header.Get("If-Match") != `"`+d.Hex()+`"` {
					t.Error("unpinned page")
				}

				w.Header().Set("Content-Length", "8")

				first := r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", sdk.PageSize-8, sdk.PageSize-1)
				if first {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sdk.PageSize-8, sdk.PageSize-1, sdk.PageSize+8))
					w.WriteHeader(206)
					w.(http.Flusher).Flush()

					select {
					case <-*gate.Load():
					case <-r.Context().Done():
						return
					}
				} else {
					if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", sdk.PageSize, sdk.PageSize+7) {
						t.Error("unexpected page range", r.Header.Get("Range"))
					}

					attempt := next.Add(1)
					if mode != "retry" || attempt == 1 {
						close(*gate.Load())
					}

					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sdk.PageSize, sdk.PageSize+7, sdk.PageSize+8))

					switch mode {
					case "retry":
						if attempt == 1 {
							w.Header().Set("Content-Length", "0")
							w.Header().Set("Retry-After", "0")
							w.WriteHeader(503)

							return
						}
					case "forbidden":
						w.WriteHeader(403)
						return
					case "version":
						w.Header().Set("ETag", `"`+digestOf([]byte("changed")).Hex()+`"`)
					}

					if mode != "cancel" {
						w.WriteHeader(206)
					}
				}

				if mode == "cancel" {
					<-r.Context().Done()

					canceled <- struct{}{}

					return
				}

				body := "abcdefgh"
				if first {
					body = "12345678"
				} else if mode == "truncated" {
					body = "abc"
				}

				_, _ = io.WriteString(w, body)
			}), sdk.ClientOptions{Timeout: 5 * time.Second, PageLookahead: true})
			results := make(chan pipelineResult, 1)
			registry := &metadataOnlyRegistry{}
			cfg := reviewConfig()
			cfg.RacerMaxConcurrentTransfers = 1
			server := mirror.NewRacer(cfg, registry, &gantryracer.Backend{Client: client}, mirror.WithRacerMetrics(func(s sdk.TransferStats, p bool, err error) {
				results <- pipelineResult{s, p, err}
			}))
			finished := make(chan struct{}, 1)

			m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server.Handler().ServeHTTP(w, r)

				finished <- struct{}{}
			}))
			defer m.Close()

			iterations := 1
			if mode == "cancel" {
				iterations = 10 // Exceeds the shared eight-slot speculative budget.
			}

			for i := range iterations {
				if i != 0 {
					ch := make(chan struct{})
					gate.Store(&ch)
				}

				r, err := http.NewRequestWithContext(t.Context(), "GET", m.URL+"/v2/repo/blobs/"+d.String(), nil)
				if err != nil {
					t.Fatal(err)
				}

				r.Header.Set("Authorization", "Bearer pipeline")
				r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", sdk.PageSize-8, sdk.PageSize+7))

				resp, err := m.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}

				if resp.StatusCode != 206 || resp.ContentLength != 16 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", sdk.PageSize-8, sdk.PageSize+7, sdk.PageSize+8) || resp.Header.Get("Docker-Content-Digest") != d.String() || resp.Header.Get("ETag") != `"`+d.Hex()+`"` || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Type") != "application/octet-stream" {
					t.Fatal("changed mirror framing", resp.Status, resp.Header)
				}

				if mode == "cancel" {
					pipelineReceive(t, *gate.Load())

					_ = resp.Body.Close()

					pipelineReceive(t, canceled)
					pipelineReceive(t, canceled)
				} else {
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()

					want := "12345678abcdefgh"
					if mode == "forbidden" || mode == "version" {
						want = "12345678"
					}

					if mode == "truncated" {
						want = "12345678abc"
					}

					if string(body) != want || (readErr != nil) != (mode != "success" && mode != "retry") {
						t.Fatal("replayed or misordered response", string(body), readErr)
					}
				}

				result := pipelineReceive(t, results)
				pipelineReceive(t, finished)

				requests, retries := int64(2), int64(0)
				if mode == "retry" {
					requests, retries = 3, 1
				}

				if result.stats.PageRequests != requests || result.stats.PageRetries != retries || !result.partial || result.stats.PageHeaderWait <= 0 || result.stats.ForwardDuration < 0 || (result.err != nil) != (mode != "success" && mode != "retry") {
					t.Fatal("incorrect final observable stats", result)
				}
			}
		})
	}
}

func TestRacerPipelineFullDigestAndConcurrentRanges(t *testing.T) {
	data := make([]byte, sdk.PageSize+31)
	for i := range data {
		data[i] = byte((i*17 ^ i>>11) % 251)
	}

	d := digestOf(data)
	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}), sdk.ClientOptions{Timeout: 10 * time.Second, PageLookahead: true})
	results := make(chan pipelineResult, 16)
	server := mirror.NewRacer(reviewConfig(), &metadataOnlyRegistry{}, &gantryracer.Backend{Client: client}, mirror.WithRacerMetrics(func(s sdk.TransferStats, p bool, err error) {
		results <- pipelineResult{s, p, err}
	}))

	m := httptest.NewServer(server.Handler())
	defer m.Close()

	get := func(partial bool) {
		r, err := http.NewRequestWithContext(t.Context(), "GET", m.URL+"/v2/repo/blobs/"+d.String(), nil)
		if err != nil {
			t.Error(err)
			return
		}

		want := data

		if partial {
			r.Header.Set("Range", fmt.Sprintf("bytes=%d-", sdk.PageSize-17))
			want = data[sdk.PageSize-17:]
		}

		resp, err := m.Client().Do(r)
		if err != nil {
			t.Error(err)
			return
		}

		body, err := io.ReadAll(resp.Body)

		_ = resp.Body.Close()

		status := http.StatusOK
		if partial {
			status = http.StatusPartialContent
		}

		if err != nil || resp.StatusCode != status || !bytes.Equal(body, want) || resp.ContentLength != int64(len(want)) || resp.Header.Get("Docker-Content-Digest") != d.String() {
			t.Error("incorrect pipelined payload", err, len(body), resp.Header)
		}

		if !partial && digestOf(body) != d {
			t.Error("full digest mismatch")
		}
	}
	get(false)

	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() { get(true) })
	}

	workers.Wait()

	for i := range 13 {
		result := pipelineReceive(t, results)

		length := int64(len(data))
		if i != 0 {
			length = 48
		}

		if result.err != nil || result.partial != (i != 0) || result.stats.PageRequests != 2 || result.stats.PageRetries != 0 || result.stats.SpliceBytes+result.stats.BufferedBytes != length {
			t.Fatal(result)
		}
	}
}
