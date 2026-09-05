// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"testing"
	"time"
)

// tick is a time on the day of the release this was written against.
func tick(hour, minute int) time.Time {
	return time.Date(2026, 9, 2, hour, minute, 0, 0, time.UTC)
}

// deployKeyOwner stands in for whoever registered RELEASE_TAG_DEPLOY_KEY.
//
// Named for what it is rather than for a person, because the whole point is
// that this login appears on every release build and means nothing about who
// cut that release.
const deployKeyOwner = "key-owner"

// prepareRun builds a finished release-prepare run dispatched by someone.
func prepareRun(actor string, from, until time.Time) Run {
	return Run{
		ID:         1,
		Workflow:   WorkflowPrepare,
		Status:     "completed",
		Conclusion: "success",
		Event:      "workflow_dispatch",
		HeadBranch: "main",
		CreatedAt:  from,
		UpdatedAt:  until,
		Actor:      actor,
		URL:        "https://github.com/Azure/unbounded/actions/runs/1",
	}
}

// buildRun builds a release.yaml run created by a tag push.
func buildRun(actor string, at time.Time) Run {
	return Run{
		ID:         2,
		Workflow:   WorkflowRelease,
		Status:     "completed",
		Conclusion: "success",
		Event:      "push",
		HeadBranch: "v0.5.0",
		CreatedAt:  at,
		// A push run's actor and triggering_actor agree, and both are the
		// deploy key's owner. Neither names the cutter.
		Actor:           actor,
		TriggeringActor: actor,
		URL:             "https://github.com/Azure/unbounded/actions/runs/2",
	}
}

