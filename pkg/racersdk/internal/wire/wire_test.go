// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRequestRedaction(t *testing.T) {
	for _, request := range []Request{
		{},
		{Key: [32]byte{1}, Operation: OperationHead, AdapterMetadata: "private-metadata", Authorization: "Bearer private-credential"},
	} {
		for _, value := range []any{request, &request} {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				if got := fmt.Sprintf(verb, value); got != "Request([redacted])" {
					t.Fatalf("%s did not redact request: %s", verb, got)
				}
			}

			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{"AdapterMetadata", "Authorization"} {
				if _, ok := fields[name]; ok {
					t.Fatalf("JSON contains %s", name)
				}
			}

			if strings.Contains(string(data), "private-") {
				t.Fatal("JSON contains private request data")
			}
		}
	}
}

func assertKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()

	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != kind {
		t.Fatalf("error = %v; want kind %v", err, kind)
	}
}

func keyString(k [32]byte) string { return hex.EncodeToString(k[:]) }

func rawRequest(method, fields string) []byte {
	return []byte(method + " " + ObjectPrefix + keyString([32]byte{}) + " HTTP/1.1\r\nHost: racer\r\n" + fields + "\r\n")
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
		{"GET", "Range: bytes=16777216-50331647\r\nIf-Match: \"v\"\r\n", OperationPinned, false},
	} {
		head := rawRequest(tt.method, tt.fields+"Racer-Metadata: opaque,\xff value\r\nAuthorization: Bearer secret\r\n")

		r, err := ParseRequestHead(head, tt.origin)
		if err != nil || r.Operation != tt.op {
			t.Fatalf("%s %s: %v", tt.method, tt.fields, err)
		}

		if r.AdapterMetadata != "opaque,\xff value" || r.Authorization != "Bearer secret" {
			t.Fatal("lost context bytes")
		}

		canonical, err := RequestHead(r)
		if err != nil {
			t.Fatal(err)
		}

		roundTrip, err := ParseRequestHead(canonical, tt.origin)
		if err != nil || roundTrip != r {
			t.Fatal("request round trip")
		}
	}

	for _, fields := range []string{
		"", "Range: bytes=0-1\r\n", "Range: bytes=1-16777215\r\nIf-Match: \"v\"\r\n", "Range: bytes=0-16777216\r\nIf-Match: \"v\"\r\n", "Range: bytes=-1\r\nIf-Match: \"v\"\r\n", "Range: bytes=0-\r\nIf-Match: \"v\"\r\n",
		"Range: bytes=0-16777215\r\nContent-Length: 1\r\n", "Range: bytes=0-16777215\r\nIf-Match: W/\"v\"\r\n",
	} {
		_, err := ParseRequestHead(rawRequest("GET", fields), true)
		assertKind(t, err, ErrorInvalidArgument)
	}

	for _, field := range []string{"Transfer-Encoding: identity", "Content-Encoding: identity", "Expect: 100-continue", "Trailer: X", "If-None-Match: *", "If-Modified-Since: date", "If-Unmodified-Since: date", "If-Range: \"v\"", "Connection: Upgrade", "Content-Length: 00", "Content-Range: bytes */0", "If-Match:", "Authorization:", "Host: racer"} {
		_, err := ParseRequestHead(rawRequest("HEAD", field+"\r\n"), true)
		if err == nil {
			t.Fatalf("accepted %s", field)
		}
	}

	_, err := ParseRequestHead(rawRequest("HEAD", "Range: bytes=0-0\r\n"), true)
	assertKind(t, err, ErrorInvalidArgument)
	_, err = ParseRequestHead(rawRequest("POST", ""), true)

	var typed *Error
	if !errors.As(err, &typed) || typed.Status != 405 {
		t.Fatal("method status")
	}

	valid := string(rawRequest("HEAD", ""))
	for _, target := range []string{ObjectPrefix + keyString([32]byte{}) + "?", ObjectPrefix + keyString([32]byte{}) + "#x", "http://racer" + ObjectPrefix + keyString([32]byte{}), ObjectPrefix + strings.Repeat("A", 64), ObjectPrefix + "%30" + strings.Repeat("0", 63), "/v1//objects/" + keyString([32]byte{})} {
		head := strings.Replace(valid, ObjectPrefix+keyString([32]byte{}), target, 1)

		_, err := ParseRequestHead([]byte(head), true)
		if err == nil {
			t.Fatalf("accepted target %s", target)
		}
	}

	for _, head := range []string{strings.Replace(valid, "HTTP/1.1", "HTTP/1.0", 1), strings.Replace(valid, "Host: racer\r\n", "", 1), strings.Replace(valid, "Host: racer", "Host: other", 1)} {
		_, err := ParseRequestHead([]byte(head), true)
		if err == nil {
			t.Fatal("accepted bad envelope")
		}
	}

	_, err = RequestHead(Request{})
	assertKind(t, err, ErrorInvalidArgument)
}

func TestOutgoingHeaderBudget(t *testing.T) {
	// The largest possible outgoing descriptor must fit without aggregate-size
	// validation in Get. Exercise net/http serialization, not only requestHead.
	r := Request{
		Operation:       OperationPinned,
		Range:           Range{Present: true, First: math.MaxInt64 - 1, Last: math.MaxInt64},
		Pin:             `"` + strings.Repeat("v", MaxFieldBytes-2) + `"`,
		AdapterMetadata: strings.Repeat("m", MaxFieldBytes),
		Authorization:   strings.Repeat("a", MaxFieldBytes),
	}
	if err := ValidateRequest(r); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://racer"+ObjectPrefix+keyString(r.Key), nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header = RequestHeaders(r)
	req.Header["User-Agent"] = nil

	var wire bytes.Buffer
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}

	if wire.Len() > MaxHeadBytes {
		t.Fatalf("outgoing head exceeds limit: %d", wire.Len())
	}

	got, err := ParseRequestHead(wire.Bytes(), false)
	if err != nil || got != r {
		t.Fatalf("outgoing request failed round trip: %v", err)
	}
}

