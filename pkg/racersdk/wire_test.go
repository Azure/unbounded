// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rawRequest(method, fields string) []byte {
	return []byte(method + " " + objectPrefix + (Key{}).String() + " HTTP/1.1\r\nHost: racer\r\n" + fields + "\r\n")
}

func rawResponse(status int, fields string) []byte {
	return []byte("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" + fields + "\r\n")
}

func TestRequestWire(t *testing.T) {
	for _, tt := range []struct {
		method, fields string
		op             Operation
		origin         bool
	}{
		{"HEAD", "", OperationHead, true},
		{"HEAD", "If-Match: \"\"\r\n", OperationHead, true},
		{"GET", "Range: bytes=0-16777215\r\n", OperationBootstrap, true},
		{"GET", "Range: bytes=16777216-33554431\r\nIf-Match: \"a,b\\c\"\r\n", OperationPinned, true},
		{"GET", "Range: bytes=0-1\r\nIf-Match: \"v\"\r\n", OperationPinned, true},
		{"GET", "Range: bytes=-0\r\nIf-Match: \"v\"\r\n", OperationPinned, false},
		{"GET", "Range: bytes=16777216-\r\nIf-Match: \"v\"\r\n", OperationPinned, false},
	} {
		head := rawRequest(tt.method, tt.fields+"Racer-Metadata: opaque,\xff value\r\nAuthorization: Bearer secret\r\n")

		r, err := parseRequestHead(head, tt.origin)
		if err != nil || r.Operation() != tt.op {
			t.Fatalf("%s %s: %v", tt.method, tt.fields, err)
		}

		if r.Context().Metadata().ForOrigin() != "opaque,\xff value" || r.Context().Authorization().ForOrigin() != "Bearer secret" {
			t.Fatal("lost context bytes")
		}

		canonical, err := requestHead(r)
		if err != nil {
			t.Fatal(err)
		}

		roundTrip, err := parseRequestHead(canonical, tt.origin)
		if err != nil || roundTrip != r {
			t.Fatal("request round trip")
		}
	}

	for _, fields := range []string{
		"", "Range: bytes=0-1\r\n", "Range: bytes=1-16777215\r\nIf-Match: \"v\"\r\n", "Range: bytes=0-16777216\r\nIf-Match: \"v\"\r\n", "Range: bytes=-1\r\nIf-Match: \"v\"\r\n", "Range: bytes=0-\r\nIf-Match: \"v\"\r\n",
		"Range: bytes=0-16777215\r\nContent-Length: 1\r\n", "Range: bytes=0-16777215\r\nIf-Match: W/\"v\"\r\n",
	} {
		_, err := parseRequestHead(rawRequest("GET", fields), true)
		assertKind(t, err, ErrorInvalidArgument)
	}

	for _, field := range []string{"Transfer-Encoding: identity", "Content-Encoding: identity", "Expect: 100-continue", "Trailer: X", "If-None-Match: *", "If-Modified-Since: date", "If-Unmodified-Since: date", "If-Range: \"v\"", "Connection: Upgrade", "Content-Length: 00", "Content-Range: bytes */0", "If-Match:", "Authorization:", "Host: racer"} {
		_, err := parseRequestHead(rawRequest("HEAD", field+"\r\n"), true)
		if err == nil {
			t.Fatalf("accepted %s", field)
		}
	}

	_, err := parseRequestHead(rawRequest("HEAD", "Range: bytes=0-0\r\n"), true)
	assertKind(t, err, ErrorInvalidArgument)
	_, err = parseRequestHead(rawRequest("POST", ""), true)

	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode() != 405 {
		t.Fatal("method status")
	}

	valid := string(rawRequest("HEAD", ""))
	for _, target := range []string{objectPrefix + (Key{}).String() + "?", objectPrefix + (Key{}).String() + "#x", "http://racer" + objectPrefix + (Key{}).String(), objectPrefix + strings.Repeat("A", 64), objectPrefix + "%30" + strings.Repeat("0", 63), "/v1//objects/" + (Key{}).String()} {
		head := strings.Replace(valid, objectPrefix+(Key{}).String(), target, 1)

		_, err := parseRequestHead([]byte(head), true)
		if err == nil {
			t.Fatalf("accepted target %s", target)
		}
	}

	for _, head := range []string{strings.Replace(valid, "HTTP/1.1", "HTTP/1.0", 1), strings.Replace(valid, "Host: racer\r\n", "", 1), strings.Replace(valid, "Host: racer", "Host: other", 1)} {
		_, err := parseRequestHead([]byte(head), true)
		if err == nil {
			t.Fatal("accepted bad envelope")
		}
	}

	_, err = requestHead(OriginRequest{})
	assertKind(t, err, ErrorInvalidArgument)
}

