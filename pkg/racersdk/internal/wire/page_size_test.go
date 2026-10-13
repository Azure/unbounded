// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"testing"
	"time"
)

func TestFourMiBSubscriptionGeometry(t *testing.T) {
	const size = 1 << 30

	m := Metadata{Size: size, ETag: `"v"`, ExpiresAt: time.UnixMilli(1).UTC()}

	headers, err := SubscriptionHeaders(m, 0, size)
	if err != nil {
		t.Fatal(err)
	}

	if got := headers.Get("Content-Length"); got != "1073747221" {
		t.Fatalf("1 GiB subscription length = %s; want payload plus 257 frames", got)
	}

	var head bytes.Buffer
	if err := WriteSubscriptionHead(&head, 200, headers); err != nil {
		t.Fatal(err)
	}

	response, err := ParseSubscriptionResponse(head.Bytes(), SubscriptionOptions{})
	if err != nil || response.Pages != 256 {
		t.Fatalf("1 GiB response = %+v, %v; want 256 pages", response, err)
	}

	sequence := NewSequence(0, size, true)
	for number := uint64(0); number < 256; number++ {
		if err := sequence.Accept(Frame{Kind: PageFrame, Number: number, Offset: number * 4194304, Length: 4194304}, "4 MiB page"); err != nil {
			t.Fatal(err)
		}
	}

	if err := sequence.Accept(Frame{Kind: CompleteFrame, Number: 256, Offset: size}, "complete"); err != nil {
		t.Fatal(err)
	}

	headers.Set("Content-Length", "1073743189")
	head.Reset()

	if err := WriteSubscriptionHead(&head, 200, headers); err != nil {
		t.Fatal(err)
	}

	_, err = ParseSubscriptionResponse(head.Bytes(), SubscriptionOptions{})
	assertKind(t, err, ErrorProtocol)
	assertKind(t, NewSequence(0, size, true).Accept(Frame{Kind: PageFrame, Length: 16 << 20}, "old page"), ErrorProtocol)
}

func TestFourMiBOriginRejectsOldPageRanges(t *testing.T) {
	for _, fields := range []string{
		"Range: bytes=0-16777215\r\n",
		"Range: bytes=16777216-33554431\r\nIf-Match: \"v\"\r\n",
	} {
		_, err := ParseRequestHead(rawRequest("GET", fields), true)
		assertKind(t, err, ErrorInvalidArgument)
	}
}
