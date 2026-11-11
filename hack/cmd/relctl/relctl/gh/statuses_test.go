// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// statusServer is a stub of the endpoints the candidate status code uses.
type statusServer struct {
	mu sync.Mutex
	// statuses is what the combined-status endpoint returns, by commit.
	statuses map[string][]map[string]string
	// created records every status written.
	created []map[string]string
	// deleted records every release ID deleted.
	deleted []string
	// commits maps a ref to the commit it names.
	commits map[string]string
}

func (s *statusServer) client(t *testing.T) *Client {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v3/repos/Azure/unbounded/commits/{ref}/status",
		func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")

			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":    "success",
				"statuses": s.statuses[r.PathValue("ref")],
			})
		})

	mux.HandleFunc("POST /api/v3/repos/Azure/unbounded/statuses/{sha}",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode status: %v", err)
			}

			body["sha"] = r.PathValue("sha")

			s.mu.Lock()
			s.created = append(s.created, body)
			s.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		})

	mux.HandleFunc("GET /api/v3/repos/Azure/unbounded/commits/{ref}",
		func(w http.ResponseWriter, r *http.Request) {
			sha, ok := s.commits[r.PathValue("ref")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			// The SHA media type returns the bare hash as the body.
			_, _ = w.Write([]byte(sha))
		})

	mux.HandleFunc("DELETE /api/v3/repos/Azure/unbounded/releases/{id}",
		func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.deleted = append(s.deleted, r.PathValue("id"))
			s.mu.Unlock()

			w.WriteHeader(http.StatusNoContent)
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

	return client
}

func TestCandidateStatusReadsOnlyItsOwnContext(t *testing.T) {
	t.Parallel()

	stub := &statusServer{statuses: map[string][]map[string]string{
		"abc": {
			{"context": "ci/other", "state": "failure"},
			{"context": CandidateContext, "state": "success", "description": "v0.5.0-rc.2 passed"},
		},
		"def": {{"context": "ci/other", "state": "success"}},
	}}

	client := stub.client(t)

	got, err := client.CandidateStatus(t.Context(), "abc")
	if err != nil {
		t.Fatalf("CandidateStatus: %v", err)
	}

	if !got.Passed() || got.Description != "v0.5.0-rc.2 passed" {
		t.Errorf("CandidateStatus(abc) = %+v, want the passing candidate status", got)
	}

	// Another system's green check is not a candidate soak.
	none, err := client.CandidateStatus(t.Context(), "def")
	if err != nil {
		t.Fatalf("CandidateStatus: %v", err)
	}

	if none != nil || none.Passed() {
		t.Errorf("CandidateStatus(def) = %+v, want nil", none)
	}
}

func TestSetCandidateStatus(t *testing.T) {
	t.Parallel()

	stub := &statusServer{}
	client := stub.client(t)

	if err := client.SetCandidateStatus(t.Context(), "abc", StatusSuccess,
		"v0.5.0-rc.2 passed its soak", "https://example.invalid/run/1"); err != nil {
		t.Fatalf("SetCandidateStatus: %v", err)
	}

	if len(stub.created) != 1 {
		t.Fatalf("created %d statuses, want 1", len(stub.created))
	}

	got := stub.created[0]
	for key, want := range map[string]string{
		"sha":        "abc",
		"state":      StatusSuccess,
		"context":    CandidateContext,
		"target_url": "https://example.invalid/run/1",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// TestSetCandidateStatusTruncatesTheDescription keeps a long caption from
// losing the result: GitHub rejects the whole status past 140 characters.
func TestSetCandidateStatusTruncatesTheDescription(t *testing.T) {
	t.Parallel()

	stub := &statusServer{}

	if err := stub.client(t).SetCandidateStatus(t.Context(), "abc", StatusFailure,
		strings.Repeat("x", 300), ""); err != nil {
		t.Fatalf("SetCandidateStatus: %v", err)
	}

	if got := len([]rune(stub.created[0]["description"])); got != 140 {
		t.Errorf("description is %d characters, want 140", got)
	}
}

func TestSetCandidateStatusRefusesOtherStates(t *testing.T) {
	t.Parallel()

	stub := &statusServer{}

	for _, state := range []string{"pending", "error", ""} {
		if err := stub.client(t).SetCandidateStatus(t.Context(), "abc", state, "", ""); err == nil {
			t.Errorf("SetCandidateStatus(%q) = nil, want a refusal", state)
		}
	}

	if len(stub.created) != 0 {
		t.Errorf("wrote %d statuses for refused states", len(stub.created))
	}
}

func TestCommitOf(t *testing.T) {
	t.Parallel()

	stub := &statusServer{commits: map[string]string{"v0.5.0-rc.1": "abc123"}}
	client := stub.client(t)

	got, err := client.CommitOf(t.Context(), "v0.5.0-rc.1")
	if err != nil {
		t.Fatalf("CommitOf: %v", err)
	}

	if got != "abc123" {
		t.Errorf("CommitOf = %q, want abc123", got)
	}

	if _, err := client.CommitOf(t.Context(), "v9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("CommitOf(missing) = %v, want a does-not-exist error", err)
	}
}

func TestDeleteRelease(t *testing.T) {
	t.Parallel()

	stub := &statusServer{}
	client := stub.client(t)

	if err := client.DeleteRelease(t.Context(), Release{ID: 42, Tag: "v0.5.0-rc.1"}); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}

	if len(stub.deleted) != 1 || stub.deleted[0] != "42" {
		t.Errorf("deleted %v, want [42]", stub.deleted)
	}

	// A release with no ID cannot be the one anyone meant.
	if err := client.DeleteRelease(t.Context(), Release{Tag: "v0.5.0-rc.1"}); err == nil {
		t.Error("DeleteRelease without an ID = nil, want a refusal")
	}

	if len(stub.deleted) != 1 {
		t.Errorf("deleted %v after a refusal", stub.deleted)
	}
}
