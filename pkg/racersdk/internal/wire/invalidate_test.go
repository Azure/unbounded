// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"strings"
	"testing"
)

func TestInvalidateHead(t *testing.T) {
	var key [32]byte

	key[0] = 0xab

	head, err := InvalidateHead(key, `"v1"`)
	if err != nil {
		t.Fatal(err)
	}

	want := "DELETE /v1/objects/ab" + strings.Repeat("0", 62) + " HTTP/1.1\r\nHost: racer\r\nIf-Match: \"v1\"\r\nContent-Length: 0\r\n\r\n"
	if string(head) != want {
		t.Fatalf("head = %q; want %q", head, want)
	}

	for _, etag := range []string{"", `W/"v1"`, "v1", `"a b"`, "*"} {
		if _, err := InvalidateHead(key, etag); err == nil {
			t.Errorf("InvalidateHead(%q) succeeded", etag)
		} else {
			assertKind(t, err, ErrorInvalidArgument)
		}
	}
}

func TestParseInvalidateResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		want ErrorKind
	}{
		{"no content", "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n", 0},
		{"zero length", "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n", 0},
		{"bad request", "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", ErrorInvalidArgument},
		{"overloaded", "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n", ErrorUnavailable},
		{"header limit", "HTTP/1.1 431 Too Large\r\nContent-Length: 0\r\n\r\n", ErrorHeaderLimit},
		{"no content with body", "HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\n", ErrorProtocol},
		{"ok is not success", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", ErrorProtocol},
		{"error with body", "HTTP/1.1 400 Bad Request\r\nContent-Length: 2\r\n\r\n", ErrorProtocol},
		{"error without length", "HTTP/1.1 400 Bad Request\r\n\r\n", ErrorProtocol},
		{"metadata", "HTTP/1.1 204 No Content\r\nETag: \"v\"\r\n\r\n", ErrorProtocol},
		{"method not allowed", "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nAllow: HEAD, POST\r\n\r\n", ErrorProtocol},
		{"http 1.0", "HTTP/1.0 204 No Content\r\n\r\n", ErrorProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ParseInvalidateResponse([]byte(tc.head))
			if tc.want == 0 {
				if err != nil {
					t.Fatal(err)
				}

				return
			}

			assertKind(t, err, tc.want)
		})
	}
}
