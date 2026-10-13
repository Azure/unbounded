// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testOriginHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "test-access", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "test-session",
	} {
		t.Setenv(key, value)
	}

	cfg, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	client, ok := cfg.HTTPClient.(*http.Client)
	if !ok {
		t.Fatalf("HTTP client type = %T", cfg.HTTPClient)
	}

	t.Cleanup(client.CloseIdleConnections)

	return client
}

func TestOriginHTTPTransport(t *testing.T) {
	client := testOriginHTTPClient(t)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want a dedicated *http.Transport", client.Transport)
	}

	defaults := http.DefaultTransport.(*http.Transport)
	if transport == defaults {
		t.Fatal("origin shares the default transport")
	}

	if transport.MaxIdleConnsPerHost < 68 || transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		t.Fatalf("idle pool cannot retain origin concurrency: per-host=%d total=%d", transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}

	if client.Timeout != time.Minute || transport.DialContext == nil || transport.Proxy == nil ||
		transport.TLSHandshakeTimeout != defaults.TLSHandshakeTimeout || transport.IdleConnTimeout != defaults.IdleConnTimeout ||
		transport.ExpectContinueTimeout != defaults.ExpectContinueTimeout || transport.ForceAttemptHTTP2 != defaults.ForceAttemptHTTP2 {
		t.Fatal("request timeout or default transport behavior changed")
	}

	other := testOriginHTTPClient(t).Transport.(*http.Transport)
	if other == transport {
		t.Fatal("origin clients share a transport")
	}

	defaultLimit := defaults.MaxIdleConnsPerHost

	transport.MaxIdleConnsPerHost++
	if defaults.MaxIdleConnsPerHost != defaultLimit || other.MaxIdleConnsPerHost == transport.MaxIdleConnsPerHost {
		t.Fatal("transport configuration is not isolated")
	}
}

func TestOriginHTTPTransportInvalidDefault(t *testing.T) {
	defaults := http.DefaultTransport

	t.Cleanup(func() { http.DefaultTransport = defaults })

	http.DefaultTransport = nil

	_, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute})
	if err == nil || err.Error() != "could not configure origin HTTP transport" {
		t.Fatalf("error = %v, want fixed transport configuration error", err)
	}
}

func TestOriginHTTPConnectionReuse(t *testing.T) {
	const concurrency = 64

	client := testOriginHTTPClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)

	var connections atomic.Int32

	entered := make(chan struct{}, concurrency)
	release := make(chan struct{}, concurrency)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Length", "1")
		_, _ = io.WriteString(w, "x")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()
	defer cancel()

	for burst := range 2 {
		var workers sync.WaitGroup

		results := make(chan error, concurrency)
		for range concurrency {
			workers.Go(func() {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				if err != nil {
					results <- err
					return
				}

				response, err := client.Do(request)
				if err != nil {
					results <- err
					return
				}

				_, err = io.Copy(io.Discard, response.Body)

				closeErr := response.Body.Close()
				if err == nil {
					err = closeErr
				}

				results <- err
			})
		}

		for range concurrency {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("concurrent requests did not all arrive:", ctx.Err())
			}
		}

		for range concurrency {
			release <- struct{}{}
		}

		workers.Wait()

		for range concurrency {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}

		if got := connections.Load(); got != concurrency {
			t.Fatalf("burst %d opened %d total connections, want %d with keep-alive reuse", burst+1, got, concurrency)
		}
	}
}
