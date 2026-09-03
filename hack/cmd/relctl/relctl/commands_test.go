// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

	// dispatched counts workflow_dispatch calls, so a test can assert that
	// nothing was sent as easily as that something was.
	dispatched int
	// lastRef and lastInputs record the most recent dispatch.
	lastRef    string
	lastInputs map[string]any

	// failPrepares makes the correlation fetch return 500 while leaving every
	// other listing alone.
	//
	// Keyed on the absence of a status filter rather than on the workflow,
	// because inFlight lists release-prepare.yaml too. Failing by workflow
	// would fail the dashboard's own request and prove nothing about
	// degrading.
	failPrepares bool

	// failRunsTimes makes the next N run listings fail with failRunsStatus,
	// after which they succeed. A watch that rides out a bad minute and one
	// that never notices look identical from the outside unless the failures
	// stop.
	failRunsTimes  int
	failRunsStatus int

	// mu guards the fields below, which the handler goroutines write while the
	// command runs.
	mu sync.Mutex
	// runRequests records every run listing, in order, as the workflow file
	// plus its status filter.
	//
	// The filter is part of the key for the same reason: an unfiltered
	// release-prepare.yaml listing is the correlation fetch, and a filtered one
	// is a row of the in-flight table. Order is the assertion, not just the
	// count - the candidate list has to be fetched AFTER the runs it explains,
	// and that is a property no amount of inspecting the output can show.
	runRequests []string
}

// requested returns the run listings served so far, in order.
func (s *stubGitHub) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.runRequests)
}

type stubRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	// UpdatedAt closes a prepare run's window for attribution. Absent on a run
	// still in progress, where there is no closing edge.
	UpdatedAt string `json:"updated_at,omitempty"`
	// Actor is an object on the wire, not a login, so the stub carries one: a
	// bare string here would pass a test the real API would fail.
	Actor   *stubUser `json:"actor,omitempty"`
	HTMLURL string    `json:"html_url"`
}

// stubUser is the shape GitHub returns for a run's actor.
type stubUser struct {
	Login string `json:"login"`
}

type stubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
	// CreatedAt dates the release. Real drafts have this and have a null
	// published_at, which is why the draft window reads created_at.
	CreatedAt string `json:"created_at,omitempty"`
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
			runStatus := r.URL.Query().Get("status")

			key := file
			if runStatus != "" {
				key = file + ":" + runStatus
			}

			s.mu.Lock()
			s.runRequests = append(s.runRequests, key)

			failing := s.failRunsTimes > 0
			if failing {
				s.failRunsTimes--
			}

			status := s.failRunsStatus
			s.mu.Unlock()

			if failing {
				w.WriteHeader(status)
				write(w, map[string]string{"message": http.StatusText(status)})

				return
			}

			if s.failPrepares && file == gh.WorkflowPrepare && runStatus == "" {
				w.WriteHeader(http.StatusInternalServerError)
				write(w, map[string]string{"message": "Server Error"})

				return
			}

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

	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/release-prepare.yaml/dispatches",
		s.recordDispatch(t))
	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/release-upgrade.yaml/dispatches",
		s.recordDispatch(t))
	mux.HandleFunc("/api/v3/repos/Azure/unbounded/actions/workflows/create-release-branch.yaml/dispatches",
		s.recordDispatch(t))

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
		"Drafts (2)",
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

