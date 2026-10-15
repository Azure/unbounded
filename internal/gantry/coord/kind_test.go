// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coord

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	coordv1 "github.com/Azure/unbounded/internal/gantry/proto/coord/v1"
)

func TestInvalidKindRejectedBeforePump(t *testing.T) {
	s := &Server{pullerPump: func(context.Context, string, string, digest.Digest, ifaces.OriginRefKind) PumpResult {
		t.Fatal("invalid kind reached pump")
		return PumpResult{}
	}}

	for _, kind := range []coordv1.PleasePullRequest_Kind{coordv1.PleasePullRequest_KIND_UNSPECIFIED, 99, -1} {
		req := &coordv1.PleasePullRequest{Kind: kind, UpstreamRegistry: "registry.example", Repository: "repo"}

		data, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}

		r := httptest.NewRequest(http.MethodPost, ChairHTTPPath, bytes.NewReader(data))
		w := httptest.NewRecorder()
		// A nil starter also proves validation precedes the local call.
		NewChairHTTPHandler(nil, nil).ServeHTTP(w, r)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("HTTP kind %d: status %d", kind, w.Code)
		}
	}

	if _, err := s.StartLocalPull(t.Context(), "registry.example", "repo", 99, nil); err == nil {
		t.Fatal("invalid local kind accepted")
	}
}

func TestWireKindRoundTrip(t *testing.T) {
	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindManifest, ifaces.KindConfig} {
		got, err := pleasePullKindFromProto(pleasePullKindToProto(kind))
		if err != nil || got != kind {
			t.Fatalf("kind %v: got %v, %v", kind, got, err)
		}
	}
}

func TestHTTPSRejectsMissingAssignmentAndMalformedAuthorization(t *testing.T) {
	for _, req := range []*coordv1.PleasePullRequest{
		{Kind: coordv1.PleasePullRequest_KIND_BLOB},
		{Kind: coordv1.PleasePullRequest_KIND_BLOB, ChairAssignment: &coordv1.ChairAssignment{Generation: -1, AssignmentEpoch: 1}},
		{Kind: coordv1.PleasePullRequest_KIND_BLOB, ChairAssignment: &coordv1.ChairAssignment{Generation: 1, AssignmentEpoch: 1}, Authorization: "Basic invalid"},
	} {
		body, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}

		w := httptest.NewRecorder()
		NewChairHTTPHandler(nil, nil).ServeHTTP(w, httptest.NewRequest(http.MethodPost, ChairHTTPPath, bytes.NewReader(body)))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid request status = %d", w.Code)
		}
	}
}

func TestHTTPSUsesConfiguredBatchLimit(t *testing.T) {
	for _, limit := range []int{1, 300} {
		s := NewServer(WithMaxDigestsPerPleasePull(limit))

		req := &coordv1.PleasePullRequest{
			Kind:             coordv1.PleasePullRequest_KIND_BLOB,
			UpstreamRegistry: "registry.example", Repository: "repo",
			ChairAssignment: &coordv1.ChairAssignment{Generation: 1, AssignmentEpoch: 1},
		}
		for range 257 {
			req.Digests = append(req.Digests, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}

		body, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}

		w := httptest.NewRecorder()
		NewChairHTTPHandler(s, nil).ServeHTTP(w, httptest.NewRequest(http.MethodPost, ChairHTTPPath, bytes.NewReader(body)))

		want := http.StatusOK // Valid batch, but stale chair outcomes since no validator is wired.
		if limit == 1 {
			want = http.StatusBadRequest
		}

		if w.Code != want {
			t.Fatalf("limit %d: got %d want %d", limit, w.Code, want)
		}
	}
}
