// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// A stub GitHub API for the command-level tests.
//
// status, preflight and watch could not be tested at all before Options gained
// a BaseURL: they build a client and there was no way to aim it anywhere but
// api.github.com. That is why next, the one command needing no client, was the
// one command with tests.

// stubGitHub serves the endpoints the read commands use.
type stubGitHub struct {
	// runs maps a workflow file name to its runs, newest last.
	runs map[string][]stubRun
	// releases are returned by the list endpoint, newest first.
	releases []stubRelease
	// latest is returned by /releases/latest. Empty means 404.
	latest string
	// branches are returned by the branch list.
	branches []string
	// releaseByTag maps a tag to a release. Absent means 404.
	releaseByTag map[string]stubRelease
}

type stubRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	HTMLURL    string `json:"html_url"`
}

type stubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
}

func (s *stubGitHub) start(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode: %v", err)
		}
	}

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/",
		func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			file := parts[len(parts)-2]

			var matched []stubRun

			for _, run := range s.runs[file] {
				if branch := r.URL.Query().Get("branch"); branch != "" && run.HeadBranch != branch {
					continue
				}

				if sha := r.URL.Query().Get("head_sha"); sha != "" && run.HeadSHA != sha {
					continue
				}

				// The server-side status filter the dashboard relies on, so a
				// still-running job cannot be pushed out by newer completed
				// ones.
				if status := r.URL.Query().Get("status"); status != "" && run.Status != status {
					continue
				}

				matched = append(matched, run)
			}

			write(w, map[string]any{"total_count": len(matched), "workflow_runs": matched})
		})

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			if s.latest == "" {
				w.WriteHeader(http.StatusNotFound)
				write(w, map[string]string{"message": "Not Found"})

				return
			}

			write(w, stubRelease{TagName: s.latest})
		})

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/releases/tags/",
		func(w http.ResponseWriter, r *http.Request) {
			tag := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/Azure/unbounded/releases/tags/")

			release, ok := s.releaseByTag[tag]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				write(w, map[string]string{"message": "Not Found"})

				return
			}

			write(w, release)
		})

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/releases",
		func(w http.ResponseWriter, _ *http.Request) {
			write(w, s.releases)
		})

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/branches",
		func(w http.ResponseWriter, _ *http.Request) {
			out := make([]map[string]any, 0, len(s.branches))
			for _, name := range s.branches {
				out = append(out, map[string]any{"name": name})
			}

			write(w, out)
		})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server.URL + "/"
}

// runCommand executes relctl against a stub API and a real git fixture.
//
// The credential is injected rather than looked up. Relying on the ambient one
// meant these passed on a machine with gh logged in and failed in CI, which is
// precisely the environment dependency the stub exists to remove.
func runCommand(t *testing.T, stub *stubGitHub, repoPath string, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	cmd := newRoot(&Options{
		Repo:    gh.DefaultRepo,
		Output:  OutputText,
		BaseURL: stub.start(t),
		Token:   func(context.Context) (string, error) { return "test-token", nil },
	})
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	full := append([]string{}, args...)
	if repoPath != "" {
		full = append(full, "--repo-path", repoPath)
	}

	cmd.SetArgs(full)

	err := cmd.ExecuteContext(t.Context())

	return out.String(), err
}

