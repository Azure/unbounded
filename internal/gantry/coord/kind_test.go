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
		if _, err := s.servePleasePull(t.Context(), "", req); err == nil {
			t.Fatalf("wire kind %d accepted", kind)
		}

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
