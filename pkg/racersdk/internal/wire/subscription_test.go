// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
