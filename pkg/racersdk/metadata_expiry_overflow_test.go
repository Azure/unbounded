// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func expiryOverflowCases() []struct {
	name   string
	expiry time.Time
	valid  bool
	millis int64
} {
	return []struct {
		name   string
		expiry time.Time
		valid  bool
		millis int64
	}{
		{"zero time", time.Time{}, false, 0},
		{"negative millisecond", time.UnixMilli(-1), false, 0},
		{"negative nanosecond", time.Unix(0, -1), false, 0},
		{"negative wraparound", time.Unix(-18446744073709551, 0), false, 0},
		{"far future wraparound", time.Unix(18446744073709552, 0), false, 0},
		{"seconds overflow", time.Unix(math.MaxInt64/1000+1, 0), false, 0},
		{"milliseconds overflow", time.UnixMilli(math.MaxInt64).Add(time.Millisecond), false, 0},
		{"epoch", time.Unix(0, 0), true, 0},
		{"epoch submillisecond", time.Unix(0, 999999), true, 0},
		{"truncate milliseconds", time.Unix(123, 456789123), true, 123456},
		{"time zone", time.Unix(123, 456789123).In(time.FixedZone("offset", 3600)), true, 123456},
		{"maximum seconds", time.Unix(math.MaxInt64/1000, 0), true, math.MaxInt64 / 1000 * 1000},
		{"below maximum", time.UnixMilli(math.MaxInt64 - 1), true, math.MaxInt64 - 1},
		{"maximum", time.UnixMilli(math.MaxInt64), true, math.MaxInt64},
		{"maximum submillisecond", time.UnixMilli(math.MaxInt64).Add(time.Millisecond - time.Nanosecond), true, math.MaxInt64},
	}
}

func TestMetadataWireExpiryOverflow(t *testing.T) {
	for _, tt := range expiryOverflowCases() {
		t.Run(tt.name, func(t *testing.T) {
			metadata := Metadata{Size: 3, ETag: `"v"`, ContentType: "text/plain", ExpiresAt: tt.expiry}
			got := metadata.wire()
			require.EqualValues(t, metadata.Size, got.Size)
			require.Equal(t, metadata.ETag, got.ETag)
			require.Equal(t, metadata.ContentType, got.ContentType)

			if !tt.valid {
				require.Error(t, got.Validate())
				return
			}

			require.NoError(t, got.Validate())
			require.Equal(t, time.UnixMilli(tt.millis).UTC(), got.ExpiresAt)
		})
	}
}

func TestOriginExpiryOverflow(t *testing.T) {
	for _, tt := range expiryOverflowCases() {
		t.Run(tt.name, func(t *testing.T) {
			for _, mode := range []string{"head", "get", "range error"} {
				t.Run(mode, func(t *testing.T) {
					path, cancel, done := startOrigin(t, nil, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
						metadata := Metadata{ETag: `"v"`, ExpiresAt: tt.expiry}
						if mode == "range error" {
							return metadata, nil, ErrRangeNotSatisfiable
						}

						return metadata, nil, nil
					})

					defer func() { cancel(); <-done }()

					method, fields := "GET", "Range: bytes=0-16777215\r\n"
					if mode == "head" {
						method, fields = "HEAD", ""
					}

					response := originExchange(t, path, method, fields)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.Empty(t, body)
					require.Zero(t, response.ContentLength)

					if !tt.valid {
						require.Equal(t, http.StatusBadGateway, response.StatusCode)
						require.Empty(t, response.Header.Get("Racer-Expires-At"))
						require.Empty(t, response.Header.Get("ETag"))
						require.Empty(t, response.Header.Get("Content-Range"))

						return
					}

					if mode == "range error" {
						require.Equal(t, http.StatusRequestedRangeNotSatisfiable, response.StatusCode)
						require.Equal(t, "bytes */0", response.Header.Get("Content-Range"))

						return
					}

					require.Equal(t, http.StatusOK, response.StatusCode)
					require.Equal(t, strconv.FormatInt(tt.millis, 10), response.Header.Get("Racer-Expires-At"))
				})
			}
		})
	}
}
