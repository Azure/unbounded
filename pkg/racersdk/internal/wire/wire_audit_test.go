// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"fmt"
	"strings"
	"testing"
)

func TestOriginMethodNotAllowed(t *testing.T) {
	for _, tc := range []struct {
		fields string
		want   ErrorKind
	}{
		{"Allow: HEAD, GET\r\n", ErrorInvalidArgument},
		{"Allow: HEAD, POST\r\n", ErrorProtocol},
		{"", ErrorProtocol},
	} {
		_, err := ParseResponseHead(rawResponse(405, "Content-Length: 0\r\n"+tc.fields), Request{Operation: OperationHead}, nil)
		assertKind(t, err, tc.want)
	}
}

func TestClientHeadResponseRejectsOtherOperations(t *testing.T) {
	_, err := ParseClientHeadResponse(rawResponse(405, "Content-Length: 0\r\nAllow: HEAD, POST\r\n"), Request{Operation: OperationBootstrap})
	assertKind(t, err, ErrorProtocol)
}

func TestRawCanonicalHeaders(t *testing.T) {
	headRequest := Request{Operation: OperationHead}
	bootstrap := Request{Operation: OperationBootstrap, Range: BootstrapRange()}
	metadata := "ETag: \"v\"\r\nRacer-Expires-At: 0\r\n"
	parseHead := func(head []byte) error {
		_, err := ParseClientHeadResponse(head, headRequest)
		return err
	}
	parseOrigin := func(head []byte) error {
		_, err := ParseResponseHead(head, bootstrap, nil)
		return err
	}
	parseSubscription := func(head []byte) error {
		_, err := ParseSubscriptionResponse(head, SubscriptionOptions{})
		return err
	}

	parseRequest := func(head []byte) error {
		_, err := ParseRequestHead(head, true)
		return err
	}
	for _, tc := range []struct {
		name           string
		head           []byte
		fields         []string
		parse          func([]byte) error
		valid, invalid ErrorKind
	}{
		{"head", rawResponse(200, metadata+"Content-Length: 1\r\n"), []string{"Content-Length", "Racer-Expires-At"}, parseHead, 0, ErrorProtocol},
		{"origin", rawResponse(206, metadata+"Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\n"), []string{"Content-Length", "Racer-Expires-At", "Content-Range"}, parseOrigin, 0, ErrorProtocol},
		{"subscription", rawResponse(200, metadata+fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\nContent-Type: application/octet-stream\r\nRacer-Object-Length: 1\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 1\r\n", 1+2*FrameSize)), []string{"Content-Length", "Racer-Expires-At", "Racer-Object-Length", "Racer-Range-Start", "Racer-Range-End"}, parseSubscription, 0, ErrorProtocol},
		{"request", rawRequest("GET", "Content-Length: 0\r\nRange: bytes=0-16777215\r\n"), []string{"Content-Length", "Range"}, parseRequest, 0, ErrorInvalidArgument},
		{"head error", rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */1\r\n"), []string{"Content-Length", "Content-Range"}, parseHead, ErrorUnsatisfiableRange, ErrorProtocol},
		{"origin error", rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */1\r\n"), []string{"Content-Length", "Content-Range"}, parseOrigin, ErrorUnsatisfiableRange, ErrorProtocol},
		{"subscription error", rawResponse(416, "Content-Length: 0\r\nContent-Range: bytes */1\r\n"), []string{"Content-Length", "Content-Range"}, parseSubscription, ErrorUnsatisfiableRange, ErrorProtocol},
	} {
		for _, field := range tc.fields {
			for _, padding := range []struct {
				name, prefix, suffix string
				valid                bool
			}{
				{"separator", " ", "", true},
				{"no separator", "", "", true},
				{"extra space", "  ", "", false},
				{"tab separator", "\t", "", false},
				{"leading tab", " \t", "", false},
				{"trailing space", " ", " ", false},
				{"trailing tab", " ", "\t", false},
			} {
				t.Run(tc.name+"/"+field+"/"+padding.name, func(t *testing.T) {
					raw := string(tc.head)
					start := strings.Index(raw, field+": ") + len(field) + 2
					end := start + strings.Index(raw[start:], "\r\n")
					raw = raw[:start-1] + padding.prefix + raw[start:end] + padding.suffix + raw[end:]
					err := tc.parse([]byte(raw))

					want := tc.invalid
					if padding.valid {
						want = tc.valid
					}

					if want == 0 {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						assertKind(t, err, want)
					}
				})
			}
		}
	}
}

func TestHeadHeadersPreservesOtherSemantics(t *testing.T) {
	head := rawResponse(200, "Content-Length: 0\r\nRacer-Expires-At: 0\r\nETag:\t \"v\" \t\r\nContent-Type:\t application/octet-stream \t\r\nConnection: \tkeep-alive, CLOSE\t \r\nX-Unknown: \tvalue\t \r\nRacer-Content-Type: text/plain; charset=utf-8\r\nRacer-Metadata: opaque value\r\nAuthorization: Bearer token\r\n")
	if err := ValidateRawHead(head, true); err != nil {
		t.Fatal(err)
	}

	h := HeadHeaders(head)
	for name, want := range map[string]string{
		"ETag": `"v"`, "Content-Type": "application/octet-stream", "X-Unknown": "value",
		"Racer-Content-Type": "text/plain; charset=utf-8", "Racer-Metadata": "opaque value", "Authorization": "Bearer token",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q; want %q", name, got, want)
		}
	}

	response, err := ParseClientHeadResponse(head, Request{Operation: OperationHead})
	if err != nil || !response.Close {
		t.Fatalf("response = %+v, %v", response, err)
	}
}

func TestHeadHeadersPreservesCreditPadding(t *testing.T) {
	for _, field := range []string{"Racer-Page-Credits", "Racer-Byte-Credits", "Racer-Ordered"} {
		for _, value := range []string{" 1", "\t1", "1 ", "1\t"} {
			head := rawRequest("HEAD", strings.ToLower(field)+": "+value+"\r\n")
			if err := ValidateRawHead(head, false); err != nil {
				t.Fatal(err)
			}

			if got := HeadHeaders(head).Get(field); got != value {
				t.Errorf("%s = %q; want raw value %q", field, got, value)
			}
		}
	}
}
