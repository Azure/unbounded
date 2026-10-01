// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// Deliberately hides ReaderFrom while preserving ResponseController operations.
type racerHTTPFallback struct{ http.ResponseWriter }

func (w racerHTTPFallback) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Each block depends on both the object and its absolute offset, rather than
// repeating a pattern at copy-chunk or page boundaries.
func racerDistinctPayload(object uint64, size int) []byte {
	data := make([]byte, size)

	var seed [16]byte
	binary.LittleEndian.PutUint64(seed[:8], object)

	for offset := 0; offset < size; offset += sha256.Size {
		binary.LittleEndian.PutUint64(seed[8:], uint64(offset))
		block := sha256.Sum256(seed[:])
		copy(data[offset:], block[:])
	}

	return data
}

func racerDistinctOrigin(t *testing.T, objects ...[]byte) racersdk.Origin {
	t.Helper()

	origins := make(map[racersdk.Key]racersdk.Origin, len(objects))
	for _, data := range objects {
		d := racerDigest(data)

		key, err := racersdk.ParseKey(d.Hex())
		if err != nil {
			t.Fatal(err)
		}

		origins[key] = racerPageOrigin(t, d, data)
	}

	return func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		origin, ok := origins[req.Key()]
		if !ok {
			return racersdk.Metadata{}, nil, errors.New("unexpected object key")
		}

		return origin(ctx, req)
	}
}

// Hash the bytes actually consumed, independently of the response's digest
// headers and SDK metadata. Each invocation owns its hash state.
func racerCheckStreamHash(t *testing.T, resp *http.Response, data []byte, offset int) {
	t.Helper()

	defer resp.Body.Close()

	want := sha256.Sum256(data[offset:])
	hash := sha256.New()

	n, err := io.Copy(hash, resp.Body)
	if err != nil || n != int64(len(data)-offset) || !bytes.Equal(hash.Sum(nil), want[:]) {
		t.Fatalf("offset=%d bytes=%d want=%d sha256=%x want=%x err=%v", offset, n, len(data)-offset, hash.Sum(nil), want, err)
	}
}

func TestRacerDistinctPayloadDetectsAlignedSubstitution(t *testing.T) {
	data := racerDistinctPayload(1, int(racersdk.PageSize)+65539)
	other := racerDistinctPayload(2, len(data))

	want := sha256.Sum256(data)
	for _, offset := range []int{16, 32 * 1024, 256 * 1024, int(racersdk.PageSize)} {
		for _, source := range [][]byte{data, other} {
			corrupt := bytes.Clone(data)
			copy(corrupt[offset:offset+16], source[:16])

			if sha256.Sum256(corrupt) == want {
				t.Fatalf("payload hides aligned substitution at offset %d", offset)
			}
		}
	}

	if sha256.Sum256(other) == want {
		t.Fatal("payload hides whole-object substitution")
	}
}

func TestRacerStreamingHTTPReuse(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			objects := [][]byte{racerDistinctPayload(1, int(racersdk.PageSize)+65539), racerDistinctPayload(2, int(racersdk.PageSize)+65539)}
			client := racerFakeClient(t, racerDistinctOrigin(t, objects...))
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

			for _, offset := range []int{0, int(racersdk.PageSize) - 7, int(racersdk.PageSize), len(objects[0]) - 1} {
				for _, data := range objects {
					d := racerDigest(data)

					rangeHeader, status := "", http.StatusOK
					if offset != 0 {
						rangeHeader, status = fmt.Sprintf("bytes=%d-", offset), http.StatusPartialContent
					}

					resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")
					racerCheckStreamHash(t, resp, data, offset)

					if resp.StatusCode != status || resp.ContentLength != int64(len(data)-offset) || resp.ProtoMajor != 1 || resp.Close {
						t.Fatalf("offset=%d status=%d length=%d proto=%s close=%v", offset, resp.StatusCode, resp.ContentLength, resp.Proto, resp.Close)
					}

					if offset != 0 && resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)) {
						t.Fatal("incorrect range headers", resp.Header)
					}

					select {
					case o := <-observed:
						if o.Aborted || o.Status != status || o.Bytes != int64(len(data)-offset) {
							t.Fatalf("observation=%+v", o)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("missing observation")
					}
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
					data := racerDistinctPayload(3, 1024*1024)
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

// Hold an actual downstream Write until released or canceled. This makes
// backpressure deterministic without sleeps or assumptions about socket buffers.
// The held streams use Write; unheld streams retain their normal ReaderFrom path.
type racerHeldResponse struct {
	http.ResponseWriter
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
}

func (w *racerHeldResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *racerHeldResponse) Write(p []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)

		w.entered = nil
		select {
		case <-w.release:
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		}
	}

	return w.ResponseWriter.Write(p)
}

