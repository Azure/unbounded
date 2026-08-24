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

When local resolution fails — a stale checkout, a wrong `--repo-path`, running
outside a clone — the local half reports `UNKNOWN` rather than `(none)`. "I could
not tell" and "there are none" are different answers.

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
