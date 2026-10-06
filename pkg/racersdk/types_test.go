// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestEffectiveCapacityStats(t *testing.T) {
	volume, err := ParseVolumeName("capacity")
	require.NoError(t, err)
	c, err := NewClient(ClientConfig{Volume: volume})
	require.NoError(t, err)

	defer c.Close()

	s := c.Stats()
	require.Equal(t, 64, s.BulkLimit)
	require.Equal(t, 4, s.MetadataLimit)
	require.Equal(t, 4, s.SmallObjectLimit)
	require.Equal(t, 128, s.BulkQueueLimit)
	require.Equal(t, 16, s.MetadataQueueLimit)
	require.Equal(t, 128, s.SmallObjectQueueLimit)
}

func TestStreamingOptionsValidation(t *testing.T) {
	c := testClient(t, "unused", 1)
	for _, tt := range []struct {
		name    string
		ctx     context.Context
		options []ReadOptions
	}{
		{"negative pages", t.Context(), []ReadOptions{{PageCredits: -1}}},
		{"too many pages", t.Context(), []ReadOptions{{PageCredits: 65}}},
		{"too few bytes", t.Context(), []ReadOptions{{ByteCredits: PageSize - 1}}},
		{"too many bytes", t.Context(), []ReadOptions{{ByteCredits: 64*PageSize + 1}}},
		{"nil context", nil, nil},
		{"multiple options", t.Context(), []ReadOptions{{}, {}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.GetStreaming(tt.ctx, Request{}, tt.options...)
			assertKind(t, err, ErrorInvalidArgument)
		})
	}

	if c.Stats().Dials != 0 {
		t.Fatal("invalid options dialed")
	}
}

func TestIntegrationResponseHeadBoundary(t *testing.T) {
	for _, size := range []int{maxHeadBytes, maxHeadBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := socketDir(t) + "/socket"

			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(listener)

			finished := make(chan struct{})

			go func() {
				defer close(finished)

				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer closeBody(conn)

				_, err = readRawHead(bufio.NewReader(conn), false)
				if err != nil {
					return
				}

				fields := "Content-Length: 21\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Object-Length: 0\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 0\r\nConnection: close\r\nX: \r\n"
				padding := strings.Repeat("x", size-len(rawResponse(200, fields)))
				_, _ = conn.Write(rawResponse(200, strings.Replace(fields, "X: ", "X: "+padding, 1)))
			}()

			client := testClient(t, path, 1)

			v, err := client.Get(context.Background(), Request{})
			if size == maxHeadBytes {
				if err != nil {
					t.Fatal("exact limit rejected", err)
				}

				closeBody(v)
			} else {
				assertKind(t, err, ErrorProtocol)
			}

			<-finished
		})
	}
}

func TestIntegrationExpiredBootstrapDoesNotCacheVersion(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(_ context.Context, request OriginRequest) (Metadata, io.ReadCloser, error) {
		if _, pinned := request.Pin(); pinned || request.Operation() != OperationBootstrap {
			t.Error("fresh Get reused an old pin")
		}

		version := strconv.Itoa(int(calls.Add(1)))
		metadata := originMeta(1)
		metadata.ETag = ETag{value: `"` + version + `"`}

		return metadata, io.NopCloser(strings.NewReader(version)), nil
	})

	defer func() { cancel(); <-done }()

	client := originClient(t, path, 1)
	for _, want := range []string{"1", "2"} {
		value, err := client.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(value)
		closeBody(value)

		if err != nil || string(data) != want || value.Metadata().ETag.String() != `"`+want+`"` {
			t.Fatal("fresh read failed to select a new immutable version", err)
		}
	}

	if calls.Load() != 2 {
		t.Fatal("unexpected bootstrap count")
	}
}