func racerAwaitStream(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestRacerStreamingHTTPConcurrentDistinctObjects(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			objects := [][]byte{racerDistinctPayload(11, 2*int(racersdk.PageSize)+65539), racerDistinctPayload(12, 2*int(racersdk.PageSize)+65539)}
			client := racerFakeClient(t, racerDistinctOrigin(t, objects...))
			entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			done := [2]chan struct{}{make(chan struct{}), make(chan struct{})}

			var aborted atomic.Int32

			handler := mirror.RacerHTTPHandler(mirror.New(racerConfig(), &racerLegacyTrap{}, &racerLegacyTrap{}, mirror.WithRacer(client)).Handler(), 10*time.Second, func(o mirror.RacerHTTPObservation) {
				if o.Aborted {
					aborted.Add(1)
				}
			})

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "fallback" {
					w = racerHTTPFallback{w}
				}

				if hold := r.Header.Get("X-Test-Hold"); hold != "" {
					i := 0
					if hold == "slow" {
						i = 1
					}
					defer close(done[i])

					w = &racerHeldResponse{ResponseWriter: w, ctx: r.Context(), entered: entered[i], release: release[i]}
				}

				handler.ServeHTTP(w, r)
			}))
			if mode == "TLS" {
				server.StartTLS()
			} else {
				server.Start()
			}

			t.Cleanup(server.Close)
			server.Client().Timeout = 20 * time.Second

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Ensure a failed assertion also unblocks the slow writer before Close.
			slowCtx, stopSlow := context.WithCancel(t.Context())
			defer stopSlow()

			startHeld := func(ctx context.Context, d digest.Digest, hold string) *http.Response {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/library/image/blobs/"+d.String(), nil)
				if err != nil {
					t.Fatal(err)
				}

				req.Header.Set("X-Test-Hold", hold)

				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { resp.Body.Close() })

				if resp.StatusCode != http.StatusOK {
					t.Fatalf("held response status=%d", resp.StatusCode)
				}

				return resp
			}
			victim := startHeld(ctx, racerDigest(objects[0]), "cancel")
			slow := startHeld(slowCtx, racerDigest(objects[1]), "slow")

			racerAwaitStream(t, entered[0], "cancelable downstream write")
			racerAwaitStream(t, entered[1], "backpressured downstream write")

			if stats := client.Stats(); stats.ActiveBulk != 2 {
				t.Fatalf("held streams not active: %+v", stats)
			}

			// A third stream completes while both earlier streams retain buffers.
			resp := racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[1]), "", "")
			racerCheckStreamHash(t, resp, objects[1], 0)
			cancel()

			_, err := io.Copy(io.Discard, victim.Body)
			victim.Body.Close()

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled read: %v", err)
			}

			racerAwaitStream(t, done[0], "canceled handler cleanup")

			if aborted.Load() != 1 {
				t.Fatalf("abort count=%d", aborted.Load())
			}

			if stats := client.Stats(); stats.ActiveBulk != 1 {
				t.Fatalf("canceled stream retained capacity: %+v", stats)
			}

			// Reuse released capacity for the other object at an unaligned page
			// boundary, then read the previously backpressured object to completion.
			offset := int(racersdk.PageSize) - 7

			resp = racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[0]), fmt.Sprintf("bytes=%d-", offset), "")
			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("range status=%d", resp.StatusCode)
			}

			racerCheckStreamHash(t, resp, objects[0], offset)
			close(release[1])
			racerCheckStreamHash(t, slow, objects[1], 0)
			racerAwaitStream(t, done[1], "slow handler completion")
			resp = racerRequest(t, server, http.MethodGet, "blobs", racerDigest(objects[0]), "", "")
			racerCheckStreamHash(t, resp, objects[0], 0)

			if stats := client.Stats(); stats.ActiveBulk != 0 {
				t.Fatalf("completed streams retained capacity: %+v", stats)
			}

			if aborted.Load() != 1 {
				t.Fatalf("healthy stream aborted: %d", aborted.Load())
			}
		})
	}
}
