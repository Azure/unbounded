# Releasing Unbounded Kubernetes

How to cut, soak, and publish a release, and what to do when one of those goes
wrong. Maintainer-facing; for using a release, see the
[documentation site](https://unbounded-cloud.io/).

Releases are cut from `main` only. There are no maintenance branches today (see
[Hotfixes](#hotfixes)).

## At a glance

Three workflows, in order. Only the first is started by a human.

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
failure is a release failure you have not had yet.

Note that a nightly failure caused solely by unreachable nodes is tolerated and
reported rather than failing the run; see
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

The prerelease suffix is `rc.N`. `alpha` and `beta` were previously used
interchangeably with no defined meaning; do not add new ones.

Iterate a train by re-running prepare with the **same** `bump` and an
incremented `pre`: `rc.1`, then `rc.2`, and so on. Promote when it is good.

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
tag; it builds nothing, so a mistake here is cheap to undo.

```sh
# Cut a release candidate. Bump is relative to the latest FINAL tag.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=prerelease -f bump=patch -f pre=rc.1

# Iterate the train. Same bump, next rc.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=prerelease -f bump=patch -f pre=rc.2

# Promote the train to its final version. No bump: v0.2.5-rc.2 becomes v0.2.5.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=promote -f version=v0.2.5

# Cut a final release directly, with no candidate.
gh workflow run release-prepare.yaml --repo Azure/unbounded \
  -f mode=release -f bump=patch
```

### Three things that bite

**`bump` is relative to the latest final tag, every time.** It is not
remembered across a train. If you cut `v0.2.5-rc.1` with `bump=patch` and then
run `rc.2` with `bump=minor`, you get `v0.3.0-rc.2` and now have two live
trains.

**`pre` is not validated.** Nothing checks that `rc.2` follows `rc.1`. Skipping
or repeating a number is accepted silently.

**`promote` with a blank `version` picks the highest prerelease in the whole
repository, not the train you were working on.** If two trains are live it will
silently finalise the wrong one and orphan the other. This has happened: the
`v0.1.24` train reached `rc.18` and was abandoned when a `v0.2.0` train started
alongside it. **Always pass `version` explicitly.**

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
4. waits for every component workload to roll out,
5. runs the smoke tests in `hack/release/smoke/`,
6. and only then flips the draft to published.

A clean deploy plus green smoke is the soak gate. **Publishing is not a manual
step.** If you find yourself running `gh release edit --draft=false` by hand,
use the [break-glass path](#break-glass) instead so the bypass is recorded.

Prereleases are published too, but stay flagged as prereleases and never become
"Latest".

### Degraded clusters

A DaemonSet counts every node toward its desired count, including nodes the
cluster has lost contact with, so one dead node would otherwise block every
release forever. The gate tolerates a shortfall when it is caused **only** by
NotReady nodes and everything on a reachable node is healthy.

`MAX_NOTREADY_NODES` caps how much is excusable: **2** for the release deploy,
**0** for the nightly. Beyond the cap the shortfall is not excused and the
rollout fails, with the offending nodes named and grouped by site.

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
- The forced path uses the `release-force-publish` environment, so it can be
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
starts clean.

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

## Hotfixes

**Not currently supported.** `release-prepare.yaml` always cuts from `main`, so
there is no way to patch an older release without shipping everything merged
since. The agreed model, not yet implemented, is a `release-X.Y` branch created
on demand from the release tag, with fixes cherry-picked from `main` and patch
releases cut from the branch. Tracked in
[#610](https://github.com/Azure/unbounded/issues/610).

Until then the only supported remedy is rolling forward, which matches the
support policy in [SECURITY.md](SECURITY.md): fixes land on the latest release
and `main`, and older releases are not routinely supported.

## Rehearsing locally

`./hack/test-release-local.sh` rehearses the build phase on a workstation. See
[CONTRIBUTING.md](CONTRIBUTING.md#testing-the-release-pipeline-locally).
