// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"bytes"
	"context"
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