func TestReadOptionsExactRangesAndMetadata(t *testing.T) {
	const size = 3*int64(PageSize) + 13
	for _, options := range []ReadOptions{{}, {Offset: 7, Length: 19}, {Offset: ByteOffset(PageSize) - 5, Length: PageSize + 17}, {Offset: ByteOffset(size - 1)}, {Offset: ByteOffset(size)}, {Offset: 3, Length: 2, Pin: ETag{value: `"v"`}}} {
		t.Run(strconv.FormatUint(uint64(options.Offset), 10)+"/"+strconv.FormatUint(uint64(options.Length), 10), func(t *testing.T) {
			var heads, gets atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Racer-Content-Type", "application/vnd.oci.image.manifest.v1+json")

				if r.Method == "HEAD" {
					heads.Add(1)

					if r.Header.Get("Range") != "" || r.Header.Get("If-Match") != options.Pin.String() {
						t.Error("HEAD envelope")
					}

					w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
					w.Header().Set("ETag", `"v"`)
					w.Header().Set("Racer-Expires-At", "0")

					return
				}

				gets.Add(1)

				length := int64(options.Length)
				if length == 0 {
					length = size - int64(options.Offset)
				}

				want := "bytes=" + strconv.FormatUint(uint64(options.Offset), 10) + "-"
				if options.Length != 0 {
					want += strconv.FormatInt(int64(options.Offset)+length-1, 10)
				}

				if options.Offset == 0 && options.Length == 0 {
					want = ""
				}

				if r.Method != "POST" || r.Header.Get("Range") != want || r.Header.Get("If-Match") != options.Pin.String() {
					t.Error("not an exact pinned range", r.Header)
				}

				if int64(options.Offset) == size {
					w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(416)

					return
				}

				streamResponseHead(w, int64(options.Offset), length, size, `"v"`)
				_, _ = io.CopyN(w, &offsetStream{offset: int64(options.Offset)}, length)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, options)
			if int64(options.Offset) == size {
				assertKind(t, err, ErrorUnsatisfiableRange)
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			defer closeBody(v)

			want := int64(options.Length)
			if want == 0 {
				want = size - int64(options.Offset)
			}

			n, err := v.WriteTo(&offsetSink{offset: int64(options.Offset)})
			if err != nil || n != want {
				t.Fatal(n, err)
			}

			wantHeads, wantGets := int32(0), int32(1)

			if v.Metadata().Size != ByteLength(size) || v.Metadata().ContentType != "application/vnd.oci.image.manifest.v1+json" || heads.Load() != wantHeads || gets.Load() != wantGets {
				t.Fatal("metadata or exchange count")
			}
		})
	}
}

func TestReadOptionsValidationAndPinnedHead(t *testing.T) {
	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Method != "POST" {
			t.Error("range must use one subscription")
		}

		if r.Header.Get("If-Match") == `"old"` {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(412)

			return
		}

		w.Header().Set("Content-Length", "0")
		w.Header().Set("Content-Range", "bytes */3")
		w.WriteHeader(416)
	}))

	c := testClient(t, path, 1)
	for _, o := range []ReadOptions{{Offset: math.MaxUint64}, {Length: math.MaxUint64}, {Offset: math.MaxInt64, Length: 1}, {Pin: ETag{value: "bad"}}} {
		_, err := c.Get(context.Background(), Request{}, o)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := c.Get(context.Background(), Request{}, ReadOptions{}, ReadOptions{})
	assertKind(t, err, ErrorInvalidArgument)

	if calls.Load() != 0 {
		t.Fatal("invalid arguments performed I/O")
	}

	for _, o := range []ReadOptions{{Offset: 4}, {Offset: 2, Length: 2}} {
		_, err := c.Get(context.Background(), Request{}, o)
		assertKind(t, err, ErrorUnsatisfiableRange)
	}

	_, err = c.Get(context.Background(), Request{}, ReadOptions{Pin: ETag{value: `"old"`}})
	assertKind(t, err, ErrorVersionUnavailable)
}

