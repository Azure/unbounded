// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

// Exercise the actual hijacked TCP/splice path with a complete verified object,
// not only a tiny partial range. Each page fails once before returning payload.
func TestRacerPinnedRetryCompletesVerifiedObject(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux splice and proc fd paths")
	}

	payload := make([]byte, sdk.PageSize+17)
	for i := range payload {
		payload[i] = byte(i * 31)
	}

	d := digestOf(payload)

	var heads, first, later atomic.Int32

	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == "HEAD" {
			heads.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))

			return
		}

		if r.Header.Get("If-Match") != `"`+d.Hex()+`"` {
			t.Error("version pin lost")
		}

		start, end, count := int64(0), sdk.PageSize-1, &first
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", sdk.PageSize, len(payload)-1) {
			start, end, count = sdk.PageSize, int64(len(payload)-1), &later
		} else if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", sdk.PageSize-1) {
			t.Error("range changed", r.Header)
		}

		if count.Add(1) == 1 {
			w.Header().Set("Content-Length", "0")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)

			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(206)
		_, _ = w.Write(payload[start : end+1])
	}))
	up := &authorizationCapturingOrigin{seen: make(chan string, 1)}
	finished := make(chan sdk.TransferStats, 1)
	server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client}, mirror.WithRacerMetrics(func(stats sdk.TransferStats, _ bool, err error) {
		if err != nil {
			t.Error(err)
		}

		finished <- stats
	}, nil))

	m := httptest.NewServer(server.Handler())
	defer m.Close()

	resp, err := m.Client().Get(m.URL + "/v2/repo/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	_ = resp.Body.Close()

	stats := <-finished
	if err != nil || resp.StatusCode != 200 || n != int64(len(payload)) || fmt.Sprintf("sha256:%x", h.Sum(nil)) != d.String() || heads.Load() != 1 || first.Load() != 2 || later.Load() != 2 || stats.SpliceBytes == 0 || len(up.seen) != 0 {
		t.Fatal("retry truncated/corrupted/replayed object or fell back", resp.Status, n, err, stats, heads.Load(), first.Load(), later.Load())
	}
}
