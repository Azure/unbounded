// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type racerHijackWriter struct {
	*httptest.ResponseRecorder
	conn     net.Conn
	err      error
	onHijack func()
}

func (w *racerHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.onHijack != nil {
		w.onHijack()
	}

	if w.err != nil {
		return nil, nil, w.err
	}

	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

type racerFaultConn struct {
	net.Conn
	deadlineErr error
	writeErr    error
	closes      int
}

func (c *racerFaultConn) SetDeadline(deadline time.Time) error {
	if c.deadlineErr != nil {
		return c.deadlineErr
	}

	return c.Conn.SetDeadline(deadline)
}

func (c *racerFaultConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}

	return c.Conn.Write(p)
}

func (c *racerFaultConn) Close() error {
	c.closes++
	return c.Conn.Close()
}

func TestRacerForwardOwnershipBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}

	for _, mode := range []string{"unsupported-hijack", "failed-hijack", "drain-during-hijack", "deadline-failure", "header-flush-failure"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("forward boundary"), 4096)
			d := digestOf(data)
			canceled := make(chan struct{})
			client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"`+d.Hex()+`"`)
				w.Header().Set("Content-Length", "65536")

				if r.Method == http.MethodHead {
					return
				}

				w.Header().Set("Content-Range", "bytes 1-3/65536")
				w.Header().Set("Content-Length", "3")
				w.WriteHeader(http.StatusPartialContent)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 1)}

			var (
				fallbacks, streams, completed int
				streamErr                     error
			)

			cfg := reviewConfig()
			cfg.RacerMaxConcurrentTransfers = 1
			server := mirror.NewRacer(cfg, up, &gantryracer.Backend{Client: client},
				mirror.WithRacerMetrics(func(_ sdk.TransferStats, partial bool, err error) {
					streams++
					streamErr = err

					if !partial {
						t.Error("lost partial outcome")
					}
				}, func() { fallbacks++ }),
				mirror.WithLiveStreamCompletedHook(func(digest.Digest) { completed++ }))
			recorder := httptest.NewRecorder()

			var writer http.ResponseWriter = recorder

			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()

			fault := &racerFaultConn{Conn: conn}
			hijacker := &racerHijackWriter{ResponseRecorder: recorder, conn: fault}
			injected := errors.New("injected transport failure")

			switch mode {
			case "failed-hijack":
				hijacker.err = injected
			case "drain-during-hijack":
				hijacker.onHijack = server.Drain
			case "deadline-failure":
				fault.deadlineErr = injected
			case "header-flush-failure":
				fault.writeErr = injected
			}

			if mode != "unsupported-hijack" {
				writer = hijacker
			}

			r := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+d.String(), nil)
			r.Header.Set("Range", "bytes=1-3")
			server.Handler().ServeHTTP(writer, r)

			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("prepared upstream stream leaked")
			}

			if mode == "unsupported-hijack" || mode == "failed-hijack" {
				if fallbacks != 0 || len(up.seen) != 0 || streams != 0 || completed != 0 || recorder.Code != http.StatusServiceUnavailable {
					t.Fatal("pre-hijack failure bypassed Racer", fallbacks, streams, completed, recorder.Code)
				}

				for _, header := range []string{"Content-Range", "Connection", "ETag", "Accept-Ranges", "Docker-Content-Digest", "Content-Length"} {
					if recorder.Header().Get(header) != "" {
						t.Error("forwarding header leaked into error", header)
					}
				}
			} else if fallbacks != 0 || len(up.seen) != 0 || streams != 1 || streamErr == nil || completed != 0 || fault.closes == 0 || recorder.Body.Len() != 0 {
				t.Fatal("ownership failure substituted fallback or leaked connection", fallbacks, streams, streamErr, completed, fault.closes)
			}

			if mode != "drain-during-hijack" {
				probe := httptest.NewRecorder()
				server.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodHead, r.URL.String(), nil))

				if probe.Code != http.StatusOK {
					t.Fatal("forwarding failure retained admission", probe.Code)
				}
			}

			closes := fault.closes

			server.Drain()
			server.Drain()

			if fault.closes != closes {
				t.Fatal("finished connection remained registered during drain")
			}
		})
	}
}

func TestRacerDrainInterruptsHijackedStream(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}

	d := digestOf([]byte("stalled response"))
	canceled := make(chan struct{})
	client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Length", "65536")

		if r.Method == http.MethodHead {
			return
		}

		w.Header().Set("Content-Range", "bytes 0-65535/65536")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	up := &authorizationCapturingOrigin{seen: make(chan string, 1)}

	var fallbacks, completed int

	results := make(chan error, 1)
	server := mirror.NewRacer(reviewConfig(), up, &gantryracer.Backend{Client: client},
		mirror.WithRacerMetrics(func(_ sdk.TransferStats, _ bool, err error) { results <- err }, func() { fallbacks++ }),
		mirror.WithLiveStreamCompletedHook(func(digest.Digest) { completed++ }))
	finished := make(chan struct{})
	handler := server.Handler()

	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)

		handler.ServeHTTP(w, r)
	}))
	defer m.Close()
	defer server.Drain()

	resp, err := m.Client().Get(m.URL + "/v2/repo/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	server.Drain()
	server.Drain()

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("drained response completed")
	}

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("drain did not release hijacked handler")
	}

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("drain did not cancel upstream")
	}

	if err := <-results; err == nil || fallbacks != 0 || completed != 0 || len(up.seen) != 0 {
		t.Fatal("drain reported completion or used fallback", err, fallbacks, completed)
	}
}