func TestContentTypeWireAndOrigin(t *testing.T) {
	for _, s := range []string{"application/vnd.oci.image.manifest.v1+json", "text/plain; charset=utf-8", `text/plain; x="a;b\"c"`, "a/" + strings.Repeat("b", 254)} {
		if err := validateContentType(s); err != nil {
			t.Fatal(s, err)
		}
	}

	for _, s := range []string{"text", "text/", "/plain", " text/plain", "text/plain ", "text/plain, text/html", "text/plain;", "text/plain;x", "text/plain;x=", `text/plain;x="`, "text/plain;x=a;X=a", "text/plain\r\nx:y", "text/\tplain", "text/pläin", "a/" + strings.Repeat("b", 255)} {
		if validateContentType(s) == nil {
			t.Fatal("accepted invalid MIME", s)
		}
	}

	m := originMeta(3)
	m.ExpiresAt = m.ExpiresAt.UTC()
	m.ContentType = "text/plain; charset=utf-8"

	c, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Operation() == OperationHead {
			return m, nil, nil
		}

		return m, io.NopCloser(strings.NewReader("abc")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got, err := c.Stat(context.Background(), Request{})
	if err != nil || got != m {
		t.Fatal("origin MIME did not survive HEAD", got, err)
	}

	v, err := c.Get(context.Background(), Request{}, ReadOptions{Offset: 1, Length: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	b, err := io.ReadAll(v)
	if err != nil || string(b) != "b" || v.Metadata() != m {
		t.Fatal("fake range or MIME", string(b), err)
	}

	r := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}
	for _, field := range []string{"Racer-Content-Type: \r\n", "Racer-Content-Type: text\r\n", "Racer-Content-Type: text/plain\r\nRacer-Content-Type: text/plain\r\n"} {
		_, err := parseResponseHead(rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"+field), r, nil)
		assertKind(t, err, ErrorProtocol)
	}
}

func TestSmallObjectSizeAndBootstrap(t *testing.T) {
	for _, size := range []ByteLength{0, 3, PageSize, PageSize + 1} {
		t.Run(strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			var calls atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)

				if r.Method != "POST" || r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "" {
					t.Error("SmallObject did not use a single unpinned subscription")
				}

				streamResponse(w, 0, int64(min(size, PageSize)), int64(size), `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true})
			if size > PageSize {
				assertKind(t, err, ErrorInvalidArgument)

				if v != nil || c.Stats().BytesRead != 0 || c.Stats().ActiveSmallObjects != 0 {
					t.Fatal("oversized body exposed", c.Stats())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}

				defer closeBody(v)

				if n, err := v.WriteTo(io.Discard); err != nil || n != int64(size) {
					t.Fatal(n, err)
				}
			}

			if calls.Load() != 1 {
				t.Fatal("unexpected remainder or HEAD", calls.Load())
			}
		})
	}

	c := testClient(t, "unused", 1)
	m := originMeta(PageSize + 1)
	_, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true, Length: 1, Metadata: &m})
	assertKind(t, err, ErrorInvalidArgument)

	if c.Stats().Dials != 0 {
		t.Fatal("oversized snapshot performed I/O")
	}
}

func TestSmallObjectPinnedRangeAndHeadSizeValidation(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			t.Run(strconv.FormatBool(oversized)+"/snapshot="+strconv.FormatBool(snapshot), func(t *testing.T) {
				m := originMeta(3)
				if oversized {
					m.Size = PageSize + 1
				}

				var heads, gets atomic.Int32

				path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "HEAD" {
						heads.Add(1)
						w.Header().Set("Content-Length", strconv.FormatUint(uint64(m.Size), 10))
						w.Header().Set("ETag", `"v"`)
						w.Header().Set("Racer-Expires-At", "0")

						return
					}

					gets.Add(1)

					pin := ""
					if snapshot {
						pin = `"v"`
					}

					if r.Header.Get("If-Match") != pin || r.Header.Get("Range") != "bytes=1-1" {
						t.Error("small range lost pin or bounds")
					}

					streamResponse(w, 1, 1, int64(m.Size), `"v"`)
				}))
				c := testClient(t, path, 1)

				o := ReadOptions{SmallObject: true, Offset: 1, Length: 1}
				if snapshot {
					o.Metadata = &m
				}

				v, err := c.Get(context.Background(), Request{}, o)
				if oversized {
					assertKind(t, err, ErrorInvalidArgument)

					wantGets := int32(1)
					if snapshot {
						wantGets = 0
					}

					if v != nil || gets.Load() != wantGets {
						t.Fatal("oversized pinned object fetched")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}

					if c.Stats().ActiveSmallObjects != 1 || c.Stats().ActiveBulk != 0 {
						t.Fatal(c.Stats())
					}

					if n, err := v.WriteTo(io.Discard); n != 1 || err != nil {
						t.Fatal(n, err)
					}

					closeBody(v)

					if gets.Load() != 1 {
						t.Fatal("extra small GET")
					}
				}

				wantHeads := int32(0)

				if heads.Load() != wantHeads {
					t.Fatal("wrong HEAD count", heads.Load())
				}

				closeBody(c)

				if c.Stats().Connections != 0 {
					t.Fatal("small pool connections retained")
				}
			})
		}
	}
}

func TestKey(t *testing.T) {
	for _, s := range []string{strings.Repeat("0", 64), strings.Repeat("0123456789abcdef", 4)} {
		key, err := ParseKey(s)
		if err != nil || key.String() != s {
			t.Fatalf("key round trip: %v", err)
		}
	}

	for _, s := range []string{"", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), " " + strings.Repeat("0", 63)} {
		_, err := ParseKey(s)
		assertKind(t, err, ErrorInvalidArgument)
	}

	if (Key{}).String() != strings.Repeat("0", 64) {
		t.Fatal("zero key")
	}
}

func TestVolumeName(t *testing.T) {
	for _, s := range []string{"a", "a-b.c0", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		n, err := ParseVolumeName(s)
		if err != nil || n.String() != s {
			t.Fatalf("valid name %q: %v", s, err)
		}
	}

	for _, s := range []string{"", "A", ".a", "a.", "a..b", "-a", "a-", "a/b", "a_b", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		_, err := ParseVolumeName(s)
		assertKind(t, err, ErrorInvalidArgument)

		if !strings.Contains(err.Error(), "volume name") {
			t.Fatalf("invalid name %q: %v", s, err)
		}
	}
}

func TestOpaqueAndDiagnostics(t *testing.T) {
	secret := "never-print-this, credential\xff"

	m, err := ParseAdapterMetadata(secret)
	if err != nil {
		t.Fatal(err)
	}

	a, err := ParseAuthorization(secret)
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewFetchContext(m, a)
	if err != nil {
		t.Fatal(err)
	}

	if c.Metadata().ForOrigin() != secret || c.Authorization().ForOrigin() != secret {
		t.Fatal("context changed")
	}

	request := Request{Context: c}
	origin := OriginRequest{context: c}
	cause := errors.New(secret)

	err = NewOriginError(ErrorUnavailable, cause)

	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatal("missing typed error")
	}

	for _, value := range []any{m, &m, a, &a, c, &c, request, &request, origin, &origin, err, *typed} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(verb, value), "never-print-this") {
				t.Fatalf("unsafe %s diagnostic", verb)
			}
		}

		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil || strings.Contains(string(encoded), "never-print-this") {
			t.Fatal("unsafe serialization")
		}
	}

	if !errors.Is(err, cause) {
		t.Fatal("lost explicit cause")
	}
}

func TestZeroValues(t *testing.T) {
	if (VolumeName{}).String() != "" || (ETag{}).String() != "" {
		t.Fatal("zero strings")
	}

	c, err := NewFetchContext(AdapterMetadata{}, Authorization{})
	if err != nil || c != (FetchContext{}) {
		t.Fatal("zero context")
	}

	r := OriginRequest{}
	if r.Operation() != 0 || r.Context() != c || r.Key() != (Key{}) {
		t.Fatal("zero origin")
	}

	if _, ok := r.Pin(); ok {
		t.Fatal("zero pin present")
	}

	if _, ok := r.Range(); ok {
		t.Fatal("zero range present")
	}

	_, _, err = (Range{}).Resolve(1)
	assertKind(t, err, ErrorInvalidArgument)

	var e *Error
	if e.Kind() != 0 || e.StatusCode() != 0 || e.Operation() != "" || e.Unwrap() != nil {
		t.Fatal("nil error")
	}

	if (&Error{}).Error() == "" || e.Error() == "" {
		t.Fatal("empty diagnostic")
	}
}

func TestErrors(t *testing.T) {
	for _, tt := range []struct {
		kind   ErrorKind
		status int
	}{{ErrorInvalidArgument, 400}, {ErrorUnauthorized, 401}, {ErrorForbidden, 403}, {ErrorNotFound, 404}, {ErrorVersionUnavailable, 412}, {ErrorUnsatisfiableRange, 416}, {ErrorHeaderLimit, 431}, {ErrorInternal, 500}, {ErrorBadGateway, 502}, {ErrorUnavailable, 503}, {ErrorCanceled, 503}, {ErrorDeadline, 503}, {ErrorIO, 500}, {0, 500}} {
		if got := callbackStatus(NewOriginError(tt.kind, nil), false); got != tt.status {
			t.Fatalf("%v: %d", tt.kind, got)
		}
	}

	if callbackStatus(NewOriginError(ErrorNotFound, nil), true) != 412 || callbackStatus(errors.New("private"), false) != 500 {
		t.Fatal("callback classification")
	}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		err := ioFailure("read", cause)
		if !errors.Is(err, cause) {
			t.Fatal("lost cause")
		}

		if cause != io.ErrUnexpectedEOF && callbackStatus(err, false) != 503 {
			t.Fatal("context status")
		}
	}

	if ioFailure("read", io.EOF) != io.EOF || ioFailure("read", nil) != nil {
		t.Fatal("EOF changed")
	}

	err := statusError(412)
	if err.StatusCode() != 412 || err.Operation() != "response" {
		t.Fatal("error details")
	}
}

func FuzzValidatedTypes(f *testing.F) {
	for _, s := range []string{"", `""`, `"a,b"`, `"a\b"`, "opaque\xff", " padded ", strings.Repeat("a", 64), "a.b", "9223372036854775808"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if key, err := ParseKey(s); err == nil && key.String() != s {
			t.Fatal("key normalized")
		}

		if tag, err := ParseETag(s); err == nil && tag.String() != s {
			t.Fatal("tag normalized")
		}

		if name, err := ParseVolumeName(s); err == nil && name.String() != s {
			t.Fatal("name normalized")
		}

		if a, err := ParseAuthorization(s); err == nil && a.ForOrigin() != s {
			t.Fatal("authorization normalized")
		}

		if m, err := ParseAdapterMetadata(s); err == nil && m.ForOrigin() != s {
			t.Fatal("metadata normalized")
		}
	})
}

func TestErrorKindWireParity(t *testing.T) {
	for _, tt := range []struct {
		wire wire.ErrorKind
		sdk  ErrorKind
		name string
	}{
		{0, 0, "unspecified"},
		{wire.ErrorInvalidArgument, ErrorInvalidArgument, "invalid argument"},
		{wire.ErrorClosed, ErrorClosed, "closed"},
		{wire.ErrorProtocol, ErrorProtocol, "protocol"},
		{wire.ErrorUnauthorized, ErrorUnauthorized, "unauthorized"},
		{wire.ErrorForbidden, ErrorForbidden, "forbidden"},
		{wire.ErrorNotFound, ErrorNotFound, "not found"},
		{wire.ErrorVersionUnavailable, ErrorVersionUnavailable, "version unavailable"},
		{wire.ErrorUnsatisfiableRange, ErrorUnsatisfiableRange, "unsatisfiable range"},
		{wire.ErrorHeaderLimit, ErrorHeaderLimit, "header limit"},
		{wire.ErrorInternal, ErrorInternal, "internal"},
		{wire.ErrorBadGateway, ErrorBadGateway, "bad gateway"},
		{wire.ErrorUnavailable, ErrorUnavailable, "unavailable"},
		{wire.ErrorCanceled, ErrorCanceled, "canceled"},
		{wire.ErrorDeadline, ErrorDeadline, "deadline"},
		{wire.ErrorIO, ErrorIO, "I/O"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, uint8(tt.sdk), uint8(tt.wire))
			require.Equal(t, tt.name, tt.sdk.String())
			require.Equal(t, tt.name, ErrorKind(tt.wire).String())
		})
	}
}

func TestWireErrorMapping(t *testing.T) {
	cause := errors.New("private cause")

	for value := range 256 {
		kind := ErrorKind(value)

		wantKind := kind
		if value == 0 || kind == ErrorClosed || kind > ErrorIO {
			wantKind = ErrorInternal
		}

		for _, status := range []int{0, 503} {
			err := fromWireError(&wire.Error{Kind: wire.ErrorKind(value), Operation: "response", Status: status, Err: cause})
			assertKind(t, err, wantKind)

			want := "racersdk response: " + wantKind.String()
			if status != 0 {
				want += " (HTTP 503)"
			}

			if err.Error() != want || !errors.Is(err, cause) {
				t.Fatal("changed diagnostic or cause", err)
			}

			if fmt.Sprintf("%#v", err) != want {
				t.Fatal("unsafe diagnostic")
			}
		}
	}

	for _, err := range []error{nil, io.EOF, cause} {
		if fromWireError(err) != err {
			t.Fatal("changed non-wire error")
		}
	}

	nested := fromWireError(&wire.Error{Kind: wire.ErrorBadGateway, Operation: "origin metadata", Err: &wire.Error{Kind: wire.ErrorInvalidArgument, Operation: "metadata"}})
	assertKind(t, errors.Unwrap(nested), ErrorInvalidArgument)

	for _, tt := range []struct {
		kind ErrorKind
		err  error
	}{
		{ErrorIO, ioFailure("socket path", cause)},
		{ErrorInvalidArgument, failure(ErrorInvalidArgument, "socket path", cause)},
	} {
		t.Run(tt.kind.String(), func(t *testing.T) {
			assertKind(t, tt.err, tt.kind)
			require.EqualError(t, tt.err, "racersdk socket path: "+tt.kind.String())
			require.ErrorIs(t, tt.err, cause)
		})
	}
}
