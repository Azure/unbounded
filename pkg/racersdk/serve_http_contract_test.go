// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func completeTestValue(t *testing.T, size uint64, completion string) *Value {
	t.Helper()
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))
		for first := uint64(0); first < size; {
			n := min(uint64(PageSize), size-first)
			_ = fakeSubscriptionFrame(conn, 1, first/uint64(PageSize), first, uint32(n))
			_, _ = io.CopyN(conn, &offsetStream{offset: int64(first)}, int64(n))
			first += n
		}

		if completion != "missing" {
			pages := (size + uint64(PageSize) - 1) / uint64(PageSize)
			if completion == "bad" {
				pages++
			}

			_ = fakeSubscriptionFrame(conn, 2, pages, size, 0)
		}
	})

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(v) })

	return v
}

func TestWriteToHTTPCompleteBeforeEmptySuccessAndAbortAfterCommit(t *testing.T) {
	for _, mode := range []string{"http", "tls", "nonhijackable"} {
		for _, size := range []uint64{0, 4, uint64(PageSize) + 1} {
			for _, completion := range []string{"good", "bad", "missing"} {
				t.Run(mode+"/"+strconv.FormatUint(size, 10)+"/"+completion, func(t *testing.T) {
					v := completeTestValue(t, size, completion)
					handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						defer closeBody(v)

						w.Header().Set("Content-Length", strconv.FormatUint(size, 10))

						n, err := v.WriteToHTTP(w)
						if err != nil {
							if n > 0 {
								panic(http.ErrAbortHandler)
							}

							w.Header().Del("Content-Length")
							http.Error(w, "value unavailable", http.StatusBadGateway)
						}
					})

					if mode == "nonhijackable" {
						w := httptest.NewRecorder()

						var caught any

						func() {
							defer func() { caught = recover() }()

							handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
						}()

						if completion == "good" {
							if caught != nil || w.Code != 200 || uint64(w.Body.Len()) != size {
								t.Fatal(w.Code, w.Body.Len(), caught)
							}
						} else if size > uint64(PageSize) {
							if caught != http.ErrAbortHandler {
								t.Fatal("committed response did not abort", caught)
							}
						} else if caught != nil || w.Code < 400 {
							t.Fatal("failure before commit reported success", w.Code, caught)
						}

						return
					}

					server := httptest.NewUnstartedServer(handler)

					server.Config.ErrorLog = log.New(io.Discard, "", 0)
					if mode == "tls" {
						server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
						server.StartTLS()
					} else {
						server.Start()
					}
					defer server.Close()

					response, err := server.Client().Get(server.URL)
					if err != nil {
						if completion == "good" || size == 0 {
							t.Fatal(err)
						}

						return
					}
					defer closeBody(response.Body)

					if mode == "tls" && response.TLS.Version != tls.VersionTLS13 {
						t.Fatal("TLS 1.3 required")
					}

					count, readErr := io.Copy(io.Discard, response.Body)
					if completion == "good" {
						if response.StatusCode != 200 || count != int64(size) || readErr != nil {
							t.Fatal(response.Status, count, readErr)
						}
					} else if size == 0 {
						if response.StatusCode < 400 {
							t.Fatal("empty response committed before Complete", response.Status)
						}
					} else if response.StatusCode < 400 && readErr == nil {
						t.Fatal("incomplete response reported success", count)
					}
				})
			}
		}
	}
}
