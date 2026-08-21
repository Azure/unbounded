// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestIsMaintenance covers the decision the release soak depends on.
func TestIsMaintenance(t *testing.T) {
	cases := []struct {
		name    string
		tag     string
		tags    []string
		want    string
		wantErr bool
	}{
		{
			// The case sort -V gets backwards, and the reason this command
			// exists. Dispatching an old candidate by hand after its final has
			// shipped must not redeploy it to the soak cluster.
			name: "candidate below its own final",
			tag:  "v1.0.0-rc.1",
			tags: []string{"v1.0.0-rc.1", "v1.0.0"},
			want: "true",
		},
		{
			// Re-running the soak for the current release is supported, so
			// equality must not be treated as maintenance.
			name: "the current release itself",
			tag:  "v1.7.0",
			tags: []string{"v1.6.0", "v1.7.0"},
			want: "false",
		},
		{
			name: "patch on an older series",
			tag:  "v1.4.1",
			tags: []string{"v1.4.0", "v1.7.0"},
			want: "true",
		},
		{
			name: "candidate for a newer series",
			tag:  "v1.8.0-rc.1",
			tags: []string{"v1.7.0"},
			want: "false",
		},
		{
			name: "patch on the newest series",
			tag:  "v1.7.1",
			tags: []string{"v1.6.0", "v1.7.0"},
			want: "false",
		},
		{
			// A train in flight must not suppress the soak for everything
			// below it, so only finals set the bar.
			name: "unreleased candidate does not raise the bar",
			tag:  "v1.7.0",
			tags: []string{"v1.7.0", "v1.8.0-rc.1"},
			want: "false",
		},
		{
			// This repository's tag namespace contains real debris.
			name: "malformed tags are skipped",
			tag:  "v0.2.5",
			tags: []string{"v0.1.23-alpha.0", "not-a-tag", "v9", "v0.3.0", ""},
			want: "true",
		},
		{
			// "v9" is valid to x/mod/semver as shorthand for v9.0.0. If it were
			// accepted it would outrank every real release and mark everything
			// as maintenance, silently disabling the soak.
			name: "shorthand versions do not outrank real releases",
			tag:  "v0.3.0",
			tags: []string{"v9", "v0.2.0"},
			want: "false",
		},
		{
			name: "no finals yet",
			tag:  "v0.1.0-rc.1",
			tags: []string{"v0.1.0-rc.1"},
			want: "false",
		},
		{
			// An empty stream means the caller's git command produced nothing.
			// Answering "false" would deploy on the strength of a failed
			// command.
			name:    "empty input is an error, not an answer",
			tag:     "v1.0.0",
			tags:    nil,
			wantErr: true,
		},
		{
			name:    "invalid tag argument",
			tag:     "1.0.0",
			tags:    []string{"v1.0.0"},
			wantErr: true,
		},
		{
			name:    "shorthand tag argument is rejected",
			tag:     "v1.0",
			tags:    []string{"v1.0.0"},
			wantErr: true,
		},
		{
			name:    "build metadata is rejected",
			tag:     "v1.0.0+build.1",
			tags:    []string{"v1.0.0"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			stdin := strings.NewReader(strings.Join(tc.tags, "\n"))
			err := run([]string{"is-maintenance", tc.tag}, stdin, &stdout, &stderr)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got output %q", stdout.String())
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v (stderr: %s)", err, stderr.String())
			}

			if got := strings.TrimSpace(stdout.String()); got != tc.want {
				t.Errorf("got %q, want %q (stderr: %s)", got, tc.want, stderr.String())
			}
		})
	}
}

// TestCompare checks the general primitive, including the precedence rule that
// sort -V violates.
func TestCompare(t *testing.T) {
	cases := []struct {
		name    string
		a, b    string
		want    string
		wantErr bool
	}{
		{name: "prerelease is lower than its final", a: "v1.0.0-rc.1", b: "v1.0.0", want: "-1"},
		{name: "final is higher than its prerelease", a: "v1.0.0", b: "v1.0.0-rc.1", want: "1"},
		{name: "equal", a: "v1.2.3", b: "v1.2.3", want: "0"},
		{name: "minor ordering is numeric not lexical", a: "v1.10.0", b: "v1.9.0", want: "1"},
		{name: "rc ordering is numeric not lexical", a: "v1.0.0-rc.10", b: "v1.0.0-rc.9", want: "1"},
		{name: "invalid left", a: "nope", b: "v1.0.0", wantErr: true},
		{name: "invalid right", a: "v1.0.0", b: "nope", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			err := run([]string{"compare", tc.a, tc.b}, strings.NewReader(""), &stdout, &stderr)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", stdout.String())
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := strings.TrimSpace(stdout.String()); got != tc.want {
				t.Errorf("compare(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestUsageErrors covers invocation mistakes, which must be distinguishable
// from a legitimate "false".
func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"unknown"},
		{"compare"},
		{"compare", "v1.0.0"},
		{"compare", "v1.0.0", "v1.0.1", "v1.0.2"},
		{"is-maintenance"},
		{"is-maintenance", "v1.0.0", "extra"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if err := run(args, strings.NewReader("v1.0.0"), &stdout, &stderr); err == nil {
				t.Fatalf("expected an error for args %q", args)
			}
		})
	}
}
