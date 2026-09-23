// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"net/http/httptest"
	"testing"
)

func TestRacerRangeSemantics(t *testing.T) {
	for _, tc := range []struct {
		rangeValue           string
		ifRange              string
		size, offset, length int64
		partial, invalid     bool
	}{
		{"", "", 10, 0, 10, false, false},
		{"bytes=2-5", "", 10, 2, 4, true, false},
		{"bytes=2-", "", 10, 2, 8, true, false},
		{"bytes=-3", "", 10, 7, 3, true, false},
		{"bytes=-30", "", 10, 0, 10, true, false},
		{"bytes=2-50", "", 10, 2, 8, true, false},
		{"bytes=10-", "", 10, 0, 0, false, true},
		{"bytes=0-", "", 0, 0, 0, false, true},
		{"bytes=-0", "", 10, 0, 0, false, true},
		{"bytes=5-2", "", 10, 0, 0, false, true},
		{"bytes=+2-5", "", 10, 0, 0, false, true},
		{"bytes=2-5", `"other"`, 10, 0, 10, false, false},
		{"bytes=2-5", `"tag"`, 10, 2, 4, true, false},
		{"bytes=0-1,4-5", "", 10, 0, 10, false, false},
	} {
		t.Run(tc.rangeValue+tc.ifRange, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Range", tc.rangeValue)
			r.Header.Set("If-Range", tc.ifRange)

			offset, length, partial, invalid := mirrorRange(r, tc.size, `"tag"`)
			if offset != tc.offset || length != tc.length || partial != tc.partial || invalid != tc.invalid {
				t.Fatal(offset, length, partial, invalid)
			}
		})
	}
}
