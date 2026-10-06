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
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type pageBody struct {
	io.Reader
	closes int
}

func (b *pageBody) Close() error { b.closes++; return nil }

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}

	return len(p), nil
}

func TestSubscriptionPages(t *testing.T) {
	for _, test := range []struct {
		name             string
		size, first, end uint64
		pin              string
		failCall         int
		failure          string
		status           int
		calls            int
	}{
		{name: "bootstrap", size: 5, end: 9, status: 200, calls: 1},
		{name: "pinned", size: 5, end: 5, pin: `"v"`, status: 200, calls: 2},
		{name: "slice", size: 5, first: 1, end: 3, status: 200, calls: 2},
		{name: "cross page", size: wire.PageSize + 3, first: wire.PageSize - 2, end: wire.PageSize + 3, status: 200, calls: 3},
		{name: "outside", size: 5, first: 5, end: 9, status: 416, calls: 1},
		{name: "empty range", end: 1, status: 416, calls: 1},
		{name: "first unauthorized", size: 5, first: 1, end: 3, failCall: 2, failure: "status", status: 401, calls: 2},
		{name: "first transport", size: 5, first: 1, end: 3, failCall: 2, failure: "transport", status: 502, calls: 2},
		{name: "first malformed", size: 5, first: 1, end: 3, failCall: 2, failure: "metadata", status: 502, calls: 2},
		{name: "late status", size: wire.PageSize + 3, first: wire.PageSize - 2, end: wire.PageSize + 3, failCall: 3, failure: "status", status: 200, calls: 3},
		{name: "late transport", size: wire.PageSize + 3, first: wire.PageSize - 2, end: wire.PageSize + 3, failCall: 3, failure: "transport", status: 200, calls: 3},
		{name: "short body", size: 5, end: 5, failCall: 1, failure: "short", status: 200, calls: 1},
		{name: "framing overflow", size: math.MaxInt64, end: math.MaxInt64, status: 502, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var bodies []*pageBody

			calls := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == test.failCall && test.failure == "transport" {
					return nil, errors.New("offline")
				}

				h := http.Header{"Etag": {`"v"`}, "Racer-Expires-At": {"0"}}
				status, length := http.StatusOK, test.size

				var bodyLength uint64

				if r.Method == http.MethodGet {
					bounds, err := wire.ParseRange(r.Header.Get("Range"))
					if err != nil {
						t.Fatal(err)
					}

					first, last, err := bounds.ResolvePage(test.size)
					if test.size != 0 && err != nil {
						t.Fatal(err)
					}

					h.Set("Content-Type", "application/octet-stream")

					if test.size != 0 {
						status, length = http.StatusPartialContent, last-first+1
						h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, test.size))
					}

					bodyLength = length

					if calls > 1 && r.Header.Get("If-Match") != `"v"` {
						t.Error("continuation lost pin")
					}
				}

				h.Set("Content-Length", strconv.FormatUint(length, 10))

				if calls == test.failCall {
					switch test.failure {
					case "status":
						status, bodyLength = http.StatusUnauthorized, 0
						h = http.Header{"Content-Length": {"0"}}
					case "metadata":
						h.Del("Etag")
					case "short":
						bodyLength--
					}
				}

				body := &pageBody{Reader: io.LimitReader(repeatedByte('x'), int64(bodyLength))}
				bodies = append(bodies, body)

				return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: h, Body: body}, nil
			})
			s := wire.SubscriptionRequest{Request: wire.Request{Operation: wire.OperationHead, Pin: test.pin}, First: test.first, End: test.end, Ranged: true, PageCredits: 2, ByteCredits: 2 * wire.PageSize}

			var output bytes.Buffer
			servePages(t.Context(), bufio.NewWriter(&output), transport, s, NewCredits())

			if calls != test.calls {
				t.Fatalf("origin calls = %d, want %d", calls, test.calls)
			}

			for i, body := range bodies {
				if body.closes != 1 {
					t.Errorf("body %d closed %d times", i, body.closes)
				}
			}

			res, err := http.ReadResponse(bufio.NewReader(&output), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(res.Body)

			if res.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, test.status)
			}

			if test.status == 416 && res.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", test.size) {
				t.Fatal(res.Header)
			}

			if test.status != 200 {
				return
			}

			data, err := io.ReadAll(res.Body)
			if test.failure != "" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("stream error = %v", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			end := min(test.end, test.size)
			sequence := wire.NewSequence(test.first, end, true)
			reader := bytes.NewReader(data)

			for {
				var raw [wire.FrameSize]byte
				if _, err := io.ReadFull(reader, raw[:]); err != nil {
					t.Fatal(err)
				}

				frame := wire.DecodeFrame(raw)
				if err := sequence.Accept(frame, "test"); err != nil {
					t.Fatal(frame, err)
				}

				if frame.Kind == wire.CompleteFrame {
					break
				}

				payload := make([]byte, frame.Length)
				if _, err := io.ReadFull(reader, payload); err != nil {
					t.Fatal(err)
				}

				if string(payload) != strings.Repeat("x", len(payload)) {
					t.Fatal("wrong payload")
				}
			}

			if reader.Len() != 0 {
				t.Fatal("bytes after completion")
			}
		})
	}
}

type failingIO struct{}

func (failingIO) Read([]byte) (int, error)  { return 0, io.ErrClosedPipe }
func (failingIO) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCopySlice(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   io.Reader
		writer io.Writer
		skip   uint64
		length uint32
		want   error
	}{
		{"skip failure", strings.NewReader(""), io.Discard, 1, 1, io.EOF},
		{"short payload", strings.NewReader("x"), io.Discard, 0, 2, io.ErrUnexpectedEOF},
		{"read failure", failingIO{}, io.Discard, 0, 1, io.ErrClosedPipe},
		{"write failure", strings.NewReader("x"), failingIO{}, 0, 1, io.ErrClosedPipe},
		{"drain failure", io.MultiReader(strings.NewReader("x"), failingIO{}), io.Discard, 0, 1, io.ErrClosedPipe},
		{"success", strings.NewReader("abc"), io.Discard, 1, 1, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := copySlice(test.writer, test.body, test.skip, test.length, make([]byte, 32)); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestSubscriptionWithoutHijacker(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://racer"+wire.ClientObjectPrefix+strings.Repeat("0", 64), nil)
	r.RequestURI = r.URL.Path
	r.Header.Set("Content-Length", "0")
	NewHandler(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("origin called without hijacking")
		return nil, errors.New("unexpected")
	})).ServeHTTP(httptest.NewRecorder(), r)
}

func TestCanceledCreditWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- NewCredits().Reserve(ctx, wire.SubscriptionRequest{}, 0, 1) }()

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWriteHeadFailure(t *testing.T) {
	for _, size := range []int{1, 4096} {
		if err := writeHead(bufio.NewWriterSize(failingIO{}, size), 200, http.Header{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
}

func TestFetchInvalidRequest(t *testing.T) {
	if _, _, err := fetchPage(t.Context(), nil, wire.Request{}, nil); err == nil {
		t.Fatal("accepted invalid request")
	}
}
