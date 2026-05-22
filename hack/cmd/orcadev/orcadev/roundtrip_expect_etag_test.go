// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
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
