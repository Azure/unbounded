# Releasing Unbounded Kubernetes

How to cut, soak, and publish a release, and what to do when one of those goes
wrong. Maintainer-facing; for using a release, see the
[documentation site](https://unbounded-cloud.io/).

Releases are cut from `main` only. There are no maintenance branches today (see
[Maintenance lines](#maintenance-lines)).

## At a glance

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

## Cutting a release

One dispatch, then waiting. Nothing between the tag and the published release
needs a human.

### Patch release

The common case: fixes and incremental work with no behaviour change for
existing users.

```sh
# 1. Is main releasable? The nightly deploys it to a real cluster, so this is
#    the strongest signal available. It should be green, and green recently.
gh run list --repo Azure/unbounded --workflow nightly.yaml --limit 5

# 2. What would ship? Everything on main since the last final tag.
git fetch --tags origin
git log --oneline \
  "$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' origin/main)..origin/main"

# 3. Confirm the version without cutting anything. The run log prints the tag
#    it would create and the commit it would tag.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f bump=patch -f dry_run=true

# 4. Cut it. Same command, without dry_run.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f bump=patch

# 5. Watch it through. The tag push starts the build, which drafts the release;
#    finishing that starts the soak, which publishes it.
gh run list --repo Azure/unbounded --workflow release.yaml --limit 3
gh run list --repo Azure/unbounded --workflow release-upgrade.yaml --limit 3

# 6. Confirm. isDraft false means it shipped.
gh release view v0.2.5 --repo Azure/unbounded --json tagName,isDraft,url
```

If a step fails, see [Recovery](#recovery). If the soak cluster is the problem
rather than the release, see [Break glass](#break-glass).

### Minor release

The same, with `-f bump=minor`. Use it for new capabilities, breaking changes,
removed flags or CRD field changes; see [Choosing a version](#2-choosing-a-version).
Consider cutting a candidate first.

### With a release candidate first

Worth it for anything you would not want to publish blind. Each candidate is
built, signed and soaked on `unbounded-stable` exactly like a final release, and
published as a prerelease, so it never becomes "Latest".

```sh
# 1. Start the train. bump is relative to the latest final tag.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=prerelease -f bump=minor

# 2. Iterate. Same command with NO input changes: the train in flight is
#    detected and the next rc taken automatically.
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=prerelease

# 3. Promote when it is good.
gh workflow run release-prepare.yaml --repo Azure/unbounded -f mode=promote
```

`promote` tags the **last candidate's commit**, so anything merged to `main`
after that candidate is not in the release. That is a feature as much as a
constraint: cut a candidate as soon as your change lands and you hold that exact
tree, whatever merges afterwards.

## Shipping an urgent fix

There are no maintenance branches yet, so an urgent fix rolls forward: land it
on `main` and cut a patch release. What that costs depends on what else is
sitting on `main`.

**`main` is clean.** Land the fix, cut a patch release as above. Nothing
special.

**`main` carries work you are not ready to ship.** A release from `main` takes
all of it, and there is no way to exclude it. Read the list first (step 2
above), then pick:

- cut a candidate the moment your fix lands and promote that. `promote` ships
  the candidate's tree, so anything merged afterwards is excluded;
- revert the unready work on `main`, cut the patch, re-land it after;
- ship it anyway, having read the list.

**The fix is for an older line**, e.g. a `v0.2.x` user after `v0.3.0` shipped.
Not supported today: `release-prepare` cuts from `main` only. See
[Maintenance lines](#maintenance-lines).

## Reference

The rest of this document is what the guides above are built on: what each phase
of the pipeline does, what to do when one of them goes wrong, and the two
sanctioned overrides.

## 1. Is `main` releasable?

There is no single dashboard. Check these before cutting:

```sh
# The nightly deploys main to a real cluster and smoke-tests it. This is the
# strongest signal we have. It should be green, and green recently.
gh run list --repo Azure/unbounded --workflow nightly.yaml --limit 5

# CI on the commit you intend to release.
gh run list --repo Azure/unbounded --workflow ci.yaml --branch main --limit 5
```

A red nightly is a release blocker until it is understood. It deploys the same
component images the release will, to the same shape of cluster, so a nightly
failure is a release failure you have not had yet - unless it is only
unreachable nodes, which the rollout gate tolerates. See
[Degraded clusters](#degraded-clusters).

## 2. Choosing a version

The project is pre-1.0, so semver's compatibility guarantees are not yet in
force and breaking changes can land in a minor bump. In practice:

- **patch** (`v0.2.4` to `v0.2.5`) for fixes and incremental work with no
  behaviour change for existing users.
- **minor** (`v0.2.4` to `v0.3.0`) for new capabilities, breaking changes,
  removed flags, or CRD field changes.
- **major** is reserved for 1.0.

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

## 3. Preparing a release

Everything is `release-prepare.yaml`. It only computes a version and pushes a
tag; it builds nothing, so a mistake here is cheap to undo. The commands are in
[Cutting a release](#cutting-a-release); what follows is what they mean.

`-f dry_run=true` works with any mode and prints the version and commit it would
cut without pushing anything.

Note that `release-prepare` runs the resolver from `main`, so a change to it
takes effect only once merged. It cannot be rehearsed by dispatching the
workflow from a branch, `dry_run` included; `hack/release/next-version-test.sh`
is how you test it before that.

### How trains work

A train is **in flight** when its core version has prerelease tags, no final
tag, and is newer than the latest final tag.

- `prerelease` continues the train in flight and takes the next `rc.N`. `bump`
  is only consulted when there is no train to continue, so it cannot fork a
  train halfway through.
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

## 4. What the build does

`release.yaml` needs no input. It validates the tag shape, then builds the
binaries via GoReleaser, the frontend, every container image, the offline agent
artifacts, the manifests tarball and the storage tarballs. Everything is signed
with keyless cosign, images carry SPDX SBOM attestations, and a digest-pinned
release BOM records exactly what shipped.

It always creates the release as a **draft**. Prerelease is inferred from a `-`
in the tag.

## 5. Soaking and publishing

`release-upgrade.yaml` starts automatically when the build finishes. It:

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

This is also why `promote` tags the last candidate's commit: the soak is
evidence about one specific tree, and it is only worth anything if that is the
tree that ships.

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

## Break glass

Two sanctioned overrides. Both are manual-dispatch only: the automatic path
carries no inputs and cannot be relaxed.

### Tolerate more unreachable nodes

When a known outage takes out more nodes than the cap allows:

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.2.5 -f max_notready_nodes=7
```

This raises the ceiling only. A shortfall must still be entirely explained by
NotReady nodes, and anything unhealthy on a reachable node still fails. State
the number deliberately; it is a claim someone can review.

### Publish without a soak

For when the soak cluster itself is the problem and the release must ship:

```sh
gh workflow run release-upgrade.yaml --repo Azure/unbounded \
  -f tag=v0.2.5 -f force_publish=true \
  -f reason="unbounded-stable unreachable during DC maintenance"
```

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
git push origin :refs/tags/v0.2.5
```

**The build failed partway.** Fix the cause, delete the tag, and cut it again.
The draft release and any uploaded assets should be deleted first so the retry
starts clean. If the tag came from `promote`, note that re-promoting resolves
the same candidate commit again, so a fix that is a code change needs a new `rc`
cut first - only a fix outside the tagged tree (a secret, a runner, a registry)
can be retried by re-promoting.

**The soak failed.** Read the failure before reaching for the override. The
deploy job's `Diagnose failed rollout` step dumps node readiness, Sites,
workloads, applied images and operator logs. A missing image is named
explicitly; so is a rollout blocked by unreachable nodes.

**A release was published by mistake.** Re-draft it:

```sh
gh release edit v0.2.5 --repo Azure/unbounded --draft=true
```

**Stale drafts.** Abandoned candidates linger as drafts and should be deleted
once their train is promoted, otherwise the release list becomes misleading.

## Maintenance lines

**Not yet implemented**, tracked in
[#627](https://github.com/Azure/unbounded/issues/627). `release-prepare.yaml`
cuts from `main` only, so an older release cannot be patched without shipping
everything merged since.

The agreed model is a **lazy** `release-X.Y` branch, created on demand from a
tag or commit the first time an older line needs a fix, with every subsequent
release on that line cut from it:

- version resolution scoped to the line, so `release-0.2` cuts `v0.2.5` while
  `main` is on `v0.3.x` and neither can mint the other's numbers;
- fixes cherry-picked onto the branch through a PR, with CI and code scanning
  extended to `release-*` - today both run on `main` only, so a cherry-pick
  would be tested by nothing;
- a maintenance release **skips the soak**, because deploying an older line to
  `unbounded-stable` would downgrade it, and stays a draft until someone
  publishes it deliberately through the recorded
  [break-glass path](#publish-without-a-soak).

Until that lands, the remedy is rolling forward, which matches the support
policy in [SECURITY.md](SECURITY.md): fixes land on the latest release and
`main`, and older releases are not routinely supported. See
[Shipping an urgent fix](#shipping-an-urgent-fix).

## Rehearsing locally

`./hack/test-release-local.sh` rehearses the build phase on a workstation. See
[CONTRIBUTING.md](CONTRIBUTING.md#testing-the-release-pipeline-locally).
