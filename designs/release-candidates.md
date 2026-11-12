# Release candidates first

Every final release is a promoted release candidate. Candidates are cut on a
schedule from `main`, soaked on `unbounded-stable`, and their result is recorded
on their commit. Promoting finalizes the newest candidate that passed. The
version counter only advances for something that already built and soaked, so
the gaps between published versions stop.

This replaces releasing straight from `main`, and it retires the nightly.

## Problem

Published versions had large gaps that confused consumers. From v0.6 to v0.22,
eight of the seventeen minor versions never shipped, and v0.5 was abandoned
outright. What consumers saw was v0.8, v0.12, v0.14, v0.15, v0.18, v0.21, v0.22.

| Version | Failed in | Cause |
|---|---|---|
| v0.9.0 | build | gantry chart digest, offline agent artifacts |
| v0.10.0, v0.11.0 | build | racer-controller image (`go mod download`) |
| v0.13.0 | build | `unix.Syncfs` undefined in the darwin GoReleaser build, two image jobs |
| v0.16.0 | soak | deploy failed three times |
| v0.17.0 | build | about twelve image jobs |
| v0.19.0 | build | racer CUDA build, racer-vllm |
| v0.20.0 | build | orca, racer-vllm, six attempts |

The cause is structural. `release-prepare` minted the final version number
before anything had shown the tree could be built or deployed. Any failure that
needed a code change needed a new commit, so a new tag, so a new number. Seven
of the eight failed in the build, before reaching a cluster.

The candidate machinery already existed and promote already tagged the soaked
commit. But `mode=release` let `main` bypass it and was the documented default,
and promote never checked that the candidate it finalized had passed anything.

Each failure also left a draft release behind. There were 32 at the time of
writing.

## Design

### Every final is a promoted candidate

`release-prepare` has two modes, `prerelease` and `promote`. `mode=release` is
retired on every branch, including `release-X.Y` branches: a patch is a promoted
patch candidate too. `relctl cut` remains as a command that says what replaced
it.

A failure now costs an rc number. A gap between `rc.3` and `rc.5` is normal for
candidates and is not something consumers see, because candidates are never
published.

### Scheduled candidates

`release-prepare` cuts a candidate from `main` on a schedule:

```yaml
schedule:
  - cron: "15 0,8,16 * * *"
    timezone: "America/New_York"
```

That is about 12:15am, 8:15am and 4:15pm US Eastern, so a soaked candidate is
ready at the start of the working day. The minute is off the hour because the
top of the hour is when GitHub's scheduler is busiest.

`relctl candidate due` decides whether a scheduled run cuts. It does not when:

- the repository variable `RC_SCHEDULE` is `off`;
- `main` HEAD already carries a candidate or final tag, so nothing has changed;
- a release build or soak is still running.

The last rule matters because `release-upgrade` serializes on the cluster, and
GitHub keeps only one pending run per concurrency group: a new candidate's soak
would cancel one already waiting. Build plus soak has taken one to two and a half
hours, which is why the cadence is eight hours rather than one.

A cut by hand, `relctl rc`, ignores the first two rules, so a commit whose
candidate failed for a reason since fixed can be cut again.

### A candidate's result is a commit status

When a candidate's soak finishes, `release-upgrade` records the
`unbounded/candidate` commit status on its commit: `success` or `failure`. The
candidate is never published.

Publishing every passing candidate as a prerelease would put dozens a month on
the releases page, and deleting published releases to keep it tidy would be
unusual and confusing. A commit status is readable by any token that can read
the repository, including `release-prepare`'s read-only one, which cannot see
drafts. It also shows on the commit in GitHub.

The status is per commit. That is what promotion consumes: promote tags a commit
and the final is rebuilt from it. Two candidates of one commit share a result,
and the most recent soak of that tree is the one that counts.

A candidate cut from a release branch does not soak, because `unbounded-stable`
soaks `main` only. It records `success` once its signatures and BOM verify.

### Promote finalizes the newest candidate that passed

