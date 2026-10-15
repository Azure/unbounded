// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDebugOptions(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		for _, address := range []string{"", "127.0.0.1:6060", ":6060"} {
			args := []string{mode, "--namespace=store"}
			if address != "" {
				args = append(args, "--debug-listen="+address)
			}

			o, err := parseOptions(args, io.Discard)
			if err != nil || o.debugListen != address {
				t.Fatalf("%s %q: %+v, %v", mode, address, o, err)
			}
		}
	}
}

func TestDebugMux(t *testing.T) {
	mux := debugMux()

	for _, path := range []string{"/healthz", "/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine", "/debug/pprof/cmdline", "/debug/pprof/symbol", "/debug/pprof/profile?seconds=1", "/debug/pprof/trace?seconds=0.01"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
		})
	}
	// A handler on DefaultServeMux must not appear on the diagnostic mux.
	previous := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()

	t.Cleanup(func() { http.DefaultServeMux = previous })

	const unrelated = "/racer-object-test-unrelated"
	http.DefaultServeMux.HandleFunc(unrelated, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for _, path := range []string{unrelated, "/bucket/key"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

		if response.Code != http.StatusNotFound {
			t.Fatalf("unrelated route %s exposed", path)
		}
	}
}

func TestDebugDisabledAndBindFailure(t *testing.T) {
	want := errors.New("main failed")
	if err := runWithDebug(context.Background(), "", func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatal("disabled diagnostics changed main result", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for _, address := range []string{l.Addr().String(), "private-token"} {
		err := runWithDebug(context.Background(), address, func(context.Context) error {
			t.Error("main started despite debug bind failure")
			return nil
		})
		if err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatal("missing or unredacted bind error", err)
		}
	}

	for _, mode := range []string{"origin", "sidecar"} {
		err := run(context.Background(), []string{mode, "--namespace=store", "--debug-listen=" + l.Addr().String()}, io.Discard)
		if err == nil || err.Error() != "could not bind debug listener" {
			t.Fatalf("%s did not bind diagnostics before main startup: %v", mode, err)
		}
	}
}

func TestDebugLifecycle(t *testing.T) {
	for _, scenario := range []string{"return", "failure", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			// Reserve an available port, then let the production wrapper bind it.
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			address := l.Addr().String()
			l.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var want error
			if scenario == "failure" {
				want = errors.New("main failed")
			}

			err = runWithDebug(ctx, address, func(ctx context.Context) error {
				client := &http.Client{Timeout: time.Second}
				defer client.CloseIdleConnections()

				response, err := client.Get("http://" + address + "/healthz")
				if err != nil {
					return err
				}

				response.Body.Close()

				if response.StatusCode != http.StatusOK {
					t.Errorf("health status %d", response.StatusCode)
				}

				if scenario == "cancel" {
					cancel()
					<-ctx.Done()
				}

				return want
			})
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}

			conn, err := net.DialTimeout("tcp", address, time.Second)
			if err == nil {
				conn.Close()
				t.Fatal("debug listener remained open after main exited")
			}
		})
	}
}