func TestAttribute(t *testing.T) {
	t.Parallel()

	const prepareURL = "https://github.com/Azure/unbounded/actions/runs/1"

	cases := []struct {
		name     string
		build    Run
		prepares []Run
		want     Attribution
	}{
		{
			// The case this exists for. GitHub says the deploy key's owner;
			// the answer is whoever dispatched the prepare that pushed the tag.
			name:     "names the cutter rather than the deploy key owner",
			build:    buildRun(deployKeyOwner, tick(16, 33)),
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
		{
			// A prepare that is still going has not necessarily pushed yet, and
			// its updated_at is only the last thing that changed. Closing the
			// window there would exclude the push it is in the middle of making.
			name:  "a prepare still in progress has no closing edge",
			build: buildRun(deployKeyOwner, tick(16, 33)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "in_progress", Event: "workflow_dispatch",
				CreatedAt: tick(16, 32), UpdatedAt: tick(16, 32), Actor: "cchildress",
				URL: prepareURL,
			}},
			want: Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
		{
			// The push happens a couple of steps before the job ends, so this
			// ordering is the unusual one: a queued build whose created_at
			// lands just after the prepare closed.
			name:     "a push just after the prepare finished is still its own",
			build:    buildRun(deployKeyOwner, tick(16, 35)),
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
		{
			// Hand-pushed tags are real: v0.2.3-alpha.0 through alpha.2 have no
			// prepare run at all. There the actor IS the person who pushed.
			name:     "a hand-pushed tag names whoever pushed it",
			build:    buildRun("bcho", tick(23, 30)),
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{By: "bcho", Source: SourcePush},
		},
		{
			// Same shape as above, but the candidate list starts AFTER the
			// build, so finding no match says nothing at all.
			name:     "a build older than every candidate is unknown",
			build:    buildRun(deployKeyOwner, tick(9, 0)),
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{Source: SourceUnknown},
		},
		{
			// A prepare that failed did not get as far as its push - that is
			// the last thing the job does - so it cannot be what created this
			// tag. Somebody pushed it by hand while a prepare was failing, and
			// the actor is that person.
			name:  "a failed prepare is not a candidate",
			build: buildRun("bcho", tick(16, 33)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "completed", Conclusion: "failure",
				Event: "workflow_dispatch", CreatedAt: tick(16, 32), UpdatedAt: tick(16, 34),
				Actor: "cchildress", URL: prepareURL,
			}},
			want: Attribution{By: "bcho", Source: SourcePush},
		},
		{
			// A run that was stopped is a failure to push like any other, and
			// is caught by the same rule rather than by naming the conclusion.
			// The literal is GitHub's spelling, which is why .golangci.yaml
			// excludes it from misspell: `make fmt` would otherwise rewrite it
			// to a value the API never sends.
			name:  "a stopped prepare is not a candidate",
			build: buildRun("bcho", tick(16, 33)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "completed", Conclusion: "cancelled",
				Event: "workflow_dispatch", CreatedAt: tick(16, 32), UpdatedAt: tick(16, 34),
				Actor: "cchildress", URL: prepareURL,
			}},
			want: Attribution{By: "bcho", Source: SourcePush},
		},
		{
			// The exclusion is for COMPLETED failures only. This one has not
			// failed at anything yet and may be a step from its push.
			name:  "a prepare that has not finished is still a candidate",
			build: buildRun(deployKeyOwner, tick(16, 33)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "in_progress", Event: "workflow_dispatch",
				CreatedAt: tick(16, 32), UpdatedAt: tick(16, 32),
				Actor: "cchildress", URL: prepareURL,
			}},
			want: Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
		{
			// A failed prepare is excluded from containment but NOT from
			// covers: it still proves the list reaches back past this build,
			// which is what makes the non-match mean "hand-pushed" rather than
			// "we did not look far enough".
			name:  "a failed prepare still proves the list reaches back",
			build: buildRun("bcho", tick(23, 30)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "completed", Conclusion: "failure",
				Event: "workflow_dispatch", CreatedAt: tick(16, 32), UpdatedAt: tick(16, 34),
				Actor: "cchildress", URL: prepareURL,
			}},
			want: Attribution{By: "bcho", Source: SourcePush},
		},
		{
			// A completed run with no conclusion is an absence of evidence,
			// not a failure. Excluding it would drop a prepare that may well
			// have pushed, and the fall-through names the deploy key's owner -
			// so this errs toward answering.
			name:  "a completed prepare with no conclusion is still a candidate",
			build: buildRun(deployKeyOwner, tick(16, 33)),
			prepares: []Run{{
				Workflow: WorkflowPrepare, Status: "completed", Event: "workflow_dispatch",
				CreatedAt: tick(16, 32), UpdatedAt: tick(16, 34),
				Actor: "cchildress", URL: prepareURL,
			}},
			want: Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
		{
			name:     "no candidates at all is unknown",
			build:    buildRun(deployKeyOwner, tick(16, 33)),
			prepares: nil,
			want:     Attribution{Source: SourceUnknown},
		},
		{
			// Nothing to correlate: somebody pressed a button, and GitHub
			// records who.
			name: "a dispatched run is attributed to its own actor",
			build: Run{
				Workflow: WorkflowPrepare, Event: "workflow_dispatch",
				CreatedAt: tick(16, 32), Actor: "jwilder",
			},
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{By: "jwilder", Source: SourceDispatch},
		},
		{
			// The soak fires on workflow_run from the release build, so it
			// inherits the build's actor: the deploy key's owner, at one more
			// remove. An inherited actor is not evidence of anything.
			name: "a workflow_run inherits its actor and so is unknown",
			build: Run{
				Workflow: WorkflowUpgrade, Event: "workflow_run",
				HeadBranch: "main", CreatedAt: tick(16, 40), Actor: deployKeyOwner,
			},
			prepares: []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))},
			want:     Attribution{Source: SourceUnknown},
		},
		{
			// Serialization should stop this arising, but a re-run widens a
			// window, and the later prepare is the one that was actually going
			// on when the push happened.
			name:  "the newest containing prepare wins",
			build: buildRun(deployKeyOwner, tick(16, 33)),
			prepares: []Run{
				{
					Workflow: WorkflowPrepare, Status: "completed", Event: "workflow_dispatch",
					CreatedAt: tick(16, 32), UpdatedAt: tick(16, 34), Actor: "cchildress",
					URL: prepareURL,
				},
				{
					Workflow: WorkflowPrepare, Status: "completed", Event: "workflow_dispatch",
					CreatedAt: tick(16, 20), UpdatedAt: tick(16, 40), Actor: "bcho",
					URL: "https://github.com/Azure/unbounded/actions/runs/9",
				},
			},
			want: Attribution{By: "cchildress", Source: SourceDispatch, RunURL: prepareURL},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Attribute(tc.build, tc.prepares); got != tc.want {
				t.Errorf("Attribute() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAttributeSurvivesARerunOfTheBuild is the reason correlation reads
// created_at and never run_started_at.
//
// A re-run leaves created_at at the moment of the push and moves
// run_started_at to the retry, which can be hours later. Run 33655638847 is the
// real example: created 16:33:23Z by the tag push, attempt 6 started 18:29:20Z.
// A run_started_at rule would place the build outside every prepare window and
// report the deploy key's owner for exactly the runs someone is investigating.
func TestAttributeSurvivesARerunOfTheBuild(t *testing.T) {
	t.Parallel()

	build := buildRun(deployKeyOwner, tick(16, 33))
	build.RunStartedAt = tick(18, 29)
	build.TriggeringActor = "cchildress" // whoever pressed re-run

	prepares := []Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))}

	got := Attribute(build, prepares)
	if got.By != "cchildress" || got.Source != SourceDispatch {
		t.Fatalf("Attribute() = %+v, want cchildress via %s", got, SourceDispatch)
	}

	// The same build correlated on run_started_at finds nothing, which is what
	// makes the choice of clock load-bearing rather than incidental.
	onStarted := build
	onStarted.CreatedAt = build.RunStartedAt

	if bad := Attribute(onStarted, prepares); bad.By == "cchildress" {
		t.Error("run_started_at happened to match; the fixture no longer proves the point")
	}
}

