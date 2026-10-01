// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type racerPausedReader struct {
	resume <-chan struct{}
	reader io.Reader
}

func (r racerPausedReader) Read(p []byte) (int, error) {
	<-r.resume
	return r.reader.Read(p)
}

func TestRacerIOHTTP2UpstreamWaits(t *testing.T) {
	for _, phase := range []string{"WriteHeader", "early Flush", "Write", "ReadFrom fallback"} {
		t.Run(phase, func(t *testing.T) {
			const budget = 50 * time.Millisecond

			server := httptest.NewUnstartedServer(RacerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "6")
				w.WriteHeader(http.StatusOK)

				if phase == "WriteHeader" {
					time.Sleep(4 * budget)
				}

				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}

				if phase == "early Flush" {
					time.Sleep(4 * budget)
				}

				if _, err := w.Write([]byte("abc")); err != nil {
					t.Error(err)
				}

				if phase == "Write" {
					time.Sleep(4 * budget)
				}

				if phase == "ReadFrom fallback" {
					// Simulate the SDK deadline surrounding a ReadFrom call whose
					// HTTP/2 fallback waits on source bytes before invoking Write.
					if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Error(err)
					}

					resume := make(chan struct{})

					timer := time.AfterFunc(4*budget, func() { close(resume) })
					defer timer.Stop()

					if _, err := w.(io.ReaderFrom).ReadFrom(racerPausedReader{resume, bytes.NewReader([]byte("def"))}); err != nil {
						t.Error(err)
					}

					if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
						t.Error(err)
					}
				} else if _, err := w.Write([]byte("def")); err != nil {
					t.Error(err)
				}
			}), budget, nil))
			server.EnableHTTP2 = true

			server.StartTLS()
			defer server.Close()

			server.Client().Timeout = 5 * time.Second
			for range 2 {
				resp, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()

				if resp.ProtoMajor != 2 || string(body) != "abcdef" || err != nil {
					t.Fatalf("proto=%s body=%q err=%v", resp.Proto, body, err)
				}
				// The finalization deadline must not poison a completed stream or
				// the next stream on this connection after the handler returns.
				time.Sleep(2 * budget)
			}
		})
	}
}
