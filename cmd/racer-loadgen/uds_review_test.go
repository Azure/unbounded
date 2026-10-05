// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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

				client, cleanup, err := racersdktest.NewClient(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					if request.Operation() == racersdk.OperationHead {
						heads.Add(1)
					}

					if request.Operation() == racersdk.OperationBootstrap {
						bootstrap.Add(1)

						if _, pinned := request.Pin(); pinned {
							return racersdk.Metadata{}, nil, errors.New("bootstrap unexpectedly pinned")
						}
					}

					metadata, body, err := origin(ctx, request)

					switch mode {
					case "wrong-etag":
						metadata.ETag, _ = racersdk.ParseETag(`"wrong"`)
					case "wrong-size":
						metadata.Size++
					case "incomplete":
						if body != nil {
							body = &corruptBody{Reader: io.LimitReader(body, 100), Closer: body}
						}
					}

					return metadata, body, err
				})
				require.NoError(t, err)
				t.Cleanup(cleanup)
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
					require.Zero(t, testutil.ToFloat64(metrics.receivedBytes), "SDK drops the incomplete page, even though origin delivered partial bytes")
				}

				require.Equal(t, int32(1), bootstrap.Load(), "full reads select fresh metadata with an unpinned bootstrap")
				require.Zero(t, heads.Load(), "no pinned-read validation HEAD or separate Stat")
			})
		}
	}

	options := udsReadOptions()
	require.Equal(t, 1, options.PageCredits)
	require.Zero(t, options.Pin)
	require.Nil(t, options.Metadata)

	opts := pullTestOptions("http://unused")
	opts.Cache = "test"
	config, err := sdkClientConfig(opts, 64)
	require.NoError(t, err)
	require.Equal(t, 1, config.PageWindow)
	require.Equal(t, 128, config.MaxConnections, "one credit must not reduce configured admission")
}

func TestUDSOriginProtocolReadiness(t *testing.T) {
	for _, mode := range []string{"success", "status", "etag", "size", "expiry", "wrong-expiry", "content-type", "no-witness", "disconnect", "startup-canceled", "lifetime-canceled"} {
		t.Run(mode, func(t *testing.T) {
			catalog, err := newBlobCatalog(t.Context(), "test/blob", "ready", 1, 1024)
			require.NoError(t, err)
			origin, key, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
			require.NoError(t, err)
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

				client, cleanup, err := racersdktest.NewClient(callback)
				if err != nil {
					return err
				}
				defer cleanup()

				server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					metadata, err := client.Stat(r.Context(), racersdk.Request{Key: key})
					if err != nil {
						http.Error(w, "stat failed", http.StatusBadGateway)
						return
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

					w.Header().Set("ETag", metadata.ETag.String())
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
			if mode == "success" {
				require.NoError(t, err)
				require.NotNil(t, stop)
				stop()
			} else {
				require.Error(t, err)
				require.Nil(t, stop)

				if strings.HasSuffix(mode, "canceled") {
					require.ErrorIs(t, err, context.Canceled)
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

func TestSDKSingleCreditFullReadContinues(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "single-credit", 1, int64(racersdk.PageSize)+71)
	require.NoError(t, err)
	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)

	var bootstrap, pinned atomic.Int32

	client, cleanup, err := racersdktest.NewClient(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		switch request.Operation() {
		case racersdk.OperationBootstrap:
			bootstrap.Add(1)
		case racersdk.OperationPinned:
			pinned.Add(1)
		}

		return origin(ctx, request)
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	p.acquire = udsAcquirer(client)
	require.NoError(t, p.pullBatch(t.Context(), catalog.batches[0]))
	require.Equal(t, float64(int64(racersdk.PageSize)+71), testutil.ToFloat64(metrics.verifiedBytes))
	require.Equal(t, int32(1), bootstrap.Load())
	require.Equal(t, int32(1), pinned.Load(), "SDK pins continuation pages to the version selected by unpinned bootstrap")
}
