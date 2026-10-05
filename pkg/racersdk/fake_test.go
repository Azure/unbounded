// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Root white-box tests use the daemon directly, without importing racersdktest
// (which would cycle back to this package under test).
func newFakeClient(t *testing.T, origin Origin) (*Client, func(), error) {
	t.Helper()

	if origin == nil {
		return nil, nil, failure(ErrorInvalidArgument, "fake origin", nil)
	}

	path, cancel, done := startOrigin(t, OriginConfig{}, origin)
	c := originClient(t, path, 64)

	var once sync.Once

	return c, func() { once.Do(func() { closeBody(c); cancel(); <-done }) }, nil
}

func TestFakeClientPages(t *testing.T) {
	for _, size := range []ByteLength{0, 1, PageSize, PageSize + 13, 3*PageSize + 13} {
		t.Run(strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			var (
				calls atomic.Int32
				seen  sync.Map
			)

			request := Request{Key: Key{1, 2, 3}, Context: FetchContext{
				metadata:      AdapterMetadata{value: "opaque, interior  spaces\\\xff"},
				authorization: Authorization{value: "Scheme opaque,credential\x80"},
			}}

			client, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				call := calls.Add(1)

				if r.Key() != request.Key || r.Context() != request.Context {
					t.Error("key or context changed")
				}

				m := originMeta(size)

				page, ok := r.Range()
				if !ok || validatePageShape(page) != nil {
					t.Error("origin received a non-page range")
				}

				if call == 1 {
					if _, pinned := r.Pin(); pinned || r.Operation() != OperationBootstrap {
						t.Error("bootstrap changed")
					}
				} else {
					m.ExpiresAt = time.UnixMilli(int64(call))

					pin, ok := r.Pin()
					if !ok || pin != m.ETag || r.Operation() != OperationPinned {
						t.Error("continuation lost immutable pin")
					}
				}

				if size == 0 {
					return m, nil, nil
				}

				first, last, err := page.Resolve(size)
				if err != nil {
					return m, nil, err
				}

				if _, duplicate := seen.LoadOrStore(first, true); duplicate || uint64(first)%uint64(PageSize) != 0 {
					t.Error("duplicate or unaligned page scheduling")
				}

				return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			value, err := client.Get(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			if calls.Load() != 1 {
				t.Fatal("Get eagerly fetched a continuation")
			}

			sink := &offsetSink{}

			n, err := io.Copy(sink, value)
			if err != nil || n != int64(size) || sink.offset != int64(size) {
				t.Fatal("stream mismatch", n, err)
			}

			m := value.Metadata()
			if calls.Load() != int32(max(1, (size+PageSize-1)/PageSize)) || m.Size != size || m.ETag != originMeta(size).ETag || m.ExpiresAt.UnixMilli() != 0 {
				t.Fatal("page count or metadata snapshot changed")
			}
		})
	}
}

func fakeSubscriptionSocket(t *testing.T, client *Client, headers string) (net.Conn, *http.Response) {
	t.Helper()

	conn, _, err := client.bulk.Get(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(conn) })

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	_, err = fmt.Fprintf(conn, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n%s\r\n", (Key{}).String(), headers)
	if err != nil {
		t.Fatal(err)
	}

	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(res.Body) })

	return conn, res
}

func fakeReadFrame(t *testing.T, reader io.Reader, kind byte, page, offset uint64, length uint32) {
	t.Helper()

	var frame [21]byte
	if _, err := io.ReadFull(reader, frame[:]); err != nil {
		t.Fatal(err)
	}

	if frame[0] != kind || binary.BigEndian.Uint64(frame[1:9]) != page || binary.BigEndian.Uint64(frame[9:17]) != offset || binary.BigEndian.Uint32(frame[17:]) != length {
		t.Fatalf("unexpected frame: %x; want kind=%d page=%d offset=%d length=%d", frame, kind, page, offset, length)
	}

	if length != 0 {
		sink := &offsetSink{offset: int64(offset)}
		if n, err := io.CopyN(sink, reader, int64(length)); err != nil || n != int64(length) {
			t.Fatal("payload", n, err)
		}
	}
}

func fakeRelease(t *testing.T, conn net.Conn, page uint64, length uint32) {
	t.Helper()

	var release [12]byte
	binary.BigEndian.PutUint64(release[:8], page)
	binary.BigEndian.PutUint32(release[8:], length)

	if _, err := conn.Write(release[:]); err != nil {
		t.Fatal(err)
	}
}

