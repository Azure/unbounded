// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Distinct target bytes select distinct objects, so normalization cannot hide
// behind a fixture that returns the same representation for every request.
type conformanceStore struct{ sdk *memoryStore }

func (s conformanceStore) Stat(ctx context.Context, target string, _ []byte) (Metadata, error) {
	if target == "/sdk%2Fblob?b=2&a=1&a=3" {
		return s.sdk.Stat(ctx, target, nil)
	}

	if target == "/missing" {
		return Metadata{}, fs.ErrNotExist
	}

	tag := checksumTag(conformanceBody(target))

	return Metadata{Size: int64(len(conformanceBody(target))), ETag: tag, TTL: durationPointer(0)}, nil
}

func (s conformanceStore) Open(ctx context.Context, target, etag string, _ []byte) (Source, error) {
	if target == "/sdk%2Fblob?b=2&a=1&a=3" {
		return s.sdk.Open(ctx, target, etag, nil)
	}

	m, err := s.Stat(ctx, target, nil)
	if err != nil {
		return nil, err
	}

	if target == "/changed" {
		m.ETag = checksumTag([]byte("replacement"))
	}

	if m.ETag != etag {
		return nil, ErrVersionChanged
	}

	return conformanceSource{bytes.NewReader(conformanceBody(target))}, nil
}

type conformanceSource struct{ *bytes.Reader }

func (conformanceSource) Close() error { return nil }

func conformanceBody(target string) []byte {
	if target == "/empty" {
		return nil
	}

	return []byte("object:" + target)
}

func TestOriginConformance(t *testing.T) {
	o, _ := NewOrigin(conformanceStore{})

	s := unixTestServer(t, o)

	runReadConformance(t, s.Listener.Addr().String())
}

