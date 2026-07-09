# Integration: single `unbounded-system` namespace + operator-driven migration

Status: in progress
Integration branch: `feature/unbounded-system`

This document is the source of truth for the namespace-consolidation effort. It
lives on the integration branch and its checklist is kept current as each chunk
is opened and merged.

## Problem

Unbounded installs its components across several namespaces: machina, metalman,
and unbounded-storage land in `unbounded-kube`, unbounded-net lands in
`unbounded-net`, and the operator itself runs in `unbounded-kube` (orca,
inventory, machine-ops, and gantry add still more). The split is historical
rather than meaningful. It fragments RBAC, namespace-scoped discovery,
NetworkPolicy, and day-2 operations, and there is no single namespace that
represents "Unbounded" on a cluster.

We want one configurable namespace, `unbounded-system`, for the operator and
everything it manages, plus a safe, automated path to move existing clusters
(already running the split layout) onto it. Kubernetes namespaces cannot be
renamed in place, so existing clusters need a deliberate migration, not a config
flip.

## Strategy: integration branch, `main` stays releasable

The change is large and spans three independent bodies of work. To keep `main`
releasable and reviewable mid-development, none of the pieces land on `main`
directly. Instead they flow into this integration branch as separate,
digestible PRs; `main` receives only the single final integration -> main merge.

```
main ───────────────────────────────────────────►(final merge)──► main
   └─ feature/unbounded-system ◄─ PR-B ◄─ PR-A ◄─ PR-C  (review chunks here)
```