func fakeSubscriptionOrigin(t *testing.T, size ByteLength) Origin {
	t.Helper()

	return func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		m := originMeta(size)

		m.ContentType = "test/example"
		if r.Operation() == OperationHead || size == 0 {
			return m, nil, nil
		}

		page, _ := r.Range()

		first, last, err := page.Resolve(size)
		if err != nil {
			return m, nil, err
		}

		return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
	}
}

func TestFakeSubscriptionFrames(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    ByteLength
		headers string
		first   uint64
		end     uint64
	}{
		{name: "empty"},
		{name: "whole", size: 13, end: 13},
		{name: "open", size: PageSize + 13, headers: "Range: bytes=16777211-\r\n", first: uint64(PageSize) - 5, end: uint64(PageSize) + 13},
		{name: "closed", size: 2 * PageSize, headers: "Range: bytes=16777211-16777219\r\nRacer-Ordered: 0\r\n", first: uint64(PageSize) - 5, end: uint64(PageSize) + 4},
		{name: "clamped", size: 13, headers: "Range: bytes=3-20\r\n", first: 3, end: 13},
		{name: "pinned", size: 13, headers: "If-Match: " + originMeta(13).ETag.String() + "\r\nRacer-Ordered: 1\r\n", end: 13},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, test.size))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, res := fakeSubscriptionSocket(t, client, test.headers)
			if res.StatusCode != 200 || !res.Close || res.Header.Get("ETag") != originMeta(test.size).ETag.String() || res.Header.Get("Racer-Expires-At") != "0" || res.Header.Get("Racer-Content-Type") != "test/example" {
				t.Fatal("response", res.Status, res.Header)
			}

			for name, want := range map[string]uint64{"Racer-Object-Length": uint64(test.size), "Racer-Range-Start": test.first, "Racer-Range-End": test.end} {
				if res.Header.Get(name) != strconv.FormatUint(want, 10) {
					t.Fatal(name, res.Header.Get(name), want)
				}
			}

			pages := uint64(0)

			for offset := test.first; offset < test.end; {
				end := min((offset/uint64(PageSize)+1)*uint64(PageSize), test.end)
				fakeReadFrame(t, res.Body, 1, offset/uint64(PageSize), offset, uint32(end-offset))
				offset = end
				pages++
			}

			fakeReadFrame(t, res.Body, 2, pages, test.end-test.first, 0)

			if res.ContentLength != int64(test.end-test.first+21*(pages+1)) {
				t.Fatal("content length", res.ContentLength)
			}

			if _, err := res.Body.Read(make([]byte, 1)); err != io.EOF {
				t.Fatal("missing EOF", err)
			}
		})
	}
}

func TestFakeSubscriptionCredits(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers string
		window  uint64
	}{
		{name: "defaults", window: 2},
		{name: "pages", headers: "Racer-Page-Credits: 1\r\n", window: 1},
		{name: "bytes", headers: "Racer-Page-Credits: 64\r\nRacer-Byte-Credits: 16777216\r\n", window: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			conn, res := fakeSubscriptionSocket(t, client, test.headers)
			for page := range test.window {
				fakeReadFrame(t, res.Body, 1, page, page*uint64(PageSize), uint32(PageSize))
			}

			if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}

			var b [1]byte

			_, err = res.Body.Read(b[:])

			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal("stream advanced without credits", err)
			}

			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}

			for page := test.window; page < 3; page++ {
				fakeRelease(t, conn, page-test.window, uint32(PageSize))
				fakeReadFrame(t, res.Body, 1, page, page*uint64(PageSize), uint32(PageSize))
			}

			fakeReadFrame(t, res.Body, 2, 3, 3*uint64(PageSize), 0)
		})
	}
}

func TestFakeSubscriptionInvalidRelease(t *testing.T) {
	for _, test := range []struct {
		name   string
		page   uint64
		length uint32
	}{
		{name: "unknown", page: 4, length: uint32(PageSize)},
		{name: "length", length: uint32(PageSize) - 1},
		{name: "zero"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 2*PageSize))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)
			conn, res := fakeSubscriptionSocket(t, client, "Racer-Page-Credits: 1\r\n")
			fakeReadFrame(t, res.Body, 1, 0, 0, uint32(PageSize))
			fakeRelease(t, conn, test.page, test.length)

			if _, err := res.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("invalid release did not close the subscription", err)
			}
		})
	}
}