func TestResponseWire(t *testing.T) {
	bootstrap := Request{Operation: OperationBootstrap, Range: BootstrapRange()}

	tag := `"a,b"`

	err := ValidateETag(tag)
	if err != nil {
		t.Fatal(err)
	}

	pinnedRange, err := ClosedRange(PageSize, PageSize)
	if err != nil {
		t.Fatal(err)
	}

	pinned := Request{Operation: OperationPinned, Pin: tag, Range: pinnedRange}

	meta := "ETag: \"a,b\"\r\nRacer-Expires-At: 0\r\n"
	for _, tt := range []struct {
		request Request
		status  int
		fields  string
		size    uint64
		length  int64
	}{
		{Request{Operation: OperationHead}, 200, "Content-Length: 9223372036854775807\r\n", math.MaxInt64, 0},
		{bootstrap, 200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\n", 0, 0},
		{bootstrap, 206, "Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\n", 1, 1},
		{bootstrap, 206, "Content-Length: 16777216\r\nContent-Range: bytes 0-16777215/16777217\r\nContent-Type: application/octet-stream\r\n", PageSize + 1, int64(PageSize)},
		{pinned, 206, "Content-Length: 1\r\nContent-Range: bytes 16777216-16777216/16777217\r\nContent-Type: application/octet-stream\r\n", PageSize + 1, 1},
	} {
		got, err := ParseResponseHead(rawResponse(tt.status, meta+tt.fields), tt.request, nil)
		if err != nil || got.Metadata.Size != tt.size || got.Length != tt.length || got.Metadata.ETag != tag {
			t.Fatalf("success response: %+v %v", got, err)
		}

		snapshot := got.Metadata
		if _, err := ParseResponseHead(rawResponse(tt.status, strings.Replace(meta, "At: 0", "At: 1", 1)+tt.fields), tt.request, &snapshot); err != nil {
			t.Fatal("expiry refresh rejected")
		}

		snapshot.Size++
		_, err = ParseResponseHead(rawResponse(tt.status, meta+tt.fields), tt.request, &snapshot)
		assertKind(t, err, ErrorProtocol)
	}

	valid := string(rawResponse(206, meta+"Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\n"))
	for _, pair := range [][2]string{{"206 Partial Content", "200 OK"}, {"206 Partial Content", "302 Found"}, {"206 Partial Content", "100 Continue"}, {"HTTP/1.1", "HTTP/1.0"}, {"Content-Length: 1\r\n", ""}, {"Content-Length: 1", "Content-Length: 01"}, {"Content-Length: 1", "Content-Length: 2"}, {"Content-Length: 1", "Content-Length: 1, 1"}, {"Content-Length: 1", "Content-Length: 1\r\nContent-Length: 1"}, {"bytes 0-0/1", "bytes 1-1/2"}, {"bytes 0-0/1", "bytes */1"}, {"bytes 0-0/1", "bytes 0-0/*"}, {"ETag: \"a,b\"", "ETag: W/\"a,b\""}, {"Racer-Expires-At: 0", "Racer-Expires-At: 9223372036854775808"}, {"application/octet-stream", "text/plain"}, {"Content-Length: 1", "Content-Length: 1\r\nTransfer-Encoding: chunked"}, {"Content-Length: 1", "Content-Length: 1\r\nContent-Encoding: identity"}} {
		_, err := ParseResponseHead([]byte(strings.Replace(valid, pair[0], pair[1], 1)), bootstrap, nil)
		assertKind(t, err, ErrorProtocol)
	}

	wrongPin := bootstrap
	wrongPin.Operation, wrongPin.Pin = OperationPinned, `"other"`
	_, err = ParseResponseHead([]byte(valid), wrongPin, nil)
	assertKind(t, err, ErrorProtocol)
	_, err = ParseResponseHead(rawResponse(200, meta+"Content-Length: 0\r\nContent-Type: application/octet-stream\r\n"), pinned, nil)
	assertKind(t, err, ErrorProtocol)
}

func TestErrorResponses(t *testing.T) {
	r := Request{Operation: OperationBootstrap, Range: BootstrapRange()}

	for _, status := range []int{400, 401, 403, 404, 405, 412, 416, 431, 500, 502, 503} {
		fields := "Content-Length: 0\r\n"
		if status == 416 {
			fields += "Content-Range: bytes */0\r\n"
		}

		if status == 405 {
			fields += "Allow: HEAD, GET\r\n"
		}

		_, err := ParseResponseHead(rawResponse(status, fields), r, nil)

		var typed *Error
		if !errors.As(err, &typed) || typed.Status != status {
			t.Fatalf("status %d: %v", status, err)
		}

		for _, extra := range []string{"ETag: \"v\"\r\n", "Racer-Expires-At: 0\r\n"} {
			_, err = ParseResponseHead(rawResponse(status, fields+extra), r, nil)
			assertKind(t, err, ErrorProtocol)
		}
	}

	for _, fields := range []string{"Content-Length: 0\r\n", "Content-Length: 0\r\nContent-Range: bytes 0-0/1\r\n", "Content-Length: 1\r\nContent-Range: bytes */1\r\n"} {
		_, err := ParseResponseHead(rawResponse(416, fields), r, nil)
		assertKind(t, err, ErrorProtocol)
	}

	for _, s := range []string{"", "00", "+1", "-1", " 1", "1 ", "1,1", "9223372036854775808"} {
		if _, err := Decimal(s); err == nil {
			t.Fatal("noncanonical decimal accepted")
		}
	}

	if n, err := Decimal("9223372036854775807"); err != nil || n != math.MaxInt64 {
		t.Fatal("max decimal")
	}

	if s, err := ContentRangeValue(0, math.MaxInt64-1, math.MaxInt64); err != nil || s != "bytes 0-9223372036854775806/9223372036854775807" {
		t.Fatal("max range")
	}

	if _, err := ContentRangeValue(1, 0, 2); err == nil {
		t.Fatal("reversed content range")
	}
}

func TestOriginResponseSelection(t *testing.T) {
	tag := `"v"`

	err := ValidateETag(tag)
	if err != nil {
		t.Fatal(err)
	}

	m := Metadata{Size: PageSize + 1, ETag: tag, ExpiresAt: time.UnixMilli(0)}

	page, err := ClosedRange(PageSize, 2*PageSize-1)
	if err != nil {
		t.Fatal(err)
	}

	r := Request{Operation: OperationPinned, Pin: tag, Range: page}

	got, err := OriginResponse(r, m)
	if err != nil || got.Length != 1 || got.First != PageSize || got.Last != got.First {
		t.Fatal("short final page", err)
	}

	wrong := m
	wrong.ETag = `"other"`
	_, err = OriginResponse(r, wrong)
	assertKind(t, err, ErrorBadGateway)
	_, err = OriginResponse(r, Metadata{})
	assertKind(t, err, ErrorBadGateway)

	m.Size = PageSize
	_, err = OriginResponse(r, m)
	assertKind(t, err, ErrorUnsatisfiableRange)

	partial, err := ClosedRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	r.Range = partial
	_, err = OriginResponse(r, m)
	assertKind(t, err, ErrorInvalidArgument)

	bootstrap := Request{Operation: OperationBootstrap, Range: BootstrapRange()}

	got, err = OriginResponse(bootstrap, m)
	if err != nil || got.Length != int64(PageSize) {
		t.Fatal("bootstrap", err)
	}

	m.Size = 0

	got, err = OriginResponse(bootstrap, m)
	if err != nil || got.Length != 0 {
		t.Fatal("empty bootstrap", err)
	}

	got, err = OriginResponse(Request{Operation: OperationHead}, m)
	if err != nil || got.Length != 0 {
		t.Fatal("HEAD", err)
	}

	_, err = OriginResponse(Request{}, m)
	assertKind(t, err, ErrorInvalidArgument)
}

func TestClientHeadIsV2WithoutChangingOrigin(t *testing.T) {
	r := Request{Operation: OperationHead}

	head, err := ClientHead(r)
	if err != nil || !bytes.HasPrefix(head, []byte("HEAD /v2/objects/")) {
		t.Fatal(string(head), err)
	}

	head, err = RequestHead(r)
	if err != nil || !bytes.HasPrefix(head, []byte("HEAD /v1/objects/")) {
		t.Fatal(string(head), err)
	}
}

func TestSnapshotContentTypeMustMatchIncludingPresence(t *testing.T) {
	for _, initial := range []string{"", "text/plain", "application/octet-stream"} {
		for _, next := range []string{"", "text/plain", "application/octet-stream"} {
			fields := "Content-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"
			if next != "" {
				fields += "Racer-Content-Type: " + next + "\r\n"
			}

			snapshot := Metadata{ETag: `"v"`, ContentType: initial}

			_, err := ParseResponseHead(rawResponse(200, fields), Request{Operation: OperationHead}, &snapshot)
			if (err == nil) != (initial == next) {
				t.Fatalf("MIME %q -> %q: %v", initial, next, err)
			}
		}
	}
}

func FuzzWireHead(f *testing.F) {
	for _, head := range [][]byte{rawRequest("HEAD", ""), rawRequest("GET", "Range: bytes=0-16777215\r\nAuthorization: opaque\xff\r\n"), rawRequest("HEAD", "Content-Length: 0\r\nContent-Length: 0\r\n"), rawResponse(200, "Content-Length: 0\r\nETag: \"\"\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\n"), rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */9223372036854775807\r\n")} {
		f.Add(head)
	}

	f.Fuzz(func(t *testing.T, head []byte) {
		if len(head) > MaxHeadBytes+1 {
			return
		}

		if request, err := ParseRequestHead(head, false); err == nil {
			canonical, err := RequestHead(request)
			if err != nil {
				t.Fatal(err)
			}

			again, err := ParseRequestHead(canonical, false)
			if err != nil || again != request {
				t.Fatal("wire round trip")
			}
		}

		_, _ = ParseResponseHead(head, Request{Operation: OperationBootstrap, Range: BootstrapRange()}, nil)
	})
}

func TestConstructedRequestValidation(t *testing.T) {
	for _, request := range []Request{
		{Operation: OperationHead, Range: BootstrapRange()},
		{Operation: OperationBootstrap, Range: BootstrapRange(), Pin: `"v"`},
		{Operation: OperationBootstrap},
		{Operation: OperationPinned, Range: BootstrapRange()},
		{Operation: OperationPinned, Pin: `"v"`},
		{Operation: OperationHead, Pin: "bad\r\nHeader: injected"},
		{Operation: OperationHead, Authorization: "bad\r\nHeader: injected"},
	} {
		if _, err := RequestHead(request); err == nil {
			t.Fatal("serialized invalid private request")
		}
	}
}

func TestRangeResolution(t *testing.T) {
	for _, tt := range []struct {
		wire        string
		size        uint64
		first, last uint64
		kind        ErrorKind
	}{
		{"bytes=0-0", 1, 0, 0, 0},
		{"bytes=0-99", 2, 0, 1, 0},
		{"bytes=1-1", 2, 1, 1, 0},
		{"bytes=0-0", 0, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=2-2", 2, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=0-9223372036854775807", math.MaxInt64, 0, math.MaxInt64 - 1, 0},
		{"bytes=9223372036854775807-9223372036854775807", math.MaxInt64, 0, 0, ErrorUnsatisfiableRange},
		{"bytes=0-0", math.MaxInt64 + 1, 0, 0, ErrorInvalidArgument},
	} {
		r, err := ParseRange(tt.wire)
		if err != nil {
			t.Fatal(err)
		}

		if RangeValue(r) != tt.wire {
			t.Fatal("range normalized")
		}

		first, last, err := r.Resolve(tt.size)
		if tt.kind != 0 {
			assertKind(t, err, tt.kind)
		} else if err != nil || first != tt.first || last != tt.last {
			t.Fatalf("%s/%d: %d-%d %v", tt.wire, tt.size, first, last, err)
		}
	}

	for _, s := range []string{"", "bytes=-", "bytes=1-", "bytes=-0", "bytes=-99", "bytes=1-0", "bytes=00-1", "bytes=0-01", "bytes=+1-", "bytes=0-1,2-3", "bytes=0- 1", "bytes=0-9223372036854775808", "Bytes=0-1"} {
		_, err := ParseRange(s)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := ClosedRange(0, math.MaxInt64+1)
	assertKind(t, err, ErrorInvalidArgument)
	_, err = ClosedRange(2, 1)
	assertKind(t, err, ErrorInvalidArgument)
}

func TestWholePages(t *testing.T) {
	_, _, err := (Range{}).ResolvePage(1)
	assertKind(t, err, ErrorInvalidArgument)
	_, _, err = BootstrapRange().ResolvePage(math.MaxInt64 + 1)
	assertKind(t, err, ErrorInvalidArgument)

	p := uint64(PageSize)
	for _, size := range []uint64{1, PageSize - 1, PageSize, PageSize + 1, math.MaxInt64} {
		start := (uint64(size) - 1) / uint64(PageSize) * uint64(PageSize)
		for _, end := range []uint64{uint64(size) - 1, NominalPageEnd(start)} {
			r, err := ClosedRange(start, end)
			if err != nil {
				t.Fatal(err)
			}

			first, last, err := r.ResolvePage(size)
			if err != nil || uint64(first) != start || last != size-1 {
				t.Fatalf("page %d: %d-%d %v", size, first, last, err)
			}
		}
	}

	for _, tt := range []struct {
		first, last uint64
		size        uint64
		kind        ErrorKind
	}{
		{1, p - 1, PageSize, ErrorInvalidArgument}, {0, p, PageSize + 1, ErrorInvalidArgument}, {0, p - 2, PageSize, ErrorInvalidArgument}, {p, 2*p - 1, PageSize, ErrorUnsatisfiableRange}, {0, p - 1, 0, ErrorUnsatisfiableRange},
	} {
		r, err := ClosedRange(tt.first, tt.last)
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = r.ResolvePage(tt.size)
		assertKind(t, err, tt.kind)
	}
}

func FuzzRange(f *testing.F) {
	for _, s := range []string{"bytes=0-0", "bytes=-0", "bytes=16777216-", "bytes=0-9223372036854775807", "bytes=9223372036854775807-9223372036854775807"} {
		f.Add(s, uint64(math.MaxInt64))
	}

	f.Fuzz(func(t *testing.T, s string, n uint64) {
		r, err := ParseRange(s)
		if err != nil {
			return
		}

		if RangeValue(r) != s {
			t.Fatal("range round trip")
		}

		first, last, err := r.Resolve(n)
		if err == nil && (first > last || uint64(last) >= n || uint64(last-first)+1 > math.MaxInt64) {
			t.Fatal("invalid resolved bounds")
		}

		first, last, err = r.ResolvePage(n)
		if err == nil && (uint64(first)%uint64(PageSize) != 0 || first > last || uint64(last) >= n || uint64(last-first)+1 > uint64(PageSize)) {
			t.Fatal("invalid whole-page bounds")
		}
	})
}

func TestSubscriptionHeadRoundTrip(t *testing.T) {
	r := Request{Operation: OperationHead, Pin: `"v"`, AdapterMetadata: "opaque\xff", Authorization: "Bearer secret"}

	for _, length := range []uint64{0, 7} {
		for _, ordered := range []bool{false, true} {
			o := SubscriptionOptions{Offset: 3, Length: length, PageCredits: 2, ByteCredits: 2 * PageSize, Ordered: ordered}

			head, err := SubscriptionHead(r, o)
			if err != nil {
				t.Fatal(err)
			}

			req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
			if err != nil {
				t.Fatal(err)
			}

			parsed, err := ParseSubscriptionRequest(req)
			if err != nil || parsed.Request != r || parsed.First != 3 || !parsed.Ranged || parsed.Ordered != ordered || parsed.PageCredits != 2 || parsed.ByteCredits != 2*PageSize {
				t.Fatal("request round trip", parsed, err)
			}

			end := uint64(math.MaxInt64)
			if length != 0 {
				end = 3 + length
			}

			if parsed.End != end {
				t.Fatal("range end")
			}

			for _, field := range []string{"Racer-Page-Credits", "Racer-Byte-Credits", "Racer-Ordered"} {
				bad := req.Clone(t.Context())
				bad.Header.Set(field, "bad")

				if _, err := ParseSubscriptionRequest(bad); err == nil {
					t.Fatal("accepted malformed credit", field)
				}

				bad.Header.Add(field, "1")

				if _, err := ParseSubscriptionRequest(bad); err == nil {
					t.Fatal("accepted duplicate credit", field)
				}
			}
		}
	}

	head, err := ClientHead(r)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseClientHead(req)
	if err != nil || parsed != r {
		t.Fatal("client HEAD round trip", err)
	}
}

func TestSubscriptionResponseRoundTrip(t *testing.T) {
	for _, size := range []uint64{0, 7, PageSize + 1} {
		m := Metadata{Size: size, ETag: `"v"`, ExpiresAt: time.UnixMilli(1).UTC(), ContentType: "text/plain"}

		h, err := SubscriptionHeaders(m, 0, size)
		if err != nil {
			t.Fatal(err)
		}

		var b bytes.Buffer
		if err := WriteSubscriptionHead(&b, 200, h); err != nil {
			t.Fatal(err)
		}

		r, err := ParseSubscriptionResponse(b.Bytes(), SubscriptionOptions{})
		if err != nil || r.Metadata != m || r.First != 0 || r.End != size || r.Pages != PageCount(0, size) {
			t.Fatal("response round trip", r, err)
		}

		for _, pair := range [][2]string{{"Connection: close", "Connection: keep-alive"}, {"Racer-Range-End:", "Missing-Range-End:"}, {"ETag:", "Missing-ETag:"}, {"Etag:", "Missing-Etag:"}} {
			bad := strings.Replace(b.String(), pair[0], pair[1], 1)
			if bad != b.String() {
				if _, err := ParseSubscriptionResponse([]byte(bad), SubscriptionOptions{}); err == nil {
					t.Fatal("accepted malformed response", pair)
				}
			}
		}
	}

	if _, err := SubscriptionHeaders(Metadata{}, 0, math.MaxInt64); err == nil {
		t.Fatal("accepted overflowing frame length")
	}
}

func TestETag(t *testing.T) {
	for _, s := range []string{`""`, `"a,b"`, `"a\b"`, `"!#~"`, `"` + strings.Repeat("a", 8190) + `"`} {
		if err := ValidateETag(s); err != nil {
			t.Fatalf("valid tag: %v", err)
		}
	}

	for _, s := range []string{"", `*`, `W/"v"`, `"a", "b"`, `"a"b"`, `"a b"`, "\"\x80\"", "\"\t\"", `"` + strings.Repeat("a", 8191) + `"`} {
		assertKind(t, ValidateETag(s), ErrorInvalidArgument)
	}
}

func TestOpaque(t *testing.T) {
	for _, s := range []string{"", " leading", "trailing ", "a\tb", "a\rb", "a\nb", "a\x00b", "a\x7fb"} {
		assertKind(t, ValidateOpaque(s), ErrorInvalidArgument)
	}

	for _, s := range []string{strings.Repeat("x", 8192), "opaque, credential\xff"} {
		if err := ValidateOpaque(s); err != nil {
			t.Fatal(err)
		}
	}

	assertKind(t, ValidateOpaque(strings.Repeat("x", 8193)), ErrorHeaderLimit)
}

func TestMetadata(t *testing.T) {
	tag := `""`
	for _, expiry := range []time.Time{time.UnixMilli(0), time.UnixMilli(math.MaxInt64), time.UnixMilli(123).In(time.FixedZone("offset", 3600))} {
		m := Metadata{Size: math.MaxInt64, ETag: tag, ExpiresAt: expiry}
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}

		h, err := MetadataHeaders(m)
		if err != nil {
			t.Fatal(err)
		}

		parsed, err := Decimal(h.Get("Racer-Expires-At"))
		if err != nil || int64(parsed) != expiry.UnixMilli() {
			t.Fatal("expiry changed")
		}
	}

	for _, m := range []Metadata{{}, {ETag: tag}, {ETag: tag, ExpiresAt: time.UnixMilli(-1)}, {ETag: tag, ExpiresAt: time.Unix(0, 1)}, {ETag: tag, ExpiresAt: time.UnixMilli(math.MaxInt64).Add(time.Millisecond)}, {Size: math.MaxInt64 + 1, ETag: tag, ExpiresAt: time.UnixMilli(0)}} {
		assertKind(t, m.Validate(), ErrorInvalidArgument)
	}
}

func TestRawHeaders(t *testing.T) {
	for _, field := range []string{"Host", "Content-Length", "Content-Type", "Content-Range", "ETag", "If-Match", "Range", "Racer-Expires-At", "Racer-Metadata", "Authorization"} {
		for _, second := range []string{"same", "different"} {
			head := []byte("HEAD / HTTP/1.1\r\n" + field + ": same\r\n" + strings.ToLower(field) + ": " + second + "\r\n\r\n")
			assertKind(t, ValidateRawHead(head, false), ErrorInvalidArgument)
			assertKind(t, ValidateRawHead(head, true), ErrorProtocol)
		}
	}

	for _, value := range []string{"", "x", "  x", " x ", "\tx", " x\t", " a\r\nb", " a\x00b", " a\x7fb"} {
		for _, field := range []string{"Authorization", "Racer-Metadata"} {
			err := ValidateRawHead(rawRequest("HEAD", field+":"+value+"\r\n"), false)
			if err == nil {
				t.Fatalf("accepted malformed %s", field)
			}
		}
	}

	for _, line := range []string{" Folded: x", "X : x", "X\t: x", "X: a\nb", "X: a\rb", ": x"} {
		if err := ValidateRawHead(rawRequest("HEAD", line+"\r\n"), false); err == nil {
			t.Fatal("accepted malformed field")
		}
	}

	for _, count := range []int{8192, 8193} {
		err := ValidateRawHead(rawRequest("HEAD", "Authorization: "+strings.Repeat("a", count)+"\r\n"), false)
		if count == 8192 {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	base := rawRequest("HEAD", "X: \r\n")
	for _, size := range []int{MaxHeadBytes - 1, MaxHeadBytes, MaxHeadBytes + 1} {
		head := rawRequest("HEAD", "X: "+strings.Repeat("x", size-len(base))+"\r\n")

		got, err := ReadRawHead(bufio.NewReader(bytes.NewReader(head)), false)
		if size <= MaxHeadBytes {
			if err != nil || !bytes.Equal(got, head) {
				t.Fatal("head boundary")
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	_, err := ReadRawHead(bufio.NewReader(strings.NewReader("HTTP/1.1")), true)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncated head")
	}

	_, err = ReadRawHead(bufio.NewReader(strings.NewReader("")), false)
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

	if err := ValidateRawHead(head, false); err == nil {
		t.Fatal("guard trusted normalized headers")
	}
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }

func TestFrameReaderBoundaries(t *testing.T) {
	body := "body\r\n\r\nnot a head"
	firstHead := rawResponse(200, "Content-Length: "+strconv.Itoa(len(body))+"\r\n")
	nextHead := rawResponse(503, "Content-Length: 0\r\n")

	source := bufio.NewReader(strings.NewReader(string(firstHead) + body + string(nextHead)))
	if _, err := ReadRawHead(source, true); err != nil {
		t.Fatal(err)
	}

	frame := &FrameReader{Source: source, Remaining: int64(len(body))}

	got, err := io.ReadAll(frame)
	if err != nil || string(got) != body {
		t.Fatal("body corrupted")
	}

	head, err := ReadRawHead(source, true)
	if err != nil || !bytes.Equal(head, nextHead) {
		t.Fatal("read-ahead lost or body scanned")
	}

	frame = &FrameReader{Source: strings.NewReader("abc"), Remaining: 4}

	got, err = io.ReadAll(frame)
	if string(got) != "abc" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncation lost")
	}

	cause := errors.New("late private failure")
	frame = &FrameReader{Source: finalErrorReader{err: cause}, Remaining: 3}

	n, err := frame.Read(make([]byte, 8))
	if n != 3 || !errors.Is(err, cause) {
		t.Fatal("final error lost")
	}

	frame = &FrameReader{Source: finalErrorReader{err: io.EOF}, Remaining: 3}

	got, err = io.ReadAll(frame)
	if err != nil || string(got) != "abc" {
		t.Fatal("exact EOF")
	}

	frame = &FrameReader{Remaining: 0}
	if n, err := frame.Read(nil); n != 0 || err != nil {
		t.Fatal("empty read")
	}

	if n, err := frame.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal("zero frame")
	}
}

func TestPageStreamIntervalBound(t *testing.T) {
	tracking := &Sequence{}
	for n := uint64(0); n < 8192; n += 2 {
		if !tracking.record(n) {
			t.Fatal("interval rejected", n)
		}
	}

	if tracking.record(8192) || tracking.record(0) {
		t.Fatal("unbounded or duplicate interval accepted")
	}

	for n := uint64(1); n < 8192; n += 2 {
		if !tracking.record(n) {
			t.Fatal("merge rejected", n)
		}
	}

	if len(tracking.intervals) != 1 || tracking.intervals[0] != (pageInterval{0, 8192}) {
		t.Fatal("intervals failed to compact")
	}
}

func TestFrameAndCreditBytes(t *testing.T) {
	f := Frame{Kind: PageFrame, Number: 0x0102030405060708, Offset: 0x1112131415161718, Length: 0x21222324}
	want := []byte{1, 1, 2, 3, 4, 5, 6, 7, 8, 17, 18, 19, 20, 21, 22, 23, 24, 33, 34, 35, 36}

	b := f.Encode()
	if !bytes.Equal(b[:], want) || DecodeFrame(b) != f {
		t.Fatal("frame bytes changed")
	}

	var out bytes.Buffer
	if err := WriteFrame(&out, f); err != nil || !bytes.Equal(out.Bytes(), want) {
		t.Fatal("frame write", err)
	}

	c := Credit{Number: f.Number, Length: f.Length}

	release := c.Encode()
	if !bytes.Equal(release[:], append(want[1:9:9], want[17:]...)) || DecodeCredit(release) != c {
		t.Fatal("credit bytes changed")
	}
}

func TestSequence(t *testing.T) {
	first, end := uint64(PageSize-2), uint64(PageSize+3)
	a := Frame{Kind: PageFrame, Number: 0, Offset: first, Length: 2}
	b := Frame{Kind: PageFrame, Number: 1, Offset: PageSize, Length: 3}
	complete := Frame{Kind: CompleteFrame, Number: 2, Offset: 5}

	for _, ordered := range []bool{false, true} {
		s := NewSequence(first, end, ordered)

		frames := []Frame{a, b, complete}
		if !ordered {
			frames[0], frames[1] = b, a
		}

		for _, f := range frames {
			if err := s.Accept(f, "subscription frame"); err != nil {
				t.Fatal(err)
			}
		}

		if s.Accept(complete, "subscription frame") == nil {
			t.Fatal("accepted frame after completion")
		}
	}

	for _, f := range []Frame{b, complete, {Kind: 3}, {Kind: PageFrame, Offset: first, Length: 1}, {Kind: PageFrame, Offset: first + 1, Length: 2}, {Kind: PageFrame, Number: 1, Offset: first, Length: 2}} {
		if NewSequence(first, end, true).Accept(f, "subscription frame") == nil {
			t.Fatal("accepted malformed frame", f)
		}
	}

	s := NewSequence(first, end, false)
	if s.Accept(a, "subscription frame") != nil || s.Accept(a, "subscription frame") == nil {
		t.Fatal("duplicate page")
	}

	if err := NewSequence(0, 0, true).Accept(Frame{Kind: CompleteFrame}, "subscription complete"); err != nil {
		t.Fatal("empty completion", err)
	}

	for _, f := range []Frame{{Kind: CompleteFrame, Number: 1}, {Kind: CompleteFrame, Offset: 1}, {Kind: CompleteFrame, Length: 1}} {
		if NewSequence(0, 0, true).Accept(f, "subscription complete") == nil {
			t.Fatal("invalid empty completion")
		}
	}
}

func assertWireError(t *testing.T, err error, kind ErrorKind, operation string, status int) {
	t.Helper()
	assertKind(t, err, kind)

	var typed *Error
	if !errors.As(err, &typed) || typed.Operation != operation || typed.Status != status {
		t.Fatalf("error = %#v; want operation %q, status %d", err, operation, status)
	}
}

func TestErrorNumberingAndCauses(t *testing.T) {
	// Match the public SDK order, including its reserved wire classification.
	for i, kind := range []ErrorKind{
		ErrorInvalidArgument, ErrorClosed, ErrorProtocol, ErrorUnauthorized,
		ErrorForbidden, ErrorNotFound, ErrorVersionUnavailable, ErrorUnsatisfiableRange,
		ErrorHeaderLimit, ErrorInternal, ErrorBadGateway, ErrorUnavailable,
		ErrorCanceled, ErrorDeadline, ErrorIO,
	} {
		if kind != ErrorKind(i+1) {
			t.Fatalf("classification %d has value %d", i+1, kind)
		}
	}

	for _, tt := range []struct {
		cause error
		kind  ErrorKind
	}{
		{context.Canceled, ErrorCanceled},
		{context.DeadlineExceeded, ErrorDeadline},
		{errors.Join(context.Canceled, context.DeadlineExceeded), ErrorDeadline},
		{io.ErrUnexpectedEOF, ErrorIO},
	} {
		err := ioFailure("head read", fmt.Errorf("private cause: %w", tt.cause))
		assertWireError(t, err, tt.kind, "head read", 0)

		if !errors.Is(err, tt.cause) || err.Error() != "wire: head read" {
			t.Fatal("cause or safe diagnostic changed", err)
		}
	}
}

func TestContentTypeGrammar(t *testing.T) {
	for _, value := range []string{"", "text/plain", "application/a+b; charset=utf-8", `text/plain ; a = ""; b="escaped\"quote"`, `text/plain;a="a b"`} {
		if err := ValidateContentType(value); err != nil {
			t.Fatalf("rejected %q: %v", value, err)
		}
	}

	for _, value := range []string{
		strings.Repeat("x", 257), " text/plain", "text/plain ", "text/\x7f", "text/\xff", "text/\tplain",
		"text", "/plain", "text/", "text/plain extra", "text/plain;", "text/plain;=x",
		"text/plain;a", "text/plain;a=", "text/plain;a=x;A=y", `text/plain;a="`, `text/plain;a="x\`,
	} {
		t.Run(value, func(t *testing.T) {
			assertWireError(t, ValidateContentType(value), ErrorInvalidArgument, "content type", 0)
		})
	}
}

func TestSubscriptionResponseFailures(t *testing.T) {
	valid := "Content-Length: 49\r\nConnection: close\r\nContent-Type: application/octet-stream\r\n" +
		"Racer-Object-Length: 7\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 7\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\n"

	for _, tt := range []struct {
		name, old, replacement string
	}{
		{"length", "Content-Length: 49", "Content-Length: bad"},
		{"framing total", "Content-Length: 49", "Content-Length: 48"},
		{"object size", "Racer-Object-Length: 7", "Racer-Object-Length: bad"},
		{"first", "Racer-Range-Start: 0", "Racer-Range-Start: bad"},
		{"end", "Racer-Range-End: 7", "Racer-Range-End: bad"},
		{"expiry", "Racer-Expires-At: 1", "Racer-Expires-At: bad"},
		{"tag", `ETag: "v"`, "ETag: invalid"},
		{"reversed", "Racer-Range-Start: 0", "Racer-Range-Start: 8"},
		{"past size", "Racer-Range-End: 7", "Racer-Range-End: 8"},
		{"incomplete", "Racer-Range-End: 7", "Racer-Range-End: 6"},
		{"forbidden", "Connection: close", "Connection: close\r\nTransfer-Encoding: chunked"},
		{"content range", "Connection: close", "Connection: close\r\nContent-Range: bytes 0-6/7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSubscriptionResponse(rawResponse(200, strings.Replace(valid, tt.old, tt.replacement, 1)), SubscriptionOptions{})
			assertWireError(t, err, ErrorProtocol, "subscription response", 0)
		})
	}

	for _, tt := range []struct {
		name      string
		options   SubscriptionOptions
		kind      ErrorKind
		operation string
	}{
		{"offset", SubscriptionOptions{Offset: 1}, ErrorProtocol, "subscription response"},
		{"pin", SubscriptionOptions{Pin: `"other"`}, ErrorProtocol, "subscription response"},
		{"short request", SubscriptionOptions{Length: 6}, ErrorProtocol, "subscription response"},
		{"long request", SubscriptionOptions{Length: 8}, ErrorUnsatisfiableRange, "subscription range"},
		{"snapshot", SubscriptionOptions{Metadata: &Metadata{Size: 8, ETag: `"v"`}}, ErrorProtocol, "subscription response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSubscriptionResponse(rawResponse(200, valid), tt.options)
			assertWireError(t, err, tt.kind, tt.operation, 0)
		})
	}

	for _, line := range []string{"HTTP/1.0 200 OK", "HTTP/1.1 20", "HTTP/1.1 20x OK", "HTTP/1.1 200X"} {
		_, err := ParseSubscriptionResponse([]byte(line+"\r\n"+valid+"\r\n"), SubscriptionOptions{})
		assertWireError(t, err, ErrorProtocol, "subscription response", 0)
	}

	_, err := ParseSubscriptionResponse([]byte("truncated"), SubscriptionOptions{})
	assertWireError(t, err, ErrorProtocol, "wire head", 0)
}

func TestSubscriptionResponseSelection(t *testing.T) {
	for _, tt := range []struct {
		name             string
		size, first, end uint64
		options          SubscriptionOptions
		kind             ErrorKind
		operation        string
	}{
		{"selected range", 10, 2, 5, SubscriptionOptions{Offset: 2, Length: 3, Pin: `"v"`}, 0, ""},
		{"wrong length before end", 10, 2, 5, SubscriptionOptions{Offset: 2, Length: 4}, ErrorProtocol, "subscription response"},
		{"small object", PageSize + 1, 0, PageSize + 1, SubscriptionOptions{SmallObject: true}, ErrorInvalidArgument, "small object size"},
		{"snapshot expiry", 7, 0, 7, SubscriptionOptions{Metadata: &Metadata{Size: 7, ETag: `"v"`, ExpiresAt: time.UnixMilli(0).UTC()}}, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := Metadata{Size: tt.size, ETag: `"v"`, ExpiresAt: time.UnixMilli(1).UTC()}

			h, err := SubscriptionHeaders(m, tt.first, tt.end)
			if err != nil {
				t.Fatal(err)
			}

			var b bytes.Buffer
			if err := WriteSubscriptionHead(&b, 200, h); err != nil {
				t.Fatal(err)
			}

			got, err := ParseSubscriptionResponse(b.Bytes(), tt.options)
			if tt.kind != 0 {
				assertWireError(t, err, tt.kind, tt.operation, 0)
				return
			}

			if tt.options.Metadata != nil {
				m = *tt.options.Metadata
			}

			if err != nil || got.Metadata != m || got.First != tt.first || got.End != tt.end {
				t.Fatalf("selection = %+v, %v", got, err)
			}
		})
	}
}

func TestResponseErrorContracts(t *testing.T) {
	for _, tt := range []struct {
		status       int
		fields       string
		subscription bool
		kind         ErrorKind
		operation    string
		wantStatus   int
	}{
		{416, "Content-Range: bytes */7\r\n", true, ErrorUnsatisfiableRange, "response", 416},
		{416, "Content-Range: bytes */bad\r\n", true, ErrorProtocol, "subscription response", 0},
		{416, "", true, ErrorProtocol, "subscription response", 0},
		{503, "Content-Range: bytes */7\r\n", true, ErrorProtocol, "subscription response", 0},
		{503, "Content-Range: \r\n", true, ErrorUnavailable, "response", 503},
		{503, "Racer-Content-Type: text/plain\r\n", false, ErrorProtocol, "response", 0},
		{503, "Content-Range: \r\n", false, ErrorProtocol, "response", 0},
		{405, "", false, ErrorProtocol, "response", 0},
		{299, "", true, ErrorProtocol, "response", 299},
		{299, "", false, ErrorProtocol, "response", 0},
	} {
		t.Run(fmt.Sprintf("%d/%t/%s", tt.status, tt.subscription, tt.fields), func(t *testing.T) {
			head := rawResponse(tt.status, "Content-Length: 0\r\n"+tt.fields)

			var err error
			if tt.subscription {
				_, err = ParseSubscriptionResponse(head, SubscriptionOptions{})
			} else {
				_, err = ParseResponseHead(head, Request{Operation: OperationHead}, nil)
			}

			assertWireError(t, err, tt.kind, tt.operation, tt.wantStatus)
		})
	}

	for _, code := range []string{"+20", "-20"} {
		head := []byte("HTTP/1.1 " + code + " reason\r\nContent-Length: 0\r\n\r\n")

		status, err := strconv.Atoi(code)
		if err != nil {
			t.Fatal(err)
		}

		_, err = ParseSubscriptionResponse(head, SubscriptionOptions{})
		assertWireError(t, err, ErrorProtocol, "response", status)
		_, err = ParseResponseHead(head, Request{Operation: OperationHead}, nil)
		assertWireError(t, err, ErrorProtocol, "response", 0)
	}

	_, err := ParseSubscriptionResponse(rawResponse(503, "Content-Length: 1\r\n"), SubscriptionOptions{})
	assertWireError(t, err, ErrorProtocol, "subscription response", 0)
	_, err = ParseResponseHead(rawResponse(404, "Content-Length: 0\r\n"), Request{Operation: OperationHead, Pin: `"v"`}, nil)
	assertWireError(t, err, ErrorProtocol, "response", 0)
	_, err = ParseResponseHead(rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */7\r\n"), Request{Operation: OperationHead}, &Metadata{Size: 8})
	assertWireError(t, err, ErrorProtocol, "response", 0)
}

type failingWriter struct {
	writes, failAt int
	err            error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}

	return len(p), nil
}

func TestWriterFailures(t *testing.T) {
	cause := errors.New("write failure")
	for _, failAt := range []int{1, 2, 3} {
		w := &failingWriter{failAt: failAt, err: cause}
		if err := WriteSubscriptionHead(w, 200, http.Header{}); !errors.Is(err, cause) {
			t.Fatalf("write %d: %v", failAt, err)
		}
	}

	s := NewSequence(0, 0, true)

	w := &failingWriter{failAt: 1, err: cause}
	if err := s.Write(w, Frame{Kind: CompleteFrame}); !errors.Is(err, cause) {
		t.Fatal(err)
	}

	assertWireError(t, s.Write(w, Frame{Kind: CompleteFrame}), ErrorProtocol, "subscription frame", 0)

	if w.writes != 1 {
		t.Fatal("invalid frame reached writer")
	}

	var nilSequence *Sequence
	if nilSequence.Intervals() != 0 || s.Intervals() != 0 {
		t.Fatal("empty interval count")
	}
}
