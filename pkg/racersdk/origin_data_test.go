// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type originDataStore struct {
	memoryStore
	t    *testing.T
	want []byte
}

func (s *originDataStore) ResolveRange(ctx context.Context, target string, data []byte) (ResolvedRange, error) {
	if !bytes.Equal(data, s.want) {
		s.t.Error("Stat origin data changed")
	}

	return s.memoryStore.ResolveRange(ctx, target, data)
}

func TestOriginDataValidationAndDelivery(t *testing.T) {
	maximum := make([]byte, MaxOriginDataBytes)
	for i := range maximum {
		maximum[i] = byte(i)
	}

	for _, tc := range []struct {
		name   string
		values []string
		want   []byte
		status int
	}{
		{"absent", nil, nil, 200},
		{"empty", []string{""}, nil, 200},
		{"binary-maximum", []string{base64.StdEncoding.EncodeToString(maximum)}, maximum, 200},
		{"duplicate", []string{"YQ==", "YQ=="}, nil, 400},
		{"duplicate-empty", []string{"", ""}, nil, 400},
		{"invalid", []string{"!"}, nil, 400},
		{"unpadded", []string{"YQ"}, nil, 400},
		{"pad-bits", []string{"YR=="}, nil, 400},
		{"newline", []string{"YQ==\n"}, nil, 400},
		{"whitespace", []string{" YQ=="}, nil, 400},
		{"decoded-overflow", []string{base64.StdEncoding.EncodeToString(append(bytes.Clone(maximum), 0))}, nil, 400},
		{"encoded-overflow", []string{strings.Repeat("A", maxEncodedOriginDataBytes+1)}, nil, 431},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ranged := range []bool{false, true} {
				store := &originDataStore{t: t, want: tc.want, memoryStore: memoryStore{data: []byte("body"), meta: Metadata{Size: 4, ETag: checksumTag([]byte("body"))}}}
				ranges := &rangeTestStore{data: []byte("body")}

				origin, err := NewRangeOrigin(store)
				if ranged {
					origin, err = NewRangeOrigin(ranges)
				}

				if err != nil {
					t.Fatal(err)
				}

				for _, method := range []string{http.MethodHead, http.MethodGet} {
					r := httptest.NewRequest(method, "/object", nil)
					if tc.values != nil {
						r.Header["Racer-Origin-Data"] = tc.values
					}

					w := httptest.NewRecorder()
					origin.ServeHTTP(w, r)

					if w.Code != tc.status {
						t.Fatalf("%s ranged=%v: status %d, want %d", method, ranged, w.Code, tc.status)
					}

					if tc.status != 200 && (store.stats.Load() != 0 || store.opens.Load() != 0 || ranges.opens != 0 || w.Body.Len() != 0) {
						t.Fatal("rejected data reached store or response body")
					}

					if tc.status == 200 && ranged && ranges.auth != string(tc.want) {
						t.Fatal("range origin data changed")
					}
				}
			}
		})
	}
}