// TestAttributeNeverReportsTheDeployKeyOwner guards the one outcome that would
// be worse than saying nothing.
//
// Whatever happens, the login on a push run must not be presented as the person
// responsible unless we established that the tag was pushed by hand.
func TestAttributeNeverReportsTheDeployKeyOwner(t *testing.T) {
	t.Parallel()

	build := buildRun(deployKeyOwner, tick(16, 33))

	cases := map[string][]Run{
		"no candidates":         nil,
		"candidates too recent": {prepareRun("cchildress", tick(18, 0), tick(18, 5))},
		"containing prepare":    {prepareRun("cchildress", tick(16, 32), tick(16, 34))},
	}

	for name, prepares := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := Attribute(build, prepares); got.By == deployKeyOwner {
				t.Errorf("Attribute() reported the deploy key owner: %+v", got)
			}
		})
	}
}

func TestAttributionKnown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		attr Attribution
		want bool
	}{
		{name: "dispatch", attr: Attribution{By: "cchildress", Source: SourceDispatch}, want: true},
		{name: "push", attr: Attribution{By: "bcho", Source: SourcePush}, want: true},
		{name: "unknown", attr: Attribution{Source: SourceUnknown}, want: false},
		// A source with no login is not an answer either, whatever it claims.
		{name: "named source with no login", attr: Attribution{Source: SourceDispatch}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.attr.Known(); got != tc.want {
				t.Errorf("Known() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAttributeTrustsItsCandidateList documents the sharp edge the callers
// exist to keep away from.
//
// Attribute cannot tell a list that reaches back far enough from one that was
// simply taken too early: both are a slice with an old prepare in it and no
// match for this build. So it reports the tag as hand-pushed and names the
// run's actor - which on a real tag push is the deploy key's owner, the one
// answer this package is here to avoid.
//
// That is why Prepares must be called after the runs it explains, why
// cmd_watch.go defers the fetch to the first sighting of a build, and why
// cmd_status.go collects its runs before asking. This test is the reason those
// three things are load-bearing, kept next to the code they constrain.
func TestAttributeTrustsItsCandidateList(t *testing.T) {
	t.Parallel()

	// A build from a tag that release-prepare really did push...
	build := buildRun(deployKeyOwner, tick(16, 33))

	// ...against a list taken before that prepare run existed. Older prepares
	// are present, so the list looks deep enough to be trusted.
	stale := []Run{prepareRun("bcho", tick(9, 0), tick(9, 5))}

	got := Attribute(build, stale)
	if got.Source != SourcePush || got.By != deployKeyOwner {
		t.Fatalf("Attribute() = %+v, want the documented failure: %s naming the actor",
			got, SourcePush)
	}

	// The same build against a list that includes the containing prepare, which
	// is what the callers guarantee by fetching late.
	fresh := append([]Run{prepareRun("cchildress", tick(16, 32), tick(16, 34))}, stale...)

	if got := Attribute(build, fresh); got.By != "cchildress" || got.Source != SourceDispatch {
		t.Errorf("Attribute() = %+v, want cchildress via %s", got, SourceDispatch)
	}
}