A draft umbrella PR (#388) tracks the integration branch against `main`; it is
the single merge that lands the whole change and stays a draft until every chunk
has merged into the integration branch. Every related PR carries an
`[unbounded-system]` title prefix so they are easy to identify together.

The integration branch is periodically kept current by merging `main` into it
(drift is resolved here, never on `main`).

## Decomposition

| PR | Scope | Approx files | Source | Depends on |
|----|-------|--------------|--------|------------|
| **PR-B** | Namespace unification: `UNBOUNDED_NAMESPACE`, `internal/unbounded`, template defaults across machina/net/gantry/inventory/orca/machine-ops/storage | ~194 | existing open PR #372 (`unify-namespace-unbounded-system`), re-targeted to this branch | - |
| **PR-A** | Operator + Site redesign **and** operator-driven migration: the operator on `unbounded-system`, the Site API move to the machina group, the component-model redesign (net/machina cluster singletons, per-site metalman/storage, components install at the operator version), kubectl install consolidation, net controller/webhook/client retargeting, and the reaper (`internal/operator/migrate.go`) with its RBAC and e2e | net-new + closed PR #353 (`shared-site`) | PR-B |

PR-B and PR-A are independent to review (PR-B never touches the operator). PR-A
lands as two logical commits: (1) the operator + component-model redesign, and
(2) the operator-driven migration (Site translation + reaper). **PR-C has been
dropped**: the migration is small and cohesive with the redesign it depends on
(it reaps exactly the components the redesign reshapes), so it folds into PR-A
rather than living as a separate chunk.

## Migration workflow (delivered by PR-A, commit 2)

Migration runs continuously as an always-on, leader-elected manager Runnable
gated by the operator flag `--reap-legacy-resources` (default on). There is no
one-shot subcommand and no standalone shell migrator. The routine is idempotent;
per pass:

0. **Translate Sites.** For each pre-redesign net-group Site
   (`sites.net.unbounded-cloud.io`), create the equivalent machina-group Site
   (`sites.unbounded-cloud.io`) with the networking spec copied verbatim and
   `spec.components` inferred from the running legacy workloads: storage is
   enabled on every translated Site that had a legacy storage DaemonSet (each
   then gets its own node-selected DaemonSet and an operator-managed
   `unbounded-storage-config-<site>` ConfigMap seeded from the legacy shared
   storage config), machina on the `cluster` Site, and metalman where a
   per-site metalman Deployment is detected. Existing machina-group Sites are
   never clobbered.
1. Copy non-regenerable state (operator/user Secrets and the `machina-config`
   ConfigMap) from the legacy namespaces into `unbounded-system`, stripping
   server-managed metadata and skipping regenerable/auto-managed secrets (net
   serving cert, SA tokens, Helm release secrets). Secrets are copied only if
   absent; `machina-config` is upserted so config migrated from the legacy
   namespace wins over any default the component reconciler already created,
   and is then preserved once the source namespace is drained. Storage config is
   copied into per-Site `unbounded-storage-config-<site>` ConfigMaps (created
   from the embedded default when absent; adopted/preserved when present).
2. Rewrite the namespace embedded in cluster-scoped secret references
   (`Machine.spec.pxe.redfish.passwordRef`, `MachineOperationCredential`
   `spec.auth.secretRef`), and copy each Machine's cloud-init user-data
   ConfigMap (`Machine.spec.pxe.cloudInit.userDataConfigMapRef`) out of the
   legacy namespace into `unbounded-system`, repointing the reference so it
   survives the legacy namespace deletion.
3. Per component, once its target workloads are healthy, delete the
   operator-owned resources left behind in the legacy namespace (label-scoped;
   net reaped last; components with no legacy footprint skipped).
4. Once every component is drained, delete the legacy net-group Site CRD and the
   now-drained legacy `Namespace` objects.

Ordering makes the cutover safe: translating the Sites first gives the operator
Sites to reconcile (bringing up the new `unbounded-system` components, including
per-site metalman and the net singleton that re-labels nodes for storage
node-selection) before the reaper drains the old namespaces; the per-component
health gate ensures the new workload is Ready before the old one is removed.

## Review fixes (PR #372 review)

The initial #372 review raised four findings; disposition:

- **1 (metalman PXE lease, High):** resolved by removing the shell migrator (it
  copied live PXE Deployments verbatim, carrying old images / no `POD_NAMESPACE`)
  and having the operator recreate metalman fresh. PR-A additionally injects
  `POD_NAMESPACE` on the operator-created metalman Deployment so its lease is
  correct under any install namespace.
- **2 (release-upgrade.yaml, High):** not #372's; PR-A restructures that workflow
  and makes it `unbounded-system`-aware. Gated by this integration branch -
  no unified release is cut from `main` until PR-A merges (see below).
- **3 (namespace override not end-to-end, Medium):** fixed in #372 - namespace
  resolution is centralized in `unbounded.SystemNamespace()`, a POD_NAMESPACE-aware
  helper (the raw default is the unexported `systemNamespace` const). Components
  read their runtime namespace from the Downward-API `POD_NAMESPACE` the
  Deployments inject, so a non-default install namespace lines up end to end;
  `kubectl unbounded machine register --namespace` aligns the client-written SSH
  secret/ref. The Makefile documents this.
- **4 (docs point at kube-system, Low):** fixed in #372 - net day-2 docs now
  reference `unbounded-system`.

**Release gate:** `main` must not cut a unified (`unbounded-system`) release
until PR-A has merged into the integration branch, because `release-upgrade.yaml`
only becomes `unbounded-system`-aware in PR-A. The integration branch enforces
this: #372's manifest changes and PR-A's workflow changes reach `main` together
in the single integration -> main merge.

## Base strategy and merge flow

All PRs target `feature/unbounded-system` directly. Dependents are rebased
reactively as their prerequisites merge (PR-A after PR-B). PRs are merged by
reviewers after approval; the author does not self-merge. Each PR must be green
on `make build`, `make lint`, `make test`; PR-A's migration commit must
additionally pass the kind operator-reap e2e (`e2e/operator`, `//go:build e2e`),
which the `operator-e2e-kind` GitHub workflow runs on changes under
`internal/operator`, `cmd/unbounded-operator`, `deploy/unbounded-operator`,
`deploy/machina/crd`, or `e2e/operator`.

Two complementary reaper e2e's exist:

- **In-process simulation** (`e2e/operator`, Go, `//go:build e2e`, per-PR): drives
  the `LegacyReaper` library directly against a kind apiserver with staged inert
  legacy workloads and Ready targets. Fast; covers the reaper logic including
  storage/metalman reaping and the per-site storage gate.
- **Faithful released-version upgrade** (`hack/operator-upgrade-e2e/e2e.py`,
  standalone Python, run by the `operator-upgrade-e2e` workflow on
  `workflow_dispatch` + nightly): stands up a kind cluster with a local OCI
  registry, installs the last released multi-namespace version (default
  `v0.1.19`) via that release's real `kubectl unbounded site init`
  (`--manage-cni-plugin=false` so the real net-node coexists with kindnet), then
  upgrades via this tree's `kubectl unbounded install` and asserts the operator's
  reaper migrates net + machina for real. Storage (RDMA) and metalman (PXE)
  cannot run in vanilla kind, so they are excluded from this path and stay
  covered by the simulation + unit tests. The driver is self-contained so it runs
  the same way locally (`python3 hack/operator-upgrade-e2e/e2e.py all`) and in CI.
  The e2e also verifies the node site-label dual-write and a clean
  unbounded-net-controller restart.

Operational notes from the review hardening:

- `net.unbounded-cloud.io/site` is deprecated in favor of
  `unbounded-cloud.io/site`. During the deprecation window net dual-writes both
  labels and storage/metalman target either key so they can schedule before the
  upgraded net has converged node labels. Remove the deprecated affinity term in
  the same future release that removes the deprecated write/fallback.
- Net and machina singletons are no longer auto-deleted when the last Site is
  removed (or when no Site enables machina). Net is the dataplane and deleting
  net-node can cause a cluster-wide outage; singleton removal should be handled
  by a future explicit uninstall flow.
- The hostNetwork net-controller and metalman Deployments use RollingUpdate with
  `maxSurge: 0`, so rollouts terminate the old pod before the new one starts and
  can bind the same host ports. A broken new image can therefore cause a brief
  zero-available window; roll back on failed rollout and alert on
  `AvailableReplicas == 0`.
- Custom install namespaces are supported by manifest rewriting, but are not
  currently covered by the faithful upgrade e2e and should be treated as
  experimental until that path is exercised.

## Status checklist

- [x] Create `feature/unbounded-system` from `main`                         (me)
- [x] Add this plan doc to the integration branch                            (me)
- [x] Open umbrella tracking PR #388 (`feature/unbounded-system` -> `main`)   (me opens / reviewers merge last)
- [x] PR-B: re-target #372 -> `feature/unbounded-system`                      (me opens / reviewers merge)
- [x] PR-A: open operator + Site redesign -> integration                     (me opens / reviewers merge)
- [x] Apply #372 review fixes (findings 1-4 across #372 + PR-A)               (me)
- [x] PR-B: re-target #372 -> `feature/unbounded-system`                      (me opens / reviewers merge)
- [x] PR-B: merged into integration                                          (reviewers)
- [x] PR-A: open operator + Site redesign -> integration                     (me opens / reviewers merge)
- [x] Apply #372 review fixes (findings 1-4 across #372 + PR-A)               (me)
- [x] PR-A commit 1: operator + component-model redesign                      (me)
- [x] PR-A commit 2: operator-driven migration (Site translation + reaper)   (me)
- [x] PR-A: kind operator-reap e2e for the always-on reaper, wired into CI    (me)
- [x] PR-A: faithful released-version upgrade e2e driver + nightly workflow    (me)
- [ ] Periodic `main` -> integration syncs                                   (me)
- [ ] Mark tracking PR #388 ready once all chunks merged                      (me; reviewers merge)
- [ ] Close monolith #383                                                    (me, on approval)

### PR tracking

| PR | Branch | Number | State |
|----|--------|--------|-------|
| Tracking (umbrella) | `feature/unbounded-system` -> `main` | #388 | draft (merges last, after all chunks) |
| PR-B | `unify-namespace-unbounded-system` | #372 | merged into `feature/unbounded-system` |
| PR-A | `operator-site-redesign` | #386 | open (operator+model and migration commits; awaiting review/merge) |
| ~~PR-C~~ | - | - | dropped; folded into PR-A |

## Notes

- The known-good, fully-integrated reference is kept on `unbounded-operator-poc`
  until PR-A's migration commit is validated, so no validated work is lost.
- #353 is closed; its branch `shared-site` is the source for PR-A.
- The monolith PR #383 (everything in one) is superseded by this decomposition
  and is closed once the chunk PRs are open.
