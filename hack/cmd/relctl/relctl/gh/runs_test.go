// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubRun is one run in a fake Actions API.
type stubRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	// RunStartedAt and UpdatedAt move on a re-run while CreatedAt does not,
	// which is the distinction Attribute depends on.
	RunStartedAt string `json:"run_started_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	// Actor and TriggeringActor are objects on the wire, not logins, so the
	// stub carries them as objects: a string here would pass a test that the
	// real API would fail.
	Actor           *stubUser `json:"actor,omitempty"`
	TriggeringActor *stubUser `json:"triggering_actor,omitempty"`
	HTMLURL         string    `json:"html_url"`
}

// stubUser is the shape GitHub returns for a run's actor.
type stubUser struct {
	Login string `json:"login"`
}

// stubAPI serves workflow runs, filtering the way GitHub does.
//
// The filtering matters: Progress relies on head_sha narrowing server-side, so
// a stub that ignored the parameter would test a code path that cannot happen
// and hide one that can.
type stubAPI struct {
	runs map[string][]stubRun
}

func (s *stubAPI) server(t *testing.T) *Client {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/",
		func(w http.ResponseWriter, r *http.Request) {
			// .../workflows/<file>/runs
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			file := parts[len(parts)-2]

			var matched []stubRun

			for _, run := range s.runs[file] {
				if sha := r.URL.Query().Get("head_sha"); sha != "" && run.HeadSHA != sha {
					continue
				}

				if branch := r.URL.Query().Get("branch"); branch != "" && run.HeadBranch != branch {
					continue
				}

				if event := r.URL.Query().Get("event"); event != "" && run.Event != event {
					continue
				}

				matched = append(matched, run)
			}

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(map[string]any{
				"total_count":   len(matched),
				"workflow_runs": matched,
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
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

func at(minute int) string {
	return time.Date(2026, 8, 21, 3, minute, 0, 0, time.UTC).Format(time.RFC3339)
}

// sharedCommit reproduces the shape that makes correlation hard: a promoted
// final and its last candidate on the SAME commit, each with its own build and
// its own soak. This is real - v0.3.0 and v0.3.0-rc.1 are both 3c9621da, as are
// v0.2.4 and v0.2.4-rc.1.
func sharedCommit() *stubAPI {
	const sha = "3c9621da"

	return &stubAPI{runs: map[string][]stubRun{
		WorkflowRelease: {
			{
				ID: 1, HeadBranch: "v0.3.0-rc.1", HeadSHA: sha, Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: at(22),
			},
			{
				ID: 2, HeadBranch: "v0.3.0", HeadSHA: sha, Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: at(18 + 60),
			},
		},
		WorkflowUpgrade: {
			// The candidate's soak, plus a manual retry of it.
			{
				ID: 10, HeadBranch: "main", HeadSHA: sha, Event: "workflow_run",
				Status: "completed", Conclusion: "failure", CreatedAt: at(46),
			},
			{
				ID: 11, HeadBranch: "main", HeadSHA: sha, Event: "workflow_dispatch",
				Status: "completed", Conclusion: "failure", CreatedAt: at(54),
			},
			// The final's soak, after the second build.
			{
				ID: 12, HeadBranch: "main", HeadSHA: sha, Event: "workflow_run",
				Status: "completed", Conclusion: "success", CreatedAt: at(53 + 60),
			},
		},
	}}
}

// TestProgressSeparatesSoaksSharingACommit is the case head_sha alone cannot
// answer. Both tags build the same commit, so both have soaks with that sha;
// only the time window says which soak belongs to which.
func TestProgressSeparatesSoaksSharingACommit(t *testing.T) {
	t.Parallel()

	client := sharedCommit().server(t)

	candidate, err := client.Progress(t.Context(), "v0.3.0-rc.1")
	if err != nil {
		t.Fatalf("Progress(rc): %v", err)
	}

	if candidate.Build == nil || candidate.Build.ID != 1 {
		t.Fatalf("rc build = %v, want run 1", candidate.Build)
	}

	// The candidate's window closes when the final's build starts, so it gets
	// its own soak and the retry, and not the final's.
	assertRunIDs(t, "rc soaks", candidate.Soaks, []int64{10, 11})

	final, err := client.Progress(t.Context(), "v0.3.0")
	if err != nil {
		t.Fatalf("Progress(final): %v", err)
	}

	if final.Build == nil || final.Build.ID != 2 {
		t.Fatalf("final build = %v, want run 2", final.Build)
	}

	assertRunIDs(t, "final soaks", final.Soaks, []int64{12})
}

// TestProgressIncludesManualRetries keeps workflow_dispatch attempts in the
// window. A retried soak is still this release's soak, and dropping it would
// report the failure and hide the recovery.
func TestProgressIncludesManualRetries(t *testing.T) {
	t.Parallel()

	client := sharedCommit().server(t)

	candidate, err := client.Progress(t.Context(), "v0.3.0-rc.1")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}

	var sawDispatch bool

	for _, soak := range candidate.Soaks {
		if soak.Event == "workflow_dispatch" {
			sawDispatch = true
		}
	}

	if !sawDispatch {
		t.Error("manual retry not reported as part of this release's soak")
	}
}

// TestProgressWithNoBuild covers the ordinary early state: the tag was pushed
// and nothing has run yet.
func TestProgressWithNoBuild(t *testing.T) {
	t.Parallel()

	client := (&stubAPI{runs: map[string][]stubRun{}}).server(t)

	progress, err := client.Progress(t.Context(), "v9.9.9")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}

	if progress.Build != nil {
		t.Errorf("Build = %v, want nil", progress.Build)
	}

	if len(progress.Soaks) != 0 {
		t.Errorf("Soaks = %v, want none", progress.Soaks)
	}
}

// TestProgressWithNoSoakYet is the window still being open: the build finished
// and the soak has not started.
func TestProgressWithNoSoakYet(t *testing.T) {
	t.Parallel()

	client := (&stubAPI{runs: map[string][]stubRun{
		WorkflowRelease: {
			{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: at(10),
			},
		},
	}}).server(t)

	progress, err := client.Progress(t.Context(), "v0.5.0")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}

	if progress.Build == nil {
		t.Fatal("Build = nil, want the build run")
	}

	if len(progress.Soaks) != 0 {
		t.Errorf("Soaks = %v, want none", progress.Soaks)
	}
}

// TestProgressIgnoresSoaksBeforeTheBuild guards the lower edge of the window.
// An earlier soak of the same commit belongs to an earlier tag.
func TestProgressIgnoresSoaksBeforeTheBuild(t *testing.T) {
	t.Parallel()

	client := (&stubAPI{runs: map[string][]stubRun{
		WorkflowRelease: {
			{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: at(30),
			},
		},
		WorkflowUpgrade: {
			{
				ID: 9, HeadBranch: "main", HeadSHA: "abc", Event: "workflow_run",
				Status: "completed", Conclusion: "success", CreatedAt: at(10),
			},
			{
				ID: 10, HeadBranch: "main", HeadSHA: "abc", Event: "workflow_run",
				Status: "completed", Conclusion: "success", CreatedAt: at(40),
			},
		},
	}}).server(t)

	progress, err := client.Progress(t.Context(), "v0.5.0")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}

	assertRunIDs(t, "soaks", progress.Soaks, []int64{10})
}

func TestRunState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		run  Run
		want string
	}{
		{name: "success", run: Run{Status: "completed", Conclusion: "success"}, want: "success"},
		{name: "failure", run: Run{Status: "completed", Conclusion: "failure"}, want: "failure"},
		{name: "running", run: Run{Status: "in_progress"}, want: "in_progress"},
		{name: "queued", run: Run{Status: "queued"}, want: "queued"},
		// A completed run with no conclusion should not render as empty.
		{name: "completed without a conclusion", run: Run{Status: "completed"}, want: "completed"},
		{name: "nothing known", run: Run{}, want: "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.run.State(); got != tc.want {
				t.Errorf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}

func assertRunIDs(t *testing.T, name string, runs []Run, want []int64) {
	t.Helper()

	got := make([]int64, 0, len(runs))
	for _, run := range runs {
		got = append(got, run.ID)
	}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestProgressSeparatesBuildsCreatedInTheSameSecond covers the tie-break.
//
// created_at has second resolution, so two builds of one commit inside the same
// second both fail an After test, leaving the window open-ended and merging
// both builds' soaks into one report. Run IDs are monotonic, so they order what
// the timestamp cannot.
func TestProgressSeparatesBuildsCreatedInTheSameSecond(t *testing.T) {
	t.Parallel()

	const sha = "samesecond"

	same := at(30)

	client := (&stubAPI{runs: map[string][]stubRun{
		WorkflowRelease: {
			{
				ID: 1, HeadBranch: "v0.5.0-rc.1", HeadSHA: sha, Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: same,
			},
			{
				ID: 2, HeadBranch: "v0.5.0", HeadSHA: sha, Event: "push",
				Status: "completed", Conclusion: "success", CreatedAt: same,
			},
		},
		WorkflowUpgrade: {
			{
				ID: 10, HeadBranch: "main", HeadSHA: sha, Event: "workflow_run",
				Status: "completed", Conclusion: "success", CreatedAt: at(40),
			},
		},
	}}).server(t)

	// The candidate's window closes at the final's build despite the identical
	// timestamp, so the later soak is not attributed to it.
	candidate, err := client.Progress(t.Context(), "v0.5.0-rc.1")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}

	assertRunIDs(t, "rc soaks", candidate.Soaks, nil)
}

// TestRunsCapsThePageSize keeps Limit honest. GitHub silently truncates
// per_page above 100, so asking for more would quietly return fewer results
// than requested while looking like it worked.
func TestRunsCapsThePageSize(t *testing.T) {
	t.Parallel()

	var seen string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/",
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.URL.Query().Get("per_page")

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "workflow_runs": []stubRun{},
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
		})

	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(t.Context(), Options{
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.Runs(t.Context(), ListRuns{Workflow: WorkflowRelease, Limit: 500}); err != nil {
		t.Fatalf("Runs: %v", err)
	}

	if seen != "100" {
		t.Errorf("per_page = %q, want it capped at 100", seen)
	}
}

// TestRunsPassesTheStatusFilterServerSide is what keeps a still-running job
// visible on the dashboard when newer completed ones would otherwise fill the
// page.
func TestRunsPassesTheStatusFilterServerSide(t *testing.T) {
	t.Parallel()

	var seen string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/",
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.URL.Query().Get("status")

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "workflow_runs": []stubRun{},
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
		})

	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(t.Context(), Options{
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.Runs(t.Context(), ListRuns{
		Workflow: WorkflowRelease,
		Status:   "in_progress",
	}); err != nil {
		t.Fatalf("Runs: %v", err)
	}

	if seen != "in_progress" {
		t.Errorf("status = %q, want in_progress sent server-side", seen)
	}
}

// TestRunsCarriesTheAttributionFields checks the fields Attribute reads are
// actually decoded, since every one of them was absent until attribution
// existed and a missing one degrades silently to an unknown rather than to a
// test failure.
func TestRunsCarriesTheAttributionFields(t *testing.T) {
	t.Parallel()

	client := (&stubAPI{runs: map[string][]stubRun{
		WorkflowRelease: {{
			ID: 1, HeadBranch: "v0.5.0", Event: "push",
			Status: "completed", Conclusion: "failure",
			CreatedAt: at(33), RunStartedAt: at(50), UpdatedAt: at(55),
			Actor:           &stubUser{Login: "key-owner"},
			TriggeringActor: &stubUser{Login: "cchildress"},
		}},
	}}).server(t)

	runs, err := client.Runs(t.Context(), ListRuns{Workflow: WorkflowRelease})
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}

	run := runs[0]

	if run.Actor != "key-owner" {
		t.Errorf("Actor = %q, want key-owner", run.Actor)
	}

	if run.TriggeringActor != "cchildress" {
		t.Errorf("TriggeringActor = %q, want cchildress", run.TriggeringActor)
	}

	// The three clocks must stay distinct: correlation reads CreatedAt and
	// would be wrong if any of them collapsed onto another.
	if run.CreatedAt.Equal(run.RunStartedAt) || run.RunStartedAt.Equal(run.UpdatedAt) {
		t.Errorf("timestamps collapsed: created=%v started=%v updated=%v",
			run.CreatedAt, run.RunStartedAt, run.UpdatedAt)
	}
}

// TestPreparesAsksForTheRightWorkflow keeps the correlation candidates coming
// from release-prepare. Pointed anywhere else, Attribute would silently report
// unknown for every release.
func TestPreparesAsksForTheRightWorkflow(t *testing.T) {
	t.Parallel()

	var seen string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/",
		func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			seen = parts[len(parts)-2]

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "workflow_runs": []stubRun{},
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
		})

	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(t.Context(), Options{
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.Prepares(t.Context()); err != nil {
		t.Fatalf("Prepares: %v", err)
	}

	if seen != WorkflowPrepare {
		t.Errorf("workflow = %q, want %q", seen, WorkflowPrepare)
	}
}

// TestRunFailed pins the distinction that keeps a prepare from being discarded
// on no evidence. Anywhere failure excludes a run, "did not succeed" and "did
// not say" have to be different answers.
func TestRunFailed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		run  Run
		want bool
	}{
		{name: "failure", run: Run{Status: "completed", Conclusion: "failure"}, want: true},
		{name: "stopped", run: Run{Status: "completed", Conclusion: "cancelled"}, want: true},
		{name: "timed out", run: Run{Status: "completed", Conclusion: "timed_out"}, want: true},
		{name: "success", run: Run{Status: "completed", Conclusion: "success"}, want: false},
		// Still going: it has not failed at anything yet.
		{name: "in progress", run: Run{Status: "in_progress"}, want: false},
		// Finished without saying how. An absence of evidence.
		{name: "completed with no conclusion", run: Run{Status: "completed"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.run.Failed(); got != tc.want {
				t.Errorf("Failed() = %v, want %v", got, tc.want)
			}
		})
	}
}
