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
| **PR-B** | Namespace unification: `UNBOUNDED_NAMESPACE`, `internal/unbounded`, template defaults across machina/net/gantry/inventory/orca/machine-ops/storage, `migrate-namespace.sh`, smoke test | ~194 | existing open PR #372 (`unify-namespace-unbounded-system`), re-targeted to this branch | - |
| **PR-A** | Operator + Site redesign: the operator, the Site API move to the machina group, kubectl install consolidation, net controller/webhook/client retargeting | ~94 | closed PR #353 (`shared-site`), reconciled with `main` | - |
| **PR-C** | Operator on `unbounded-system` + operator-driven migration: operator template/Go namespace defaults, the reaper (`internal/operator/migrate.go`), `migrate-legacy` subcommand, RBAC, release-upgrade, docs, and the `operator-reap` e2e | ~24 | net-new | PR-A and PR-B |

PR-B and PR-A are independent of each other (PR-A is the operator in the old
namespaces; PR-B never touches the operator). PR-C makes the operator and its
migration consistent with the unified namespace and carries the only genuinely
novel, higher-risk logic (the reaper), isolated for focused review.

## Migration workflow (delivered by PR-C)

Migration runs either continuously (operator flag `--reap-legacy-resources`, off
by default) or as a one-shot (`unbounded-operator migrate-legacy`). Both run the
same idempotent routine; per pass:

1. Copy non-regenerable state (operator/user Secrets, the `machina-config`
   ConfigMap) from the legacy namespaces into `unbounded-system`, stripping
   server-managed metadata, never overwriting existing target copies, and
   skipping regenerable/auto-managed secrets (net serving cert, SA tokens, Helm
   release secrets).
2. Rewrite the namespace embedded in cluster-scoped secret references
   (`Machine.spec.pxe.redfish.passwordRef`, `MachineOperationCredential`
   `spec.auth.secretRef`).
3. Per component, once its target workloads are healthy, delete the
   operator-owned resources left behind in the legacy namespace (label-scoped;
   net reaped last; components with no legacy footprint skipped).

The operator never deletes the legacy `Namespace` objects; an operator removes
those manually once empty (`kubectl delete namespace unbounded-kube unbounded-net`).

## Base strategy and merge flow

All PRs target `feature/unbounded-system` directly. Dependents are rebased
reactively as their prerequisites merge (PR-A after PR-B; PR-C is prepared only
after PR-A and PR-B are merged). PRs are merged by reviewers after approval; the
author does not self-merge. Each PR must be green on `make build`, `make lint`,
`make test`; PR-C must additionally pass the kind `operator-reap` scenario in
`hack/smoke-namespace-migration.py`.

## Status checklist

- [x] Create `feature/unbounded-system` from `main`                         (me)
- [x] Add this plan doc to the integration branch                            (me)
- [x] Open umbrella tracking PR #388 (`feature/unbounded-system` -> `main`)   (me opens / reviewers merge last)
- [x] PR-B: re-target #372 -> `feature/unbounded-system`                      (me opens / reviewers merge)
- [x] PR-A: open operator + Site redesign -> integration                     (me opens / reviewers merge)
- [ ] PR-A: rebase after PR-B merges                                         (me)
- [ ] PR-C: open operator-ns + migration -> integration (after A+B merged)   (me opens / reviewers merge)
- [ ] Periodic `main` -> integration syncs                                   (me)
- [ ] Mark tracking PR #388 ready once all chunks merged                      (me; reviewers merge)
- [ ] Close monolith #383                                                    (me, on approval)

### PR tracking

| PR | Branch | Number | State |
|----|--------|--------|-------|
| Tracking (umbrella) | `feature/unbounded-system` -> `main` | #388 | draft (merges last, after all chunks) |
| PR-B | `unify-namespace-unbounded-system` | #372 | open, base = `feature/unbounded-system` (awaiting review/merge) |
| PR-A | `operator-site-redesign` | #386 | open (awaiting review/merge; rebase on integration after PR-B merges) |
| PR-C | `operator-system-migration` | TBD | blocked on PR-A + PR-B merging |

## Notes

- The known-good, fully-integrated reference is kept on `unbounded-operator-poc`
  until PR-C is cut, so no validated work is lost during the re-cut.
- #353 is closed; its branch `shared-site` is the source for PR-A.
- The monolith PR #383 (everything in one) is superseded by this decomposition
  and is closed once the chunk PRs are open.
