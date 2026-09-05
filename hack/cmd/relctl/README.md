# relctl

Drive and observe unbounded releases from a terminal.

`RELEASING.md` says of deciding whether `main` is releasable: *"There is no
single dashboard."* This is that, plus the same for a release already in flight.

## Authority

**The workflows are the interface, and `gh` calls them directly. `RELEASING.md`
documents both forms, and where the two disagree, `gh` is right and this tool
has a bug.**

That ordering is deliberate. relctl exists to make the release process legible
and hard to get wrong, not to become a second implementation of it. The one
place it *is* authoritative is version resolution: `release-prepare` calls
`relctl next`, so the answer here and the answer in CI come from the same code.

## Building

```sh
make relctl          # implies test
make relctl-build    # no lint or test
```

## What needs what

| | Needs a clone | Needs a credential |
|---|---|---|
| `next` | yes | no |
| `classify` | yes | no |
| `status` | for the train view | yes |
| `preflight` | for the train note | yes |
| `watch` | no | yes |
| `cut` `rc` `promote` | for the preview | yes |
| `branch create` `soak` `publish` | no | yes |

Version resolution is pure git, so `next` and `classify` work with no
`GITHUB_TOKEN` and no `gh` login — including inside a workflow that was never
granted one.

Tags must be current. `relctl` reads the clone it is pointed at and does not
fetch: run `git fetch --tags` first if an answer looks stale.

## Credentials

`GITHUB_TOKEN`, then `GH_TOKEN`, then whatever `gh auth token` returns.

The environment wins because that is the workflow case, where a token is always
present and `gh` may not be installed. The `gh` fallback is the interactive
case: every maintainer who can cut a release already has `gh` working, and
needing to mint a PAT to run a read-only `relctl status` would be the friction
that stops a tool being used.

## Commands

### `status`

Latest release, live and stale trains, release branches, drafts, and anything in
flight across `release-prepare`, `release.yaml`, `release-upgrade` and
`create-release-branch`.

Drafts are worth reading: a draft is a release that built and never shipped, and
the usual cause is a failed soak. They accumulate silently.

Drafts list one per line with the date of the commit they point at, highest
version first:

```
Drafts (24): built but not published, usually a soak that failed.
  TAG          COMMITTED
  v0.2.4-rc.1  2026-08-12
  v0.2.1-rc.1  2026-08-03
  22 older, back to v0.1.17 (2026-06-19). --all to list them.
```

`COMMITTED` is the date of the tagged commit, **not** the date the draft was
made. GitHub does not expose the latter: a draft's `published_at` is null until
it publishes, so `created_at` (the commit date) is the only date it has. As a
staleness signal for cleanup that is arguably the more useful of the two, but
do not read it as when someone drafted the release.

The order is by version, not by date, so a whole abandoned train stays together
and can go at once. A lower version with a later commit date will therefore sit
below a higher one.

Only the last 30 days are enumerated; older ones collapse to a single line
naming the oldest and how many there are. `--all` lists every one. The count in
the header is always the true total, so the backlog stays visible even when it
is not enumerated, and `-o json` is never windowed.

When local resolution fails — a stale checkout, a wrong `--repo-path`, running
outside a clone — the local half reports `UNKNOWN` rather than `(none)`. "I could
not tell" and "there are none" are different answers.

The in-flight table has a `BY` column naming whoever is behind each run:

```
In flight:
  WORKFLOW      REF     STATE        BY          URL
  release.yaml  v0.5.0  in_progress  cchildress  https://github.com/...
```

