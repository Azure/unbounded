// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSplitRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		slug        string
		owner, repo string
		wantErr     bool
	}{
		{name: "ordinary", slug: "Azure/unbounded", owner: "Azure", repo: "unbounded"},
		{name: "fork", slug: "someone/unbounded", owner: "someone", repo: "unbounded"},
		{name: "no slash", slug: "unbounded", wantErr: true},
		{name: "empty owner", slug: "/unbounded", wantErr: true},
		{name: "empty name", slug: "Azure/", wantErr: true},
		{name: "empty", slug: "", wantErr: true},
		// A URL is the likeliest paste, and silently treating "https:" as the
		// owner would produce 404s that look like a permissions problem.
		{name: "url", slug: "https://github.com/Azure/unbounded", wantErr: true},
		// A slug is validated by shape, not merely by having a slash, so a
		// mistyped argument fails here rather than as a 404 that reads like a
		// permissions problem.
		{name: "space in the name", slug: "Azure/un bounded", wantErr: true},
		{name: "space in the owner", slug: "Az ure/unbounded", wantErr: true},
		{name: "trailing newline", slug: "Azure/unbounded\n", wantErr: true},
		{name: "shell metacharacter", slug: "Azure/unbounded;rm", wantErr: true},
		{name: "dots and hyphens are fine", slug: "my-org/my.repo_1", owner: "my-org", repo: "my.repo_1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			owner, repo, err := SplitRepo(tc.slug)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SplitRepo(%q) = %q/%q, want error", tc.slug, owner, repo)
				}

				return
			}

			if err != nil {
				t.Fatalf("SplitRepo(%q): %v", tc.slug, err)
			}

			if owner != tc.owner || repo != tc.repo {
				t.Fatalf("SplitRepo(%q) = %q/%q, want %q/%q", tc.slug, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}

// TestEnvOrGHPrefersTheEnvironment pins the order. A workflow always has
// GITHUB_TOKEN and may not have `gh` installed, so the environment has to win;
// reversing this would work locally and fail only in CI.
func TestEnvOrGHPrefersTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	t.Setenv("PATH", "")

	token, err := EnvOrGH(t.Context())
	if err != nil {
		t.Fatalf("EnvOrGH: %v", err)
	}

	if token != "from-env" {
		t.Fatalf("token = %q, want from-env", token)
	}
}

func TestEnvOrGHAcceptsGHToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv("PATH", "")

	token, err := EnvOrGH(t.Context())
	if err != nil {
		t.Fatalf("EnvOrGH: %v", err)
	}

	if token != "from-gh-token" {
		t.Fatalf("token = %q, want from-gh-token", token)
	}
}

// TestEnvOrGHFallsBackToGH covers the interactive path, where nothing is in the
// environment and the maintainer is simply logged into gh.
func TestEnvOrGHFallsBackToGH(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", fakeGH(t, "from-gh-cli", 0))

	token, err := EnvOrGH(t.Context())
	if err != nil {
		t.Fatalf("EnvOrGH: %v", err)
	}

	if token != "from-gh-cli" {
		t.Fatalf("token = %q, want from-gh-cli", token)
	}
}

// TestEnvOrGHReportsNoTokenWhenGHIsLoggedOut keeps the failure actionable: a
// logged-out gh exits non-zero, and the message has to name both routes rather
// than leaking gh's own advice.
func TestEnvOrGHReportsNoTokenWhenGHIsLoggedOut(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", fakeGH(t, "", 1))

	_, err := EnvOrGH(t.Context())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

func TestEnvOrGHReportsNoTokenWhenGHIsAbsent(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir())

	_, err := EnvOrGH(t.Context())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

// TestNewUsesTheSuppliedTokenSource is the seam every command relies on, so
// that nothing needs a real credential under test.
func TestNewUsesTheSuppliedTokenSource(t *testing.T) {
	t.Parallel()

	client, err := New(t.Context(), Options{
		Repo:  "someone/fork",
		Token: func(context.Context) (string, error) { return "t", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if client.Repo() != "someone/fork" {
		t.Fatalf("Repo() = %q", client.Repo())
	}

	if client.Owner() != "someone" || client.Name() != "fork" {
		t.Fatalf("Owner()/Name() = %q/%q", client.Owner(), client.Name())
	}
}

func TestNewDefaultsToTheUnboundedRepo(t *testing.T) {
	t.Parallel()

	client, err := New(t.Context(), Options{
		Token: func(context.Context) (string, error) { return "t", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if client.Repo() != DefaultRepo {
		t.Fatalf("Repo() = %q, want %q", client.Repo(), DefaultRepo)
	}
}

func TestNewPropagatesTokenFailure(t *testing.T) {
	t.Parallel()

	_, err := New(t.Context(), Options{
		Token: func(context.Context) (string, error) { return "", ErrNoToken },
	})
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

func TestNewRejectsAMalformedRepo(t *testing.T) {
	t.Parallel()

	_, err := New(t.Context(), Options{
		Repo:  "nope",
		Token: func(context.Context) (string, error) { return "t", nil },
	})
	if err == nil {
		t.Fatal("New: want error for a malformed repo")
	}
}

// fakeGH installs a stub `gh` in a temp dir and returns a PATH with that dir
// first, so the lookup finds the stub rather than any real gh on the machine.
//
// The real PATH is kept on the end rather than dropped: the stub's shebang has
// to resolve an interpreter, and a PATH containing only the temp dir makes
// every invocation fail with 127 for reasons unrelated to what is being tested.
func fakeGH(t *testing.T, token string, exit int) string {
	t.Helper()

	dir := t.TempDir()

	script := "#!/usr/bin/env bash\n"
	if token != "" {
		script += "printf '%s\\n' " + token + "\n"
	}

	script += "exit " + strconv.Itoa(exit) + "\n"

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestNewHonorsBaseURL exercises the option that exists so the command tests
// can point at an httptest server instead of api.github.com.
//
// Documented as "for tests" and previously used by none, which is how an option
// quietly stops working: nothing would have noticed if go-github changed how a
// base URL is applied, and the first symptom would have been a test suite
// silently talking to the real API.
func TestNewHonorsBaseURL(t *testing.T) {
	t.Parallel()

	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"someone"}`)
	}))
	defer server.Close()

	client, err := New(t.Context(), Options{
		Repo:    "someone/fork",
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := client.API().Users.Get(t.Context(), "someone"); err != nil {
		t.Fatalf("Users.Get against the stub: %v", err)
	}

	if gotPath != "/api/v3/users/someone" {
		t.Fatalf("stub saw %q, want the request routed to the base URL", gotPath)
	}
}
