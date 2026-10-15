// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func TestSDKFullReadUnpinnedMetadataValidation(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-etag", "wrong-size", "incomplete"} {
		for _, verify := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/verify=%v", mode, verify), func(t *testing.T) {
				catalog, err := newBlobCatalog(t.Context(), "test/blob", "metadata", 1, 1024)
				require.NoError(t, err)
				origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
				require.NoError(t, err)

				var bootstrap, heads atomic.Int32

				client := racersdktest.NewClient(t, func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					if request.Head {
						heads.Add(1)
					}

					if !request.Head && request.ETag == "" {
						bootstrap.Add(1)
					}

					metadata, body, err := origin(ctx, request)

					switch mode {
					case "wrong-etag":
						metadata.ETag = `"wrong"`
					case "wrong-size":
						metadata.Size++
					case "incomplete":
						if body != nil {
							body = &corruptBody{Reader: io.LimitReader(body, 100), Closer: body}
						}
					}

					return metadata, body, err
				})
				p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
				p.opts.Verify = verify
				p.acquire = udsAcquirer(client)

				err = p.pullBatch(t.Context(), catalog.batches[0])
				if mode == "valid" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
				}

				if mode == "wrong-etag" {
					require.ErrorContains(t, err, "metadata ETag mismatch")
				}

				if mode == "incomplete" {
					require.Less(t, testutil.ToFloat64(metrics.receivedBytes), float64(1024), "SDK must not report a complete object for a truncated stream")
				}

				require.Equal(t, int32(1), bootstrap.Load(), "full reads select fresh metadata with an unpinned bootstrap")
				require.Zero(t, heads.Load(), "no pinned-read validation HEAD or separate Stat")
			})
		}
	}

	opts := pullTestOptions("http://unused")
	opts.Volume = "test"
	config := sdkClientConfig(opts, 64)
	require.Equal(t, 128, config.MaxConnections, "SDK configuration must preserve configured admission")
}

func TestUDSOriginProtocolReadiness(t *testing.T) {
	for _, mode := range []string{"success", "overlapping-head", "overlapping-wrong-expiry", "status", "etag", "size", "expiry", "wrong-expiry", "content-type", "no-witness", "wrong-witness", "disconnect", "startup-canceled", "lifetime-canceled"} {
		t.Run(mode, func(t *testing.T) {
			catalog, err := newBlobCatalog(t.Context(), "test/blob", "ready", 1, 1024)
			require.NoError(t, err)
			origin, key, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
			require.NoError(t, err)

			var calls atomic.Int64

			baseOrigin := origin
			origin = func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				metadata, body, err := baseOrigin(ctx, request)
				metadata.ExpiresAt = time.UnixMilli(1800000000000 + calls.Add(1))

				return metadata, body, err
			}

			path := filepath.Join(t.TempDir(), "socket")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			startup, stopStartup := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer stopStartup()

			var joined atomic.Bool

			serve := func(ctx context.Context, wrapped racersdk.Origin) error {
				defer joined.Store(true)

				callback := wrapped
				if mode == "no-witness" {
					callback = origin
				}

				client := racersdktest.NewClient(t, callback)
				defer client.Close()

				server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					request := racersdk.Request{Key: key, Metadata: r.Header.Get("Racer-Metadata")}
					if mode == "wrong-witness" {
						request.Metadata = "unrelated-head"
					}

					metadata, err := client.Stat(r.Context(), request)
					if err != nil {
						http.Error(w, "stat failed", http.StatusBadGateway)
						return
					}

					if strings.HasPrefix(mode, "overlapping-") {
						other, err := client.Stat(r.Context(), racersdk.Request{Key: key, Metadata: "unrelated-head"})
						if err != nil {
							http.Error(w, "overlapping stat failed", http.StatusBadGateway)
							return
						}

						if mode == "overlapping-wrong-expiry" {
							metadata.ExpiresAt = other.ExpiresAt
						}
					}
					// Real SDK callbacks have returned, but readiness must still wait
					// for a complete successful wire response from this Unix socket.
					if mode == "startup-canceled" {
						stopStartup()
						<-r.Context().Done()

						return
					}

					if mode == "lifetime-canceled" {
						cancel()
						<-r.Context().Done()

						return
					}

					if mode == "disconnect" {
						panic(http.ErrAbortHandler)
					}

					w.Header().Set("ETag", metadata.ETag)
					w.Header().Set("Content-Length", strconv.FormatUint(uint64(metadata.Size), 10))
					w.Header().Set("Racer-Expires-At", strconv.FormatInt(metadata.ExpiresAt.UnixMilli(), 10))
					w.Header().Set("Racer-Content-Type", metadata.ContentType)

					switch mode {
					case "status":
						w.WriteHeader(502)
						return
					case "etag":
						w.Header().Set("ETag", `"wrong"`)
					case "size":
						w.Header().Set("Content-Length", "999")
					case "expiry":
						w.Header().Set("Racer-Expires-At", "invalid")
					case "wrong-expiry":
						w.Header().Set("Racer-Expires-At", "0")
					case "content-type":
						w.Header().Set("Racer-Content-Type", "wrong/type")
					}

					w.WriteHeader(http.StatusOK)
				})}

				stop := context.AfterFunc(ctx, func() { _ = server.Close() })
				defer stop()
				defer server.Close()

				return server.Serve(listener)
			}
			failed := make(chan error, 1)

			stop, err := startUDSOriginOnPath(ctx, startup, path, origin, key, func(err error) { failed <- err }, serve)
			if mode == "success" || mode == "overlapping-head" {
				require.NoError(t, err)
				require.NotNil(t, stop)
				stop()
			} else {
				require.Error(t, err)
				require.Nil(t, stop)

				if strings.HasSuffix(mode, "canceled") {
					require.ErrorIs(t, err, context.Canceled)
				}

				if mode == "overlapping-wrong-expiry" {
					require.ErrorContains(t, err, "invalid expiry metadata")
				}
			}

			require.True(t, joined.Load(), "every exit joins serving before returning")

			select {
			case err := <-failed:
				t.Fatalf("unexpected serve failure: %v", err)
			default:
			}
		})
	}
}

func TestSDKFullReadContinues(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "continuation", 1, int64(racersdk.PageSize)+71)
	require.NoError(t, err)
	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)

	var bootstrap, pinned atomic.Int32

	client := racersdktest.NewClient(t, func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		switch {
		case !request.Head && request.ETag == "":
			bootstrap.Add(1)
		case !request.Head && request.ETag != "":
			pinned.Add(1)
		}

		return origin(ctx, request)
	})
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	p.acquire = udsAcquirer(client)
	require.NoError(t, p.pullBatch(t.Context(), catalog.batches[0]))
	require.Equal(t, float64(int64(racersdk.PageSize)+71), testutil.ToFloat64(metrics.verifiedBytes))
	require.Equal(t, int32(1), bootstrap.Load())
	require.Equal(t, int32(1), pinned.Load(), "SDK pins continuation pages to the version selected by unpinned bootstrap")
}