func TestFakeSubscriptionInvalidHeaders(t *testing.T) {
	client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		t.Error("invalid subscription reached origin")
		return Metadata{}, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	for _, headers := range []string{
		"Racer-Page-Credits: 0\r\n", "Racer-Page-Credits: 65\r\n",
		"Racer-Byte-Credits: 16777215\r\n", "Racer-Byte-Credits: 1073741825\r\n",
		"Racer-Ordered: 2\r\n", "Racer-Ordered: \r\n",
		"Range: bytes=-5\r\n", "Range: bytes=2-1\r\n", "Range: bytes=0-1,3-4\r\n",
		"Racer-Page-Credits: 1\r\nRacer-Page-Credits: 2\r\n", "If-Match: W/\"weak\"\r\n",
	} {
		t.Run(strings.TrimSpace(headers), func(t *testing.T) {
			_, res := fakeSubscriptionSocket(t, client, headers)
			if res.StatusCode != 400 {
				t.Fatal(res.Status)
			}
		})
	}
}

func TestFakeSubscriptionCancellation(t *testing.T) {
	for _, action := range []string{"disconnect", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: true}

			client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return originMeta(1), body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			conn, res := fakeSubscriptionSocket(t, client, "")
			if res.StatusCode != 200 {
				t.Fatal(res.Status)
			}

			if action == "disconnect" {
				closeBody(conn)
			} else {
				cleanup()
			}

			select {
			case <-body.done:
			case <-time.After(5 * time.Second):
				t.Fatal("subscription retained origin body")
			}
		})
	}
}

func TestFakeSubscriptionErrors(t *testing.T) {
	for _, kind := range []ErrorKind{ErrorUnauthorized, ErrorForbidden, ErrorNotFound, ErrorVersionUnavailable, ErrorUnavailable, ErrorInternal} {
		t.Run(kind.String(), func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return Metadata{}, nil, NewOriginError(kind, nil)
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, res := fakeSubscriptionSocket(t, client, "")
			if statusError(res.StatusCode).kind != kind || res.ContentLength != 0 || !res.Close {
				t.Fatal("origin error changed", res.Status, res.Header)
			}
		})
	}

	for _, size := range []ByteLength{0, 13} {
		t.Run("unsatisfiable/"+strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, size))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, res := fakeSubscriptionSocket(t, client, "Range: bytes=13-\r\n")
			if res.StatusCode != 416 || res.ContentLength != 0 || res.Header.Get("Content-Range") != "bytes */"+strconv.FormatUint(uint64(size), 10) {
				t.Fatal(res.Status, res.Header)
			}
		})
	}
}

func TestFakeSubscriptionDuplicateRelease(t *testing.T) {
	client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)
	conn, res := fakeSubscriptionSocket(t, client, "Racer-Page-Credits: 1\r\n")
	fakeReadFrame(t, res.Body, 1, 0, 0, uint32(PageSize))
	fakeRelease(t, conn, 0, uint32(PageSize))
	fakeReadFrame(t, res.Body, 1, 1, uint64(PageSize), uint32(PageSize))
	fakeRelease(t, conn, 0, uint32(PageSize))

	if _, err := res.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("duplicate release did not close the subscription", err)
	}
}

func TestFakeSubscriptionPartialPageRelease(t *testing.T) {
	client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, PageSize+3))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)
	conn, res := fakeSubscriptionSocket(t, client, "Range: bytes=16777214-\r\nRacer-Page-Credits: 1\r\n")
	fakeReadFrame(t, res.Body, 1, 0, uint64(PageSize)-2, 2)
	fakeRelease(t, conn, 0, 2)
	fakeReadFrame(t, res.Body, 1, 1, uint64(PageSize), 3)
	fakeReadFrame(t, res.Body, 2, 2, 5, 0)
}

func TestFakeSubscriptionPendingCallbackCancellation(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})

	client, cleanup, err := newFakeClient(t, func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)

		return Metadata{}, nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	conn, _, err := client.bulk.Get(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(conn)

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprintf(conn, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n", (Key{}).String()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("origin not called")
	}

	closeBody(conn)

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel pending origin callback")
	}
}
