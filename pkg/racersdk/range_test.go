// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"math"
	"net/http"
	"reflect"
	"testing"
)

func TestDecideRange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		meta   Metadata
		want   RangeDecision
	}{
		{"absent", nil, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"empty-object", nil, Metadata{}, RangeDecision{0, 0, 200}},
		{"empty-range", http.Header{"Range": {"bytes=0-"}}, Metadata{}, RangeDecision{0, 0, 416}},
		{"empty-malformed", http.Header{"Range": {"bytes=bad"}}, Metadata{}, RangeDecision{0, 0, 200}},
		{"empty-reversed", http.Header{"Range": {"bytes=2-1"}}, Metadata{}, RangeDecision{0, 0, 200}},
		{"reversed-outside", http.Header{"Range": {"bytes=20-10"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"mixed-case", http.Header{"Range": {"ByTeS=2-5"}}, Metadata{Size: 10}, RangeDecision{2, 4, 206}},
		{"outer-whitespace", http.Header{"Range": {" \tbytes=2-5\t "}}, Metadata{Size: 10}, RangeDecision{2, 4, 206}},
		{"inner-whitespace", http.Header{"Range": {"bytes=2- 5"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"duplicate", http.Header{"Range": {"bytes=2-5", "bytes=2-5"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"multipart", http.Header{"Range": {"bytes=2-5,7-8"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"u64-end", http.Header{"Range": {"bytes=2-18446744073709551615"}}, Metadata{Size: 10}, RangeDecision{2, 8, 206}},
		{"u64-suffix", http.Header{"Range": {"bytes=-18446744073709551615"}}, Metadata{Size: 10}, RangeDecision{0, 10, 206}},
		{"u64-start", http.Header{"Range": {"bytes=18446744073709551615-"}}, Metadata{Size: 10}, RangeDecision{0, 0, 416}},
		{"overflow-start", http.Header{"Range": {"bytes=18446744073709551616-"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"overflow-end", http.Header{"Range": {"bytes=2-18446744073709551616"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"overflow-suffix", http.Header{"Range": {"bytes=-18446744073709551616"}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"max-size-end", http.Header{"Range": {"bytes=9223372036854775806-18446744073709551615"}}, Metadata{Size: math.MaxInt64}, RangeDecision{math.MaxInt64 - 1, 1, 206}},
		{"max-size-suffix", http.Header{"Range": {"bytes=-18446744073709551615"}}, Metadata{Size: math.MaxInt64}, RangeDecision{0, math.MaxInt64, 206}},
		{"max-size-full-range", http.Header{"Range": {"bytes=0-9223372036854775807"}}, Metadata{Size: math.MaxInt64}, RangeDecision{0, math.MaxInt64, 206}},
		{"if-range-match", http.Header{"Range": {"bytes=2-5"}, "If-Range": {` "tag" `}}, Metadata{Size: 10, ETag: `"tag"`}, RangeDecision{2, 4, 206}},
		{"if-range-empty", http.Header{"Range": {"bytes=2-5"}, "If-Range": {""}}, Metadata{Size: 10, ETag: `"tag"`}, RangeDecision{0, 10, 200}},
		{"if-range-duplicate", http.Header{"Range": {"bytes=2-5"}, "If-Range": {`"tag"`, `"tag"`}}, Metadata{Size: 10, ETag: `"tag"`}, RangeDecision{0, 10, 200}},
		{"if-range-weak", http.Header{"Range": {"bytes=2-5"}, "If-Range": {`W/"tag"`}}, Metadata{Size: 10, ETag: `"tag"`}, RangeDecision{0, 10, 200}},
		{"weak-metadata", http.Header{"Range": {"bytes=2-5"}, "If-Range": {`W/"tag"`}}, Metadata{Size: 10, ETag: `W/"tag"`}, RangeDecision{0, 10, 200}},
		{"missing-etag", http.Header{"Range": {"bytes=2-5"}, "If-Range": {""}}, Metadata{Size: 10}, RangeDecision{0, 10, 200}},
		{"if-range-before-unsatisfiable", http.Header{"Range": {"bytes=10-"}, "If-Range": {`"old"`}}, Metadata{Size: 10, ETag: `"tag"`}, RangeDecision{0, 10, 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.header.Clone()

			if got := DecideRange(tc.header, tc.meta); got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}

			if !reflect.DeepEqual(tc.header, before) {
				t.Fatal("decision mutated request headers")
			}
		})
	}
}
