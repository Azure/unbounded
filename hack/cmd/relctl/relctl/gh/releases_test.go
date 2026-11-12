// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestReleaseBranchesListsOnlyReleaseXY pins that a branch which only starts
// with "release-", such as a feature branch, is not reported as a release
// branch.
func TestReleaseBranchesListsOnlyReleaseXY(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/Azure/unbounded/branches",
		func(w http.ResponseWriter, _ *http.Request) {
			branches := []map[string]string{
				{"name": "main"},
				{"name": "release-0.2"},
				{"name": "release-candidates-first"},
				{"name": "release-0.3"},
				{"name": "release-0.3-backup"},
				{"name": "feat/release-0.4"},
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(branches)
		})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := New(t.Context(), Options{
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := client.ReleaseBranches(t.Context())
	if err != nil {
		t.Fatalf("ReleaseBranches: %v", err)
	}

	want := []string{"release-0.2", "release-0.3"}
	if !slices.Equal(got, want) {
		t.Errorf("ReleaseBranches = %q, want %q", got, want)
	}
}
