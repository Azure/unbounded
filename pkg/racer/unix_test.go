// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestUnixPoolAndSocketRebinding(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	socket := filepath.Join(socketDirectory(t), "origin")

	var connections atomic.Int32

	serve := func(tag string) *httptest.Server {
		t.Helper()

		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "localhost" || r.RequestURI != "/a%2Fb//../c?x=1&x=2" {
				t.Errorf("request identity: %s %s", r.Host, r.RequestURI)
			}

			w.Header().Set("ETag", tag)
			w.Header().Set("Content-Length", "0")
		}))
		_ = s.Listener.Close()

		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}

		s.Listener = listener
		s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		s.Start()
		t.Cleanup(s.Close)

		return s
	}
	first := serve(checksumTag(nil))

	c, err := NewClient(socket, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()

	for range 3 {
		if _, err := c.Stat(context.Background(), "/a%2Fb//../c?x=1&x=2"); err != nil {
			t.Fatal(err)
		}
	}

	if connections.Load() != 1 || c.owned.Proxy != nil {
		t.Fatalf("pool connections=%d proxy configured=%v", connections.Load(), c.owned.Proxy != nil)
	}

	first.Close()
	serve(checksumTag([]byte("replacement")))

	m, err := c.Stat(context.Background(), "/a%2Fb//../c?x=1&x=2")
	if err != nil || m.ETag != checksumTag([]byte("replacement")) || connections.Load() != 2 {
		t.Fatalf("rebound socket: metadata=%+v connections=%d error=%v", m, connections.Load(), err)
	}
}
