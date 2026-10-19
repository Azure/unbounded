// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWarmHeadRoundTrip(t *testing.T) {
	r := Request{Operation: OperationHead, Pin: `"v"`, RetainSeconds: 42}

	head, err := ClientHead(r)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := http.ReadRequest(bufio.NewReader(strings.NewReader(string(head))))
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Body.Close()

	got, err := ParseClientHead(parsed)
	if err != nil || got != r {
		t.Fatalf("round trip: %v, %v", got, err)
	}

	origin, err := RequestHead(r)
	if err != nil || strings.Contains(string(origin), "Cache-Control") {
		t.Fatalf("retention leaked to origin: %q, %v", origin, err)
	}

	parsed.Method = http.MethodPost
	parsed.Header.Set("Content-Length", "0")
	parsed.Header.Set("Cache-Control", "max-age=bad, max-age=-1")

	sub, err := ParseSubscriptionRequest(parsed)
	if err != nil || sub.Request.RetainSeconds != 0 {
		t.Fatalf("POST interpreted retention: %v", err)
	}
}

func TestWarmMaxAgeGrammar(t *testing.T) {
	for _, tt := range []struct {
		values []string
		want   uint64
		bad    bool
	}{
		{nil, 0, false},
		{[]string{"max-age=0"}, 0, false},
		{[]string{"private, MAX-AGE = \"0012\", immutable"}, 12, false},
		{[]string{`extension="a, max-age=99"`, "max-age=7"}, 7, false},
		{[]string{"max-age=18446744073709551615"}, ^uint64(0), false},
		{[]string{"max-age=1", "MAX-AGE=1"}, 0, true},
		{[]string{"max-age=0, max-age=0"}, 0, true},
		{[]string{"max-age=18446744073709551616"}, 0, true},
	} {
		got, err := parseMaxAge(tt.values)
		if (err != nil) != tt.bad || !tt.bad && got != tt.want {
			t.Fatalf("%q: %d, %v", tt.values, got, err)
		}
	}

	for _, value := range []string{"", "+1", "-1", "1.0", "1 2", `""`, `"1`, `"1\2"`, "1;"} {
		if _, err := parseMaxAge([]string{"max-age=" + value}); err == nil {
			t.Errorf("accepted %q", value)
		}
	}

	if _, err := parseMaxAge([]string{"max-age"}); err == nil {
		t.Fatal("accepted missing argument")
	}
}

func TestWarmResponseDeadline(t *testing.T) {
	base := "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"

	r := Request{Operation: OperationHead}
	for _, tt := range []struct {
		fields string
		want   time.Time
		bad    bool
	}{
		{"", time.Time{}, false},
		{"Racer-Retain-Until: 0\r\n", time.UnixMilli(0), false},
		{"Racer-Retain-Until: 1234\r\n", time.UnixMilli(1234), false},
		{"Racer-Retain-Until: 9223372036854775807\r\n", time.UnixMilli(9223372036854775807), false},
		{"Racer-Retain-Until: 1\r\nracer-retain-until: 1\r\n", time.Time{}, true},
	} {
		got, err := ParseClientHeadResponse([]byte(base+tt.fields+"\r\n"), r)
		if (err != nil) != tt.bad || !tt.bad && !got.RetainUntil.Equal(tt.want) {
			t.Errorf("%q: %v, %v", tt.fields, got, err)
		}
	}

	for _, value := range []string{"", "-1", "+1", "01", "1.2", " 1", "1 ", "\t1", "1, 2", "9223372036854775808"} {
		if _, err := ParseClientHeadResponse([]byte(base+"Racer-Retain-Until: "+value+"\r\n\r\n"), r); err == nil {
			t.Errorf("accepted timestamp %q", value)
		}
	}

	if _, err := ParseClientHeadResponse([]byte("HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\nRacer-Retain-Until: 1\r\n\r\n"), r); err == nil {
		t.Fatal("accepted retention on an error")
	} else if typed, ok := err.(*Error); !ok || typed.Kind != ErrorProtocol {
		t.Fatalf("wrong error classification: %v", err)
	}
}
