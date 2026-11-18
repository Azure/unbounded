// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestNewRequestCanonicalIdentity(t *testing.T) {
	want := `{"schema":1,"bucket":"bucket","key":"a/../b%2Fc +雪","versionId":"v+/="}`

	request, err := NewRequest("bucket", "a/../b%2Fc +雪", "v+/=")
	if err != nil {
		t.Fatal(err)
	}

	if request.Key != racersdk.Key(sha256.Sum256([]byte(want))) || request.Metadata != want || request.Authorization != "" {
		t.Fatal("canonical identity changed")
	}

	for _, fields := range [][3]string{
		{"other", "a/../b%2Fc +雪", "v+/="},
		{"bucket", "b%2Fc +雪", "v+/="},
		{"bucket", "a/../b/c +雪", "v+/="},
		{"bucket", "a/../b%2Fc +雪", ""},
	} {
		other, err := NewRequest(fields[0], fields[1], fields[2])
		if err != nil || other.Key == request.Key {
			t.Fatalf("identity collision: %v", err)
		}
	}

	plain, err := NewRequest("bucket", "key", "")
	if err != nil || strings.Contains(plain.Metadata, "versionId") {
		t.Fatal("empty version must be omitted", err)
	}
}

func TestNewRequestValidation(t *testing.T) {
	for _, fields := range [][3]string{
		{"", "key", ""},
		{"with space", "key", ""},
		{"bucket\n", "key", ""},
		{"\xff", "key", ""},
		{"https://evil", "key", ""},
		{"../bucket", "key", ""},
		{"bucket", "", ""},
		{"bucket", strings.Repeat("k", 1025), ""},
		{"bucket", "\xff", ""},
		{"bucket", "key", "\xff"},
		{"bucket", "key", strings.Repeat("v", 1025)},
		{"bucket\u00a0", "key", ""},
		{"bucket\x00", "key", ""},
		{".", "key", ""},
		{"..", "key", ""},
		{strings.Repeat("b", 256), "key", ""},
		{"buckét", "key", ""},
	} {
		if _, err := NewRequest(fields[0], fields[1], fields[2]); err == nil {
			t.Fatalf("accepted invalid fields %q", fields)
		}
	}

	for _, key := range []string{"/", ".", "../", "a//b", "a\x00b", "line\nbreak", strings.Repeat("k", 1024)} {
		if _, err := NewRequest("bucket", key, "null"); err != nil {
			t.Fatalf("rejected exact key %q: %v", key, err)
		}
	}

	for _, fields := range [][3]string{
		{strings.Repeat("b", 255), strings.Repeat("k", 1024), strings.Repeat("v", 1024)},
		{"Legacy_Bucket-9.example", "key", "v\x00\n雪"},
	} {
		if _, err := NewRequest(fields[0], fields[1], fields[2]); err != nil {
			t.Fatalf("rejected valid boundary fields %q: %v", fields, err)
		}
	}

	// Both strings fit the object limits, but escaping exceeds the SDK field cap.
	_, err := NewRequest("bucket", strings.Repeat("\x00", 1024), strings.Repeat("\x00", 1024))
	originAssertError(t, err, racersdk.ErrInvalidRequest)
}

func originAssertError(t *testing.T, err, want error) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
