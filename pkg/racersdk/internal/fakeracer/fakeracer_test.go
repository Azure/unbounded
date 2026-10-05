// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeracer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCredits(t *testing.T) {
	for _, name := range []string{"pages", "bytes"} {
		t.Run(name, func(t *testing.T) {
			c := NewCredits()

			s := wire.SubscriptionRequest{PageCredits: 1, ByteCredits: 10}
			if name == "bytes" {
				s.PageCredits = 2
				s.ByteCredits = 3
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if err := c.Reserve(ctx, s, 0, 3); err != nil {
				t.Fatal(err)
			}

			reserved := make(chan error, 1)

			go func() { reserved <- c.Reserve(ctx, s, 1, 3) }()

			select {
			case err := <-reserved:
				t.Fatal("reserved without credit", err)
			case <-time.After(10 * time.Millisecond):
			}

			reader, writer := io.Pipe()
			done := make(chan struct{})

			go func() { defer close(done); c.Releases(reader, cancel) }()

			credit := (wire.Credit{Number: 0, Length: 3}).Encode()
			if _, err := writer.Write(credit[:]); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-reserved:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("release did not unblock reserve")
			}

			closeBody(writer)
			<-done
			closeBody(reader)

			if err := c.Reserve(ctx, s, 2, 1); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled reserve", err)
			}
		})
	}
}

func TestInvalidCredits(t *testing.T) {
	for _, test := range []struct {
		name    string
		records []wire.Credit
	}{
		{"unknown", []wire.Credit{{Number: 2, Length: 3}}},
		{"length", []wire.Credit{{Length: 2}}},
		{"zero", []wire.Credit{{}}},
		{"duplicate", []wire.Credit{{Length: 3}, {Length: 3}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := NewCredits()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if err := c.Reserve(ctx, wire.SubscriptionRequest{PageCredits: 1, ByteCredits: 3}, 0, 3); err != nil {
				t.Fatal(err)
			}

			var records bytes.Buffer
			for _, credit := range test.records {
				b := credit.Encode()
				records.Write(b[:])
			}

			c.Releases(&records, cancel)

			if ctx.Err() == nil {
				t.Fatal("invalid credit did not cancel")
			}

			if test.name != "duplicate" && (len(c.outstanding) != 1 || c.bytes != 3) {
				t.Fatal("invalid release changed accounting")
			}

			if test.name == "duplicate" && (len(c.outstanding) != 0 || c.bytes != 0) {
				t.Fatal("duplicate underflowed accounting")
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	h := NewHandler(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("bad request reached origin")
		return nil, errors.New("unexpected")
	}))

	for _, test := range []struct{ method, path, header, value string }{
		{"POST", wire.ClientObjectPrefix + strings.Repeat("0", 64), "Racer-Page-Credits", "0"},
		{"POST", wire.ClientObjectPrefix + strings.Repeat("0", 64), "Racer-Byte-Credits", "1"},
		{"POST", wire.ClientObjectPrefix + strings.Repeat("0", 64), "Range", "bytes=-1"},
		{"HEAD", "/invalid", "", ""},
		{"GET", wire.ClientObjectPrefix + strings.Repeat("0", 64), "", ""},
	} {
		t.Run(test.method+test.path+test.header, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://racer"+test.path, nil)
			r.RequestURI = test.path
			r.Header.Set("Content-Length", "0")

			if test.header != "" {
				r.Header.Set(test.header, test.value)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != 400 || w.Header().Get("Content-Length") != "0" || w.Body.Len() != 0 {
				t.Fatal(w.Code, w.Header(), w.Body)
			}
		})
	}
}

func TestOriginResponses(t *testing.T) {
	for _, method := range []string{"HEAD", "POST"} {
		for _, test := range []struct {
			name               string
			status             int
			malformed, failure bool
			want               int
		}{
			{name: "success", status: 200, want: 200},
			{name: "unauthorized", status: 401, want: 401},
			{name: "unavailable", status: 503, want: 503},
			{name: "bad metadata", status: 200, malformed: true, want: 502},
			{name: "transport", failure: true, want: 502},
		} {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if test.failure {
						return nil, errors.New("origin offline")
					}

					if !strings.HasPrefix(r.URL.Path, wire.ObjectPrefix) || r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("Racer-Metadata") != "opaque" {
						t.Error("forwarding", r.URL, r.Header)
					}

					h := http.Header{"Content-Length": {"0"}}
					if test.status == 200 {
						h.Set("ETag", `"v"`)
						h.Set("Racer-Expires-At", "0")

						if method == "POST" {
							h.Set("Content-Type", "application/octet-stream")
						}
					}

					if test.malformed {
						h.Del("ETag")
					}

					return &http.Response{StatusCode: test.status, Status: fmt.Sprintf("%d %s", test.status, http.StatusText(test.status)), Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
				})

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				server := httptest.NewUnstartedServer(NewHandler(transport))
				server.Config.BaseContext = func(net.Listener) context.Context { return ctx }

				server.Start()
				defer server.Close()

				conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(conn)

				if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}

				if _, err := fmt.Fprintf(conn, "%s %s%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nAuthorization: Bearer test\r\nRacer-Metadata: opaque\r\n\r\n", method, wire.ClientObjectPrefix, strings.Repeat("0", 64)); err != nil {
					t.Fatal(err)
				}

				res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(res.Body)

				if res.StatusCode != test.want {
					t.Fatal(res.Status, res.Header)
				}

				if method == "POST" && test.want == 200 {
					var raw [wire.FrameSize]byte

					_, err := io.ReadFull(res.Body, raw[:])

					frame := wire.DecodeFrame(raw)
					if err != nil || frame != (wire.Frame{Kind: wire.CompleteFrame}) {
						t.Fatal(frame, err)
					}
				}
			})
		}
	}
}
