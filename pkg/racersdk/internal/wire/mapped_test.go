// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestSubscriptionHeadDelivery(t *testing.T) {
	r := Request{Operation: OperationHead}

	for _, mapped := range []bool{false, true} {
		head, err := SubscriptionHead(r, SubscriptionOptions{PageCredits: 2, ByteCredits: 2 * PageSize, Mapped: mapped})
		if err != nil {
			t.Fatal(err)
		}

		if got := strings.Contains(string(head), "\r\nRacer-Delivery: mapped\r\n"); got != mapped {
			t.Fatalf("mapped %v: delivery header present = %v in %q", mapped, got, head)
		}

		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
		if err != nil {
			t.Fatal(err)
		}

		// The fake peer ignores the request and answers inline.
		if _, err := ParseSubscriptionRequest(req); err != nil {
			t.Fatal(err)
		}

		req.Header.Add(DeliveryHeader, DeliveryMapped)

		if _, err := ParseSubscriptionRequest(req); err == nil && mapped {
			t.Fatal("accepted duplicate delivery header")
		}
	}
}

func TestSubscriptionResponseDelivery(t *testing.T) {
	const fields = "Connection: close\r\nContent-Type: application/octet-stream\r\n" +
		"Racer-Object-Length: 7\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 7\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\n"

	// One page: inline is 7 + 21*2 = 49, mapped is 29 + 21 = 50.
	for _, tt := range []struct {
		name, extra string
		mapped, ok  bool
		wantMapped  bool
	}{
		{"inline requested inline", "Content-Length: 49\r\n", false, true, false},
		{"mapped declined", "Content-Length: 49\r\n", true, true, false},
		{"mapped accepted", "Content-Length: 50\r\nRacer-Delivery: mapped\r\n", true, true, true},
		{"lowercase header name", "Content-Length: 50\r\nracer-delivery: mapped\r\n", true, true, true},
		{"mapped not requested", "Content-Length: 50\r\nRacer-Delivery: mapped\r\n", false, false, false},
		{"mapped with inline length", "Content-Length: 49\r\nRacer-Delivery: mapped\r\n", true, false, false},
		{"inline with mapped length", "Content-Length: 50\r\n", true, false, false},
		{"unknown delivery", "Content-Length: 50\r\nRacer-Delivery: inline\r\n", true, false, false},
		{"empty delivery", "Content-Length: 50\r\nRacer-Delivery: \r\n", true, false, false},
		{"duplicate delivery", "Content-Length: 50\r\nRacer-Delivery: mapped\r\nRacer-Delivery: mapped\r\n", true, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := ParseSubscriptionResponse(rawResponse(200, tt.extra+fields), SubscriptionOptions{Mapped: tt.mapped})
			if (err == nil) != tt.ok {
				t.Fatalf("error %v, want ok %v", err, tt.ok)
			}

			if err == nil && (r.Mapped != tt.wantMapped || r.Pages != 1) {
				t.Fatalf("response %+v", r)
			}
		})
	}

	empty := "Content-Length: 21\r\nRacer-Delivery: mapped\r\nConnection: close\r\nContent-Type: application/octet-stream\r\n" +
		"Racer-Object-Length: 0\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 0\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\n"
	if r, err := ParseSubscriptionResponse(rawResponse(200, empty), SubscriptionOptions{Mapped: true}); err != nil || !r.Mapped || r.Pages != 0 {
		t.Fatalf("empty mapped response %+v, %v", r, err)
	}
}

func TestMappedFrameBytes(t *testing.T) {
	f := Frame{Kind: MappedFrame, Number: 1, Offset: PageSize + 2, Length: 3, Memory: 0x0102030405060708}
	b := f.EncodeMapped()

	want := []byte{3, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 2, 0, 0, 0, 3, 1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(b[:], want) {
		t.Fatalf("encoded %v, want %v", b, want)
	}

	if got := DecodeMappedFrame(b); got != f {
		t.Fatalf("decoded %+v, want %+v", got, f)
	}
}

func TestMappedSequence(t *testing.T) {
	page := Frame{Number: 0, Offset: 0, Length: 7}

	inline := page
	inline.Kind = PageFrame

	mapped := page
	mapped.Kind = MappedFrame

	if NewMappedSequence(0, 7, true).Accept(inline, "test") == nil {
		t.Fatal("mapped sequence accepted an inline page")
	}

	if NewSequence(0, 7, true).Accept(mapped, "test") == nil {
		t.Fatal("inline sequence accepted a mapped page")
	}

	s := NewMappedSequence(0, 7, true)
	if err := s.Accept(mapped, "test"); err != nil {
		t.Fatal(err)
	}

	if err := s.Accept(Frame{Kind: CompleteFrame, Number: 1, Offset: 7}, "test"); err != nil {
		t.Fatal(err)
	}
}