func TestRawHeaders(t *testing.T) {
	for _, field := range []string{"Host", "Content-Length", "Content-Type", "Content-Range", "ETag", "If-Match", "Range", "Racer-Expires-At", "Racer-Metadata", "Authorization"} {
		for _, second := range []string{"same", "different"} {
			head := []byte("HEAD / HTTP/1.1\r\n" + field + ": same\r\n" + strings.ToLower(field) + ": " + second + "\r\n\r\n")
			assertKind(t, validateRawHead(head, false), ErrorInvalidArgument)
			assertKind(t, validateRawHead(head, true), ErrorProtocol)
		}
	}

	for _, value := range []string{"", "x", "  x", " x ", "\tx", " x\t", " a\r\nb", " a\x00b", " a\x7fb"} {
		for _, field := range []string{"Authorization", "Racer-Metadata"} {
			err := validateRawHead(rawRequest("HEAD", field+":"+value+"\r\n"), false)
			if err == nil {
				t.Fatalf("accepted malformed %s", field)
			}
		}
	}

	for _, line := range []string{" Folded: x", "X : x", "X\t: x", "X: a\nb", "X: a\rb", ": x"} {
		if err := validateRawHead(rawRequest("HEAD", line+"\r\n"), false); err == nil {
			t.Fatal("accepted malformed field")
		}
	}

	for _, count := range []int{8192, 8193} {
		err := validateRawHead(rawRequest("HEAD", "Authorization: "+strings.Repeat("a", count)+"\r\n"), false)
		if count == 8192 {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	base := rawRequest("HEAD", "X: \r\n")
	for _, size := range []int{maxHeadBytes - 1, maxHeadBytes, maxHeadBytes + 1} {
		head := rawRequest("HEAD", "X: "+strings.Repeat("x", size-len(base))+"\r\n")

		got, err := readRawHead(bufio.NewReader(bytes.NewReader(head)), false)
		if size <= maxHeadBytes {
			if err != nil || !bytes.Equal(got, head) {
				t.Fatal("head boundary")
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	_, err := readRawHead(bufio.NewReader(strings.NewReader("HTTP/1.1")), true)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncated head")
	}

	_, err = readRawHead(bufio.NewReader(strings.NewReader("")), false)
	if err != io.EOF {
		t.Fatal("idle EOF")
	}
}

func TestStdlibNormalizationRequiresRawValidation(t *testing.T) {
	head := rawRequest("HEAD", "Authorization:  secret \r\nContent-Length: 0\r\nContent-Length: 0\r\n")

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		t.Fatal(err)
	}

	if req.Header.Get("Authorization") != "secret" || len(req.Header.Values("Content-Length")) != 1 {
		t.Fatal("stdlib normalization changed; revisit guard assumptions")
	}

	if err := validateRawHead(head, false); err == nil {
		t.Fatal("guard trusted normalized headers")
	}
}

func TestResponseWire(t *testing.T) {
	bootstrap := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}

	tag, err := ParseETag(`"a,b"`)
	if err != nil {
		t.Fatal(err)
	}

	pinnedRange, err := FromRange(ByteOffset(PageSize))
	if err != nil {
		t.Fatal(err)
	}

	pinned := OriginRequest{operation: OperationPinned, pin: tag, byteRange: pinnedRange}

	meta := "ETag: \"a,b\"\r\nRacer-Expires-At: 0\r\n"
	for _, tt := range []struct {
		request OriginRequest
		status  int
		fields  string
		size    ByteLength
		length  int64
	}{
		{OriginRequest{operation: OperationHead}, 200, "Content-Length: 9223372036854775807\r\n", math.MaxInt64, 0},
		{bootstrap, 200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\n", 0, 0},
		{bootstrap, 206, "Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\n", 1, 1},
		{bootstrap, 206, "Content-Length: 16777216\r\nContent-Range: bytes 0-16777215/16777217\r\nContent-Type: application/octet-stream\r\n", PageSize + 1, int64(PageSize)},
		{pinned, 206, "Content-Length: 1\r\nContent-Range: bytes 16777216-16777216/16777217\r\nContent-Type: application/octet-stream\r\n", PageSize + 1, 1},
	} {
		got, err := parseResponseHead(rawResponse(tt.status, meta+tt.fields), tt.request, nil)
		if err != nil || got.metadata.Size != tt.size || got.length != tt.length || got.metadata.ETag != tag {
			t.Fatalf("success response: %+v %v", got, err)
		}

		snapshot := got.metadata
		if _, err := parseResponseHead(rawResponse(tt.status, strings.Replace(meta, "At: 0", "At: 1", 1)+tt.fields), tt.request, &snapshot); err != nil {
			t.Fatal("expiry refresh rejected")
		}

		snapshot.Size++
		_, err = parseResponseHead(rawResponse(tt.status, meta+tt.fields), tt.request, &snapshot)
		assertKind(t, err, ErrorProtocol)
	}

	valid := string(rawResponse(206, meta+"Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\n"))
	for _, pair := range [][2]string{{"206 Partial Content", "200 OK"}, {"206 Partial Content", "302 Found"}, {"206 Partial Content", "100 Continue"}, {"HTTP/1.1", "HTTP/1.0"}, {"Content-Length: 1\r\n", ""}, {"Content-Length: 1", "Content-Length: 01"}, {"Content-Length: 1", "Content-Length: 2"}, {"Content-Length: 1", "Content-Length: 1, 1"}, {"Content-Length: 1", "Content-Length: 1\r\nContent-Length: 1"}, {"bytes 0-0/1", "bytes 1-1/2"}, {"bytes 0-0/1", "bytes */1"}, {"bytes 0-0/1", "bytes 0-0/*"}, {"ETag: \"a,b\"", "ETag: W/\"a,b\""}, {"Racer-Expires-At: 0", "Racer-Expires-At: 9223372036854775808"}, {"application/octet-stream", "text/plain"}, {"Content-Length: 1", "Content-Length: 1\r\nTransfer-Encoding: chunked"}, {"Content-Length: 1", "Content-Length: 1\r\nContent-Encoding: identity"}} {
		_, err := parseResponseHead([]byte(strings.Replace(valid, pair[0], pair[1], 1)), bootstrap, nil)
		assertKind(t, err, ErrorProtocol)
	}

	wrongPin := bootstrap
	wrongPin.operation, wrongPin.pin = OperationPinned, ETag{value: `"other"`}
	_, err = parseResponseHead([]byte(valid), wrongPin, nil)
	assertKind(t, err, ErrorProtocol)
	_, err = parseResponseHead(rawResponse(200, meta+"Content-Length: 0\r\nContent-Type: application/octet-stream\r\n"), pinned, nil)
	assertKind(t, err, ErrorProtocol)
}

func TestErrorResponses(t *testing.T) {
	r := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}

	for _, status := range []int{400, 401, 403, 404, 405, 412, 416, 431, 500, 502, 503} {
		fields := "Content-Length: 0\r\n"
		if status == 416 {
			fields += "Content-Range: bytes */0\r\n"
		}

		if status == 405 {
			fields += "Allow: HEAD, GET\r\n"
		}

		_, err := parseResponseHead(rawResponse(status, fields), r, nil)

		var typed *Error
		if !errors.As(err, &typed) || typed.StatusCode() != status {
			t.Fatalf("status %d: %v", status, err)
		}

		for _, extra := range []string{"ETag: \"v\"\r\n", "Racer-Expires-At: 0\r\n"} {
			_, err = parseResponseHead(rawResponse(status, fields+extra), r, nil)
			assertKind(t, err, ErrorProtocol)
		}
	}

	for _, fields := range []string{"Content-Length: 0\r\n", "Content-Length: 0\r\nContent-Range: bytes 0-0/1\r\n", "Content-Length: 1\r\nContent-Range: bytes */1\r\n"} {
		_, err := parseResponseHead(rawResponse(416, fields), r, nil)
		assertKind(t, err, ErrorProtocol)
	}

	for _, s := range []string{"", "00", "+1", "-1", " 1", "1 ", "1,1", "9223372036854775808"} {
		if _, err := decimal(s); err == nil {
			t.Fatal("noncanonical decimal accepted")
		}
	}

	if n, err := decimal("9223372036854775807"); err != nil || n != math.MaxInt64 {
		t.Fatal("max decimal")
	}

	if s, err := contentRangeValue(0, math.MaxInt64-1, math.MaxInt64); err != nil || s != "bytes 0-9223372036854775806/9223372036854775807" {
		t.Fatal("max range")
	}

	if _, err := contentRangeValue(1, 0, 2); err == nil {
		t.Fatal("reversed content range")
	}
}

func TestOriginResponseSelection(t *testing.T) {
	tag, err := ParseETag(`"v"`)
	if err != nil {
		t.Fatal(err)
	}

	m := Metadata{Size: PageSize + 1, ETag: tag, ExpiresAt: time.UnixMilli(0)}

	page, err := ClosedRange(ByteOffset(PageSize), ByteOffset(2*PageSize-1))
	if err != nil {
		t.Fatal(err)
	}

	r := OriginRequest{operation: OperationPinned, pin: tag, byteRange: page}

	got, err := originResponse(r, m)
	if err != nil || got.length != 1 || got.first != ByteOffset(PageSize) || got.last != got.first {
		t.Fatal("short final page", err)
	}

	wrong := m
	wrong.ETag = ETag{value: `"other"`}
	_, err = originResponse(r, wrong)
	assertKind(t, err, ErrorBadGateway)
	_, err = originResponse(r, Metadata{})
	assertKind(t, err, ErrorBadGateway)

	m.Size = PageSize
	_, err = originResponse(r, m)
	assertKind(t, err, ErrorUnsatisfiableRange)

	partial, err := ClosedRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	r.byteRange = partial
	_, err = originResponse(r, m)
	assertKind(t, err, ErrorInvalidArgument)

	bootstrap := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}

	got, err = originResponse(bootstrap, m)
	if err != nil || got.length != int64(PageSize) {
		t.Fatal("bootstrap", err)
	}

	m.Size = 0

	got, err = originResponse(bootstrap, m)
	if err != nil || got.length != 0 {
		t.Fatal("empty bootstrap", err)
	}

	got, err = originResponse(OriginRequest{operation: OperationHead}, m)
	if err != nil || got.length != 0 {
		t.Fatal("HEAD", err)
	}

	_, err = originResponse(OriginRequest{}, m)
	assertKind(t, err, ErrorInvalidArgument)
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }

func TestFrameReaderBoundaries(t *testing.T) {
	body := "body\r\n\r\nnot a head"
	firstHead := rawResponse(200, "Content-Length: "+strconv.Itoa(len(body))+"\r\n")
	nextHead := rawResponse(503, "Content-Length: 0\r\n")

	source := bufio.NewReader(strings.NewReader(string(firstHead) + body + string(nextHead)))
	if _, err := readRawHead(source, true); err != nil {
		t.Fatal(err)
	}

	frame := &frameReader{source: source, remaining: int64(len(body))}

	got, err := io.ReadAll(frame)
	if err != nil || string(got) != body {
		t.Fatal("body corrupted")
	}

	head, err := readRawHead(source, true)
	if err != nil || !bytes.Equal(head, nextHead) {
		t.Fatal("read-ahead lost or body scanned")
	}

	frame = &frameReader{source: strings.NewReader("abc"), remaining: 4}

	got, err = io.ReadAll(frame)
	if string(got) != "abc" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncation lost")
	}

	cause := errors.New("late private failure")
	frame = &frameReader{source: finalErrorReader{err: cause}, remaining: 3}

	n, err := frame.Read(make([]byte, 8))
	if n != 3 || !errors.Is(err, cause) {
		t.Fatal("final error lost")
	}

	frame = &frameReader{source: finalErrorReader{err: io.EOF}, remaining: 3}

	got, err = io.ReadAll(frame)
	if err != nil || string(got) != "abc" {
		t.Fatal("exact EOF")
	}

	frame = &frameReader{remaining: 0}
	if n, err := frame.Read(nil); n != 0 || err != nil {
		t.Fatal("empty read")
	}

	if n, err := frame.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal("zero frame")
	}
}

func FuzzWireHead(f *testing.F) {
	for _, head := range [][]byte{rawRequest("HEAD", ""), rawRequest("GET", "Range: bytes=0-16777215\r\nAuthorization: opaque\xff\r\n"), rawRequest("HEAD", "Content-Length: 0\r\nContent-Length: 0\r\n"), rawResponse(200, "Content-Length: 0\r\nETag: \"\"\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\n"), rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */9223372036854775807\r\n")} {
		f.Add(head)
	}

	f.Fuzz(func(t *testing.T, head []byte) {
		if len(head) > maxHeadBytes+1 {
			return
		}

		if request, err := parseRequestHead(head, false); err == nil {
			canonical, err := requestHead(request)
			if err != nil {
				t.Fatal(err)
			}

			again, err := parseRequestHead(canonical, false)
			if err != nil || again != request {
				t.Fatal("wire round trip")
			}
		}

		_, _ = parseResponseHead(head, OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}, nil)
	})
}
