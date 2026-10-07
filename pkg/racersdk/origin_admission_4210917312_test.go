// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOriginConfigAdmissionLimits(t *testing.T) {
	for _, tt := range []struct {
		configured  int
		requests    int
		connections int
	}{
		{0, 64, 128},
		{1, 1, 128},
		{64, 64, 128},
		{123, 123, 128},
		{124, 124, 129},
		{128, 128, 133},
		{256, 256, 261},
		{math.MaxInt - 5, math.MaxInt - 5, math.MaxInt},
	} {
		t.Run(strconv.Itoa(tt.configured), func(t *testing.T) {
			limits, err := (OriginConfig{Volume: "test", MaxConcurrentRequests: tt.configured}).limits()
			require.NoError(t, err)
			require.Equal(t, tt.requests, limits.maxRequests)
			require.Equal(t, 4, limits.maxHeadRequests)
			require.Equal(t, tt.connections, limits.maxConnections)
		})
	}

	for _, configured := range []int{-1, math.MaxInt - 4, math.MaxInt - 1, math.MaxInt} {
		t.Run("invalid/"+strconv.Itoa(configured), func(t *testing.T) {
			_, err := (OriginConfig{Volume: "test", MaxConcurrentRequests: configured}).limits()
			require.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
}

func TestOriginConfiguredAdmissionReservesMetadata(t *testing.T) {
	for _, configured := range []int{0, 1, 64, 128, 256} {
		t.Run(strconv.Itoa(configured), func(t *testing.T) {
			limits, err := (OriginConfig{Volume: "test", MaxConcurrentRequests: configured}).limits()
			require.NoError(t, err)

			headEntered := make(chan struct{}, limits.maxHeadRequests)
			releaseHeads := make(chan struct{})
			path, cancel, done := startOrigin(t, func(l *originLimits) { *l = limits }, func(ctx context.Context, request OriginRequest) (Metadata, io.ReadCloser, error) {
				if request.Head {
					headEntered <- struct{}{}

					select {
					case <-releaseHeads:
						return originMeta(1), nil, nil
					case <-ctx.Done():
						return Metadata{}, nil, ctx.Err()
					}
				}

				return originMeta(1), &blockedBody{done: make(chan struct{}), first: true}, nil
			})

			defer func() { cancel(); <-done }()

			for i := range limits.maxRequests {
				response := originExchange(t, path, "GET", "Range: bytes=0-16777215\r\n")
				require.Equal(t, http.StatusPartialContent, response.StatusCode, "content slot %d", i)
			}

			var heads []net.Conn

			for range limits.maxHeadRequests {
				conn := originDial(t, path)
				_, err := conn.Write(rawRequest("HEAD", ""))
				require.NoError(t, err)

				select {
				case <-headEntered:
				case <-time.After(3 * time.Second):
					t.Fatal("full content admission starved metadata")
				}

				heads = append(heads, conn)
			}

			for _, method := range []string{"GET", "HEAD"} {
				fields := "Connection: close\r\n"
				if method == "GET" {
					fields += "Range: bytes=0-16777215\r\n"
				}

				response := originExchange(t, path, method, fields)
				require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, "overload must reach request admission")
			}

			close(releaseHeads)

			for _, conn := range heads {
				response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "HEAD"})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.NoError(t, response.Body.Close())
			}
		})
	}
}
