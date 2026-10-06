// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestNewRequestCanonicalIdentity(t *testing.T) {
	want := `{"schema":1,"namespace":"store","bucket":"bucket","key":"a/../b%2Fc +雪","versionId":"v+/="}`

	request, err := NewRequest("store", "bucket", "a/../b%2Fc +雪", "v+/=")
	if err != nil {
		t.Fatal(err)
	}

	if request.Key != racersdk.Key(sha256.Sum256([]byte(want))) || request.Context.Metadata().ForOrigin() != want || request.Context.Authorization().ForOrigin() != "" {
		t.Fatal("canonical identity changed")
	}

	for _, fields := range [][4]string{
		{"other", "bucket", "a/../b%2Fc +雪", "v+/="},
		{"store", "other", "a/../b%2Fc +雪", "v+/="},
		{"store", "bucket", "b%2Fc +雪", "v+/="},
		{"store", "bucket", "a/../b/c +雪", "v+/="},
		{"store", "bucket", "a/../b%2Fc +雪", ""},
	} {
		other, err := NewRequest(fields[0], fields[1], fields[2], fields[3])
		if err != nil || other.Key == request.Key {
			t.Fatalf("identity collision: %v", err)
		}
	}

	plain, err := NewRequest("store", "bucket", "key", "")
	if err != nil || strings.Contains(plain.Context.Metadata().ForOrigin(), "versionId") {
		t.Fatal("empty version must be omitted", err)
	}
}

func TestNewRequestValidation(t *testing.T) {
	for _, fields := range [][4]string{
		{"", "bucket", "key", ""},
		{"with space", "bucket", "key", ""},
		{"store\n", "bucket", "key", ""},
		{"\xff", "bucket", "key", ""},
		{"store", "", "key", ""},
		{"store", "https://evil", "key", ""},
		{"store", "../bucket", "key", ""},
		{"store", "bucket", "", ""},
		{"store", "bucket", strings.Repeat("k", 1025), ""},
		{"store", "bucket", "\xff", ""},
		{"store", "bucket", "key", "\xff"},
		{"store", "bucket", "key", strings.Repeat("v", 1025)},
	} {
		if _, err := NewRequest(fields[0], fields[1], fields[2], fields[3]); err == nil {
			t.Fatalf("accepted invalid fields %q", fields)
		}
	}

	for _, key := range []string{"/", ".", "../", "a//b", "a\x00b", "line\nbreak", strings.Repeat("k", 1024)} {
		if _, err := NewRequest("store", "bucket", key, "null"); err != nil {
			t.Fatalf("rejected exact key %q: %v", key, err)
		}
	}

	// Both strings fit the object limits, but escaping exceeds the SDK field cap.
	_, err := NewRequest("store", "bucket", strings.Repeat("\x00", 1024), strings.Repeat("\x00", 1024))
	assertOriginKind(t, err, racersdk.ErrorHeaderLimit)
}
