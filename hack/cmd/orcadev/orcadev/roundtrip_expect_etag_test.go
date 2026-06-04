// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateExpectedETag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		got     string
		want    string
		wantErr bool
	}{
		{name: "disabled", got: "abc", want: "", wantErr: false},
		{name: "exact", got: "abc", want: "abc", wantErr: false},
		{name: "quoted", got: "\"abc\"", want: "abc", wantErr: false},
		{name: "weak", got: "W/\"abc\"", want: "abc", wantErr: false},
		{name: "weak lowercase", got: "w/\"abc\"", want: "abc", wantErr: false},
		{name: "weak with space", got: `W/ "abc"`, want: "abc", wantErr: false},
		{name: "got whitespace padded", got: "  \"abc\"  ", want: "abc", wantErr: false},
		{name: "mismatch", got: "abc", want: "def", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateExpectedETag(2, tt.got, tt.want)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateExpectedETag() = nil, want error")
				}

				if !strings.Contains(err.Error(), "iter 2") {
					t.Fatalf("error %q should include iteration", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("validateExpectedETag() unexpected error: %v", err)
			}
		})
	}
}

func TestFetchAndHashReturnsETag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	edge := newEdgeClient(server.URL, time.Second)

	hash, status, size, etag, err := fetchAndHash(context.Background(), edge, "bucket", "key", "")
	if err != nil {
		t.Fatalf("fetchAndHash() error = %v", err)
	}

	if status != http.StatusOK || size != 5 || etag != "abc123" {
		t.Fatalf("fetchAndHash() = status %d size %d etag %q, want 200 5 abc123", status, size, etag)
	}

	if hash != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("hash = %s", hash)
	}
}

// TestRunRoundtripWith_ExpectETag drives runRoundtripWith end-to-end
// against a fake origin + httptest edge so a future regression that
// drops the validateExpectedETag call site in runRoundtripWith is
// caught by tests, not in production.
func TestRunRoundtripWith_ExpectETag(t *testing.T) {
	t.Parallel()

	const (
		bucket = "test-bucket"
		key    = "test-key"
		body   = "hello world"
		etag   = "real-etag"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+etag+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name       string
		expectETag string
		wantErr    bool
		wantErrSub string
	}{
		{name: "match", expectETag: etag},
		{name: "match quoted", expectETag: `"` + etag + `"`},
		{name: "disabled", expectETag: ""},
		{name: "mismatch", expectETag: "wrong-etag", wantErr: true, wantErrSub: "ETag mismatch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			oc := newFakeOriginClient("fake", bucket)
			oc.put(key, etag, []byte(body))

			edge := newEdgeClient(server.URL, 2*time.Second)

			o := &roundtripOpts{
				key:        key,
				repeat:     1,
				expectETag: tt.expectETag,
			}

			err := runRoundtripWith(context.Background(), oc, edge, o)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("runRoundtripWith() = nil error, want error containing %q", tt.wantErrSub)
				}

				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("runRoundtripWith() error = %v, want substring %q", err, tt.wantErrSub)
				}

				return
			}

			if err != nil {
				t.Fatalf("runRoundtripWith() unexpected error: %v", err)
			}
		})
	}
}