`BY` is **not** GitHub's `actor`, and the difference matters. See
[Who cut a release](#who-cut-a-release).

### `next`

The version that would be cut, without cutting it.

```sh
relctl next                          # for the branch you are on
relctl next --branch release-0.4     # for another branch's policy
relctl next --mode prerelease        # the next candidate
```

`--branch` is **policy**: it decides what may be cut, per the versioning rule in
`RELEASING.md`. Tag discovery is separately scoped by reachability from local
`HEAD`. The two are independent, so `--branch` defaults to the checked-out branch
and warns when an explicit value disagrees — on a `release-0.3` checkout, main's
policy applied to that branch's history would answer confidently and wrongly.

### `classify`

Whether a tag soaks, and whether it is Latest. Two questions that are not the
same question; `relctl classify --help` explains why.

Needs a checkout of the **default branch** with full history and tags, because
`from_main` is answered by reachability.

### `preflight`

Whether a branch is releasable: the nightly green and green *recently*, CI on the
branch, and any train already in flight.

A red nightly is a blocker until it is understood — it deploys the same component
images the release will, to the same shape of cluster. The nightly only ever runs
on the default branch, so on a release branch the tool says so rather than
showing a tick that refers somewhere else.

### `watch`

Follows a tag through build, soak and publish.

Only `release.yaml` is identifiable by tag, because a tag push sets the run's
branch to the tag. `release-upgrade` fires on `workflow_run`, reports the default
branch, and exposes no link back to what triggered it — so its runs are matched
by commit and time window. That window matters: a promoted final and its last
candidate **share a commit**, so `head_sha` alone cannot tell their soaks apart.

Exits non-zero if the release did not publish, so it can be the last line of a
script.

It also names who cut the tag:

```
Tag:      v0.6.0
Cut by:   cchildress  (release-prepare https://github.com/Azure/unbounded/actions/runs/33667308864)
Build:    success  https://github.com/Azure/unbounded/actions/runs/33667392561
Soak:     success  (workflow_run)  https://github.com/Azure/unbounded/actions/runs/33670115829
Release:  published
```

See [Who cut a release](#who-cut-a-release) for why that line is not simply the
run's actor.

A watch runs for up to ninety minutes and makes a request every twenty seconds,
so it treats a failed poll as a fact about the minute rather than about the
release. Anything that could pass later — a 5xx, either rate limit, a connection
that never landed — is retried until the timeout, and the retry is announced on
**stderr** so it cannot corrupt `-o json`. Anything GitHub answered definitely,
such as a 404 or a bad credential, fails at once instead of spending the timeout
on an answer that will not change. `--once` is a single-shot query and never
retries.

### `cut`, `rc`, `promote`

Dispatch `release-prepare`. Each shows the version it will mint — resolved
locally by the same code the workflow runs — and every input it will send,
then asks. `--dry-run` prints that and stops.

The preview is the reason to prefer these over `gh`: the workflow's own
`dry_run` costs a dispatch and a minute to answer a question computable here
instantly.

They dispatch on `main` even when cutting from a release branch, because
`release-prepare` takes its tooling from the default branch deliberately and
dispatching it on the branch would run that branch's copy of the workflow.

### `branch create`

Opens a `release-X.Y` branch. The branch point is derived: the newest release in
the series.

### `soak`, `publish`

The break-glass paths. Both take a **typed confirmation** rather than accepting
`--yes`, so neither is reachable by reflex or by a script that passes `--yes`
everywhere. `publish` has no `--yes` flag at all.

`soak <tag>` on its own is an ordinary retry and is not treated as break-glass.
`--force-init` and `publish` are.

## Who cut a release

**GitHub's `actor` on a release build names nobody who did anything.**

`release-prepare` pushes the tag over SSH with a deploy key rather than
`GITHUB_TOKEN`, because GitHub suppresses workflow triggers for tags pushed with
the default token. GitHub then attributes a deploy-key push to whoever
registered the key, so every `release.yaml` run reports that same person no
matter who cut the release. `triggering_actor` is no help either: on a push it
equals `actor`, and on a re-run it names whoever pressed re-run.

So relctl derives the answer instead, and reports it three ways:

| Rendering | Means |
| --- | --- |
| `Cut by: <login>` with a `release-prepare` link | That person dispatched the `release-prepare` run that pushed the tag |
| `Pushed by: <login>` | The tag was pushed **by hand**, so the run's actor really is the person who pushed it |
| `Cut by: unknown`, or `?` in the `BY` column | Could not be established |

Nothing in the API links a tag push back to the run that made it, so the first
case is correlation rather than lookup: the `release-prepare` run whose
execution window contains the push. That is sound rather than merely plausible
because `release-prepare` has a `concurrency` group with `cancel-in-progress:
false`, so prepares are serialized and at most one window is open at a time.

Correlation reads `created_at` on both sides and never `run_started_at`. A
re-run moves `run_started_at` and leaves `created_at` alone, so `created_at`
stays the moment of the push however many times a build is retried.

A prepare run that GitHub reported as **not succeeding** is not a candidate. The
push is the last thing that job does, so a failed prepare almost certainly
failed before pushing and cannot be what created the tag. It still counts toward
how far back the candidate list reaches, because that is a separate question.

Three consequences worth knowing:

- A build the candidate list cannot speak to reports `unknown` rather than
  guessing. That is deliberate. The raw value is still in `-o json` as `actor`,
  alongside the derived answer under `by`.
- **The candidate list is fetched after the runs it explains**, never before.
  A non-match is read as "pushed by hand", so a list taken before the prepare
  run existed would report the deploy key's owner as the pusher — the one thing
  reliably known to be false. `watch` therefore waits until it has seen a build
  before asking, and `status` collects its runs first.
- A name appears in `BY` only where the derivation reached one. Where it did
  not, the column is `?` and never the raw actor, which on a tag push is the
  deploy key's owner and would be read as naming somebody. For a tag genuinely
  pushed by hand the column does show the actor, because there it is the person
  who pushed.

A soak reports `?`. It fires on `workflow_run` and inherits the build's actor,
which is the deploy key's owner at one further remove.

`watch` carries the derived answer once, on the result, because it is a fact
about the **tag** rather than about any one run — the build and every soak carry
the same distorted actor. Each run keeps its raw `actor` beside it for anyone
reconciling against the Actions UI. If the candidate list cannot be fetched at
all, both commands say so and report `unknown` rather than failing.

This is a workaround. The fix is to stop pushing tags with a person's deploy key.

## Output

`--output text` (default), `json`, or `github`.

`github` emits `key=value` for `$GITHUB_OUTPUT` and is what the workflows
consume. `next` emits `tag`, `base`, `bump` and `series`; `classify` emits
`from_main` and `latest`. Diagnostics go to stderr in that mode, because anything
on stdout lands in `$GITHUB_OUTPUT` and a stray line corrupts it.

Under Actions — detected via `GITHUB_ACTIONS` — warnings are emitted as
`::warning::` so they surface as annotations.

## Out of scope

`prepare-release-net.yaml`, which has never run, and the `agent-artifacts/*` tag
scheme, which has its own lifecycle. Neither is part of the release this tool
describes.
