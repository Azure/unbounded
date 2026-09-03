// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"time"
)

// Where an attribution came from.
const (
	// SourceDispatch means a person is named because they dispatched a
	// workflow: either this run directly, or the release-prepare run that
	// pushed the tag this run built.
	SourceDispatch = "dispatch"
	// SourcePush means the tag was pushed by hand rather than by
	// release-prepare, so the run's own actor is the person who pushed it.
	SourcePush = "push"
	// SourceUnknown means we could not tell, and say so rather than print a
	// name that would be read as an answer.
	SourceUnknown = "unknown"
)

// Attribution is who is actually responsible for a run.
//
// This exists because a release.yaml run's `actor` is not a person who did
// anything. release-prepare pushes the tag over SSH with a deploy key rather
// than GITHUB_TOKEN, because GitHub suppresses workflow triggers for tags
// pushed with the default token. GitHub then attributes a deploy-key push to
// whoever REGISTERED THE KEY, so every release build reports the same login no
// matter who cut it, and has done since the key was introduced.
//
// triggering_actor is no help: on a push it equals actor, and on a re-run it
// names whoever pressed re-run. Neither field can name the cutter.
type Attribution struct {
	// By is the login, empty when Source is SourceUnknown.
	By string `json:"by,omitempty"`
	// Source says how By was arrived at, so a caller can render "cut by" and
	// "pushed by" differently rather than presenting an inference as a fact.
	Source string `json:"source"`
	// RunURL links the release-prepare run By dispatched, when there is one.
	RunURL string `json:"runUrl,omitempty"`
}

// Known reports whether an actual person was identified.
func (a Attribution) Known() bool { return a.Source != SourceUnknown && a.By != "" }

// prepareCandidates is how many release-prepare runs to consider.
//
// A window rather than the whole history: correlation only ever looks at runs
// around one push, and a page is one request. Deep enough that a build from
// several releases ago is still covered, which matters because falling off the
// end of the list is reported as unknown rather than guessed at.
const prepareCandidates = 30

// prepareSlack is how long after a prepare run finishes a push may still be
// counted as its own.
//
// The push happens near the end of the job, a couple of steps before it
// completes, so in practice the build run is created BEFORE the prepare run
// closes and no slack is needed at all. It is here for the reverse case: a
// queued build run whose created_at lands fractionally after the prepare
// finished. Small on purpose, because widening this is how a hand-pushed tag
// gets misattributed to whoever last ran a prepare.
const prepareSlack = 2 * time.Minute

// Prepares lists recent release-prepare runs, newest first, for correlation.
//
// MUST be called AFTER fetching the runs it will be used to attribute, never
// before. A list is a window in time, and Attribute reads the absence of a
// match in it as evidence that a tag was pushed by hand. Taken too early the
// list cannot contain the prepare that is about to push, the absence means
// nothing, and the deploy key's owner gets reported as the pusher: the one
// answer this whole file exists to avoid.
func (c *Client) Prepares(ctx context.Context) ([]Run, error) {
	return c.Runs(ctx, ListRuns{Workflow: WorkflowPrepare, Limit: prepareCandidates})
}

// Attribute names the person behind a run.
//
// Pure, and separated from the fetching, because the correlation rule is the
// part worth testing and it is a heuristic rather than a lookup: nothing in the
// API links a tag push back to the workflow run that made it.
//
// Only workflow_dispatch carries a trustworthy actor. A push may carry the
// deploy key's owner, and everything else inherits an actor from the run that
// triggered it. For a push the rule is containment in time: a prepare run's
// window is open from when it was created until shortly after it finished, and
// the build it caused was created inside that window.
//
// What makes containment unambiguous rather than merely plausible is
// release-prepare's `concurrency: release-prepare` group with
// cancel-in-progress false: prepares are serialized, so at most one window is
// open at any moment. Without that, two overlapping prepares would both contain
// the same push and there would be no way to choose.
//
// Times come from CreatedAt on both sides, never RunStartedAt. A re-run moves
// run_started_at and leaves created_at alone, so created_at remains the moment
// of the push however many times the build is retried. Using run_started_at
// would break attribution for exactly the runs people investigate.
//
// The candidate list is trusted to have been taken after the build. Nothing
// here can check that - a list is just a slice, and one fetched an hour too
// early looks exactly like one fetched a second too late - so the callers
// carry the obligation. See Prepares, and TestAttributeTrustsItsCandidateList
// for what breaks when they do not.
func Attribute(build Run, prepares []Run) Attribution {
	switch build.Event {
	case "workflow_dispatch":
		// Somebody pressed a button and GitHub recorded which somebody. This
		// is the only event whose actor is an observation rather than an
		// inheritance.
		return Attribution{By: build.Actor, Source: SourceDispatch}
	case "push":
		// The case this whole file exists for. Fall through.
	default:
		// workflow_run and its relatives inherit the actor of the run that
		// triggered them. The soak fires on workflow_run from the release
		// build, so it inherits the build's actor, which is the deploy key's
		// owner twice removed from anyone who did anything. Inherited is not
		// observed, so we do not report it.
		return Attribution{Source: SourceUnknown}
	}

	if match, ok := containingPrepare(build, prepares); ok {
		return Attribution{By: match.Actor, Source: SourceDispatch, RunURL: match.URL}
	}

	// No prepare run contains the push. That means the tag was pushed by hand,
	// in which case the run's actor IS the person who pushed it and is worth
	// reporting - but only if the candidate list actually reaches back far
	// enough to have found a prepare run had one existed. Otherwise the absence
	// of a match says nothing, and the actor is more likely to be the deploy
	// key's owner than anyone who did something.
	if covers(build, prepares) {
		return Attribution{By: build.Actor, Source: SourcePush}
	}

	return Attribution{Source: SourceUnknown}
}

