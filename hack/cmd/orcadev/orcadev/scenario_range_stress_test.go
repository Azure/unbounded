// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVerifyRangeResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{name: "match", status: http.StatusPartialContent, body: "abcd", want: "abcd"},
		{name: "wrong status", status: http.StatusOK, body: "abcd", want: "abcd", wantErr: true},
		{name: "wrong bytes", status: http.StatusPartialContent, body: "abce", want: "abcd", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := verifyRangeResponse(edgeResponse{
				Status: tt.status,
				Body:   io.NopCloser(strings.NewReader(tt.body)),
			}, []byte(tt.want))
			if tt.wantErr {
				if err == nil {
					t.Fatal("verifyRangeResponse() = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("verifyRangeResponse() unexpected error: %v", err)
			}
		})
	}
}

func TestShouldLogRangeStressBufferNotice(t *testing.T) {
	t.Parallel()

	if shouldLogRangeStressBufferNotice(1024 * 1024 * 1024) {
		t.Fatal("notice should not be emitted at exactly 1 GiB")
	}

	if !shouldLogRangeStressBufferNotice(1024*1024*1024 + 1) {
		t.Fatal("notice should be emitted above 1 GiB")
	}
}