`relctl candidate pick` finds the candidate to promote: the newest of the train
whose status is `success`, or a named one, which must be `success`.
`release-prepare` runs it before it tags a promote and refuses if there is none.
With scheduled candidates the highest is routinely still building, or failed
where the one below it did not, so the newest passing candidate is often not the
highest.

`relctl promote` resolves the candidate locally and sends it as the `candidate`
input, so the workflow finalizes what was previewed rather than whatever is
newest by the time it runs.

### Break glass is a forced promote

When the soak cluster rather than the release is the problem, nothing can pass,
and nothing could be promoted. `relctl promote --force --reason ...` finalizes a
candidate that has not passed. It needs a typed confirmation, and in the
workflow it runs behind the `unbounded-reviewers` environment. The final it
tags is then soaked normally. If the cluster still cannot pass, `relctl publish`
records the bypass on the release, as before.

`relctl publish` refuses candidates: they are not published.

### A promoted final skips its soak

`relctl soak-plan` decides whether a release soaks. It does not when:

- it was cut from a release branch, as before; or
- it is a final whose candidate at the same commit passed its soak, and the
  build triggered the run.

A soak dispatched by hand always runs. A result that cannot be read means the
final soaks: the safe direction is the one that touches the cluster.

Signatures and the BOM are still verified before publishing. The final is a
rebuild, signed under its own tag, so its candidate's verification says nothing
about its artifacts.

### Drafts are cleaned up as they stop being useful

Only drafts of candidates are touched. Tags, images and signatures stay.

| When | `release-upgrade` deletes |
|---|---|
| A candidate's build fails | that candidate's draft |
| A candidate's soak finishes | drafts of older candidates of the same version, except the newest that passed, when this one did not |
| A final publishes | its remaining candidates' drafts |

A failed candidate is not re-run; the next candidate is the retry. A finished
candidate keeps its own draft, so its soak can be re-run with `relctl soak` once
a cluster problem is fixed, without rebuilding.

Drafts that predate this design are left for a maintainer to clear deliberately.

### The nightly is retired

Candidates now deploy `main` to `unbounded-stable` three times a day, with the
full release build, signing and smoke tests. The nightly did a subset of that on
`unbounded-nightly`. `nightly.yaml` and `setup-nightly-cluster.sh` are removed,
and `relctl preflight` judges the newest passing candidate instead of the
nightly.

## Alternatives considered

**Reuse a version number after a failure.** Delete the tag and draft, and cut the
same version again. Rejected: images are pushed publicly to ghcr and signed
partway through the build, so a re-cut overwrites `:vX.Y.Z` with a different
tree while the old signatures stay under the same identity. The tags are also Go
module versions for `github.com/Azure/unbounded`, and moving one breaks
checksums for anyone who has fetched it.

**Build once and attach the version when promoting.** Rejected for now: the
version is compiled into every binary (`internal/version.Version`), the operator
deploys component images at its own version, and signatures are tied to the
tag. Decoupling those is a large change across packages.

**Publish candidates and delete superseded ones.** Rejected: deleting published
releases is unusual and would surprise anyone who installed one.

**An hourly cadence.** Deferred: build plus soak takes longer than an hour, and
overlapping soaks cancel one another.

## Consequences

- A final is a rebuild of its candidate's commit, not the same bytes. Base
  images are referenced by tag and packages are installed at build time, so a
  final can differ from its candidate. Skipping its soak accepts that.
- A failed candidate build cannot be recovered with `gh run rerun --failed`,
  because its draft is discarded and the job that opens it is not re-run. A full
  re-run works.
- A candidate cut by hand while a soak is waiting can still displace it.
- Scheduled tags are pushed with the same deploy key as every other tag.
  `relctl` reports their cutter as `scheduled`.
- `unbounded-stable` now runs whatever candidate last soaked, not the last
  final.

## Operating it

- Switch scheduled candidates off with `gh variable set RC_SCHEDULE --body off`,
  and back on with `on`, without a commit.
- Change the cadence by editing the cron in `release-prepare.yaml`.
- `relctl status` shows the next candidate and what promote would ship.
  `relctl preflight` says whether there is something worth promoting.
