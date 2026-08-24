// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Tests for classify-release.sh.
//
// The script answers two questions that look similar and are not: whether a
// release soaks (provenance) and whether it is marked Latest (ordering). An
// earlier design answered both with one comparison and got the first one wrong,
// so the cases below deliberately include the shapes where the two answers
// diverge.
//
// Both answers depend on repository topology, so these build throwaway git
// repositories with a trunk and a divergent release branch. That is the only
// way to exercise the reachability half at all.

// semverBinary builds hack/cmd/semver once and returns its path. The script
// shells out to it, and `go run` on every invocation would dominate the runtime
// of this file.
var semverBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "classify-semver-")
	if err != nil {
		return "", err
	}

	bin := filepath.Join(dir, "semver")

	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/Azure/unbounded/hack/cmd/semver").CombinedOutput()
	if err != nil {
		return "", &buildError{output: string(out), err: err}
	}

	return bin, nil
})

type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.output }

// gitRepo is a throwaway repository under test.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()

	r := &gitRepo{t: t, dir: t.TempDir()}

	r.run("init", "-q", "-b", "main")
	r.run("config", "user.email", "test@example.com")
	r.run("config", "user.name", "test")
	r.commit("base")

	return r
}

func (r *gitRepo) run(args ...string) string {
	r.t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

func (r *gitRepo) commit(msg string)   { r.run("commit", "-q", "--allow-empty", "-m", msg) }
func (r *gitRepo) tag(name string)     { r.run("tag", name) }
func (r *gitRepo) checkout(ref string) { r.run("checkout", "-q", ref) }

// branchFrom creates a release branch at a ref and leaves HEAD on it.
func (r *gitRepo) branchFrom(name, ref string) { r.run("checkout", "-q", "-b", name, ref) }

// classify runs the script with HEAD wherever the caller left it, which models
// the workflow checking out the default branch before classifying.
func (r *gitRepo) classify(tag string) (fields map[string]string, output string, code int) {
	r.t.Helper()

	script, err := filepath.Abs("classify-release.sh")
	if err != nil {
		r.t.Fatalf("resolve script: %v", err)
	}

	bin, err := semverBinary()
	if err != nil {
		r.t.Fatalf("build semver: %v", err)
	}

	cmd := exec.Command("bash", script, tag) //nolint:gosec // fixed script path
	cmd.Dir = r.dir

	cmd.Env = append(os.Environ(), "SEMVER="+bin)

	raw, err := cmd.CombinedOutput()
	output = string(raw)

	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		r.t.Fatalf("run script: %v\n%s", err, output)
	}

	fields = map[string]string{}

	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && (key == "from_main" || key == "latest") {
			fields[key] = value
		}
	}

	return fields, output, code
}

func asExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}

	e, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns only this
	if ok {
		*target = e
	}

	return ok
}

func TestClassifyRelease(t *testing.T) {
	requireBash4(t)
	requireGit(t)

	cases := []struct {
		name         string
		build        func(r *gitRepo)
		tag          string
		wantFromMain string
		wantLatest   string
	}{
		{
			// The ordinary release. Soaks, and is Latest.
			name: "release cut from main",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
			},
			tag:          "v0.4.0",
			wantFromMain: "true",
			wantLatest:   "true",
		},
		{
			// The case that proves the two questions differ. A patch cut from a
			// release branch while main has not moved is the newest release, so
			// it IS Latest, but it must not soak: it did not come from main.
			name: "patch on a release branch, main not moved",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
				r.branchFrom("release-0.4", "v0.4.0")
				r.commit("cherry-pick")
				r.tag("v0.4.1")
				r.checkout("main")
			},
			tag:          "v0.4.1",
			wantFromMain: "false",
			wantLatest:   "true",
		},
		{
			name: "patch on a release branch after main moved on",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
				r.branchFrom("release-0.4", "v0.4.0")
				r.commit("cherry-pick")
				r.tag("v0.4.1")
				r.checkout("main")
				r.commit("more work")
				r.tag("v0.5.0")
			},
			tag:          "v0.4.1",
			wantFromMain: "false",
			wantLatest:   "false",
		},
		{
			// Backfill. v0.4.2 exists on the branch and is invisible from main,
			// so scoping to main alone would mark the older v0.4.1 Latest and
			// flip the marker backwards.
			name: "republishing a superseded patch on the same branch",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
				r.branchFrom("release-0.4", "v0.4.0")
				r.commit("first fix")
				r.tag("v0.4.1")
				r.commit("second fix")
				r.tag("v0.4.2")
				r.checkout("main")
			},
			tag:          "v0.4.1",
			wantFromMain: "false",
			wantLatest:   "false",
		},
		{
			name: "candidate from main",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
				r.commit("more")
				r.tag("v0.5.0-rc.1")
			},
			tag:          "v0.5.0-rc.1",
			wantFromMain: "true",
			wantLatest:   "false",
		},
		{
			// A stray final on an unmerged branch must not suppress Latest
			// forever, which is why the trunk half is reachability-scoped.
			name: "stray final on an unrelated branch is ignored",
			build: func(r *gitRepo) {
				r.commit("work")
				r.tag("v0.4.0")
				r.branchFrom("someones-experiment", "main")
				r.commit("experiment")
				r.tag("v9.0.0")
				r.checkout("main")
			},
			tag:          "v0.4.0",
			wantFromMain: "true",
			wantLatest:   "true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitRepo(t)
			tc.build(r)

			fields, output, code := r.classify(tc.tag)

			requireCode(t, code, 0, output)

			if got := fields["from_main"]; got != tc.wantFromMain {
				t.Errorf("from_main = %q, want %q\n--- output ---\n%s", got, tc.wantFromMain, output)
			}

			if got := fields["latest"]; got != tc.wantLatest {
				t.Errorf("latest = %q, want %q\n--- output ---\n%s", got, tc.wantLatest, output)
			}
		})
	}
}

// TestClassifyReleaseRefuses covers input the script must not answer for. A
// wrong answer here decides whether a cluster is touched, so an unknown tag has
// to be an error rather than a default.
func TestClassifyReleaseRefuses(t *testing.T) {
	requireBash4(t)
	requireGit(t)

	cases := []struct {
		name string
		tag  string
		want string
	}{
		{name: "no tag", tag: "", want: "no tag given"},
		{name: "not a version", tag: "nonsense", want: "not a release tag"},
		{name: "no v prefix", tag: "0.4.0", want: "not a release tag"},
		{name: "leading zeros", tag: "v01.2.3", want: "not a release tag"},
		{name: "unknown tag", tag: "v9.9.9", want: "does not exist here"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitRepo(t)
			r.commit("work")
			r.tag("v0.4.0")

			fields, output, code := r.classify(tc.tag)

			if code == 0 {
				t.Fatalf("expected a non-zero exit, got 0\n--- output ---\n%s", output)
			}

			requireContains(t, output, tc.want)

			if len(fields) != 0 {
				t.Errorf("expected no verdict on refusal, got %v", fields)
			}
		})
	}
}
