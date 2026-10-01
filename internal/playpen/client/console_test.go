// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStreamConsoleLogsAsync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic "+basicAuth("admin", "secret") {
			t.Fatalf("authorization = %q", got)
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // Test cleanup.

		if err := conn.Write(r.Context(), websocket.MessageBinary, []byte("booting\n")); err != nil {
			t.Fatal(err)
		}

		<-r.Context().Done()
	}))
	defer server.Close()

	metadata := testAllocResponse()
	metadata.Redfish["url"] = server.URL
	p := &Playpen{Metadata: metadata}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	buf := &lockedBuffer{}

	errCh := p.StreamConsoleLogs(ctx, buf)
	for !strings.Contains(buf.String(), "booting\n") {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

var _ io.Writer = (*lockedBuffer)(nil)

func TestConsoleStreamURL(t *testing.T) {
	got, err := consoleStreamURL(map[string]string{
		"url":                    "https://10.88.0.1:8443",
		"serialConsoleStreamURI": "/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got != "wss://10.88.0.1:8443/redfish/v1/Systems/1/Oem/Unbounded/SerialConsole/Stream" {
		t.Fatalf("url = %q", got)
	}
}

func TestConsoleTLSRequiresTLS13(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		server.TLS = &tls.Config{MinVersion: version, MaxVersion: version}
		server.StartTLS()
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		client := redfishWebSocketHTTPClient(map[string]string{"certPEM": string(certPEM)})
		client.Timeout = 5 * time.Second

		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}

		client.CloseIdleConnections()
		server.Close()

		if (err == nil) != (version == tls.VersionTLS13) {
			t.Fatalf("TLS %x: %v", version, err)
		}
	}

	for _, metadata := range []map[string]string{nil, {"certPEM": "invalid"}} {
		client := redfishWebSocketHTTPClient(metadata)

		transport := client.Transport.(*http.Transport)
		if transport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
			t.Fatal("missing or invalid PEM lowered TLS baseline")
		}
	}
}

func TestConsoleTLSDoesNotInheritWeakerDefault(t *testing.T) {
	original := http.DefaultTransport

	t.Cleanup(func() { http.DefaultTransport = original })

	weak := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	http.DefaultTransport = weak

	client := redfishWebSocketHTTPClient(nil)
	if client.Transport.(*http.Transport).TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("cloned default weakened TLS baseline")
	}

	if weak.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("mutated shared default TLS config")
	}

	http.DefaultTransport = consoleRoundTripper{}

	client = redfishWebSocketHTTPClient(nil)
	if client.Transport.(*http.Transport).TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("custom default bypassed TLS baseline")
	}
}

type consoleRoundTripper struct{}

func (consoleRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}
