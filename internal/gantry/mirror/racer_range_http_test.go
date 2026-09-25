// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type rangeHTTPStore struct {
	data     string
	opens    atomic.Int32
	resolved atomic.Int32
	closed   atomic.Int32
}

func (s *rangeHTTPStore) ResolveRange(context.Context, string, []byte) (sdk.ResolvedRange, error) {
	s.resolved.Add(1)

	return &rangeHTTPHandle{store: s}, nil
}

type rangeHTTPHandle struct {
	store *rangeHTTPStore
}

func (h *rangeHTTPHandle) Metadata() sdk.Metadata {
	return sdk.Metadata{
		Size:        int64(len(h.store.data)),
		ETag:        `"` + digestOf([]byte(h.store.data)).Hex() + `"`,
		ContentType: "application/octet-stream",
	}
}

func (h *rangeHTTPHandle) OpenRange(_ context.Context, offset, length int64) (io.ReadCloser, error) {
	h.store.opens.Add(1)

	return io.NopCloser(strings.NewReader(h.store.data[offset : offset+length])), nil
}

func (h *rangeHTTPHandle) Close() error {
	h.store.closed.Add(1)

	return nil
}

// Expected responses are explicit, independent of the shared range parser. The
// mirror uses a real SDK UDS client and hijacked TCP response; the other endpoint
// exercises the SDK origin directly with the same incoming headers.
func TestRacerAndSDKRangeHTTPContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UDS fixture uses Linux proc fd paths")
	}

	for _, data := range []string{"0123456789", ""} {
		name := "nonempty"
		if data == "" {
			name = "empty"
		}

		t.Run(name, func(t *testing.T) {
			store := &rangeHTTPStore{data: data}

			origin, err := sdk.NewRangeOrigin(store)
			if err != nil {
				t.Fatal(err)
			}

			cache := racerUDS(t, origin)
			up := &authorizationCapturingOrigin{seen: make(chan string, 1)}
			server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: cache})
			d := digestOf([]byte(data))
			tag := `"` + d.Hex() + `"`

			for _, endpoint := range []struct {
				name    string
				handler http.Handler
				path    string
			}{
				{"origin", origin, "/object"},
				{"mirror", server.Handler(), "/v2/repo/blobs/" + d.String()},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					finished := make(chan struct{}, 1)

					web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { finished <- struct{}{} }()

						endpoint.handler.ServeHTTP(w, r)
					}))
					defer web.Close()

					for _, tc := range []struct {
						name        string
						ranges      []string
						ifRange     []string
						head        bool
						status      int
						body        string
						cr          string
						emptyStatus int
					}{
						{name: "absent", status: 200, body: data},
						{name: "closed", ranges: []string{"bytes=2-5"}, status: 206, body: "2345", cr: "bytes 2-5/10", emptyStatus: 416},
						{name: "open", ranges: []string{"bytes=7-"}, status: 206, body: "789", cr: "bytes 7-9/10", emptyStatus: 416},
						{name: "suffix", ranges: []string{"bytes=-3"}, status: 206, body: "789", cr: "bytes 7-9/10", emptyStatus: 416},
						{name: "clip", ranges: []string{"bytes=7-99"}, status: 206, body: "789", cr: "bytes 7-9/10", emptyStatus: 416},
						{name: "case-insensitive", ranges: []string{"ByTeS=2-5"}, status: 206, body: "2345", cr: "bytes 2-5/10", emptyStatus: 416},
						{name: "outer-whitespace", ranges: []string{" \tbytes=2-5\t "}, status: 206, body: "2345", cr: "bytes 2-5/10", emptyStatus: 416},
						{name: "u64-end", ranges: []string{"bytes=7-18446744073709551615"}, status: 206, body: "789", cr: "bytes 7-9/10", emptyStatus: 416},
						{name: "u64-suffix", ranges: []string{"bytes=-18446744073709551615"}, status: 206, body: data, cr: "bytes 0-9/10", emptyStatus: 416},
						{name: "u64-start", ranges: []string{"bytes=18446744073709551615-"}, status: 416, cr: "bytes */10"},
						{name: "unsatisfiable", ranges: []string{"bytes=10-"}, status: 416, cr: "bytes */10"},
						{name: "zero-suffix", ranges: []string{"bytes=-0"}, status: 416, cr: "bytes */10"},
						{name: "reversed", ranges: []string{"bytes=5-2"}, status: 200, body: data},
						{name: "reversed-outside", ranges: []string{"bytes=20-10"}, status: 200, body: data},
						{name: "signed", ranges: []string{"bytes=+2-5"}, status: 200, body: data},
						{name: "missing-hyphen", ranges: []string{"bytes=2"}, status: 200, body: data},
						{name: "missing-endpoints", ranges: []string{"bytes=-"}, status: 200, body: data},
						{name: "bad-end", ranges: []string{"bytes=2-x"}, status: 200, body: data},
						{name: "bad-suffix", ranges: []string{"bytes=-x"}, status: 200, body: data},
						{name: "inner-whitespace", ranges: []string{"bytes=2- 5"}, status: 200, body: data},
						{name: "unsupported-unit", ranges: []string{"items=2-5"}, status: 200, body: data},
						{name: "empty-field", ranges: []string{""}, status: 200, body: data},
						{name: "multipart", ranges: []string{"bytes=0-1,4-5"}, status: 200, body: data},
						{name: "duplicate-different", ranges: []string{"bytes=2-5", "bytes=7-8"}, status: 200, body: data},
						{name: "duplicate-identical", ranges: []string{"bytes=2-5", "bytes=2-5"}, status: 200, body: data},
						{name: "duplicate-unsatisfiable", ranges: []string{"bytes=10-", "bytes=2-5"}, status: 200, body: data},
						{name: "overflow-start", ranges: []string{"bytes=18446744073709551616-"}, status: 200, body: data},
						{name: "overflow-end", ranges: []string{"bytes=2-18446744073709551616"}, status: 200, body: data},
						{name: "overflow-suffix", ranges: []string{"bytes=-18446744073709551616"}, status: 200, body: data},
						{name: "if-range-match", ranges: []string{"bytes=2-5"}, ifRange: []string{tag}, status: 206, body: "2345", cr: "bytes 2-5/10", emptyStatus: 416},
						{name: "if-range-whitespace", ranges: []string{"bytes=2-5"}, ifRange: []string{" \t" + tag + "\t "}, status: 206, body: "2345", cr: "bytes 2-5/10", emptyStatus: 416},
						{name: "if-range-mismatch", ranges: []string{"bytes=2-5"}, ifRange: []string{`"old"`}, status: 200, body: data},
						{name: "if-range-weak", ranges: []string{"bytes=2-5"}, ifRange: []string{"W/" + tag}, status: 200, body: data},
						{name: "if-range-date", ranges: []string{"bytes=2-5"}, ifRange: []string{"Wed, 21 Oct 2015 07:28:00 GMT"}, status: 200, body: data},
						{name: "if-range-malformed", ranges: []string{"bytes=2-5"}, ifRange: []string{"broken"}, status: 200, body: data},
						{name: "if-range-empty", ranges: []string{"bytes=2-5"}, ifRange: []string{""}, status: 200, body: data},
						{name: "if-range-duplicate", ranges: []string{"bytes=2-5"}, ifRange: []string{tag, tag}, status: 200, body: data},
						{name: "if-range-list", ranges: []string{"bytes=2-5"}, ifRange: []string{tag + ", " + tag}, status: 200, body: data},
						{name: "if-range-before-unsatisfiable", ranges: []string{"bytes=10-"}, ifRange: []string{`"old"`}, status: 200, body: data},
						{name: "head-range", ranges: []string{"bytes=2-5"}, head: true, status: 200},
						{name: "head-unsatisfiable", ranges: []string{"bytes=10-"}, head: true, status: 200},
						{name: "head-if-range", ranges: []string{"bytes=2-5"}, ifRange: []string{tag}, head: true, status: 200},
						{name: "head-duplicates", ranges: []string{"bytes=2-5", "bytes=7-8"}, ifRange: []string{tag, tag}, head: true, status: 200},
					} {
						t.Run(tc.name, func(t *testing.T) {
							status, body, cr := tc.status, tc.body, tc.cr
							if data == "" {
								body = ""

								if tc.emptyStatus != 0 {
									status = tc.emptyStatus
								}

								if status == 416 {
									cr = "bytes */0"
								}
							}

							method := http.MethodGet
							length := int64(len(body))

							if tc.head {
								method = http.MethodHead
								length = int64(len(data))
							}

							req, err := http.NewRequestWithContext(t.Context(), method, web.URL+endpoint.path, nil)
							if err != nil {
								t.Fatal(err)
							}

							req.Header["Range"] = tc.ranges
							req.Header["If-Range"] = tc.ifRange
							opens, resolved, closed := store.opens.Load(), store.resolved.Load(), store.closed.Load()

							resp, err := web.Client().Do(req)
							if err != nil {
								t.Fatal(err)
							}

							got, err := io.ReadAll(resp.Body)
							_ = resp.Body.Close()

							<-finished

							if err != nil || resp.StatusCode != status || string(got) != body || resp.ContentLength != length || resp.Header.Get("Content-Range") != cr {
								t.Fatalf("status=%d body=%q length=%d range=%q err=%v; want %d %q %d %q", resp.StatusCode, got, resp.ContentLength, resp.Header.Get("Content-Range"), err, status, body, length, cr)
							}

							wantOpens := int32(1)
							if tc.head || status == 416 || (data == "" && endpoint.name == "mirror") {
								wantOpens = 0
							}

							wantResolves := int32(1)
							if endpoint.name == "mirror" {
								wantResolves += wantOpens
							}

							if store.opens.Load()-opens != wantOpens || store.resolved.Load()-resolved != wantResolves || store.closed.Load()-closed != wantResolves {
								t.Fatalf("opens/resolved/closed=%d/%d/%d, want %d/%d/%d", store.opens.Load()-opens, store.resolved.Load()-resolved, store.closed.Load()-closed, wantOpens, wantResolves, wantResolves)
							}

							if status != 416 && (resp.Header.Get("ETag") != tag || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Type") != "application/octet-stream") {
								t.Fatal("response metadata changed", resp.Header)
							}
						})
					}
				})
			}
		})
	}
}
