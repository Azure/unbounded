// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type diagnosticRecords struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *diagnosticRecords) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.buf.Write(p)
}

func (r *diagnosticRecords) record(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, line := range strings.Split(r.buf.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "mirror: sampled Racer failure" {
			return record
		}
	}

	t.Fatal("missing diagnostic", r.buf.String())

	return nil
}

func TestRacerFailureDiagnosticPhases(t *testing.T) {
	for _, phase := range []string{"HEAD", "Prepare", "forward"} {
		t.Run(phase, func(t *testing.T) {
			d := digestOf([]byte("diagnostic object"))
			client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"`+d.Hex()+`"`)
				w.Header().Set("Content-Type", "application/octet-stream")

				if r.Method == "HEAD" {
					if phase == "HEAD" {
						w.Header().Set("Content-Length", "0")
						w.WriteHeader(504)
					} else {
						w.Header().Set("Content-Length", fmt.Sprint(sdk.PageSize+8))
					}

					return
				}

				if phase == "Prepare" || r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", sdk.PageSize, sdk.PageSize+7) {
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(503)

					return
				}

				w.Header().Set("Content-Length", "8")
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sdk.PageSize-8, sdk.PageSize-1, sdk.PageSize+8))
				w.WriteHeader(206)
				_, _ = io.WriteString(w, "12345678")
			}))
			logs := &diagnosticRecords{}
			up := &authorizationCapturingOrigin{seen: make(chan string, 1)}
			server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client}, mirror.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
			finished := make(chan struct{})

			m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)

				server.Handler().ServeHTTP(w, r)
			}))
			defer m.Close()

			req, err := http.NewRequestWithContext(t.Context(), "GET", m.URL+"/v2/repo/blobs/"+d.String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", sdk.PageSize-8, sdk.PageSize+7))

			resp, err := m.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			<-finished

			record := logs.record(t)
			if record["phase"] != phase || record["digest"] != d.String() {
				t.Fatal(record)
			}

			if phase == "HEAD" {
				if record["racer_status"] != float64(504) || resp.StatusCode != 503 || record["page_offset"] != float64(-1) {
					t.Fatal(record, resp.Status)
				}
			} else if phase == "Prepare" {
				if record["racer_status"] != float64(503) || resp.StatusCode != 503 || record["page_offset"] != float64(sdk.PageSize-8) {
					t.Fatal(record, resp.Status)
				}
			} else if record["racer_status"] != float64(503) || record["page_offset"] != float64(sdk.PageSize) || record["written"] != float64(8) || resp.StatusCode != 206 || readErr == nil || string(body) != "12345678" {
				t.Fatal("lost original later-page failure or changed framing", record, resp.Status, readErr, string(body))
			}

			if len(up.seen) != 0 {
				t.Fatal("diagnostics triggered fallback")
			}
		})
	}
}
