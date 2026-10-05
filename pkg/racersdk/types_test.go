// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func assertKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()

	var typed *Error
	if !errors.As(err, &typed) || typed.Kind() != kind {
		t.Fatalf("error = %v; want kind %v", err, kind)
	}
}

func TestKey(t *testing.T) {
	for _, s := range []string{strings.Repeat("0", 64), strings.Repeat("0123456789abcdef", 4)} {
		key, err := ParseKey(s)
		if err != nil || key.String() != s {
			t.Fatalf("key round trip: %v", err)
		}
	}

	for _, s := range []string{"", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), " " + strings.Repeat("0", 63)} {
		_, err := ParseKey(s)
		assertKind(t, err, ErrorInvalidArgument)
	}

	if (Key{}).String() != strings.Repeat("0", 64) {
		t.Fatal("zero key")
	}
}

func TestCacheName(t *testing.T) {
	for _, s := range []string{"a", "a-b.c0", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		n, err := ParseCacheName(s)
		if err != nil || n.String() != s {
			t.Fatalf("valid name %q: %v", s, err)
		}
	}

	for _, s := range []string{"", "A", ".a", "a.", "a..b", "-a", "a-", "a/b", "a_b", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		_, err := ParseCacheName(s)
		assertKind(t, err, ErrorInvalidArgument)
	}
}

func TestOpaqueAndDiagnostics(t *testing.T) {
	secret := "never-print-this, credential\xff"

	m, err := ParseAdapterMetadata(secret)
	if err != nil {
		t.Fatal(err)
	}

	a, err := ParseAuthorization(secret)
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewFetchContext(m, a)
	if err != nil {
		t.Fatal(err)
	}

	if c.Metadata().ForOrigin() != secret || c.Authorization().ForOrigin() != secret {
		t.Fatal("context changed")
	}

	request := Request{Context: c}
	origin := OriginRequest{context: c}
	cause := errors.New(secret)

	err = NewOriginError(ErrorUnavailable, cause)

	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatal("missing typed error")
	}

	for _, value := range []any{m, &m, a, &a, c, &c, request, &request, origin, &origin, err, *typed} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(verb, value), "never-print-this") {
				t.Fatalf("unsafe %s diagnostic", verb)
			}
		}

		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil || strings.Contains(string(encoded), "never-print-this") {
			t.Fatal("unsafe serialization")
		}
	}

	if !errors.Is(err, cause) {
		t.Fatal("lost explicit cause")
	}
}

func TestZeroValues(t *testing.T) {
	if (CacheName{}).String() != "" || (ETag{}).String() != "" {
		t.Fatal("zero strings")
	}

	c, err := NewFetchContext(AdapterMetadata{}, Authorization{})
	if err != nil || c != (FetchContext{}) {
		t.Fatal("zero context")
	}

	r := OriginRequest{}
	if r.Operation() != 0 || r.Context() != c || r.Key() != (Key{}) {
		t.Fatal("zero origin")
	}

	if _, ok := r.Pin(); ok {
		t.Fatal("zero pin present")
	}

	if _, ok := r.Range(); ok {
		t.Fatal("zero range present")
	}

	_, _, err = (Range{}).Resolve(1)
	assertKind(t, err, ErrorInvalidArgument)

	var e *Error
	if e.Kind() != 0 || e.StatusCode() != 0 || e.Operation() != "" || e.Unwrap() != nil {
		t.Fatal("nil error")
	}

	if (&Error{}).Error() == "" || e.Error() == "" {
		t.Fatal("empty diagnostic")
	}
}

func TestErrors(t *testing.T) {
	for _, tt := range []struct {
		kind   ErrorKind
		status int
	}{{ErrorInvalidArgument, 400}, {ErrorUnauthorized, 401}, {ErrorForbidden, 403}, {ErrorNotFound, 404}, {ErrorVersionUnavailable, 412}, {ErrorUnsatisfiableRange, 416}, {ErrorHeaderLimit, 431}, {ErrorInternal, 500}, {ErrorBadGateway, 502}, {ErrorUnavailable, 503}, {ErrorCanceled, 503}, {ErrorDeadline, 503}, {ErrorIO, 500}, {0, 500}} {
		if got := callbackStatus(NewOriginError(tt.kind, nil), false); got != tt.status {
			t.Fatalf("%v: %d", tt.kind, got)
		}
	}

	if callbackStatus(NewOriginError(ErrorNotFound, nil), true) != 412 || callbackStatus(errors.New("private"), false) != 500 {
		t.Fatal("callback classification")
	}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		err := ioFailure("read", cause)
		if !errors.Is(err, cause) {
			t.Fatal("lost cause")
		}

		if cause != io.ErrUnexpectedEOF && callbackStatus(err, false) != 503 {
			t.Fatal("context status")
		}
	}

	if ioFailure("read", io.EOF) != io.EOF || ioFailure("read", nil) != nil {
		t.Fatal("EOF changed")
	}

	err := statusError(412)
	if err.StatusCode() != 412 || err.Operation() != "response" {
		t.Fatal("error details")
	}
}

func FuzzValidatedTypes(f *testing.F) {
	for _, s := range []string{"", `""`, `"a,b"`, `"a\b"`, "opaque\xff", " padded ", strings.Repeat("a", 64), "a.b", "9223372036854775808"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if key, err := ParseKey(s); err == nil && key.String() != s {
			t.Fatal("key normalized")
		}

		if tag, err := ParseETag(s); err == nil && tag.String() != s {
			t.Fatal("tag normalized")
		}

		if name, err := ParseCacheName(s); err == nil && name.String() != s {
			t.Fatal("name normalized")
		}

		if a, err := ParseAuthorization(s); err == nil && a.ForOrigin() != s {
			t.Fatal("authorization normalized")
		}

		if m, err := ParseAdapterMetadata(s); err == nil && m.ForOrigin() != s {
			t.Fatal("metadata normalized")
		}
	})
}