// The same assertions run against the origin and the real dataplane. Expected
// bytes/ranges are explicit and do not use the implementation's range parser.
func runReadConformance(t *testing.T, endpoint string) {
	sdk, err := NewClient(endpoint, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}

	client := sdk.http
	defer client.CloseIdleConnections()

	for _, target := range []string{"/object", "//a%2fb?b=2&a=1&a=3", "/a%2Fb", "/a/b", "/a/../b", "/object?", "/metadata", "/page", "/empty"} {
		t.Run(target, func(t *testing.T) {
			want := conformanceBody(target)
			for _, method := range []string{"HEAD", "GET"} {
				r, _ := http.NewRequest(method, "http://localhost"+target, nil)
				r.Header.Set("X-Racer-Target", "/wrong")

				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()

				if err != nil || resp.StatusCode != 200 || resp.ContentLength != int64(len(want)) || resp.Header.Get("ETag") != checksumTag(want) {
					t.Fatalf("%s: %v %+v", method, err, resp)
				}

				if method == "HEAD" {
					if len(body) != 0 {
						t.Fatal("HEAD body")
					}
				} else if !bytes.Equal(body, want) {
					t.Fatalf("target changed: %q != %q", body, want)
				}
			}

			c, _ := NewClient(endpoint, ClientOptions{})
			defer c.CloseIdleConnections()

			out := make(sliceWriter, len(want))
			if _, err := c.Download(context.Background(), target, out); err != nil || !bytes.Equal(out, want) {
				t.Fatalf("SDK download: %v %q", err, out)
			}

			object, err := c.Open(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}

			p := make([]byte, 4)

			n, err := object.ReadAt(context.Background(), p, 1)
			if len(want) == 0 {
				if n != 0 || !errors.Is(err, io.EOF) {
					t.Fatal(n, err)
				}
			} else if err != nil || n != 4 || !bytes.Equal(p, want[1:5]) {
				t.Fatal(n, err, p)
			}
		})
	}

	const target = "/object"

	full := string(conformanceBody(target)) // "object:/object", 14 bytes
	for _, tc := range []struct {
		name, method, target string
		headers              http.Header
		status               int
		body, cr             string
	}{
		{"unaligned", "GET", target, http.Header{"Range": {"bytes=1-4"}}, 206, full[1:5], "bytes 1-4/14"},
		{"open", "GET", target, http.Header{"Range": {"bytes=10-"}}, 206, full[10:], "bytes 10-13/14"},
		{"suffix", "GET", target, http.Header{"Range": {"bytes=-3"}}, 206, full[11:], "bytes 11-13/14"},
		{"clip", "GET", target, http.Header{"Range": {"bytes=10-999"}}, 206, full[10:], "bytes 10-13/14"},
		{"u64", "GET", target, http.Header{"Range": {"bytes=10-18446744073709551615"}}, 206, full[10:], "bytes 10-13/14"},
		{"unsatisfiable", "GET", target, http.Header{"Range": {"bytes=14-"}}, 416, "", "bytes */14"},
		{"zero-suffix", "GET", target, http.Header{"Range": {"bytes=-0"}}, 416, "", "bytes */14"},
		{"empty-range", "GET", "/empty", http.Header{"Range": {"bytes=0-"}}, 416, "", "bytes */0"},
		{"malformed", "GET", target, http.Header{"Range": {"invalid"}}, 200, full, ""},
		{"duplicate", "GET", target, http.Header{"Range": {"bytes=1-4", "bytes=7-8"}}, 200, full, ""},
		{"multipart", "GET", target, http.Header{"Range": {"bytes=1-4,7-8"}}, 200, full, ""},
		{"overflow", "GET", target, http.Header{"Range": {"bytes=18446744073709551616-"}}, 200, full, ""},
		{"head-range", "HEAD", target, http.Header{"Range": {"bytes=999-"}}, 200, "", ""},
		{"match", "GET", target, http.Header{"If-Match": {`"old",,`, `"v1",`}}, 200, full, ""},
		{"weak-match", "GET", target, http.Header{"If-Match": {`W/"v1"`}}, 412, "", ""},
		{"precedence", "GET", target, http.Header{"If-Match": {`"old"`}, "If-None-Match": {"*"}, "Range": {"bytes=999-"}}, 412, "", ""},
		{"validate-both", "GET", target, http.Header{"If-Match": {`"old"`}, "If-None-Match": {"broken"}}, 400, "", ""},
		{"none", "GET", target, http.Header{"If-None-Match": {`"old"`, `W/"v1"`}}, 304, "", ""},
		{"head-none", "HEAD", target, http.Header{"If-None-Match": {"*"}}, 304, "", ""},
		{"head-failed", "HEAD", target, http.Header{"If-Match": {`"old"`}}, 412, "", ""},
		{"wildcard", "HEAD", target, http.Header{"If-Match": {"*"}}, 200, "", ""},
		{"none-wildcard", "GET", target, http.Header{"If-None-Match": {"*"}}, 304, "", ""},
		{"bad-list", "GET", target, http.Header{"If-Match": {"*", `"v1"`}}, 400, "", ""},
		{"if-range", "GET", target, http.Header{"Range": {"bytes=1-4"}, "If-Range": {`"v1"`}}, 206, full[1:5], "bytes 1-4/14"},
		{"if-range-old", "GET", target, http.Header{"Range": {"bytes=1-4"}, "If-Range": {`"old"`}}, 200, full, ""},
		{"if-range-date", "GET", target, http.Header{"Range": {"bytes=1-4"}, "If-Range": {"Wed, 21 Oct 2015 07:28:00 GMT"}}, 200, full, ""},
		{"missing", "GET", "/missing", nil, 404, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest(tc.method, "http://localhost"+tc.target, nil)
			if tc.headers != nil {
				r.Header = tc.headers
				for name, values := range r.Header {
					for i, value := range values {
						r.Header[name][i] = strings.ReplaceAll(value, `"v1"`, checksumTag(conformanceBody(tc.target)))
					}
				}
			}

			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()

			if err != nil || resp.StatusCode != tc.status || string(body) != tc.body || resp.Header.Get("Content-Range") != tc.cr {
				t.Fatalf("status=%d body=%q range=%q err=%v", resp.StatusCode, body, resp.Header.Get("Content-Range"), err)
			}

			if tc.status != 304 {
				length := len(tc.body)
				if tc.method == "HEAD" && tc.status == 200 {
					length = len(conformanceBody(tc.target))
				}

				if resp.ContentLength != int64(length) {
					t.Fatalf("length=%d want=%d", resp.ContentLength, length)
				}
			}
		})
	}

	c, _ := NewClient(endpoint, ClientOptions{})
	defer c.CloseIdleConnections()

	if _, err := c.Download(context.Background(), "/changed", discardWriterAt{}); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("changed snapshot: %v", err)
	}
	// Absolute form must discard authority, preserving the raw path/query.
	conn, err := net.Dial("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	rawTarget := "//raw%2f?x=1&x=2"

	_, err = io.WriteString(conn, "GET http://unused.invalid"+rawTarget+" HTTP/1.1\r\nHost: unused.invalid\r\nConnection: close\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}

	wire, err := io.ReadAll(conn)
	if err != nil || !bytes.HasSuffix(wire, conformanceBody(rawTarget)) || !bytes.Contains(bytes.ToLower(wire), []byte("content-length: "+strconv.Itoa(len(conformanceBody(rawTarget))))) {
		t.Fatalf("absolute form: %q %v", wire, err)
	}
}
