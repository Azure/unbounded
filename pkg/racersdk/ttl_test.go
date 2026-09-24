// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientMetadataTTL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy, age []string
		want        *time.Duration
		invalid     bool
	}{
		{name: "unspecified"},
		{name: "public only", policy: []string{"public"}},
		{name: "age only", age: []string{"12"}},
		{name: "zero", policy: []string{"max-age=0"}, want: durationPointer(0)},
		{name: "positive", policy: []string{"max-age=60"}, want: durationPointer(time.Minute)},
		{name: "final directive", policy: []string{"public, max-age=60"}, want: durationPointer(time.Minute)},
		{name: "final quoted directive", policy: []string{`public, max-age="60"`}, want: durationPointer(time.Minute)},
		{name: "final quoted comma", policy: []string{`max-age=60, ext="a,"`}, want: durationPointer(time.Minute)},
		{name: "final escaped backslash", policy: []string{`max-age=60, ext="a\\"`}, want: durationPointer(time.Minute)},
		{name: "shared and quoted", policy: []string{"max-age=100, public", `S-MAXAGE="40", ext="a,b\"c"`}, age: []string{"3"}, want: durationPointer(37 * time.Second)},
		{name: "expired", policy: []string{"max-age=10"}, age: []string{"99"}, want: durationPointer(0)},
		{name: "no cache", policy: []string{`max-age=60, no-cache="etag, other"`}, want: durationPointer(0)},
		{name: "no store", policy: []string{"no-store"}, want: durationPointer(0)},
		{name: "private", policy: []string{"max-age=60, private"}, want: durationPointer(0)},
		{name: "duration boundary", policy: []string{"max-age=9223372036"}, want: durationPointer(9223372036 * time.Second)},
		{name: "duration overflow", policy: []string{"max-age=9223372037"}, invalid: true},
		{name: "integer overflow", policy: []string{"max-age=18446744073709551616"}, invalid: true},
		{name: "negative", policy: []string{"max-age=-1"}, invalid: true},
		{name: "plus", policy: []string{"max-age=+1"}, invalid: true},
		{name: "fraction", policy: []string{"max-age=1.5"}, invalid: true},
		{name: "missing", policy: []string{"max-age"}, invalid: true},
		{name: "empty", policy: []string{`max-age=""`}, invalid: true},
		{name: "empty header", policy: []string{""}, invalid: true},
		{name: "comma only", policy: []string{","}, invalid: true},
		{name: "trailing comma", policy: []string{"max-age=60,"}, invalid: true},
		{name: "final dangling escape", policy: []string{`max-age=60, ext="a\`}, invalid: true},
		{name: "duplicate", policy: []string{"max-age=1, MAX-AGE=1"}, invalid: true},
		{name: "duplicate headers", policy: []string{"s-maxage=1", "s-maxage=2"}, invalid: true},
		{name: "unterminated", policy: []string{`max-age="1`}, invalid: true},
		{name: "extra quotes", policy: []string{`max-age=""1""`}, invalid: true},
		{name: "bad extension", policy: []string{`max-age=60, ext="bad\"`}, invalid: true},
		{name: "invalid age", policy: []string{"max-age=1"}, age: []string{"-1"}, invalid: true},
		{name: "duplicate age", age: []string{"1", "1"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "0")
				w.Header().Set("ETag", checksumTag(nil))

				for _, v := range tc.policy {
					w.Header().Add("Cache-Control", v)
				}

				for _, v := range tc.age {
					w.Header().Add("Age", v)
				}
			}), ClientOptions{})

			m, err := c.Stat(context.Background(), "/object")
			if tc.invalid {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("expected ErrProtocol, got %v", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if (m.TTL == nil) != (tc.want == nil) || m.TTL != nil && *m.TTL != *tc.want {
				t.Fatalf("TTL=%v want=%v", m.TTL, tc.want)
			}
		})
	}
}

func TestOriginTTL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ttl    *time.Duration
		want   string
		status int
	}{
		{"unspecified", nil, "", 200},
		{"zero", durationPointer(0), "max-age=0", 200},
		{"positive", durationPointer(time.Minute), "max-age=60", 200},
		{"fraction", durationPointer(1500 * time.Millisecond), "max-age=1", 200},
		{"subsecond", durationPointer(time.Nanosecond), "max-age=0", 200},
		{"negative", durationPointer(-time.Nanosecond), "", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := NewOrigin(&memoryStore{meta: Metadata{ETag: checksumTag(nil), TTL: tc.ttl}})
			r := httptest.NewRequest("HEAD", "/metadata", nil)
			r.Header.Set("X-Racer-Target", "/object")

			w := httptest.NewRecorder()
			o.ServeHTTP(w, r)

			if w.Code != tc.status || w.Header().Get("Cache-Control") != tc.want {
				t.Fatalf("status=%d Cache-Control=%q", w.Code, w.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestObjectMetadataCopiesTTL(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Cache-Control", "max-age=60")
	}), ClientOptions{})

	o, err := c.Open(context.Background(), "/object")
	if err != nil {
		t.Fatal(err)
	}

	m := o.Metadata()

	*m.TTL = 0
	if *o.Metadata().TTL != time.Minute {
		t.Fatal("metadata exposed mutable snapshot TTL")
	}
}
