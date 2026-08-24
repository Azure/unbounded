// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// Tests for the terminating conditions of a watch.
//
// Getting these wrong does not produce a wrong answer, it produces no answer:
// the loop polls every 20 seconds until a 90 minute timeout with nothing
// running, and then reports "gave up waiting". A failed soak leaves the release
// as a draft forever, so Done cannot be read from publication alone.

func TestWatchEndsOnAFailedSoak(t *testing.T) {
	t.Parallel()

	done := watchDone(
		&gh.Run{Status: "completed", Conclusion: "success"},
		[]gh.Run{{Status: "completed", Conclusion: "failure"}},
		&gh.Release{Tag: "v0.5.0", Draft: true},
	)

	if !done {
		t.Error("a failed soak leaves a draft forever; the watch must end rather than time out")
	}
}

// TestWatchKeepsGoingWhileASoakRetryIsRunning is the other half. Only the
// NEWEST soak decides: an earlier failure followed by a running retry is a
// recovery in progress, not an ending.
func TestWatchKeepsGoingWhileASoakRetryIsRunning(t *testing.T) {
	t.Parallel()

	done := watchDone(
		&gh.Run{Status: "completed", Conclusion: "success"},
		[]gh.Run{
			{Status: "completed", Conclusion: "failure"},
			{Status: "in_progress"},
		},
		&gh.Release{Tag: "v0.5.0", Draft: true},
	)

	if done {
		t.Error("ended while a retry was still running")
	}
}

func TestWatchEndsWhenPublished(t *testing.T) {
	t.Parallel()

	done := watchDone(
		&gh.Run{Status: "completed", Conclusion: "success"},
		[]gh.Run{{Status: "completed", Conclusion: "success"}},
		&gh.Release{Tag: "v0.5.0"},
	)

	if !done {
		t.Error("a published release is the end of the line")
	}
}

func TestWatchEndsOnAFailedBuild(t *testing.T) {
	t.Parallel()

	done := watchDone(
		&gh.Run{Status: "completed", Conclusion: "failure"},
		nil,
		nil,
	)

	if !done {
		t.Error("a failed build never produces a release; nothing further will happen")
	}
}

func TestWatchKeepsGoingBeforeAnythingHasRun(t *testing.T) {
	t.Parallel()

	if watchDone(nil, nil, nil) {
		t.Error("ended before the build started")
	}
}

// TestWatchVerdict covers the exit status, so `relctl watch` can be the last
// line of a script and mean something.
func TestWatchVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		result  watchResult
		wantErr string
	}{
		{
			name:   "published",
			result: watchResult{Tag: "v0.5.0", Release: "published"},
		},
		{
			name:    "still a draft",
			result:  watchResult{Tag: "v0.5.0", Release: "draft"},
			wantErr: "still a draft",
		},
		{
			name:    "no release at all",
			result:  watchResult{Tag: "v0.5.0"},
			wantErr: "no release exists",
		},
		{
			name: "failed build",
			result: watchResult{
				Tag:     "v0.5.0",
				Build:   &runSummary{State: "failure"},
				Release: "published",
			},
			wantErr: "build for v0.5.0 is failure",
		},
		{
			// A successful build carries the verdict rather than having it
			// re-derived from the state string, so a conclusion-less
			// "completed" cannot masquerade as a failure.
			name: "successful build",
			result: watchResult{
				Tag:     "v0.5.0",
				Build:   &runSummary{State: "success", Succeeded: true},
				Release: "published",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := watchVerdict(tc.result)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("watchVerdict: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("watchVerdict: want an error containing %q", tc.wantErr)
			}

			if got := err.Error(); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", got, tc.wantErr)
			}
		})
	}
}
