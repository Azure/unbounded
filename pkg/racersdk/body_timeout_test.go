// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type transferDiscard struct{ *httptest.ResponseRecorder }

func (transferDiscard) ReadFrom(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

func TestBodyReadTimeoutReleasesAdmission(t *testing.T) {
	for _, fast := range []bool{false, true} {
		name := "copy"
		if fast {
			name = "HTTP transfer"
		}

		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, 8192, 8192, `"v"`)
				w.(http.Flusher).Flush()
				<-done
			}))

			defer close(done)

			c := testClient(t, path, 1)
			c.config.BodyReadTimeout = 30 * time.Millisecond

			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			if fast {
				_, err = v.WriteToHTTP(transferDiscard{httptest.NewRecorder()})
			} else {
				_, err = io.Copy(io.Discard, v)
			}

			if err == nil || c.Stats().ActiveBulk != 0 {
				t.Fatal("stalled body retained admission", err, c.Stats())
			}
		})
	}
}

func TestBodyReadTimeoutDoesNotBoundCallerThinkTime(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, 2, 2, `"v"`)
		_, _ = w.Write([]byte("ok"))
	}))
	c := testClient(t, path, 1)
	c.config.BodyReadTimeout = 20 * time.Millisecond

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	buf := make([]byte, 1)
	if _, err := v.Read(buf); err != nil {
		t.Fatal(err)
	}

	time.Sleep(40 * time.Millisecond)

	if _, err := v.Read(buf); err != nil || string(buf) != "k" {
		t.Fatal(string(buf), err)
	}
}