// recordDispatch captures a workflow_dispatch and answers 204, as GitHub does.
func (s *stubGitHub) recordDispatch(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ref    string         `json:"ref"`
			Inputs map[string]any `json:"inputs"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode dispatch: %v", err)
		}

		s.dispatched++
		s.lastRef = body.Ref
		s.lastInputs = body.Inputs

		// 204 with no body, which is why the caller cannot learn the run ID.
		w.WriteHeader(http.StatusNoContent)
	}
}

// draftsFixture builds a stub whose drafts straddle the 30-day window and are
// deliberately out of order, in both the ways the real API is: not semver
// order, and not date order either.
func draftsFixture() *stubGitHub {
	now := time.Now()
	day := func(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }

	return &stubGitHub{
		latest: "v0.4.0",
		releases: []stubRelease{
			{TagName: "v0.4.0", CreatedAt: day(1)},
			// rc.9 before rc.8 before rc.10 is the interleaving the real
			// repository returns, and the reason sorting is not optional.
			{TagName: "v0.1.24-rc.9", Draft: true, CreatedAt: day(100)},
			{TagName: "v0.2.4-rc.1", Draft: true, CreatedAt: day(2)},
			{TagName: "v0.1.24-rc.8", Draft: true, CreatedAt: day(101)},
			{TagName: "v0.1.24-rc.10", Draft: true, CreatedAt: day(99)},
			{TagName: "v0.2.1-rc.1", Draft: true, CreatedAt: day(3)},
		},
	}
}

// draftRows returns the draft entries from status output as [tag, date] pairs.
//
// Selected by the tag starting with "v" rather than by position, so the
// COMMITTED header and the "N older" summary are excluded structurally. An
// earlier version took every indented line and happened to work only because
// no test produced a header and a summary at once.
func draftRows(out string) [][]string {
	var (
		rows [][]string
		in   bool
	)

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Drafts") {
			in = true

			continue
		}

		if !in {
			continue
		}

		if !strings.HasPrefix(line, "  ") {
			break
		}

		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "v") {
			continue
		}

		rows = append(rows, fields)
	}

	return rows
}

// draftTags returns just the tags, in the order printed.
func draftTags(out string) []string {
	rows := draftRows(out)

	tags := make([]string, 0, len(rows))
	for _, row := range rows {
		tags = append(tags, row[0])
	}

	return tags
}

// TestStatusListsDraftsOnePerLine is the regression this exists to prevent:
// twenty-four tags joined into one wrapped cell.
func TestStatusListsDraftsOnePerLine(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, draftsFixture(), tagRepo(t), "status", "--all")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	// Five drafts must occupy five lines. That is the property the original
	// bug violated: twenty-four tags joined into one wrapped cell.
	rows := draftRows(out)
	if len(rows) != 5 {
		t.Fatalf("got %d draft lines, want 5:\n%s", len(rows), out)
	}

	for _, row := range rows {
		// One tag per line. A second tag in the same row would mean the
		// join is back, and the date column must not be mistaken for one.
		for _, field := range row[1:] {
			if strings.HasPrefix(field, "v") && strings.Contains(field, ".") {
				t.Errorf("draft line carries more than one tag: %v", row)
			}
		}
	}
}

// TestStatusSortsDraftsBySemver pins the order against the API's, including the
// numeric prerelease comparison that puts rc.10 above rc.9.
func TestStatusSortsDraftsBySemver(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, draftsFixture(), tagRepo(t), "status", "--all")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	want := []string{
		"v0.2.4-rc.1",
		"v0.2.1-rc.1",
		"v0.1.24-rc.10",
		"v0.1.24-rc.9",
		"v0.1.24-rc.8",
	}

	if got := draftTags(out); !slices.Equal(got, want) {
		t.Errorf("draft order:\n got %v\nwant %v", got, want)
	}
}

// TestStatusWindowsOldDraftsButKeepsTheCount is the property that makes the
// window safe: a backlog that stops being counted is a backlog nobody deals
// with, so the header stays honest even when the list is short.
func TestStatusWindowsOldDraftsButKeepsTheCount(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, draftsFixture(), tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Drafts (5)") {
		t.Errorf("header does not report all 5 drafts:\n%s", out)
	}

	if strings.Contains(out, "v0.1.24-rc.9") {
		t.Errorf("a draft outside the window was listed:\n%s", out)
	}

	// The summary names the oldest and says how to see the rest, so the pile
	// is visible as a pile.
	for _, want := range []string{"3 older", "back to v0.1.24-rc.8", "--all"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// TestStatusOmitsTheSummaryWhenNothingIsHidden keeps the line from appearing
// with a count of zero on a repository that is not behind.
func TestStatusOmitsTheSummaryWhenNothingIsHidden(t *testing.T) {
	t.Parallel()

	now := time.Now().AddDate(0, 0, -1).Format(time.RFC3339)
	stub := &stubGitHub{
		latest: "v0.4.0",
		releases: []stubRelease{
			{TagName: "v0.2.4-rc.1", Draft: true, CreatedAt: now},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if strings.Contains(out, "older") {
		t.Errorf("summary line printed with nothing hidden:\n%s", out)
	}
}

// TestStatusShowsUndatedDrafts covers a draft the API gave no date. That is our
// gap, not evidence the draft is old, so it must not be what hides it.
func TestStatusShowsUndatedDrafts(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest:   "v0.4.0",
		releases: []stubRelease{{TagName: "v0.2.4-rc.1", Draft: true}},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !slices.Contains(draftTags(out), "v0.2.4-rc.1") {
		t.Errorf("an undated draft was hidden by the window:\n%s", out)
	}
}

// TestStatusJSONIsNeverWindowed keeps the display window out of the machine
// output. A script asking for drafts wants all of them.
func TestStatusJSONIsNeverWindowed(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"status", "-o", "json"},
		{"status", "--all", "-o", "json"},
	} {
		out, err := runCommand(t, draftsFixture(), tagRepo(t), args...)
		if err != nil {
			t.Fatalf("status %v: %v\n%s", args, err, out)
		}

		var decoded statusResult
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}

		if len(decoded.Drafts) != 5 {
			t.Errorf("status %v returned %d drafts, want all 5", args, len(decoded.Drafts))
		}

		for _, draft := range decoded.Drafts {
			if draft.Committed == nil {
				t.Errorf("status %v: %s carries no date", args, draft.Tag)
			}
		}
	}
}

// TestStatusDatesEachDraft is the column itself: the date sits with its tag,
// not on a line of its own and not against the wrong tag.
func TestStatusDatesEachDraft(t *testing.T) {
	t.Parallel()

	now := time.Now()
	stub := &stubGitHub{
		latest: "v0.4.0",
		releases: []stubRelease{
			{
				TagName: "v0.2.4-rc.1", Draft: true,
				CreatedAt: now.AddDate(0, 0, -2).Format(time.RFC3339),
			},
			{
				TagName: "v0.2.1-rc.1", Draft: true,
				CreatedAt: now.AddDate(0, 0, -9).Format(time.RFC3339),
			},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "COMMITTED") {
		t.Errorf("no column heading, so the date reads as a drafted-on date:\n%s", out)
	}

	want := map[string]string{
		"v0.2.4-rc.1": now.AddDate(0, 0, -2).UTC().Format("2006-01-02"),
		"v0.2.1-rc.1": now.AddDate(0, 0, -9).UTC().Format("2006-01-02"),
	}

	for _, row := range draftRows(out) {
		if len(row) != 2 {
			t.Errorf("row %v is not tag plus date", row)

			continue
		}

		if got := row[1]; got != want[row[0]] {
			t.Errorf("%s dated %s, want %s", row[0], got, want[row[0]])
		}
	}
}

// TestStatusDatesSurviveAll keeps the column from being a property of the
// short list only.
func TestStatusDatesSurviveAll(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, draftsFixture(), tagRepo(t), "status", "--all")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	for _, row := range draftRows(out) {
		if len(row) != 2 {
			t.Errorf("row %v lost its date under --all", row)
		}
	}
}

// TestStatusLeavesUndatedDraftsBlank is the zero-time trap on a visible
// surface: 0001-01-01 reads as a real answer, and an empty cell does not.
func TestStatusLeavesUndatedDraftsBlank(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest:   "v0.4.0",
		releases: []stubRelease{{TagName: "v0.2.4-rc.1", Draft: true}},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if strings.Contains(out, "0001-01-01") {
		t.Errorf("undated draft rendered a zero date:\n%s", out)
	}

	rows := draftRows(out)
	if len(rows) != 1 {
		t.Fatalf("got %d draft rows, want 1:\n%s", len(rows), out)
	}

	if len(rows[0]) != 1 {
		t.Errorf("undated draft carries a date: %v", rows[0])
	}
}

// TestStatusOmitsAbsentDatesFromJSON is the same fact in the machine output. A
// null-ish zero timestamp would be indistinguishable from a real one.
func TestStatusOmitsAbsentDatesFromJSON(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest:   "v0.4.0",
		releases: []stubRelease{{TagName: "v0.2.4-rc.1", Draft: true}},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if strings.Contains(out, "0001-01-01") {
		t.Errorf("JSON carries a zero timestamp:\n%s", out)
	}

	if strings.Contains(out, "committed") {
		t.Errorf("JSON carries a committed key for a draft with no date:\n%s", out)
	}
}

// The attribution tests.
//
// A release.yaml run is a tag push made by release-prepare over an SSH deploy
// key, and GitHub credits a deploy-key push to whoever registered the key. So
// the actor on every release build is the same person regardless of who cut it.
// These check that relctl says who actually cut it, and says nothing at all
// rather than repeating the actor when it cannot tell.

// deployKeyOwner stands in for whoever registered RELEASE_TAG_DEPLOY_KEY.
const deployKeyOwner = "key-owner"

func user(login string) *stubUser { return &stubUser{Login: login} }

// TestWatchNamesTheCutterNotTheDeployKeyOwner is the whole point.
func TestWatchNamesTheCutterNotTheDeployKeyOwner(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(125 * time.Minute), UpdatedAt: ago(120 * time.Minute),
				Actor: user("cchildress"), HTMLURL: "https://example.invalid/prepare/9",
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(122 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	for _, want := range []string{"Cut by:", "cchildress", "https://example.invalid/prepare/9"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("watch named the deploy key owner:\n%s", out)
	}
}

// TestWatchSaysWhenATagWasPushedByHand keeps the two cases apart. A tag
// release-prepare did not push skipped every guard that workflow applies, which
// is worth seeing rather than smoothing over.
func TestWatchSaysWhenATagWasPushedByHand(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			// Old enough to prove the candidate list reaches back past the
			// push, so finding no match means something.
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(600 * time.Minute), UpdatedAt: ago(595 * time.Minute),
				Actor: user("cchildress"),
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(120 * time.Minute), Actor: user("bcho"),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	for _, want := range []string{"Pushed by:", "bcho", "not by release-prepare"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "cchildress") {
		t.Errorf("watch attributed a hand-pushed tag to an unrelated prepare:\n%s", out)
	}
}

// TestWatchSaysUnknownRatherThanTheDeployKeyOwner covers the outcome that
// would be worse than saying nothing.
func TestWatchSaysUnknownRatherThanTheDeployKeyOwner(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(120 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	if !strings.Contains(out, "unknown") {
		t.Errorf("watch did not report an unknown cutter:\n%s", out)
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("watch named the deploy key owner:\n%s", out)
	}
}

// TestStatusNamesTheCutterInFlight covers the dashboard, which is where anyone
// looks while a release is actually running.
func TestStatusNamesTheCutterInFlight(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest: "v0.4.0",
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(5 * time.Minute), UpdatedAt: ago(4 * time.Minute),
				Actor: user("cchildress"), HTMLURL: "https://example.invalid/prepare/9",
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "in_progress", CreatedAt: ago(3 * time.Minute),
				Actor: user(deployKeyOwner),
			}},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	for _, want := range []string{"In flight:", "BY", "cchildress", "v0.5.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("status named the deploy key owner:\n%s", out)
	}
}

// TestStatusMarksAnUnattributableRun checks the column degrades to "?" rather
// than to the actor, and that a soak is treated as unattributable: it fires on
// workflow_run and inherits the build's actor, which is inherited in turn from
// the deploy key.
func TestStatusMarksAnUnattributableRun(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest: "v0.4.0",
		runs: map[string][]stubRun{
			"release-upgrade.yaml": {{
				ID: 2, HeadBranch: "main", HeadSHA: "abc", Event: "workflow_run",
				Status: "in_progress", CreatedAt: ago(time.Minute),
				Actor: user(deployKeyOwner),
			}},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "?") {
		t.Errorf("status did not mark the run unattributable:\n%s", out)
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("status named the deploy key owner:\n%s", out)
	}
}

// TestStatusJSONCarriesBothActorAndCutter keeps the raw value available. A
// consumer reconciling relctl against the Actions UI needs to see the field the
// UI shows as well as the answer relctl derived from it.
func TestStatusJSONCarriesBothActorAndCutter(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest: "v0.4.0",
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(5 * time.Minute), UpdatedAt: ago(4 * time.Minute),
				Actor: user("cchildress"), HTMLURL: "https://example.invalid/prepare/9",
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "in_progress", CreatedAt: ago(3 * time.Minute),
				Actor: user(deployKeyOwner),
			}},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	var result statusResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	if len(result.InFlight) != 1 {
		t.Fatalf("got %d in-flight runs, want 1:\n%s", len(result.InFlight), out)
	}

	run := result.InFlight[0]

	if run.Actor != deployKeyOwner {
		t.Errorf("actor = %q, want the raw value %q", run.Actor, deployKeyOwner)
	}

	if run.By == nil {
		t.Fatalf("by is absent:\n%s", out)
	}

	if run.By.By != "cchildress" || run.By.Source != gh.SourceDispatch {
		t.Errorf("by = %+v, want cchildress via %s", *run.By, gh.SourceDispatch)
	}

	if run.By.RunURL != "https://example.invalid/prepare/9" {
		t.Errorf("by.runUrl = %q, want the prepare run", run.By.RunURL)
	}
}

// TestWatchDefersTheCandidateFetchUntilThereIsABuild pins the fix for a wrong
// answer, not merely a wasted request.
//
// The candidate list used to be fetched before the poll loop. `watch <tag>` is
// routinely started before the tag exists, so that list could not contain the
// prepare run about to push it - and Attribute reads a non-match as "pushed by
// hand" and reports the run's actor, which on a tag push is the deploy key's
// owner. Deferring the fetch to the first sighting of a build removes the race:
// the prepare pushed the tag, so it precedes the run the push created.
func TestWatchDefersTheCandidateFetchUntilThereIsABuild(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(125 * time.Minute), UpdatedAt: ago(120 * time.Minute),
				Actor: user("cchildress"),
			}},
		},
	}

	// --once on an unbuilt tag reports and exits zero; the assertion here is
	// about what was requested, not about the verdict.
	out, err := runCommand(t, stub, "", "watch", "v0.9.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	if !strings.Contains(out, "not started") {
		t.Errorf("watch did not report an unbuilt tag:\n%s", out)
	}

	if slices.Contains(stub.requested(), "release-prepare.yaml") {
		t.Errorf("watch fetched correlation candidates with no build to attribute: %v",
			stub.requested())
	}
}

// TestWatchFetchesCandidatesAfterTheBuild is the other half: once there is a
// build, the list is fetched, and fetched after the run it explains.
func TestWatchFetchesCandidatesAfterTheBuild(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(125 * time.Minute), UpdatedAt: ago(120 * time.Minute),
				Actor: user("cchildress"), HTMLURL: "https://example.invalid/prepare/9",
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(122 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	requested := stub.requested()

	build := slices.Index(requested, "release.yaml")
	prepare := slices.Index(requested, "release-prepare.yaml")

	if build < 0 || prepare < 0 {
		t.Fatalf("expected both listings, got %v", requested)
	}

	if prepare < build {
		t.Errorf("candidates fetched before the build they explain: %v", requested)
	}

	if !strings.Contains(out, "cchildress") {
		t.Errorf("watch did not name the cutter:\n%s", out)
	}
}

// TestStatusSkipsTheCandidateFetchWhenNothingIsInFlight covers the ordinary
// answer for a dashboard people re-run. There is nobody to attribute, so the
// request is not worth making.
func TestStatusSkipsTheCandidateFetchWhenNothingIsInFlight(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{latest: "v0.4.0"}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Nothing in flight.") {
		t.Errorf("status did not report an empty table:\n%s", out)
	}

	if slices.Contains(stub.requested(), "release-prepare.yaml") {
		t.Errorf("status fetched correlation candidates with nothing to attribute: %v",
			stub.requested())
	}
}

// TestStatusFetchesCandidatesAfterTheRuns keeps the ordering that makes a
// non-match mean anything. Fetching first is the shape of the bug.
func TestStatusFetchesCandidatesAfterTheRuns(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest: "v0.4.0",
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "in_progress", CreatedAt: ago(3 * time.Minute),
				Actor: user(deployKeyOwner),
			}},
		},
	}

	if _, err := runCommand(t, stub, tagRepo(t), "status"); err != nil {
		t.Fatalf("status: %v", err)
	}

	requested := stub.requested()

	prepare := slices.Index(requested, "release-prepare.yaml")
	if prepare < 0 {
		t.Fatalf("candidates never fetched despite a run to attribute: %v", requested)
	}

	// Every listing the table is built from precedes the candidate fetch, not
	// merely the one that happened to match.
	if prepare != len(requested)-1 {
		t.Errorf("candidates fetched before a run listing: %v", requested)
	}
}

// TestWatchSurvivesAFailedCandidateFetch keeps a cosmetic column from costing
// the state the command exists to report.
func TestWatchSurvivesAFailedCandidateFetch(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		failPrepares: true,
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(122 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err != nil {
		t.Fatalf("a failed candidate fetch killed the watch: %v\n%s", err, out)
	}

	// The state still arrives.
	if !strings.Contains(out, "success") {
		t.Errorf("watch lost the build state:\n%s", out)
	}

	// And the unknown says which kind of unknown it is.
	if !strings.Contains(out, "could not list release-prepare runs") {
		t.Errorf("watch blamed the tag for a failed request:\n%s", out)
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("watch named the deploy key owner:\n%s", out)
	}
}

// TestStatusSurvivesAFailedCandidateFetch is the same for the dashboard, where
// a table of "?" must not read as four runs nobody could be found for.
func TestStatusSurvivesAFailedCandidateFetch(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		latest:       "v0.4.0",
		failPrepares: true,
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "in_progress", CreatedAt: ago(3 * time.Minute),
				Actor: user(deployKeyOwner),
			}},
		},
	}

	out, err := runCommand(t, stub, tagRepo(t), "status")
	if err != nil {
		t.Fatalf("a failed candidate fetch killed the dashboard: %v\n%s", err, out)
	}

	for _, want := range []string{"v0.5.0", "?", "BY is UNKNOWN for every run"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, deployKeyOwner) {
		t.Errorf("status named the deploy key owner:\n%s", out)
	}
}

// The retry tests.
//
// A watch runs for ninety minutes and makes on the order of a thousand
// requests. Until now any one of them coming back wrong ended it, which made
// the command least reliable exactly when a release was taking a long time -
// the case it exists for.

// TestWatchRidesOutATransientFailure is the point: a bad minute costs a poll,
// not the watch.
func TestWatchRidesOutATransientFailure(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		failRunsTimes:  2,
		failRunsStatus: http.StatusBadGateway,
		runs: map[string][]stubRun{
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(122 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0", Draft: false}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0",
		"--interval", "1ms", "--timeout", "30s")
	if err != nil {
		t.Fatalf("watch gave up on a transient failure: %v\n%s", err, out)
	}

	if !strings.Contains(out, "retrying in 1ms") {
		t.Errorf("watch retried without saying so:\n%s", out)
	}

	if !strings.Contains(out, "published") {
		t.Errorf("watch did not reach the published release:\n%s", out)
	}
}

// TestWatchDoesNotRetryAPermanentFailure keeps the other half honest. A 404
// will say the same thing in ninety minutes, and retrying it turns a clear
// error into a timeout.
func TestWatchDoesNotRetryAPermanentFailure(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		// More failures than the run could possibly need, so a retry loop
		// would be visible as extra requests rather than as a hang.
		failRunsTimes:  100,
		failRunsStatus: http.StatusNotFound,
	}

	// Short, because the failure mode being guarded against is a retry loop:
	// if this ever regresses it should fail in seconds rather than make CI
	// wait out a realistic timeout.
	out, err := runCommand(t, stub, "", "watch", "v0.5.0",
		"--interval", "1ms", "--timeout", "5s")
	if err == nil {
		t.Fatalf("watch treated a 404 as transient:\n%s", out)
	}

	if strings.Contains(out, "retrying") {
		t.Errorf("watch retried a permanent failure:\n%s", out)
	}

	// One request, not a loop of them.
	if got := len(stub.requested()); got != 1 {
		t.Errorf("made %d requests for a permanent failure, want 1: %v", got, stub.requested())
	}
}

// TestWatchOnceDoesNotRetry holds the single-shot contract. --once is
// documented to report current state and exit, so a caller who asked one
// question gets one answer, never a wait.
func TestWatchOnceDoesNotRetry(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		failRunsTimes:  1,
		failRunsStatus: http.StatusBadGateway,
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once")
	if err == nil {
		t.Fatalf("watch --once swallowed a failure:\n%s", out)
	}

	if strings.Contains(out, "retrying") {
		t.Errorf("watch --once retried:\n%s", out)
	}
}

// TestWatchNamesTheCauseWhenItGivesUp keeps a watch that spent its timeout
// being told 502 distinguishable from one that spent it waiting for a release
// that never came. Both used to say only "gave up waiting".
func TestWatchNamesTheCauseWhenItGivesUp(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		failRunsTimes:  100,
		failRunsStatus: http.StatusBadGateway,
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0",
		"--interval", "1ms", "--timeout", "20ms")
	if err == nil {
		t.Fatalf("watch should have given up:\n%s", out)
	}

	if !strings.Contains(err.Error(), "gave up waiting") {
		t.Errorf("error does not read as a timeout: %v", err)
	}

	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// TestWatchJSONCarriesTheCutterOnceMakes the shape explicit rather than
// incidental.
//
// Who cut a release is a fact about the TAG. The build, the soak and any retry
// all carry the same distorted actor, so emitting the derived answer against
// each of them would repeat one finding three times and read as three
// observations. It belongs on the result, once, and the raw actor stays beside
// each run for anyone reconciling against the Actions UI.
func TestWatchJSONCarriesTheCutterOnce(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs: map[string][]stubRun{
			"release-prepare.yaml": {{
				ID: 9, HeadBranch: "main", Event: "workflow_dispatch",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(125 * time.Minute), UpdatedAt: ago(120 * time.Minute),
				Actor: user("cchildress"), HTMLURL: "https://example.invalid/prepare/9",
			}},
			"release.yaml": {{
				ID: 1, HeadBranch: "v0.5.0", HeadSHA: "abc", Event: "push",
				Status: "completed", Conclusion: "success",
				CreatedAt: ago(122 * time.Minute), Actor: user(deployKeyOwner),
			}},
		},
		releaseByTag: map[string]stubRelease{"v0.5.0": {TagName: "v0.5.0"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0", "--once", "-o", "json")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	var result watchResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	if result.By == nil || result.By.By != "cchildress" {
		t.Fatalf("by is absent from the result:\n%s", out)
	}

	if result.Build == nil {
		t.Fatalf("build is absent:\n%s", out)
	}

	if result.Build.By != nil {
		t.Errorf("by is repeated under build: %+v\n%s", *result.Build.By, out)
	}

	// The raw value stays, because it is what the Actions UI shows.
	if result.Build.Actor != deployKeyOwner {
		t.Errorf("build.actor = %q, want the raw value %q", result.Build.Actor, deployKeyOwner)
	}
}
