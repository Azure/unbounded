// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestFrontendUpstreamFailures(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name, headers, body string
		short               bool
	}{
		{"short", "Content-Length: 16\r\n", "short", true},
		{"chunked", "Transfer-Encoding: chunked\r\n", "0\r\n\r\n", false},
		{"encoded", "Content-Length: 4\r\nContent-Encoding: gzip\r\n", "body", false},
		{"duplicate-encoding", "Content-Length: 4\r\nContent-Encoding: identity\r\nContent-Encoding: gzip\r\n", "body", false},
		{"duplicate-etag", "Content-Length: 4\r\nETag: \"wrong\"\r\n", "body", false},
		{"unsolicited-range", "Content-Length: 4\r\nContent-Range: bytes 0-3/8\r\n", "body", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := startFrontend(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h, ok := w.(http.Hijacker)
				if !ok {
					t.Error("missing hijacker")
					return
				}

				conn, _, err := h.Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()

				_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nETag: %s\r\n%s\r\n%s", c.Objects[0].etag, tc.headers, tc.body)
			}))

			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()

			r, err := client.Get(endpoint + "/models/model.safetensors")
			if !tc.short {
				if err == nil {
					r.Body.Close()
					t.Fatal("accepted invalid framing")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			defer r.Body.Close()

			body, err := io.ReadAll(r.Body)
			if err == nil || string(body) != "short" {
				t.Fatalf("body=%q error=%v", body, err)
			}
		})
	}
}

func TestFrontendConditions(t *testing.T) {
	c := testConfig(t)
	// 304 must not splice or consume the next persistent response.
	endpoint, f := startFrontend(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", c.Objects[0].etag)

		if r.Header.Get("If-None-Match") == c.Objects[0].etag {
			w.WriteHeader(304)
			return
		}

		w.Header().Set("Content-Length", "4")
		_, _ = io.WriteString(w, "body")
	}))

	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()

	for _, conditional := range []bool{true, false} {
		r, _ := http.NewRequest("GET", endpoint+"/models/model.safetensors", nil)
		if conditional {
			r.Header.Set("If-None-Match", c.Objects[0].etag)
		}

		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil || conditional && (resp.StatusCode != 304 || len(body) != 0) || !conditional && string(body) != "body" {
			t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
		}
	}

	if f.bytes.Load() != 4 {
		t.Fatal(f.bytes.Load())
	}
}

func TestSpliceBackpressureAndCancellation(t *testing.T) {
	for _, action := range []string{"deadline", "disconnect", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "u"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer ul.Close()

			tl, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer tl.Close()

			client, err := net.DialTCP("tcp", nil, tl.Addr().(*net.TCPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			dst, err := tl.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()

			producer, err := net.DialUnix("unix", nil, ul.Addr().(*net.UnixAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer producer.Close()

			src, err := ul.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()

			if err := dst.SetWriteBuffer(4096); err != nil {
				t.Fatal(err)
			}

			if err := client.SetReadBuffer(4096); err != nil {
				t.Fatal(err)
			}

			p, err := newSplicePipe()
			if err != nil {
				t.Fatal(err)
			}
			defer p.close()

			payload := bytes.Repeat([]byte("x"), 16<<20)
			written := make(chan struct{})

			go func() { defer close(written); _, _ = producer.Write(payload) }()

			done := make(chan error, 1)

			go func() { _, err := p.transfer(dst, src, int64(len(payload))); done <- err }()

			time.Sleep(20 * time.Millisecond)

			switch action {
			case "deadline":
				if err := dst.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				client.Close()
			case "shutdown":
				dst.Close()
				src.Close()
			}

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked transfer succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("transfer did not unblock")
			}

			producer.Close()
			<-written
		})
	}
}

func TestFrontendShutdownBlockedOrigin(t *testing.T) {
	c := testConfig(t)

	ul, err := net.Listen("unix", filepath.Join(t.TempDir(), "u"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()

	tl, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := newFrontend(c, ul.Addr().String(), 1, time.Hour)
	done := make(chan error, 1)

	go func() { done <- f.serve(ctx, tl) }()

	client, err := net.Dial("tcp", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, _ = io.WriteString(client, "GET /models/model.safetensors HTTP/1.1\r\nHost: local\r\n\r\n")

	upstream, err := ul.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	if _, err := http.ReadRequest(bufio.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked on origin")
	}
}

func TestConfigIdentity(t *testing.T) {
	c := testConfig(t)
	original := c.Objects[0]

	c.Objects[0].Key = "alias"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}

	if c.Objects[0].target != original.target {
		t.Fatal("S3 alias changed backing identity")
	}

	c.Objects[0].Blob = "new"
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}

	if c.Objects[0].target == original.target {
		t.Fatal("changed blob reused identity")
	}

	c.Endpoint += "?sig=secret"
	if err := c.validate(); err == nil {
		t.Fatal("accepted credentials in identity")
	}
}

func TestRangeValidation(t *testing.T) {
	for _, v := range []string{"bytes=0-1", "bytes=3-", "bytes=-2"} {
		if !validRange(http.Header{"Range": {v}}) {
			t.Fatal(v)
		}
	}

	for _, v := range []string{"bytes=-", "bytes=2-1", "bytes=0-1,3-4", "bytes=+1-2", "bytes=999999999999999999999-"} {
		if validRange(http.Header{"Range": {v}}) {
			t.Fatal(v)
		}
	}

	if validRange(http.Header{"Range": {"bytes=0-1", "bytes=2-3"}}) {
		t.Fatal("duplicate range")
	}

	r, _ := http.NewRequest("GET", "http://local/", nil)
	r.Header.Set("Range", "bytes=0-3")

	for _, v := range []string{"bytes 1-4/8", "bytes 0-2/8", "bytes 0-3/3", "bytes 0-3/8 junk"} {
		resp := &http.Response{StatusCode: 206, ContentLength: 4, Header: http.Header{"Content-Range": {v}}}
		if validateResponseRange(r, resp) == nil {
			t.Fatal(v)
		}
	}
}
