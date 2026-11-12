# Releasing Unbounded Kubernetes

How a release is cut, soaked and published, and what to do when one of those
goes wrong. Maintainer-facing; for using a release, see the
[documentation site](https://unbounded-cloud.io/).

**Every final release is a promoted release candidate.** Candidates
(`vX.Y.Z-rc.N`) are cut from `main` on a schedule, three times a day, and soaked
on `unbounded-stable`. Shipping means promoting the newest candidate that
passed. A failure costs an rc number rather than a version number, so the
published versions have no gaps. The reasoning is in
[designs/release-candidates.md](designs/release-candidates.md).

`main` releases minors and majors (`vX.Y.0`); a `release-X.Y` branch releases
patches (`vX.Y.Z`) to a series that has already shipped. See
[The tag and branch model](#the-tag-and-branch-model).

## How a release happens

Three workflows, in order.

| Phase | Workflow | Trigger | Result |
|---|---|---|---|
| 1. Prepare | `release-prepare.yaml` | the schedule, or a manual dispatch | pushes a candidate tag `vX.Y.Z-rc.N`, or a final `vX.Y.Z` on promote. Nothing else |
| 2. Build | `release.yaml` | the tag push | builds, signs, attests, creates a **draft** release |
| 3. Soak | `release-upgrade.yaml` | automatic, on phase 2 completing | see below |

What phase 3 does depends on the tag:

- **A candidate** is deployed to `unbounded-stable` and smoke-tested. The result
  is recorded as the `unbounded/candidate` commit status on its commit, and the
  candidate stays a draft. **Candidates are never published.**
- **A final** promoted from a candidate that passed is not soaked again: same
  commit. Its signatures and BOM are verified and it is published.
- **A final** that has no passing candidate behind it (a forced promote) is
  soaked, and published if that passes.

The split exists so phase 2 runs in the tag's context. Keyless cosign signatures
carry the identity `release.yaml@refs/tags/vX.Y.Z`, and phase 3 verifies exactly
that. A build dispatched from a branch would sign as `@refs/heads/main` and fail
verification, which is why `release.yaml` deliberately has **no** manual
trigger.

The tag is pushed with an SSH deploy key rather than `GITHUB_TOKEN`, because
GitHub suppresses workflow triggers for tags pushed with the default token.

One consequence is worth knowing before it confuses someone: GitHub attributes a
deploy-key push to whoever **registered the key**, so the actor shown on every
`release.yaml` run is the same person regardless of who cut the release. It is
not evidence that they did anything. `relctl watch <tag>` reports the real cutter
on a `Cut by:` line, and `relctl status` in a `BY` column. A candidate the
schedule cut reports `scheduled`.

## Driving this: `relctl` or `gh`

Every procedure below is given twice: with `relctl`, and with `gh` directly.

**`gh` is authoritative.** The workflows are the interface and `gh` dispatches
them; `relctl` is a convenience wrapper that also answers questions `gh` cannot.

```sh
make relctl-build     # bin/relctl
```

The full account, including the one exception where `relctl` is authoritative,
is in [relctl and gh](#relctl-and-gh).

## Which release do I want?

Land the fix on `main` first, always. Where it ships from depends on who needs
it and what else is sitting on `main`.

| Situation | What to do |
|---|---|
| `main` is clean, and a candidate has passed | [Promote it](#ship-a-release-from-main). Nothing else. |
| Your fix just merged and you cannot wait for the schedule | [Cut a candidate now](#cut-a-candidate-now), wait for it to pass, promote it. |
| `main` carries work you are not ready to ship | Promote a candidate cut **before** that work merged: promote ships the candidate's tree, not `main` HEAD. `relctl promote --candidate vX.Y.Z-rc.N`. Otherwise revert the unready work and wait for a candidate. |
| Someone is on an older series | [Cut a patch](#cut-a-patch-for-a-series-that-already-shipped) from that series' branch. This is the case release branches exist for, and the only way to ship the fix without everything merged since. |
| The fix is needed on both | Land on `main`, then cherry-pick down. Never the reverse: the flow is one-way, so nothing needs forward-porting. |
| A patch on the newest series | Allowed immediately. `release-0.5` can be opened the moment `v0.5.0` ships, without waiting for `main` to move, because `main` will never take `v0.5.1`. This is the property the whole rule buys. |
| Two branches at once | Independent. Their numbers cannot collide. |
| An incompatible change | [A major](#cutting-a-major-instead), from `main`. It is never derived; you ask for it. |

## Ship a release from `main`

The common case: everything merged up to the newest candidate that passed. One
dispatch, then waiting - nothing between the tag and the published release needs
a human.

### Before you start

There has to be a candidate that passed its soak, recently enough to be worth
shipping, with CI green on `main`.

```sh
relctl preflight
```

<details>
<summary>The same check with <code>gh</code></summary>

```sh
# Candidates on main, newest first.
git fetch --tags origin
git tag --list 'v*-rc.*' --sort=-version:refname --merged origin/main | head

# A candidate's soak result is the unbounded/candidate status on its commit.
gh api "repos/Azure/unbounded/commits/v0.5.0-rc.3/status" \
  --jq '.statuses[] | select(.context == "unbounded/candidate") | .state'

# CI on main.
gh run list --repo Azure/unbounded --workflow ci.yaml --branch main --limit 5
```

</details>

See [Is there something to promote?](#is-there-something-to-promote) for what
each check means.

### Promote it

```sh
# 1. What would ship? The newest candidate that passed, and its commit.
relctl candidate pick

# 2. Promote it. Shows the candidate, the final it becomes and every workflow
#    input, then asks. Add --dry-run to see that without dispatching.
relctl promote

# 3. Follow it through build and publish. Exits non-zero if it does not
#    publish, so this can be the last line of a script.
relctl watch v0.5.0
```

<details>
<summary>The same thing with <code>gh</code> only</summary>

```sh
# 1. Confirm what would be cut without cutting anything. The run summary shows
#    the final, the candidate it is promoted from and its commit.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=promote -f dry_run=true

# 2. Promote. release-prepare picks the newest candidate that passed and refuses
#    if there is none.
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=promote

# 3. Watch it through.
gh run list --repo Azure/unbounded --workflow release.yaml --limit 3
gh run list --repo Azure/unbounded --workflow release-upgrade.yaml --limit 3

# 4. Confirm. isDraft false means it shipped.
gh release view v0.5.0 --repo Azure/unbounded --json tagName,isDraft,url
```

</details>

Promote tags **the candidate's commit**, not `main` HEAD, so anything merged
after that candidate is not in the release. That is a feature as much as a
constraint: the tree that was built, deployed and smoke-tested is the tree that
ships. `relctl promote` and the dry run both say how many commits are being
left out.

The final is rebuilt from that commit, because the version is compiled into
every binary and signatures are tied to the tag. It does **not** soak again,
since its candidate already did; its signatures and BOM are verified before it
publishes.

### Cut a candidate now

The schedule cuts a candidate around 12:15am, 8:15am and 4:15pm US Eastern when
`main` has moved. When you need one sooner - a fix that just merged - cut it by
hand:

```sh
relctl rc
relctl watch v0.5.0-rc.4     # exits zero when it passes its soak
relctl promote
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=prerelease
```

</details>

A hand-cut candidate is cut even if `main` HEAD already has one, which is how to
retry a commit whose candidate failed for a reason that has since been fixed.

### Cutting a major instead

There is no `bump` input. `main` starts a minor train unless you ask for a
major, and a major is the one deliberate choice in the scheme:

```sh
relctl rc --major     # starts vX.0.0-rc.1
relctl promote        # once it passes
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=prerelease -f major=true
```

</details>

Ask for it when the release contains incompatible changes. While the project is
pre-1.0 the compatibility signal lives entirely in the major, so `v1.0.0` is the
release that says compatibility now matters. See
[Choosing a version](#choosing-a-version).

A minor train already in flight is continued rather than replaced, so starting a
major while one is live needs `--allow-concurrent-trains`, and promote then needs
`--version`. See [How trains work](#how-trains-work).

### If something fails

See [Recovery](#recovery). If the soak cluster is the problem rather than the
release, see [Break glass](#break-glass).

## Cut a patch for a series that already shipped

For when someone is on `v0.3.x` and needs a fix without everything merged to
`main` since. Open the branch if it does not exist, cherry-pick the fix, cut a
patch candidate, promote it.

### What is different

Three things differ from a release from `main`, and all three bite before you
start:

- **It will not soak.** `unbounded-stable` soaks `main` only. A candidate from a
  release branch skips deploy, Orca and smoke, and passes once its signatures and
  BOM verify. The schedule never cuts on a release branch; you cut these by hand.
  [Why](#release-branches-do-not-soak)
- **It may not be marked Latest.** A patch on an older series leaves the install
  command pointing at the newer release, so shipping `v0.3.1` after `v0.5.0`
  changes nothing for new users. [The rules](#what-gets-marked-latest)
- **A branch cut from an old tag runs no CI at all**, because the trigger lists
  live in the branch's own copy of `.github/workflows/`, and the `release-*`
  ruleset requires those checks - so every pull request to it would be
  unmergeable. Branches cut from `v0.4.0` or later are fine. From `v0.3.x` or
  earlier, add `release-*` to `ci.yaml`'s `push` and `pull_request` branch
  filters as the branch's first commit.

  `create-release-branch` checks this itself and warns, but only once
  dispatched: it is the workflow that resolves the branch point, so the `gh`
  form with `-f dry_run=true` below is what reports it without creating
  anything. `relctl branch create --dry-run` prints what it would send and
  dispatches nothing, so it cannot tell you.

### Before you start

Check whether the branch already exists:

```sh
relctl status        # "Release branches:" lists them
```

### Open the branch

Only if it does not exist. The branch point is derived rather than given: the
newest release in the series.

```sh
relctl branch create 0.3      # --dry-run prints the dispatch without sending it
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run create-release-branch.yaml --repo Azure/unbounded \
  -f series=0.3 -f dry_run=true    # resolve the branch point only
gh workflow run create-release-branch.yaml --repo Azure/unbounded -f series=0.3
```

</details>

### Cherry-pick, then cut and promote

```sh
# 1. Cherry-pick the fix onto the branch through a pull request. Fixes land on
#    main first and are cherry-picked down; never the other way round.

# 2. Check what it will cut. Run this ON the branch, or pass --branch: the
#    version is resolved against whatever is checked out.
git checkout release-0.3 && git fetch --tags
relctl next                   # v0.3.1-rc.1

# 3. Cut the patch candidate, then promote it once it has built and verified.
#    --branch defaults to the checkout, so on the branch this is just:
relctl rc
relctl watch v0.3.1-rc.1
relctl promote

# 4. Confirm it will not be marked Latest if main has moved past it. classify
#    answers relative to the trunk, so it needs main checked out and refuses
#    from anywhere else.
git checkout main && git fetch --tags
relctl classify v0.3.1
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=prerelease -f branch=release-0.3
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=promote -f branch=release-0.3
```

</details>

Step 2 is worth doing rather than skipping: `relctl next` resolves against the
CHECKOUT, so running it on `main` while passing `--branch release-0.3` applies
that branch's policy to main's history. It warns that the two disagree, and for
that particular pairing it then refuses outright, because the version it
computes falls outside the series the branch is allowed to cut.

The branch cuts `v0.3.1`, then `v0.3.2`, and can never mint a number `main`
owns.

### If something fails

See [Recovery](#recovery).

## Reference

The rest of this document is what the guides above are built on: what each phase
of the pipeline does, what to do when one of them goes wrong, and the sanctioned
overrides.

## How the pipeline works

### Preparing

Everything is `release-prepare.yaml`. It only computes a version and pushes a
tag; it builds nothing, so a mistake here is cheap to undo.

| Mode | Cuts |
|---|---|
| `prerelease` (the default) | the next candidate, `vX.Y.Z-rc.N` |
| `promote` | the final `vX.Y.Z`, at the commit of the newest candidate that passed its soak, or of `-f candidate=`, which must have passed |

There is no mode that cuts a final from HEAD. `mode=release` did that and is
retired: it minted the version number before anything had shown the tree could
build, so every failure that needed a code change cost a number.

`-f dry_run=true` works with either mode and prints the version and commit it
would cut without pushing anything.

`-f branch=` selects what to cut from: `main`, or a `release-X.Y` branch.
Anything else is refused, so a tag can never come from a feature branch. The
bump follows from that choice and is not an input.

The release tooling itself always comes from `main`, even when cutting from a
release branch. The two supply different things: the branch supplies the history
the version is computed against, which is what scopes resolution to its series,
while `main` supplies the tool that does the computing. An old branch would
otherwise cut releases with whatever tooling existed when it was created.

A consequence is that a change to the resolver takes effect only once merged to
`main`. It cannot be rehearsed by dispatching this workflow from a branch,
`dry_run` included; the Go tests under `hack/cmd/relctl` are how you test it
before that.

### The schedule

`release-prepare` also runs on a schedule, around 12:15am, 8:15am and 4:15pm US
Eastern (`America/New_York`, so daylight saving does not move it). Each run asks
`relctl candidate due` and cuts a candidate from `main` unless:

- the repository variable `RC_SCHEDULE` is `off`;
- `main` HEAD already has a candidate or final tag, so nothing has changed; or
- a release build or soak is still running.

A skipped run is green, and its summary says why. The last rule exists because
`release-upgrade` serializes on the cluster and GitHub keeps only one pending
run per concurrency group: a new candidate's soak would cancel one already
waiting.

```sh
gh variable set RC_SCHEDULE --body off    # stop scheduled candidates
gh variable set RC_SCHEDULE --body on     # start them again
```

The cadence is the cron in `release-prepare.yaml`. Build plus soak takes one to
two and a half hours, so it should not go much below a few hours.

### Building

`release.yaml` needs no input. It validates the tag shape, then builds the
binaries via GoReleaser, the frontend, every container image, the offline agent
artifacts and the manifests tarball. Everything is signed
with keyless cosign, images carry SPDX SBOM attestations, and a digest-pinned
release BOM records exactly what shipped. The exception is racer-vllm, whose
SBOM is too large for the public Rekor log to accept as an attestation. It is
published as a release asset with a cosign signature bundle instead.

It always creates the release as a **draft**. Prerelease is inferred from a `-`
in the tag.

### Image vulnerability scans

`release.yaml` can scan the images with Trivy before it pushes them, and fail
the release on what it finds. The scans run only when the repository variable
`RELEASE_TRIVY_SCAN` is `true`, and they are off by default. Since 2026-11-03 the
Trivy installer cannot reach `get.trivy.dev`
([aquasecurity/trivy#11533](https://github.com/aquasecurity/trivy/discussions/11533)),
so a scan fails before it starts. Every release run with scans off carries a
warning saying so.

```sh
gh variable set RELEASE_TRIVY_SCAN --body true    # scans on
gh variable set RELEASE_TRIVY_SCAN --body false   # scans off
```

The variable is read when the workflow runs, so changing it takes no commit and
no new tag. GitHub documents that a re-run reads it again, which would let you
turn scans off and re-run a release that failed in Trivy. That has not been
tried here yet, and it only applies to tags whose `release.yaml` has the
variable. Images that are never scanned, such as the host images and
racer-vllm, are marked `trivy: false` in the `component-images` matrix.

### Soaking

`release-upgrade.yaml` starts automatically when the build finishes. It first
asks `relctl soak-plan` whether the release soaks at all. It does not when:

- it was cut from a [release branch](#release-branches-do-not-soak); or
- it is a final whose candidate at the same commit passed its soak.

A soak dispatched by hand always runs, and a result that cannot be read means
the release soaks. When it soaks, the workflow:

1. downloads the draft release's artifacts,
2. verifies their signatures and the BOM against the tag's cosign identity,
3. deploys to the `unbounded-stable` cluster,
4. waits for the gated component workloads to roll out,
5. deploys Orca, the origin cache, as an integration workload,
6. runs the smoke tests in `hack/release/smoke/`, taken from the default branch
   so an old tag still gets today's checks.

When it does not soak, it verifies signatures and the BOM and goes straight to
the outcome. **Verification is never skipped**: a promoted final is a rebuild
signed under its own tag, so its candidate's verification says nothing about it.

Step 4 gates on `unbounded-operator`, `unbounded-net-controller`,
`unbounded-net-node`, `machina-controller`, `gantry`, and on
`metalman-controller-<site>` for every Site that enables metalman. Metalman is a
per-Site component, so those targets are discovered from the cluster rather than
assumed: on `unbounded-stable` the cluster's own Site is `stable` while metalman
runs for a remote site. A cluster where **no** Site enables it fails the deploy,
because this one is expected to run it.

Smoke tests cannot be skipped. Discovery fails if `hack/release/smoke/` is empty
on the default branch, and passing requires both discovery and every task to
succeed.

### The outcome

**A candidate** records its result as the `unbounded/candidate` commit status on
its commit: `success` when the soak passed (or, from a release branch, when
verification did), `failure` otherwise. The candidate is never published, so it
never appears on the releases page and is never "Latest". Promote reads the
status, and so does the final's soak plan.

The status is per commit, which is what promotion consumes. Two candidates of
one commit - a hand-cut retry - share it, and the most recent soak of that tree
is the one that counts.

**A final** is published once it has passed a soak, its own or its candidate's.
**Publishing is not a manual step.** If you find yourself running
`gh release edit --draft=false` by hand, use the [break-glass path](#break-glass)
instead so the bypass is recorded.

### Drafts

Every release is a draft until it publishes, and candidates never do, so
`release-upgrade` deletes candidate drafts as they stop being useful:

| When | Deleted |
|---|---|
| A candidate's build fails | that candidate's draft |
| A candidate's soak finishes | drafts of older candidates of the same version, except the newest that passed, when this one did not |
| A final publishes | its remaining candidates' drafts |

Only candidates' drafts are touched. Tags, images and signatures stay, because
images, signatures and the Go module proxy may refer to them. A finished
candidate keeps its own draft, so its soak can be re-run once a cluster problem
is fixed, without a rebuild. `relctl drafts prune --dry-run <tag>` shows what a
cleanup would do.

Drafts from before candidates were mandatory are not cleaned up automatically.
`relctl status` lists them.

### Degraded clusters

A DaemonSet counts every node toward its desired count, including nodes the
cluster has lost contact with, so one dead node would otherwise block every
release forever. The gate tolerates a shortfall when it is caused **only** by
NotReady nodes and everything on a reachable node is healthy.

`MAX_NOTREADY_NODES` caps how much is excusable: **2**. Beyond the cap the
shortfall is not excused and the rollout fails, with the offending nodes named
and grouped by site.

Tolerance is not a way to skip the upgrade. A shortfall is only excusable on a
workload that already carries the release's image tag, so a rollout cannot be
declared done while the operator is still reconciling the previous version.

## Is there something to promote?

```sh
relctl preflight
```

It says RELEASABLE when there is a candidate worth promoting, and NOT
RELEASABLE with the reason when there is not:

- **No candidate has passed.** Either none has been cut since the last final, or
  every one failed. Cut one, or find out why they fail.
- **The passing candidate is stale**: it passed more than two days ago and
  `main` has moved since. Promoting it would ship a tree from days ago without
  everything since. Cut a fresh one. A candidate of a `main` that has not moved
  is not stale however old it is: it is the current tree.
- **CI is not green on the branch.**

A newer candidate that failed does not block: promote ships the one that passed,
not the one that failed. Read why the newer one failed before promoting past it
all the same. `relctl preflight` reports it.

`relctl status` shows the same thing at a glance: the next candidate the
schedule would cut, and under "Promotable:" what promote would ship.

On a release branch, candidates are verified but not soaked, and preflight says
so rather than showing a tick that means more than it does.

## Choosing a version

You do not choose. The version is determined by where you are cutting from:

| Number | Cut from | Means |
|---|---|---|
| **major** (`v0.4.0` to `v1.0.0`) | `main`, with `-f major=true` | incompatible changes |
| **minor** (`v0.3.0` to `v0.4.0`) | `main` | every other release from `main` |
| **patch** (`v0.3.0` to `v0.3.1`) | `release-0.3` | cherry-picked fixes to a shipped series |

Each is reached the same way: candidates of it are cut, and one is promoted.

`main` never cuts a patch, and a release branch never cuts anything else. That
is what lets the two coexist: every series' patch space belongs to exactly one
branch, so `main` and `release-0.3` can never compute the same number. Without
it they would compete, and pre-1.0 they would compete for months, because a
series here runs twenty-odd releases before the minor turns over.

A major is the one deliberate choice, and it is never derived.

### On semantic versioning

This follows [semver 2.0.0](https://semver.org/) with one declared deviation.

While the project is pre-1.0 it is not even a deviation: clause 4 gives `0.y.z`
total latitude, and the specification's own FAQ recommends exactly this scheme,
to *"start your initial development release at 0.1.0 and then increment the
minor version for each subsequent release"*.

After 1.0, clause 6 says a release containing only backward compatible bug fixes
MUST increment the patch. A fixes-only release cut from `main` takes a minor
here instead.

The clearest way to hold this is that **`main`'s minor is a release counter, and
the compatibility signal lives entirely in the major**. That is structural rather
than an oversight: patch cannot also be a counter on `main` without colliding
with the branch that owns it. Every other case is covered by clause 7, which
permits a minor for new functionality and says it may include patch level
changes, so an ordinary mixed release from `main` is correctly a minor. Where it
matters, the compliant route is also the better one: cherry-pick the fixes to a
release branch and cut a patch, which is what the branch is for.

What `main`'s numbering no longer expresses is "this release contains no
behavior change", because minor absorbs patch. That signal was never reliable:
a release from `main` takes everything merged since, with no way to exclude it.
Patches now come only from a release branch carrying a deliberate set of
cherry-picks, so the patch signal means more than it used to, not less.

Because only promoted candidates become finals, the counter now advances only
for a release that ships. Gaps in the rc numbers are expected; gaps in the
versions are not.

### Prereleases use `rc` only

The prerelease suffix is `rc.N`, and `release-prepare` rejects anything else.
`alpha` and `beta` were previously used interchangeably with no defined
meaning.

### Every component shares one version

The operator resolves each component image as
`<registry>/<repository>:<operator version>`, where the version is the one
compiled into the operator binary. So `machina`, `gantry`, `metalman`
and the two `unbounded-net-*` images are all
deployed at the release tag. There is no per-component versioning, and a
component's image must be built by the release pipeline or its workload will not
start. `internal/operator/imagecoverage_test.go` enforces that.

This is also why a promoted final is rebuilt rather than relabeled: its
candidate's binaries carry the candidate's version.

## How trains work

A train is **in flight** when its core version has candidate tags, no final tag,
and is newer than the latest final tag.

- `prerelease` continues the train in flight and takes the next `rc.N`. Which
  bump the branch would otherwise take is only consulted when there is no train
  to continue, so it cannot fork a train halfway through. The schedule cuts this
  way too.
- `pre` is almost never needed. Leave it blank. An explicit value must be
  `rc.N` and must be ahead of the current highest.
- `promote` resolves the train on its own when exactly one is in flight. With
  several it refuses and asks which, rather than guessing.

`promote` creates the tag on **the candidate's commit**, not on `main` HEAD, and
the candidate is the **newest that passed its soak**, not the highest. With a
candidate cut three times a day, the highest is routinely still building, or
failed where the one below it did not. `-f candidate=` names a specific one,
which must also have passed. Anything merged after the candidate is **not** in
the release; `dry_run` prints the commit and how many commits are being left
out.

If you want those commits, wait for, or cut, a candidate that includes them.

Starting a second train while one is in flight requires
`-f allow_concurrent_trains=true`, and once two exist, `promote` requires an
explicit `version`. Scheduled runs fail rather than guess which train to
continue, so settle on one train, by promoting or abandoning the other, to get
them going again.

That guard exists because of a real incident: the `v0.1.24` train reached
`rc.18` and was silently orphaned when a `v0.2.0` train started beside it.
Promote picked the newer train, `v0.1.24` was never cut, and its twelve
candidate tags are still in the repository. Those older candidates are now
classified as **stale** rather than in flight, so they are reported for cleanup
and never offered as something to continue or promote.

## The tag and branch model

`main` owns `.0`; a release branch owns `.1` upward. Nothing else releases at
all. Because those spaces are disjoint, two branches can never compute the same
number, and the collision is impossible rather than merely detected.

A **release branch** is `release-X.Y`, created on demand. A **maintenance
release** is a patch cut from one. `main` is never a release branch, even though
releases are cut from it.

A worked sequence:

| Event | Branch | Tag | Soaked? |
|---|---|---|---|
| Scheduled candidate | `main` | `v0.5.0-rc.1` | yes; fails |
| Scheduled candidate, with the fix | `main` | `v0.5.0-rc.2` | yes; passes |
| Promote | `main` | `v0.5.0` | no; `rc.2` already did, same commit |
| A `v0.4.0` user needs a fix | `release-0.4` created from `v0.4.0` | - | - |
| Cherry-pick, then cut | `release-0.4` | `v0.4.1-rc.1` | no; verified |
| Promote | `release-0.4` | `v0.4.1` | no; verified |
| Work continues | `main` | `v0.6.0-rc.1` ... | yes |
| The big one | `main` | `v1.0.0-rc.1`, then `v1.0.0` | yes |

### Release branches do not soak

`unbounded-stable` soaks `main`, and only `main`. A release cut from a release
branch, candidate or final, skips deploy, Orca and smoke entirely. A candidate
passes once it verifies, and the final then publishes.

That is a decision about provenance, not about version numbers. The soak cluster
runs whatever `main` last put there, so "is this version newer" is the wrong
question: a release from a branch has no business on that cluster whatever its
number. Maintenance branches will get their own soak clusters; until they do,
these releases ship unsoaked and that is deliberate.

**Verification is not skipped.** Signatures and the release BOM are checked
before a candidate is recorded and before a final publishes, by the same script
the soaked path uses. Skipping a soak is a decision someone made; shipping
something unverified is not.

### What gets marked "Latest"

"Latest" is what `releases/latest/download` resolves to, which is the install
command in the README and every guide, so it is set explicitly on every publish
rather than left to GitHub's default of "whatever was published most recently".

A release is marked Latest when no higher release exists on `main`'s trunk or in
its own series, and it is not a prerelease. So:

- a release from `main` is Latest, as you would expect;
- the first patch on a branch **is** Latest while `main` has not moved on, because
  it genuinely is the newest release;
- a patch from an older series is not, so shipping `v0.3.1` after `v0.5.0` leaves
  the install command pointing at `v0.5.0`;
- republishing an older patch does not steal the marker back from a newer one;
- a candidate never is, since it is never published.

### Which branches are maintained

Not published anywhere: the branch list is the answer, which is why branches are
created on demand rather than at every release. Security fixes land on `main`
and reach a maintained branch as cherry-picks; see [SECURITY.md](SECURITY.md).

Note that a branch cut from a release predating `release-*` CI coverage will not
run any workflow, because the trigger lists live in the branch's own copy of
`.github/workflows/`. `create-release-branch` detects that and warns;
[Cut a patch](#cut-a-patch-for-a-series-that-already-shipped) covers what to do
about it.

## relctl and gh

`gh` is authoritative, as [Driving this](#driving-this-relctl-or-gh) says at the
top: the workflows are the interface, `gh` dispatches them, and where the two
disagree `gh` is right and `relctl` has a bug. What `relctl` adds is the
questions `gh` cannot answer, like which trains are live and which candidate
promote would ship, which are computed from tags and statuses rather than
stored anywhere.

The one exception, where `relctl` is authoritative, is the release decisions.
`release-prepare` calls `relctl next`, `relctl candidate pick` and
`relctl candidate due`, and `release-upgrade` calls `relctl soak-plan`, so
`relctl` and the workflows give the same answer by construction rather than by
agreement.

See [hack/cmd/relctl/README.md](hack/cmd/relctl/README.md) for what each command
needs; some work with no GitHub credential at all.

## Break glass

Four sanctioned overrides. All are manual-dispatch only: the automatic path
carries no inputs and cannot be relaxed.

They are not equally dangerous, and `relctl` asks for a different confirmation
for each rather than treating them alike:

| Override | What it bypasses | Command | Confirmation |
|---|---|---|---|
| [Tolerate more unreachable nodes](#tolerate-more-unreachable-nodes) | how many NotReady nodes are excusable | `relctl soak <tag> --max-notready-nodes N` | `y/N`, with a warning |
| [Re-initialize the cluster](#re-initialize-the-soak-cluster) | `site init` instead of `upgrade-apply` | `relctl soak <tag> --force-init` | typed phrase |
| [Promote without a passing candidate](#promote-without-a-passing-candidate) | the candidate gate | `relctl promote --force --reason ...` | typed phrase |
| [Publish without a soak](#publish-without-a-soak) | deploy, Orca and smoke entirely | `relctl publish <tag> --reason ...` | typed phrase, no `--yes` |

A typed phrase cannot be satisfied by `--yes`, so the last three are not
reachable by reflex or by a script that passes `--yes` everywhere. Raising the
NotReady ceiling takes an ordinary prompt because it is a bounded relaxation
that still fails on anything unhealthy, not a way past the gate.

Re-running a soak with no override at all is an ordinary retry, for a candidate
or a final:

```sh
relctl soak v0.5.0-rc.2
```

A candidate's re-run records a fresh result on its commit.

### Tolerate more unreachable nodes

When a known outage takes out more nodes than the cap allows:

```sh
relctl soak v0.5.0-rc.2 --max-notready-nodes 7
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.5.0-rc.2 -f max_notready_nodes=7
```

</details>

This raises the ceiling only. A shortfall must still be entirely explained by
NotReady nodes, and anything unhealthy on a reachable node still fails. State
the number deliberately; it is a claim someone can review.

### Re-initialize the soak cluster

`site init` instead of `upgrade-apply`, for a first-ever bootstrap:

```sh
relctl soak v0.5.0-rc.2 --force-init
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.5.0-rc.2 -f force_init=true
```

</details>

This is not a recovery tool. On a cluster that is already initialized, `site
init` creates a fresh Site rather than migrating the existing one, which is why
the workflow otherwise selects init mode only when it detects the pre-redesign
layout, and why `relctl` asks for a typed phrase here and an ordinary prompt for
a plain retry.

If the soak failed and the cluster is intact, re-run it without this.

### Promote without a passing candidate

For when the soak cluster itself is the problem, so no candidate can pass, and a
release must ship:

```sh
relctl promote --force --reason "unbounded-stable unreachable during DC maintenance"
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=promote -f force=true \
  -f reason="unbounded-stable unreachable during DC maintenance"
```

</details>

- It promotes the train's highest candidate, or `--candidate`, without checking
  its result.
- It runs behind the `unbounded-reviewers` environment, so it can be gated on
  reviewer approval independently of ordinary releases.
- The final it tags **does** soak, since there is no passing candidate to stand
  in for one. If the cluster still cannot pass, the next step is
  [Publish without a soak](#publish-without-a-soak), which records the bypass on
  the release.

### Publish without a soak

For a final whose soak cannot pass because the soak cluster is the problem:

```sh
relctl publish v0.5.0 --reason "unbounded-stable unreachable during DC maintenance"
```

There is no `--yes`. The confirmation phrase must be typed in full.

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.5.0 -f force_publish=true \
  -f reason="unbounded-stable unreachable during DC maintenance"
```

</details>

- The reason is **required** and is written into the release body, where it
  stays visible long after the CI logs expire.
- Artifact signatures and the release BOM are **still verified**. Forcing means
  "we accept an unsoaked release", never "we accept an unverified one".
- The deploy and smoke jobs still run, so their diagnostics are preserved.
- The forced path uses the `unbounded-reviewers` environment, so it can be
  gated on reviewer approval independently of normal releases.
- Candidates are refused: they are never published. Promote one with `--force`
  instead.

If you use either forced path, file a follow-up to fix whatever made it
necessary.

## Recovery

**A tag was pushed but no build started.** Usually a deploy-key problem. Delete
the tag and re-run prepare. There is no manual trigger on `release.yaml` by
design: a dispatched build would sign with the wrong identity and fail
verification downstream.

```sh
git push origin :refs/tags/v0.5.0-rc.3
```

**A candidate's build failed.** Do nothing to it. Its draft is discarded
automatically and the next candidate is the retry: the schedule cuts one once
`main` moves, or cut one now with `relctl rc`, which also re-cuts the same commit
when the cause was not in the tree. The rc number it used is simply skipped.

A failed candidate can still be re-run in full (`gh run rerun <run-id>`, not
`--failed`), because the job that opens the draft runs again. `--failed` does
not work for a candidate: its draft is already gone, and the job that opens it
succeeded the first time, so it is not re-run.

**A final's build failed partway.** Re-run it. `release.yaml` is safe to re-run:
the draft release is opened once, up front, and every job that writes to it
replaces rather than appends. A final's draft is never discarded.

```sh
gh run rerun <run-id> --failed --repo Azure/unbounded
```

`--failed` re-runs only what failed and leaves successful jobs alone, which is
what you want for a transient cause: a registry blip, a runner, a permission
that has since been granted. Fix the cause first, because a re-run re-runs the
same tree. Since the final is a rebuild of a candidate that already built, a
cause outside the tree is the likely one.

Two limits on this:

- **It has a seven-day shelf life.** `finalize` downloads the GoReleaser dist
  and the manifests tarball by name, and those artifacts
  are kept for seven days. Past that, `--failed` will not help: the jobs that
  produced them are still green, so it will not rebuild them, and `finalize`
  fails on the missing artifact. Use a full `gh run rerun <run-id>` instead,
  which reruns everything.
- **The release must still be a draft.** If it was already published, the build
  refuses to run rather than rewriting a shipped release's artifacts. Re-draft
  it first with `gh release edit <tag> --draft=true`.

**A final's build cannot be fixed by a re-run.** Leave its tag where it is and
promote a new candidate; the next final takes the next number. Do not delete the
tag and promote again. Deleting it would let promote compute the same version a
second time, for a different tree, while the first attempt's public images and
signatures are still under that version. This should be rare: the final is a
rebuild of a commit that already built.

**Where did it get to?** `relctl watch <tag> --once` reports the build, every
soak attempt including manual retries, and the outcome: published for a final,
the recorded soak result for a candidate. It correlates runs across all three
workflows, which `gh run list` cannot: only the build names the tag.

**The soak failed.** Read the failure before reaching for an override. The
deploy job's `Diagnose failed rollout` step dumps node readiness, Sites,
workloads, applied images and operator logs. A missing image is named
explicitly; so is a rollout blocked by unreachable nodes. Once the cause is
fixed, re-run the soak with `relctl soak <tag>`; a candidate keeps its draft for
exactly this.

**A release was published by mistake.** Re-draft it:

```sh
gh release edit v0.5.0 --repo Azure/unbounded --draft=true
```

**Old drafts.** Drafts from before candidates were mandatory are not cleaned up
automatically. `relctl status` lists them with the date of the commit each
points at, and `relctl status --all` shows every one rather than the last 30
days. Delete them deliberately with `gh release delete <tag>` (without
`--cleanup-tag`, so the tag stays).

## Rehearsing locally

`./hack/test-release-local.sh` rehearses the build phase on a workstation. See
[CONTRIBUTING.md](CONTRIBUTING.md#testing-the-release-pipeline-locally).
