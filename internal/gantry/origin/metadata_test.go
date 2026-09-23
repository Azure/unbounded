// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestPullWithMetadataPreservesGETAndAccounting(t *testing.T) {
	for _, contentType := range []string{"", "application/vnd.oci.image.index.v1+json; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			var gets, starts, readBytes atomic.Int64

			client, _ := rangeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("metadata API probed HEAD")
				}

				gets.Add(1)

				if strings.Contains(r.URL.Path, "/blobs/") {
					w.WriteHeader(404)
					return
				}

				w.Header()["Content-Type"] = nil
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				}

				_, _ = io.WriteString(w, "payload")
			})
			client.metrics.onPullStart = func(string) { starts.Add(1) }
			client.metrics.onBytesRead = func(_ string, n int64) { readBytes.Add(n) }

			body, size, gotType, err := client.PullWithMetadata(t.Context(), rangeRef())
			if err != nil {
				t.Fatal(err)
			}

			got, err := io.ReadAll(body)
			_ = body.Close()

			if err != nil || size != 7 || string(got) != "payload" || gotType != contentType || gets.Load() != 2 || starts.Load() != 1 || readBytes.Load() != 7 {
				t.Fatal(size, gotType, err, gets.Load(), starts.Load(), readBytes.Load())
			}
		})
	}
}

func TestPullWithMetadataFailure(t *testing.T) {
	client, _ := rangeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://registry.example/token"`)
		w.WriteHeader(403)
	})

	body, _, _, err := client.PullWithMetadata(t.Context(), rangeRef())
	if body != nil || err == nil {
		t.Fatal(body, err)
	}

	var originError *ifaces.OriginError
	if !errors.As(err, &originError) || originError.StatusCode != 403 || originError.Challenge == "" {
		t.Fatal(err)
	}
}