// containingPrepare finds the prepare run whose window contains the push.
//
// A prepare that GitHub reported as not succeeding is not a candidate. The push
// is the last thing that job does - everything after `git push origin "$TAG"`
// in release-prepare.yaml is an echo and a step summary, and the only later
// step is skipped on a real cut - so a failed prepare almost certainly failed
// before pushing anything and cannot be what created this tag. Left in, its
// window would claim a tag someone pushed by hand and name the wrong person,
// which is the same class of error as reporting the deploy key's owner.
//
// Only runs GitHub reported as not succeeding are excluded. A prepare still in
// progress has not failed at anything yet and may be a couple of steps from its
// push, and a completed run with no conclusion is an absence of evidence rather
// than a failure. Excluding either would drop a prepare that did push, and the
// fall-through from that is naming the deploy key's owner. See Run.Failed.
//
// This filter deliberately does not apply to covers(). A failed prepare still
// proves the list reaches back to its timestamp, and dropping it there would
// shrink the window and push honest hand-pushed tags into unknown.
//
// Dry runs cannot be filtered: they succeed and push nothing, and the API does
// not report a run's dispatch inputs. Reaching that case needs a tag pushed by
// hand inside a dry run's window, and the dry run's dispatcher is the likely
// answer anyway.
//
// The newest match wins. Serialization means there should never be two, but
// preferring the newest is the right tie-break if a window is ever widened by a
// prepare being re-run: the later run is the one that was actually going on.
func containingPrepare(build Run, prepares []Run) (Run, bool) {
	var (
		best  Run
		found bool
	)

	for _, prepare := range prepares {
		if prepare.Failed() {
			continue
		}

		if !prepareWindow(prepare).contains(build.CreatedAt) {
			continue
		}

		if !found || prepare.CreatedAt.After(best.CreatedAt) {
			best, found = prepare, true
		}
	}

	return best, found
}

// window is a half-open-ended interval a run was live for.
type window struct {
	from time.Time
	// until is the closing edge, zero meaning still open.
	until time.Time
}

func (w window) contains(at time.Time) bool {
	if at.Before(w.from) {
		return false
	}

	return w.until.IsZero() || !at.After(w.until)
}

// prepareWindow is the interval during which a prepare run could have pushed.
//
// It closes at UpdatedAt rather than at the run's own CreatedAt plus a guess,
// so a slow prepare is not cut short. A run still in progress has no closing
// edge at all: its UpdatedAt is merely the last time anything changed, and
// treating that as an ending would exclude the push it is about to make.
func prepareWindow(prepare Run) window {
	w := window{from: prepare.CreatedAt}

	if prepare.Done() && !prepare.UpdatedAt.IsZero() {
		w.until = prepare.UpdatedAt.Add(prepareSlack)
	}

	return w
}

// covers reports whether the candidate list reaches back far enough for the
// absence of a match to mean anything.
//
// Runs arrive newest first and the list is a fixed window, so a build older
// than every candidate is simply out of range. Distinguishing that from a
// genuine hand-pushed tag is the difference between "nobody prepared this" and
// "we did not look far enough back", and only the first is worth reporting.
func covers(build Run, prepares []Run) bool {
	for _, prepare := range prepares {
		if !prepare.CreatedAt.After(build.CreatedAt) {
			return true
		}
	}

	return false
}
