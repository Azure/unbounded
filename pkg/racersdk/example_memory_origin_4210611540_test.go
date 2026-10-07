// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk_test

import (
	"errors"
	"io"
	"math"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestExampleMemoryOriginRanges4210611540(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		offset  int64
		length  int64
		head    bool
		missing bool
		want    string
	}{
		{name: "ordinary page", content: "hello, racer", length: racersdk.PageSize, want: "hello, racer"},
		{name: "short range", content: "hello, racer", length: 5, want: "hello"},
		{name: "clamped range", content: "hello, racer", offset: 7, length: racersdk.PageSize, want: "racer"},
		{name: "at end", content: "hello, racer", offset: 12, length: racersdk.PageSize},
		{name: "past end", content: "hello, racer", offset: racersdk.PageSize, length: racersdk.PageSize},
		{name: "zero length", content: "hello, racer", offset: 7},
		{name: "empty object", length: racersdk.PageSize},
		{name: "final protocol page", content: "hello, racer", offset: math.MaxInt64 - racersdk.PageSize + 1, length: racersdk.PageSize},
		{name: "empty object final protocol page", offset: math.MaxInt64 - racersdk.PageSize + 1, length: racersdk.PageSize},
		{name: "head", content: "hello, racer", head: true},
		{name: "missing key", content: "hello, racer", length: racersdk.PageSize, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := racersdk.OriginRequest{
				Request: racersdk.Request{Key: key},
				Offset:  tc.offset,
				Length:  tc.length,
				Head:    tc.head,
			}
			if tc.missing {
				request.Key = racersdk.Key{}
			}

			metadata, body, err := exampleMemoryOrigin([]byte(tc.content))(t.Context(), request)
			if body != nil {
				defer body.Close()
			}

			if tc.missing {
				if !errors.Is(err, racersdk.ErrNotFound) || body != nil || metadata != (racersdk.Metadata{}) {
					t.Fatalf("missing key: metadata=%+v body=%v err=%v", metadata, body, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if metadata.Size != int64(len(tc.content)) || metadata.ETag != `"v1"` || metadata.ExpiresAt.IsZero() {
				t.Fatalf("unexpected metadata: %+v", metadata)
			}

			if tc.want == "" {
				if body != nil {
					t.Fatal("empty range or HEAD must return a nil body")
				}

				return
			}

			if body == nil {
				t.Fatal("nonempty range returned a nil body")
			}

			got, err := io.ReadAll(body)
			if err != nil || string(got) != tc.want {
				t.Fatalf("body=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}
