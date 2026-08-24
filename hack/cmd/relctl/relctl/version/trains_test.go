// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"strings"
	"testing"
)

// Tests for the live/stale train model.
//
// Written by hand rather than bolted onto the ported fixtures, because this is
// the part the rest of the CLI is built on: status shows it, prerelease decides
// whether to start or continue from it, and promote refuses to guess between
// entries. It is also the exact thing the shell got wrong. v0.1.24 reached
// rc.18 and was orphaned when a v0.2.0 train started beside it, because promote
// finalised the highest prerelease in the whole repository rather than the one
// being worked on. There is still no v0.1.24 tag.
//
// A LIVE train is a core with candidates, no final of its own, AND newer than
// the latest final. That last clause is what makes an abandoned train invisible
// rather than merely unlucky.

func TestResolveReportsTrains(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		tags        []string
		wantLatest  string
		wantLive    []string
		wantStale   []string
		wantReport  []string
		wantWarning string
	}{
		{
			name:       "no tags at all",
			tags:       nil,
			wantLatest: "v0.0.0",
			wantReport: []string{"Latest final: v0.0.0", "Live trains:  (none)"},
		},
		{
			name:       "a final with no candidates",
			tags:       []string{"v0.2.4"},
			wantLatest: "v0.2.4",
			wantReport: []string{"Latest final: v0.2.4", "Live trains:  (none)"},
		},
		{
			name:       "candidates ahead of the latest final are live",
			tags:       []string{"v0.2.4", "v0.2.5-rc.1"},
			wantLatest: "v0.2.4",
			wantLive:   []string{"v0.2.5"},
		},
		{
			// The v0.1.24 shape. Candidates exist, no final was ever cut, and a
			// newer series has since shipped. Reporting this as live is what
			// let promote finalise it months later.
			name:       "candidates behind the latest final are stale",
			tags:       []string{"v0.2.4", "v0.1.24-rc.1", "v0.1.24-rc.2"},
			wantLatest: "v0.2.4",
			wantStale:  []string{"v0.1.24"},
			wantReport: []string{"Stale trains: v0.1.24"},
		},
		{
			name:       "live and stale at once",
			tags:       []string{"v0.2.4", "v0.1.24-rc.1", "v0.3.0-rc.1"},
			wantLatest: "v0.2.4",
			wantLive:   []string{"v0.3.0"},
			wantStale:  []string{"v0.1.24"},
		},
		{
			name:       "two live trains are both reported",
			tags:       []string{"v0.2.4", "v0.2.5-rc.1", "v0.3.0-rc.1"},
			wantLatest: "v0.2.4",
			wantLive:   []string{"v0.2.5", "v0.3.0"},
		},
		{
			// A core that shipped is no longer in flight, however many
			// candidates it collected on the way.
			name:       "a promoted core is neither live nor stale",
			tags:       []string{"v0.2.4", "v0.2.5-rc.1", "v0.2.5"},
			wantLatest: "v0.2.5",
		},
		{
			// Discovery is reachability-scoped, so a candidate cut on someone
			// else's branch starts no train here.
			name:       "off-branch candidates start no train",
			tags:       []string{"v0.2.4", "v0.2.5-rc.1@off"},
			wantLatest: "v0.2.4",
		},
		{
			// Malformed metadata is not a train. Each of these is a shape that
			// has appeared in the repository at some point.
			name:       "malformed candidates start no train",
			tags:       []string{"v0.2.4", "v0.2.5-rc.0", "v0.2.5-rc.01", "v0.2.5-alpha.1", "v0.2.5-rc"},
			wantLatest: "v0.2.4",
		},
		{
			name:        "cutting past a live train warns that it is stranded",
			tags:        []string{"v0.2.4", "v0.3.0-rc.1"},
			wantLatest:  "v0.2.4",
			wantLive:    []string{"v0.3.0"},
			wantWarning: "that train will be stranded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// mode=release with a patch bump is the least opinionated probe:
			// it reads the whole state and never starts or continues a train.
			result, err := Resolve(newFakeRepo(tc.tags), Request{
				Mode: ModeRelease,
				Bump: BumpPatch,
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if result.LatestFinal != tc.wantLatest {
				t.Errorf("LatestFinal = %q, want %q", result.LatestFinal, tc.wantLatest)
			}

			assertSlice(t, "Live", result.Live, tc.wantLive)
			assertSlice(t, "Stale", result.Stale, tc.wantStale)

			report := strings.Join(result.Report, "\n")
			for _, want := range tc.wantReport {
				if !strings.Contains(report, want) {
					t.Errorf("report missing %q\n--- report ---\n%s", want, report)
				}
			}

			if tc.wantWarning != "" {
				warnings := strings.Join(result.Warnings, "\n")
				if !strings.Contains(warnings, tc.wantWarning) {
					t.Errorf("warnings missing %q, got: %v", tc.wantWarning, result.Warnings)
				}
			}
		})
	}
}

// TestPrereleaseContinuesTheLiveTrain pins the behaviour the train model exists
// to drive: a second candidate joins the train in flight rather than starting
// another one, and the bump that would have started a different train is
// ignored rather than obeyed.
func TestPrereleaseContinuesTheLiveTrain(t *testing.T) {
	t.Parallel()

	result, err := Resolve(newFakeRepo([]string{"v0.2.4", "v0.2.5-rc.1"}), Request{
		Mode: ModePrerelease,
		Bump: BumpMinor,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if result.Tag != "v0.2.5-rc.2" {
		t.Errorf("tag = %q, want v0.2.5-rc.2 (bump=minor must not start v0.3.0)", result.Tag)
	}

	if report := strings.Join(result.Report, "\n"); !strings.Contains(report, "Continuing live train v0.2.5") {
		t.Errorf("report does not say it continued the train:\n%s", report)
	}
}

func assertSlice(t *testing.T, name string, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)

		return
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)

			return
		}
	}
}