func ago(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

func TestStatusReportsTheWholePicture(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest:   "v0.4.0",
		branches: []string{"main", "release-0.4", "feat/x"},
		releases: []stubRelease{
			{TagName: "v0.4.0"},
			{TagName: "v0.3.0-rc.1", Draft: true},
			{TagName: "v0.2.4-rc.1", Draft: true},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	for _, want := range []string{
		"v0.4.0",
		"release-0.4",
		"v0.3.0-rc.1",
		"2 draft(s)",
		"Nothing in flight",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	// Only release-* branches, not every branch on the repository.
	if strings.Contains(out, "feat/x") {
		t.Errorf("status listed a non-release branch:\n%s", out)
	}
}

// TestStatusSaysWhenLocalStateIsUnknown is the difference between "I could not
// tell" and "there are none". For the command whose purpose is being the
// dashboard, rendering the second when the first is true is the worst failure
// it has.
func TestStatusSaysWhenLocalStateIsUnknown(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{latest: "v0.4.0"}

	// A directory that is not a repository, which is what a wrong --repo-path
	// or running outside a clone looks like.
	out, err := runCommand(t, stub, t.TempDir(), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "UNKNOWN") {
		t.Errorf("status did not report that local state is unknown:\n%s", out)
	}

	if strings.Contains(out, "Live trains:   (none)") {
		t.Errorf("status claimed there are no live trains when it could not tell:\n%s", out)
	}
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{latest: "v0.4.0", branches: []string{"release-0.4"}}

	out, err := runCommand(t, stub, tagRepo(t), "status", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	var decoded statusResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	if decoded.LatestRelease != "v0.4.0" {
		t.Errorf("LatestRelease = %q", decoded.LatestRelease)
	}

	if decoded.NextFromLocal != "v0.5.0" {
		t.Errorf("NextFromLocal = %q, want v0.5.0", decoded.NextFromLocal)
	}
}

func TestPreflightBlocksOnARedNightly(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{runs: map[string][]stubRun{
		"nightly.yaml": {{ID: 1, Status: "completed", Conclusion: "failure", CreatedAt: ago(time.Hour)}},
		"ci.yaml":      {{ID: 2, Status: "completed", Conclusion: "success", HeadBranch: "main", CreatedAt: ago(time.Hour)}},
	}}

	out, _ := runCommand(t, stub, tagRepo(t), "preflight")

	if !strings.Contains(out, "NOT RELEASABLE") {
		t.Errorf("a red nightly must block:\n%s", out)
	}

	if !strings.Contains(out, "release blocker until it is understood") {
		t.Errorf("output does not explain why:\n%s", out)
	}
}

// TestPreflightBlocksOnAStaleNightly covers "green, and green recently". A
// week-old pass describes a tree nobody has released since.
func TestPreflightBlocksOnAStaleNightly(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{runs: map[string][]stubRun{
		"nightly.yaml": {{ID: 1, Status: "completed", Conclusion: "success", CreatedAt: ago(7 * 24 * time.Hour)}},
		"ci.yaml":      {{ID: 2, Status: "completed", Conclusion: "success", HeadBranch: "main", CreatedAt: ago(time.Hour)}},
	}}

	out, _ := runCommand(t, stub, tagRepo(t), "preflight")

	if !strings.Contains(out, "NOT RELEASABLE") {
		t.Errorf("a stale nightly must block:\n%s", out)
	}
}

func TestPreflightPassesWhenEverythingIsGreen(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{runs: map[string][]stubRun{
		"nightly.yaml": {{ID: 1, Status: "completed", Conclusion: "success", CreatedAt: ago(4 * time.Hour)}},
		"ci.yaml":      {{ID: 2, Status: "completed", Conclusion: "success", HeadBranch: "main", CreatedAt: ago(time.Hour)}},
	}}

	out, err := runCommand(t, stub, tagRepo(t), "preflight")
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}

	if !strings.Contains(out, "RELEASABLE") || strings.Contains(out, "NOT RELEASABLE") {
		t.Errorf("want RELEASABLE:\n%s", out)
	}
}

// TestPreflightSaysTheNightlyIsIrrelevantOnAReleaseBranch keeps a green tick
// from referring to somewhere else. The nightly only ever runs on the default
// branch.
func TestPreflightSaysTheNightlyIsIrrelevantOnAReleaseBranch(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{runs: map[string][]stubRun{
		"ci.yaml": {{
			ID: 2, Status: "completed", Conclusion: "success",
			HeadBranch: "release-0.4", CreatedAt: ago(time.Hour),
		}},
	}}

	out, err := runCommand(t, stub, tagRepo(t), "preflight", "--branch", "release-0.4")
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}

	if !strings.Contains(out, "says nothing about release-0.4") {
		t.Errorf("output does not explain the nightly's scope:\n%s", out)
	}
}

func TestWatchReportsAPublishedRelease(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.4.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: ago(2 * time.Hour),
			}},
			"release-upgrade.yaml": {{
				ID: 2, HeadBranch: "main", HeadSHA: "abc", Event: "workflow_run",
				Status: "completed", Conclusion: "success", CreatedAt: ago(time.Hour),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.4.0": {TagName: "v0.4.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.4.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	for _, want := range []string{"v0.4.0", "success", "published"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output missing %q:\n%s", want, out)
		}
	}
}

// TestWatchFailsOnAStuckDraft is the exit status a script depends on. A failed
// soak leaves a draft, and reporting that as success would be worse than
// reporting nothing.
func TestWatchFailsOnAStuckDraft(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "def", Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: ago(2 * time.Hour),
			}},
			"release-upgrade.yaml": {{
				ID: 2, HeadBranch: "main", HeadSHA: "def", Event: "workflow_run",
				Status: "completed", Conclusion: "failure", CreatedAt: ago(time.Hour),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0", Draft: true}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0")
	if err == nil {
		t.Fatalf("watch: want a non-zero verdict for a stuck draft:\n%s", out)
	}

	if !strings.Contains(err.Error(), "still a draft") {
		t.Errorf("error = %q", err)
	}
}
