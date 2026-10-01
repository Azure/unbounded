// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// Deliberately hides ReaderFrom while preserving ResponseController operations.
type racerHTTPFallback struct{ http.ResponseWriter }

func (w racerHTTPFallback) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestRacerStreamingHTTPReuse(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("0123456789abcdef"), int(racersdk.PageSize)/16+4096)
			d := racerDigest(data)
			client := racerFakeClient(t, racerPageOrigin(t, d, data))
			observed := make(chan mirror.RacerHTTPObservation, 1)
			handler := mirror.RacerHTTPHandler(mirror.New(racerConfig(), &racerLegacyTrap{}, &racerLegacyTrap{}, mirror.WithRacer(client)).Handler(), time.Second, func(o mirror.RacerHTTPObservation) { observed <- o })

			var connections atomic.Int32

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "fallback" {
					w = racerHTTPFallback{w}
				}

				handler.ServeHTTP(w, r)
			}))

			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			if mode == "TLS" {
				server.StartTLS()
			} else {
				server.Start()
			}

			t.Cleanup(server.Close)
			server.Client().Timeout = 10 * time.Second

			for _, offset := range []int{0, int(racersdk.PageSize) - 7, len(data) - 1} {
				rangeHeader, status := "", http.StatusOK
				if offset != 0 {
					rangeHeader, status = fmt.Sprintf("bytes=%d-", offset), http.StatusPartialContent
				}

				resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()

				if err != nil || !bytes.Equal(got, data[offset:]) || resp.StatusCode != status || resp.ContentLength != int64(len(data)-offset) || resp.ProtoMajor != 1 || resp.Close {
					t.Fatalf("offset=%d status=%d bytes=%d proto=%s close=%v err=%v", offset, resp.StatusCode, len(got), resp.Proto, resp.Close, err)
				}

				if offset != 0 && resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)) {
					t.Fatal("incorrect range headers", resp.Header)
				}

				select {
				case o := <-observed:
					if o.Aborted || o.Status != status || o.Bytes != int64(len(got)) {
						t.Fatalf("observation=%+v", o)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("missing observation")
				}
			}

			if connections.Load() != 1 {
				t.Fatalf("successful full/range responses did not reuse connection: %d", connections.Load())
			}
		})
	}
}

func TestRacerStreamingHTTPTruncation(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		for _, failure := range []string{"mid-page", "terminal origin error"} {
			for _, offset := range []int{0, 65536} {
				t.Run(fmt.Sprintf("%s/%s/%d", mode, failure, offset), func(t *testing.T) {
					data := bytes.Repeat([]byte("0123456789abcdef"), 64*1024)
					d := racerDigest(data)
					metadata := racerMetadata(t, d, len(data))
					client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
						if req.Operation() == racersdk.OperationHead {
							return metadata, nil, nil
						}

						payload := data
						if failure == "mid-page" {
							payload = data[:len(data)/2]
						}
						// A terminal origin error prevents the fake from sending Complete.
						// ServeOrigin also gates its final byte, so malformed Complete
						// after a full page is covered by the SDK's raw protocol tests.
						return metadata, io.NopCloser(io.MultiReader(bytes.NewReader(payload), racerReadError{})), nil
					})
					observed := make(chan mirror.RacerHTTPObservation, 1)
					handler := mirror.RacerHTTPHandler(mirror.New(racerConfig(), &racerLegacyTrap{}, &racerLegacyTrap{}, mirror.WithRacer(client)).Handler(), time.Second, func(o mirror.RacerHTTPObservation) { observed <- o })

					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if mode == "fallback" {
							w = racerHTTPFallback{w}
						}

						handler.ServeHTTP(w, r)
					}))
					if mode == "TLS" {
						server.StartTLS()
					} else {
						server.Start()
					}

					t.Cleanup(server.Close)
					server.Client().Timeout = 10 * time.Second

					rangeHeader, status := "", http.StatusOK
					if offset != 0 {
						rangeHeader, status = fmt.Sprintf("bytes=%d-", offset), http.StatusPartialContent
					}

					resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()

					if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) == 0 || len(got) >= len(data)-offset || !bytes.Equal(got, data[offset:offset+len(got)]) || resp.StatusCode != status {
						t.Fatalf("failed stream: status=%d bytes=%d err=%v", resp.StatusCode, len(got), err)
					}

					select {
					case o := <-observed:
						if !o.Aborted || o.Status != status || o.Bytes < int64(len(got)) || o.Bytes >= int64(len(data)-offset) {
							t.Fatalf("observation=%+v", o)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("missing abort observation")
					}

					if stats := client.Stats(); stats.ActiveBulk != 0 {
						t.Fatalf("aborted stream retained SDK capacity: %+v", stats)
					}
				})
			}
		}
	}
}
