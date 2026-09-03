# Releasing Unbounded Kubernetes

How to cut, soak, and publish a release, and what to do when one of those goes
wrong. Maintainer-facing; for using a release, see the
[documentation site](https://unbounded-cloud.io/).

Releases come from two places. `main` cuts minors and majors (`vX.Y.0`); a
`release-X.Y` branch cuts patches (`vX.Y.Z`) against a series that has already
shipped. See [The tag and branch model](#the-tag-and-branch-model).

## How a release happens

Three workflows, in order. In the normal flow only the first is started by a
human; `release-upgrade` is also dispatchable by hand for backfills, rollbacks
and the [break-glass paths](#break-glass).

| Phase | Workflow | Trigger | Result |
|---|---|---|---|
| 1. Prepare | `release-prepare.yaml` | manual dispatch | pushes a `vX.Y.Z` tag, nothing else |
| 2. Build | `release.yaml` | the tag push | builds, signs, attests, creates a **draft** release |
| 3. Soak and publish | `release-upgrade.yaml` | automatic, on phase 2 completing | deploys to `unbounded-stable`, smokes it, flips the draft to published |

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
on a `Cut by:` line, and `relctl status` in a `BY` column.

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
| `main` is clean | [Cut from `main`](#cut-a-minor-or-major-release-from-main). It is a minor. Nothing special. |
| `main` carries work you are not ready to ship | A release from `main` takes all of it, and there is no way to exclude it. Read the list first, then: [cut a candidate](#cut-a-release-candidate-first) the moment your fix lands and promote that, which holds that exact tree; or revert the unready work, cut, and re-land it after; or ship it anyway, having read the list. |
| Someone is on an older series | [Cut a patch](#cut-a-patch-for-a-series-that-already-shipped) from that series' branch. This is the case release branches exist for, and the only way to ship the fix without everything merged since. |
| The fix is needed on both | Land on `main`, then cherry-pick down. Never the reverse: the flow is one-way, so nothing needs forward-porting. |
| A patch on the newest series | Allowed immediately. `release-0.5` can be opened the moment `v0.5.0` ships, without waiting for `main` to move, because `main` will never take `v0.5.1`. This is the property the whole rule buys. |
| Two branches at once | Independent. Their numbers cannot collide. |
| An incompatible change | [A major](#cutting-a-major-instead), from `main`. It is never derived; you ask for it. |

## Cut a minor or major release from `main`

The common case: everything merged since the last release. One dispatch, then
waiting - nothing between the tag and the published release needs a human.

### Before you start

`main` has to be releasable: the nightly green and green recently, CI green on
the commit you intend to release, and no candidate train already in flight.

```sh
relctl preflight
```

<details>
<summary>The same checks with <code>gh</code></summary>

```sh
# The nightly deploys main to a real cluster and smoke-tests it. This is the
# strongest signal we have. It should be green, and green recently.
gh run list --repo Azure/unbounded --workflow nightly.yaml --limit 5

# CI on the commit you intend to release.
gh run list --repo Azure/unbounded --workflow ci.yaml --branch main --limit 5
```

</details>

A red nightly is a release blocker until it is understood. See
[Is `main` releasable?](#is-main-releasable) for what each check means and what
"green recently" is judged as.

### Cut it

```sh
# 1. What version, and what would ship?
relctl next
git log --oneline "$(relctl next -o json | jq -r .latestFinal)..origin/main"

# 2. Cut it. Shows the version and every workflow input, then asks.
#    Add --dry-run to see that without dispatching.
relctl cut

# 3. Follow it through build, soak and publish. Exits non-zero if it does not
#    publish, so this can be the last line of a script.
relctl watch v0.5.0
```

<details>
<summary>The same thing with <code>gh</code> only</summary>

```sh
# 1. What would ship? Everything on main since the last final tag.
git fetch --tags origin
git log --oneline \
  "$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' origin/main)..origin/main"

# 2. Confirm the version without cutting anything. The run log prints the tag
#    it would create and the commit it would tag.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f dry_run=true

# 3. Cut it. Same command, without dry_run.
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=release

# 4. Watch it through. The tag push starts the build, which drafts the release;
#    finishing that starts the soak, which publishes it.
gh run list --repo Azure/unbounded --workflow release.yaml --limit 3
gh run list --repo Azure/unbounded --workflow release-upgrade.yaml --limit 3

# 5. Confirm. isDraft false means it shipped.
gh release view v0.5.0 --repo Azure/unbounded --json tagName,isDraft,url
```

The `dry_run=true` step exists because there was no other way to see the version
before minting it. `relctl next` answers the same question locally and instantly,
so the relctl path skips it - but the dispatch remains the only way to prove the
WORKFLOW agrees, which is what makes it worth keeping before a first release from
a branch nobody has cut from.

</details>

### Cutting a major instead

There is no `bump` input. `main` cuts a minor unless you ask for a major, and a
major is the one deliberate choice in the scheme:

```sh
relctl cut --major
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f major=true
```

</details>

Ask for it when the release contains incompatible changes. While the project is
pre-1.0 the compatibility signal lives entirely in the major, so `v1.0.0` is the
release that says compatibility now matters. See
[Choosing a version](#choosing-a-version).

### If something fails

See [Recovery](#recovery). If the soak cluster is the problem rather than the
release, see [Break glass](#break-glass).

## Cut a patch for a series that already shipped

For when someone is on `v0.3.x` and needs a fix without everything merged to
`main` since. Open the branch if it does not exist, cherry-pick the fix, cut a
patch.

### What is different

Three things differ from a release cut from `main`, and all three bite before
you start:

- **It will not soak.** `unbounded-stable` soaks `main` only. The patch skips
  deploy, Orca and smoke entirely, and then publishes - verified, but deployed
  nowhere, and it does not sit as a draft waiting for anyone.
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

Release candidates are also pointless here: a candidate exists to soak, and one
cut from a release branch will not. Cut candidates on `main`.

### Before you start

The nightly only ever runs on the default branch, so it says nothing about a
release branch. `relctl preflight --branch release-0.3` reports that rather than
showing a tick that refers somewhere else.

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

### Cherry-pick, then cut

```sh
# 1. Cherry-pick the fix onto the branch through a pull request. Fixes land on
#    main first and are cherry-picked down; never the other way round.

# 2. Check what it will cut. Run this ON the branch, or pass --branch: the
#    version is resolved against whatever is checked out.
git checkout release-0.3 && git fetch --tags
relctl next

# 3. Cut the patch. --branch defaults to the checkout, so on the branch this
#    is just:
relctl cut

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
  -f mode=release -f branch=release-0.3 -f dry_run=true
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f branch=release-0.3
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

## Cut a release candidate first

Worth it for anything you would not want to publish blind. Each candidate is
built, signed and soaked on `unbounded-stable` exactly like a final release, and
published as a prerelease, so it never becomes "Latest".

```sh
# 1. Start the train. Cut candidates on main: --branch release-X.Y is accepted
#    but the candidate will not soak, which is the only reason to cut one.
relctl rc

# 2. Iterate. Same command: the train in flight is detected and the next rc
#    taken automatically.
relctl rc

# 3. See where the train is at any point.
relctl status                   # live and stale trains
relctl next --mode prerelease   # the rc that would be cut next

# 4. Promote when it is good.
relctl promote
```

<details>
<summary>The same thing with <code>gh</code> only</summary>

```sh
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=prerelease
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=prerelease
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=promote
```

There is no `gh` equivalent of step 3: which trains are live is computed from
tags rather than stored anywhere.

</details>

`relctl status` is the reason to prefer it here: which trains are live has no
`gh` equivalent, because it is computed from tags rather than stored anywhere.
A train that was abandoned shows as **stale**, which is the state that orphaned
`v0.1.24` at rc.18.

`promote` tags the **last candidate's commit**, not `main` HEAD, so anything
merged after that candidate is not in the release. That is a feature as much as
a constraint: cut a candidate as soon as your change lands and you hold that
exact tree, whatever merges afterwards. See
[How trains work](#how-trains-work) for the detail.

## Reference

The rest of this document is what the guides above are built on: what each phase
of the pipeline does, what to do when one of them goes wrong, and the two
sanctioned overrides.

## How the pipeline works

### Preparing

Everything is `release-prepare.yaml`. It only computes a version and pushes a
tag; it builds nothing, so a mistake here is cheap to undo. The commands are in
the guides above - [from `main`](#cut-a-minor-or-major-release-from-main) or
[from a release branch](#cut-a-patch-for-a-series-that-already-shipped); what
follows is what they mean.

`-f dry_run=true` works with any mode and prints the version and commit it would
cut without pushing anything.

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

### Building

`release.yaml` needs no input. It validates the tag shape, then builds the
binaries via GoReleaser, the frontend, every container image, the offline agent
artifacts, the manifests tarball and the storage tarballs. Everything is signed
with keyless cosign, images carry SPDX SBOM attestations, and a digest-pinned
release BOM records exactly what shipped.

It always creates the release as a **draft**. Prerelease is inferred from a `-`
in the tag.

### Soaking and publishing

`release-upgrade.yaml` starts automatically when the build finishes. A release
cut from a [release branch](#release-branches-do-not-soak) skips straight past
the cluster work and publishes. Otherwise it:

1. downloads the draft release's artifacts,
2. verifies their signatures and the BOM against the tag's cosign identity,
3. deploys to the `unbounded-stable` cluster,
4. waits for the gated component workloads to roll out,
5. deploys Orca, the origin cache, as an integration workload,
6. runs the smoke tests in `hack/release/smoke/`, taken from the default branch
   so an old tag still gets today's checks,
7. and only then flips the draft to published.

Step 4 gates on `unbounded-operator`, `unbounded-net-controller`,
`unbounded-net-node`, `machina-controller`, `gantry`, and on
`metalman-controller-<site>` for every Site that enables metalman. Metalman is a
per-Site component, so those targets are discovered from the cluster rather than
assumed: on `unbounded-stable` the cluster's own Site is `stable` while metalman
runs for a remote site. A cluster where **no** Site enables it fails the deploy,
because this one is expected to run it.

**`unbounded-storage-supervisor` is still not gated**, so a release can publish
with it failing to start; tracked in
[#625](https://github.com/Azure/unbounded/issues/625).

A clean deploy, a clean Orca deploy and green smoke are the soak gate.
**Publishing is not a manual step.** If you find yourself running
`gh release edit --draft=false` by hand, use the
[break-glass path](#break-glass) instead so the bypass is recorded.

Smoke tests cannot be skipped. Discovery fails if `hack/release/smoke/` is empty
on the default branch, and publishing requires both discovery and every task to
succeed, so a release that ran no smoke tests stays a draft. Shipping one anyway
is the [break-glass path](#break-glass), where it is recorded.

This is also why [`promote` tags the last candidate's commit](#how-trains-work):
the soak is evidence about one specific tree, and it is only worth anything if
that is the tree that ships.

Prereleases are published too, but stay flagged as prereleases and never become
"Latest".

### Degraded clusters

A DaemonSet counts every node toward its desired count, including nodes the
cluster has lost contact with, so one dead node would otherwise block every
release forever. The gate tolerates a shortfall when it is caused **only** by
NotReady nodes and everything on a reachable node is healthy.

`MAX_NOTREADY_NODES` caps how much is excusable: **2**, for both the release
deploy and the nightly. Beyond the cap the shortfall is not excused and the
rollout fails, with the offending nodes named and grouped by site.

Tolerance is not a way to skip the upgrade. A shortfall is only excusable on a
workload that already carries the release's image tag, so a rollout cannot be
declared done while the operator is still reconciling the previous version.

## Is `main` releasable?

```sh
relctl preflight
```

It checks the nightly, CI on the branch, and whether a train is already in
flight, and says RELEASABLE or NOT RELEASABLE with the reason.

<details>
<summary>The same checks with <code>gh</code></summary>

There is no single dashboard. Check these before cutting:

```sh
# The nightly deploys main to a real cluster and smoke-tests it. This is the
# strongest signal we have. It should be green, and green recently.
gh run list --repo Azure/unbounded --workflow nightly.yaml --limit 5

# CI on the commit you intend to release.
gh run list --repo Azure/unbounded --workflow ci.yaml --branch main --limit 5
```

</details>

A red nightly is a release blocker until it is understood. It deploys the same
component images the release will, to the same shape of cluster, so a nightly
failure is a release failure you have not had yet - unless it is only
unreachable nodes, which the rollout gate tolerates. See
[Degraded clusters](#degraded-clusters).

"Green recently" is judged as within 48 hours, which allows for a weekend gap in
scheduling while still refusing a week-old pass - that describes a tree nobody
has released since.

The nightly only ever runs on the default branch, so it says nothing about a
release branch. `relctl preflight --branch` reports that rather than showing a
tick that refers somewhere else; see
[Cut a patch](#cut-a-patch-for-a-series-that-already-shipped).

## Choosing a version

You do not choose. The version is determined by where you are cutting from:

| Number | Cut from | Means |
|---|---|---|
| **major** (`v0.4.0` to `v1.0.0`) | `main`, with `-f major=true` | incompatible changes |
| **minor** (`v0.3.0` to `v0.4.0`) | `main` | every other release from `main` |
| **patch** (`v0.3.0` to `v0.3.1`) | `release-0.3` | cherry-picked fixes to a shipped series |

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

### Prereleases use `rc` only

The prerelease suffix is `rc.N`, and `release-prepare` rejects anything else.
`alpha` and `beta` were previously used interchangeably with no defined
meaning.

Iterate a train by re-running prepare in `prerelease` mode with no input
changes; the next `rc` is chosen for you. Promote when it is good.

### Every component shares one version

The operator resolves each component image as
`<registry>/<repository>:<operator version>`, where the version is the one
compiled into the operator binary. So `machina`, `gantry`, `metalman`,
`unbounded-storage-supervisor` and the two `unbounded-net-*` images are all
deployed at the release tag. There is no per-component versioning, and a
component's image must be built by the release pipeline or its workload will not
start. `internal/operator/imagecoverage_test.go` enforces that.

## How trains work

A train is **in flight** when its core version has prerelease tags, no final
tag, and is newer than the latest final tag.

- `prerelease` continues the train in flight and takes the next `rc.N`. Which
  bump the branch would otherwise take is only consulted when there is no train
  to continue, so it cannot fork a train halfway through.
- `pre` is almost never needed. Leave it blank. An explicit value must be
  `rc.N` and must be ahead of the current highest.
- `promote` resolves on its own when exactly one train is in flight. With
  several it refuses and asks which, rather than guessing.

`promote` creates the tag on **the last candidate's commit**, not on `main`
HEAD. The point of a candidate is that it was built, deployed to
`unbounded-stable` and smoke-tested; tagging HEAD would ship a different tree,
including everything merged since, under a version whose only claim to being
trustworthy is that soak. Anything merged after the last `rc` is therefore
**not** in the release. `dry_run` prints the commit and how many commits are
being left out.

If you want those commits, cut another candidate first and promote that one.

Starting a second train while one is in flight requires
`-f allow_concurrent_trains=true`, and once two exist, `promote` requires an
explicit `version`.

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
| Normal release | `main` | `v0.4.0` | yes |
| Start a candidate train | `main` | `v0.5.0-rc.1` | yes, as a prerelease |
| Promote | `main` | `v0.5.0` | yes |
| A `v0.4.0` user needs a fix | `release-0.4` created from `v0.4.0` | - | - |
| Cherry-pick, then cut | `release-0.4` | `v0.4.1` | no; publishes unsoaked |
| Work continues | `main` | `v0.6.0` | yes |
| Second fix on the branch | `release-0.4` | `v0.4.2` | no; publishes unsoaked |
| The big one | `main` | `v1.0.0` | yes |

### Release branches do not soak

`unbounded-stable` soaks `main`, and only `main`. A release cut from a release
branch skips deploy, Orca and smoke entirely, and then **publishes** - it does
not sit as a draft waiting for someone.

That is a decision about provenance, not about version numbers. The soak cluster
runs whatever `main` last put there, including a candidate, so "is this version
newer" is the wrong question: a release from a branch has no business on that
cluster whatever its number. Maintenance branches will get their own soak
clusters; until they do, these releases ship unsoaked and that is deliberate.

**Verification is not skipped.** Signatures and the release BOM are checked
before publishing, by the same script the soaked path uses. Skipping a soak is a
decision someone made; shipping something unverified is not.

Candidates on a release branch are a special case worth knowing. A candidate
exists to soak, so one cut from a branch cannot do its job: it will skip the
soak like everything else from that branch. Cut candidates on `main`.

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
- republishing an older patch does not steal the marker back from a newer one.

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
questions `gh` cannot answer, like which trains are live, which is computed from
tags rather than stored anywhere.

The one exception, where `relctl` is authoritative, is version resolution.
`release-prepare` calls `relctl next` internally, so `relctl next` and the
workflow give the same answer by construction rather than by agreement.

See [hack/cmd/relctl/README.md](hack/cmd/relctl/README.md) for what each command
needs; some work with no GitHub credential at all.

## Break glass

Three sanctioned overrides. All are manual-dispatch only: the automatic path
carries no inputs and cannot be relaxed.

They are not equally dangerous, and `relctl` asks for a different confirmation
for each rather than treating them alike:

| Override | What it bypasses | Command | Confirmation |
|---|---|---|---|
| [Tolerate more unreachable nodes](#tolerate-more-unreachable-nodes) | how many NotReady nodes are excusable | `relctl soak <tag> --max-notready-nodes N` | `y/N`, with a warning |
| [Re-initialize the cluster](#re-initialize-the-soak-cluster) | `site init` instead of `upgrade-apply` | `relctl soak <tag> --force-init` | typed phrase |
| [Publish without a soak](#publish-without-a-soak) | deploy, Orca and smoke entirely | `relctl publish <tag> --reason ...` | typed phrase, no `--yes` |

A typed phrase cannot be satisfied by `--yes`, so the last two are not reachable
by reflex or by a script that passes `--yes` everywhere. Raising the NotReady
ceiling takes an ordinary prompt because it is a bounded relaxation that still
fails on anything unhealthy, not a way past the gate.

Re-running a soak with no override at all is an ordinary retry:

```sh
relctl soak v0.4.0
```

### Tolerate more unreachable nodes

When a known outage takes out more nodes than the cap allows:

```sh
relctl soak v0.4.0 --max-notready-nodes 7
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.4.0 -f max_notready_nodes=7
```

</details>

This raises the ceiling only. A shortfall must still be entirely explained by
NotReady nodes, and anything unhealthy on a reachable node still fails. State
the number deliberately; it is a claim someone can review.

### Re-initialize the soak cluster

`site init` instead of `upgrade-apply`, for a first-ever bootstrap:

```sh
relctl soak v0.4.0 --force-init
```

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.4.0 -f force_init=true
```

</details>

This is not a recovery tool. On a cluster that is already initialized, `site
init` creates a fresh Site rather than migrating the existing one, which is why
the workflow otherwise selects init mode only when it detects the pre-redesign
layout, and why `relctl` asks for a typed phrase here and an ordinary prompt for
a plain retry.

If the soak failed and the cluster is intact, re-run it without this.

### Publish without a soak

For when the soak cluster itself is the problem and the release must ship:

```sh
relctl publish v0.4.0 --reason "unbounded-stable unreachable during DC maintenance"
```

There is no `--yes`. The confirmation phrase must be typed in full.

<details>
<summary>With <code>gh</code></summary>

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.4.0 -f force_publish=true \
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

If you use this, file a follow-up to fix whatever made it necessary.

## Recovery

**A tag was pushed but no build started.** Usually a deploy-key problem. Delete
the tag and re-run prepare. There is no manual trigger on `release.yaml` by
design: a dispatched build would sign with the wrong identity and fail
verification downstream.

```sh
git push origin :refs/tags/v0.4.0
```

**The build failed partway.** Re-run it. `release.yaml` is safe to re-run: the
draft release is opened once, up front, and every job that writes to it
replaces rather than appends.

```sh
gh run rerun <run-id> --failed --repo Azure/unbounded
```

`--failed` re-runs only what failed and leaves successful jobs alone, which is
what you want for a transient cause: a registry blip, a runner, a permission
that has since been granted. Fix the cause first, because a re-run re-runs the
same tree.

Two limits on this:

- **It has a seven-day shelf life.** `finalize` downloads the GoReleaser dist,
  the manifests tarball and the storage tarballs by name, and those artifacts
  are kept for seven days. Past that, `--failed` will not help: the jobs that
  produced them are still green, so it will not rebuild them, and `finalize`
  fails on the missing artifact. Use a full `gh run rerun <run-id>` instead,
  which reruns everything.
- **The release must still be a draft.** If the soak already published it, the
  build refuses to run rather than rewriting a shipped release's artifacts.
  Re-draft it first with `gh release edit <tag> --draft=true`.

**A re-run cannot fix it.** Only then delete the tag and cut it again. This is
the expensive path: it burns a version number.

```sh
git push origin :refs/tags/v0.4.0
```

Deleting the tag leaves the draft release behind. Deleting the draft too is
tidier but no longer required: the next cut of the same version adopts it and
overwrites its assets. If the tag came from `promote`, note that re-promoting
resolves the same candidate commit again, so a fix that is a code change needs
a new `rc` cut first - only a fix outside the tagged tree (a secret, a runner, a
registry) can be retried by re-promoting.

**Where did it get to?** `relctl watch <tag> --once` reports the build, every
soak attempt including manual retries, and whether the release published. It
correlates runs across all three workflows, which `gh run list` cannot: only the
build names the tag.

**The soak failed.** Read the failure before reaching for the override. The
deploy job's `Diagnose failed rollout` step dumps node readiness, Sites,
workloads, applied images and operator logs. A missing image is named
explicitly; so is a rollout blocked by unreachable nodes.

**A release was published by mistake.** Re-draft it:

```sh
gh release edit v0.4.0 --repo Azure/unbounded --draft=true
```

**Stale drafts.** Abandoned candidates linger as drafts and should be deleted
once their train is promoted, otherwise the release list becomes misleading.
`relctl status` lists them with the date of the commit each points at, and
`relctl status --all` shows every one rather than the last 30 days. At the time
of writing there are 24, going back to `v0.1.17` (2026-06-19).

## Rehearsing locally

`./hack/test-release-local.sh` rehearses the build phase on a workstation. See
[CONTRIBUTING.md](CONTRIBUTING.md#testing-the-release-pipeline-locally).
