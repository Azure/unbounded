// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

func TestCachedRangeAdmissionPreservesMissingRangeStatus(t *testing.T) {
	d := mustDigest([]byte("partial"))
	s := New(fakes.NewCache(), WithMaxConcurrentRangeServes(1),
		WithRangeSource(&cachedRanges{digest: d, start: 0, body: []byte("xy"), total: 10}))

	release, ok := s.tryAcquireServe("bytes=0-1")
	if !ok {
		t.Fatal("could not occupy range lane")
	}

	for _, test := range []struct {
		rangeHeader string
		status      int
	}{
		{rangeHeader: "bytes=0-1", status: http.StatusTooManyRequests},
		{rangeHeader: "bytes=5-6", status: http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v2/r/blobs/"+d.String(), nil)
		req.Header.Set(MirroredHeader, "1")
		req.Header.Set("Range", test.rangeHeader)

		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, req)

		if response.Code != test.status {
			t.Fatalf("range %s: status = %d, want %d", test.rangeHeader, response.Code, test.status)
		}
	}

	release()

	req := httptest.NewRequest(http.MethodGet, "/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Range", "bytes=0-1")

	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, req)

	if response.Code != http.StatusPartialContent || response.Body.String() != "xy" {
		t.Fatalf("available lane: status/body = %d/%q", response.Code, response.Body.String())
	}
}
