// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest_test

import (
	"errors"
	"io"
	"math"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestHelloOriginRange4210611697(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request racersdk.OriginRequest
		want    string
		wantErr error
	}{
		{name: "normal", request: racersdk.OriginRequest{Length: racersdk.PageSize}, want: "hello"},
		{name: "short range", request: racersdk.OriginRequest{Length: 3}, want: "hel"},
		{name: "pinned", request: racersdk.OriginRequest{Length: racersdk.PageSize, ETag: `"v1"`}, want: "hello"},
		{name: "empty length", request: racersdk.OriginRequest{}},
		{name: "at end", request: racersdk.OriginRequest{Offset: 5, Length: racersdk.PageSize}},
		{name: "past end", request: racersdk.OriginRequest{Offset: racersdk.PageSize, Length: racersdk.PageSize}},
		{name: "near MaxInt64", request: racersdk.OriginRequest{Offset: math.MaxInt64 / racersdk.PageSize * racersdk.PageSize, Length: racersdk.PageSize}},
		{name: "head", request: racersdk.OriginRequest{Head: true, Length: racersdk.PageSize}},
		{name: "version mismatch", request: racersdk.OriginRequest{Length: racersdk.PageSize, ETag: `"v2"`}, wantErr: racersdk.ErrVersionMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata, body, err := helloOrigin(t.Context(), tc.request)
			if body != nil {
				defer body.Close()
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v; want %v", err, tc.wantErr)
			}

			if metadata.Size != 5 || metadata.ETag != `"v1"` || metadata.ExpiresAt.IsZero() {
				t.Fatalf("metadata = %+v", metadata)
			}

			if tc.want == "" {
				if body != nil {
					t.Fatal("expected nil body")
				}

				return
			}

			if body == nil {
				t.Fatal("expected body")
			}

			data, err := io.ReadAll(body)
			if err != nil || string(data) != tc.want {
				t.Fatalf("body = %q, %v; want %q", data, err, tc.want)
			}
		})
	}
}
