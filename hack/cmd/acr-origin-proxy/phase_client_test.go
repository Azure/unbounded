// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSetRemotePhaseRetriesConflict(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}

		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := setRemotePhase(context.Background(), server.Client(), server.URL, "secret", phaseBaseline); err != nil {
		t.Fatalf("setRemotePhase: %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}
