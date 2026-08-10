# Component Workload Overrides

**Status:** Draft for team review
**Scope:** A mechanism for users to customize the Deployments and DaemonSets that
unbounded-operator generates and reconciles.

---

## Table of contents

1. [Problem statement](#1-problem-statement)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [Constraints from the current design](#3-constraints-from-the-current-design)
4. [Security model](#4-security-model)
5. [Alternatives considered](#5-alternatives-considered)
6. [Format](#6-format)
7. [Merge semantics](#7-merge-semantics)
8. [Permitted and protected fields](#8-permitted-and-protected-fields)
9. [Failure and update semantics](#9-failure-and-update-semantics)
10. [Wiring](#10-wiring)
11. [Drift visibility and observability](#11-drift-visibility-and-observability)
12. [CLI](#12-cli)
13. [Operational notes](#13-operational-notes)
14. [Implementation plan](#14-implementation-plan)
15. [Testing](#15-testing)
16. [Prior art](#16-prior-art)
17. [Open questions](#17-open-questions)

---

## 1. Problem statement

unbounded-operator generates and reconciles the workloads for five components:
`net`, `machina`, `gantry` (cluster singletons) and `metalman`, `storage`
(per-Site). Today a user's entire influence over the shape of those workloads is:

- `spec.components.<c>.enabled` on the Site (`api/machina/v1alpha3/site_types.go:147`)
- `spec.components.metalman.replicas` (`site_types.go:167`)
- `spec.components.metalman.dhcpAutoInterface` (`site_types.go:161`)
- The operator-wide `--image-registry` flag (`cmd/unbounded-operator/main.go:76`)

This is deliberate. `SiteComponentSpec` says so directly at
`api/machina/v1alpha3/site_types.go:141-143`:

> Components install into the unbounded-system namespace at the operator's own
> version, so neither namespace nor image is configurable per component.

And `docs/content/reference/architecture.md:189`:

> The manifests below are plain numbered YAML files (no Helm or Kustomize) that
> the operator renders and applies; they are not meant to be applied by hand.

**This document proposes reversing that stance.** It is a deliberate change and
both statements above are amended by the implementation.

The requests that motivate it, collected from users and the team:

| Requirement | Reachable today |
|---|---|
| CPU/memory requests and limits | No |
| Tolerations, nodeSelector, affinity | No |
| Sidecar containers and extra volumes | No |
| Environment variables | No |
| Command-line arguments | No |
| imagePullSecrets and private registry auth | Registry prefix only, cluster-wide |
| Labels, annotations, priorityClassName | No |
| Container images and tags | Registry prefix only, cluster-wide |

Every item is a field inside a `Deployment` or `DaemonSet` the operator already
emits. None of them requires creating, renaming, or deleting an object. That
observation shapes the entire design.

Because the operator applies with server-side apply and `ForceOwnership`
(`internal/operator/component/env.go:242`), there is no workaround. A
`kubectl edit` on a managed workload is reverted on the next reconcile, and a
GitOps controller managing the same object will fight the operator indefinitely.
Users currently have no supported and no unsupported path.

## 2. Goals and non-goals

### Goals

- Cover every requirement in the table above through a single mechanism.
- Bound the blast radius: a user cannot create, rename, retarget, or delete an
  object, and cannot break the operator's ability to find and reconcile what it
  manages.
- Introduce no new Go dependency.
- Make deviation from operator defaults visible in `kubectl get site` and on the
  affected workload, so support can identify a customized install immediately.
- Reuse the existing reconcile, watch, and status-condition plumbing rather than
  adding a parallel path.
- Behave identically for the four components rendered from embedded YAML and for
  `metalman`, which is built from typed Go structs.

### What the project supports

**Syntactic compatibility** of the mechanism is supported and versioned:

- The document schema, gated by a required `apiVersion`.
- The merge semantics, including patch strategy, additive scheduling, and
  `extraArgs` precedence.
- The set of permitted and protected paths, which may gain permitted paths
  within an `apiVersion` but will not lose them.
- The failure model in [§9](#9-failure-and-update-semantics), including that
  invalid overrides skip rather than revert.
- That a document accepted by one release continues to **parse and validate** in
  later releases within its declared `apiVersion`.

**Revert behavior**, scoped precisely: removing an override restores the
operator default for fields the operator currently declares, on objects the
operator currently emits.

### What the project does not support

**Target resolution is release-specific and explicitly not guaranteed.** A
document that parses is not a document that resolves. Container names, volume
names, and the shape of generated workloads are implementation details and may
change in any release. A patch naming a container that a later release renames
is a *resolution* failure, reported as a Degraded condition per
[§9](#9-failure-and-update-semantics), not a schema failure. Separating these is
what makes the syntactic promise above honest.

Also not supported:

- That a component with an override applied functions correctly, meets its
  health checks, or upgrades cleanly.
- Any behavior of a component running an image other than the operator's own
  version (see [§11](#11-drift-visibility-and-observability)).
- Revert of anything outside the scope stated above. Server-side apply only
  reclaims fields the operator still declares and still owns, so state can
  persist when it was introduced by an admission controller, is owned by a
  competing field manager, or belongs to an object the operator has stopped
  emitting. The operator does not prune
  ([§3.4](#3-constraints-from-the-current-design)).

### Non-goals

- Creating, renaming, or deleting objects. Only workloads the operator already
  emits can be patched, and only in place.
- Patching RBAC, Services, webhooks, APIServices, ConfigMaps, or CRDs. Component
  configuration already has an escape hatch: the operator seeds each component
  ConfigMap only when absent and never overwrites user content
  (`net.go:156-187`, `machina.go:155-205`, `gantry.go:236-267`,
  `storage.go:115-154`).
- Patching the operator's own Deployment, which is applied by
  `cmd/kubectl-unbounded/app/install.go` under a different field manager and is
  not operator-managed.
- Replacing the build-time `render-manifests` templating in `deploy/`. That
  remains how defaults are authored.

## 3. Constraints from the current design

Five properties of the existing operator bound what any solution can look like.

**3.1 One server-side-apply chokepoint.** Every object the operator writes goes
through `Env.ApplyObject` (`internal/operator/component/env.go:240`) with field
manager `unbounded-operator` and `client.ForceOwnership`. The four YAML-driven
components reach it via `applyManifestData` (`env.go:202-237`); `metalman`
calls it directly (`metalman.go:49`). It is the only point both paths share.

**3.2 Out-of-band edits cannot work.** `ForceOwnership` means the operator wins
every field it declares. Customization must be an input to the operator, not a
mutation of its output.

**3.3 Version lockstep is an explicit invariant.** Component images resolve as
`<ImageRegistry>/<repository>:<ImageTag>` (`env.go:83`) where `ImageTag` is the
operator's own compiled version (`cmd/unbounded-operator/main.go:207`).
`internal/operator/manifests_guard_test.go` enforces that no embedded manifest
carries a floating tag. Any mechanism that can set an image is an opt-out from
this invariant and must be treated as such.

**3.4 The operator does not prune.** Nothing reconciles away an object the
operator has stopped emitting. Cluster singletons are deliberately retained even
when no Site enables them (`machina.go:57-69`). A mechanism that can rename or
add resources therefore creates permanently orphaned objects.

**3.5 Two different component scopes.** `net`, `machina`, and `gantry` are
cluster singletons whose enablement is resolved as "any Site enables it"
(`machina.go:47-55`, `gantry.go:81-95`). `metalman` and `storage` are per-Site,
producing objects named from the Site (`metalman.go:100`, `storage.go:241`). Any
configuration surface hung off an individual Site is ambiguous for the
singletons.

**3.6 Two different generation paths.** Four components decode embedded YAML into
`unstructured.Unstructured` and run a mutator (`net.go:115`, `machina.go:123`,
`gantry.go:187`, `storage.go:223`). `metalman` builds a typed
`appsv1.Deployment` in Go (`metalman.go:98-196`). A mechanism implemented at the
YAML layer would not apply to `metalman`.

**3.7 Apply is GVK-directed and the operator holds `escalate` and `bind`.**
`ApplyObject` derives the target resource from the object's own
`apiVersion` and `kind` (`env.go:241`) and performs no kind assertion. The
operator's ClusterRole grants `escalate` and `bind` on `roles`, `rolebindings`,
`clusterroles`, and `clusterrolebindings`
(`deploy/unbounded-operator/02-rbac.yaml.tmpl:60-66`), deliberately, so it can
install component RBAC granting permissions it does not itself hold. Any
mechanism that can influence the GVK of an applied object can therefore mint a
cluster-admin binding. GVK containment is not optional.

**3.8 The workloads are already maximally privileged.** This bounds what any
field restriction can achieve.

| Workload | Privilege |
|---|---|
| `unbounded-net-node` | `hostNetwork: true`, `hostPID: true` (`deploy/net/node/03-daemonset.yaml.tmpl:32-33`), `privileged: true` on two containers (`:53`, `:108`), four hostPath mounts (`:125-137`) |
| `unbounded-storage-supervisor` | `privileged: true` (`04-daemonset.yaml.tmpl:64`, `:100`), three hostPath mounts (`:103-111`) |
| `metalman` | `HostNetwork: true` (`metalman.go:165`) |
| `gantry` | `hostNetwork: false` by deliberate design decision, one hostPath cache mount (`daemonset.yaml.tmpl:99`, `:284`) |

## 4. Security model

### 4.1 Threat model

**Write access to the overrides ConfigMap is equivalent to root on every node in
every affected Site, and therefore to cluster-admin.** This is a property of the
mechanism, not a defect in it, and it must be stated plainly in user-facing
documentation.

The reasoning follows from [§3.8](#3-constraints-from-the-current-design). The
workloads being patched already run with `hostNetwork`, `hostPID`,
`privileged: true`, and hostPath mounts of the host root filesystem. Against
pods in that state:

- Rejecting `securityContext.privileged: true` achieves nothing. The containers
  are already privileged.
- Rejecting new `hostPath` volumes achieves nothing. The host filesystem is
  already mounted.
- Changing a container **image** substitutes arbitrary code into a privileged,
  host-namespaced process on every node in the Site.
- Changing **args** or **command** does the same.
- Injecting an **environment variable** into an existing privileged container
  does the same, for example through `LD_PRELOAD`.
- Adding a **sidecar** does the same. `hostNetwork` and `hostPID` are pod-level
  and inherited by every container, and the existing hostPath volumes are
  mountable by any container in the pod.

A field allowlist bounds this only if image, args, command, env, sidecars, and
volumes are all excluded. That is precisely the closed typed set rejected in
[§5.2](#52-why-not-a-typed-override-struct), and it removes five of the eight
requirements in [§1](#1-problem-statement).

The requirement set and containment are therefore in direct conflict. This
design resolves that conflict by **accepting the privilege level and being
explicit about it** rather than by implying a boundary that does not exist.

### 4.2 The allowlist is an integrity control, not a security control

Given [§4.1](#41-threat-model), the restrictions in
[§8](#8-permitted-and-protected-fields) are not containing an adversary. Their
purpose is to stop a legitimate, authorized operator from accidentally breaking
the operator's ability to reconcile what it manages: severing a workload from
its selector, detaching it from its ServiceAccount and therefore its RBAC,
collapsing per-Site node isolation, or clobbering the annotations the operator
uses to detect drift.

Two exceptions in [§8](#8-permitted-and-protected-fields) are genuine security
controls rather than integrity controls, because they escape the workload
boundary entirely rather than merely damaging a workload:

- **GVK** ([§3.7](#3-constraints-from-the-current-design)). Without containment,
  an override applies as any resource the operator's ClusterRole permits,
  including `ClusterRoleBinding` with `escalate` and `bind`. This converts
  node-root into cluster-admin without touching a node.
- **`serviceAccountName`.** Retargeting a workload at a different ServiceAccount
  borrows that identity's API permissions rather than the host's.

### 4.3 Required posture for cluster operators

Documentation must instruct operators to treat write access to
`unbounded-component-overrides` with the same care as write access to
`clusterrolebindings`:

- Grant it to cluster administrators only. Do not include it in namespace-wide
  ConfigMap grants for platform or application teams.
- Audit changes to it. The override hash annotations in
  [§11](#11-drift-visibility-and-observability) make cluster-side changes detectable, and the
  object itself should be under change control.
- Prefer managing it through the same review path as RBAC changes.

### 4.4 Residual risk and how it is closed

Exactly one component ServiceAccount can write an arbitrary ConfigMap in the
operator namespace:

| Role | Grant | Assessment |
|---|---|---|
| `machina-controller` | `configmaps`, all verbs, no `resourceNames`, no admission constraint (`deploy/machina/02-rbac.yaml.tmpl:15-17`) | **Over-granted.** Can write the overrides ConfigMap. |
| `metalman-controller` | `configmaps` `["get","list","watch"]` in the operator namespace; `resourceNames: ["cluster-info","aks-cluster-metadata"]` with `["get"]` in `kube-public` (`06-metalman-rbac.yaml.tmpl:59-61`, `:132-133`) | Read-only. Not a vector. |
| `unbounded-net-controller` | `configmaps` `["create"]` namespace-wide, plus `resourceNames: ["unbounded-net-serving-ca"]` for `["get","update","patch"]` (`net/controller/02-rbac.yaml.tmpl:169-176`) | Constrained. See below. |

`unbounded-net-controller` holds namespace-wide `create` because RBAC cannot
scope that verb, but it is already constrained at admission by
`deploy/net/controller/09-vap.yaml.tmpl`, a `ValidatingAdmissionPolicy` with
`failurePolicy: Fail` that rejects any ConfigMap create from that ServiceAccount
not named `unbounded-net-serving-ca`. It cannot create the overrides ConfigMap.

So the exposure is `machina-controller` alone: a compromised machina controller
can write the overrides ConfigMap and cause the operator to deploy arbitrary
code on every node. This is a pre-existing over-grant that this design makes
materially more dangerous.

**The gap is closable, and the project has already solved it once.** The comment
at the head of `09-vap.yaml.tmpl` states the problem exactly:

> RBAC cannot scope the "create" verb by resourceNames, so this VAP ensures the
> controller can only create resources with the expected names.

The same pattern applies here. Hardening machina has two parts, and both are
**blocking prerequisites** rather than independent work, because the feature
widens an exposure that the hardening removes:

1. Narrow the machina Role to `resourceNames` for `get`, `update`, `patch`, and
   `delete`, matching the shape already used for net.
2. Add a `ValidatingAdmissionPolicy` restricting machina's ConfigMap `create` to
   the names it legitimately creates, modelled directly on
   `09-vap.yaml.tmpl`.

With both in place, no component ServiceAccount can create or modify the
overrides ConfigMap. Write access is then held only by principals a cluster
administrator has granted it to, which is the posture
[§4.3](#43-required-posture-for-cluster-operators) requires.

**This resolves the ConfigMap versus dedicated-resource question.** An earlier
revision argued that only a dedicated resource type could scope `create`, and
recorded the choice as an open question on that basis. That argument was wrong:
admission policy scopes `create` by name, the repository already relies on this,
and a dedicated resource type is therefore not required to reach the same
posture. The ConfigMap is retained.

## 5. Alternatives considered

| | Mechanism | Expressiveness | Mechanism reach | Validation | Cost |
|---|---|---|---|---|---|
| A | Kustomize as a library | Unlimited | Any object, any GVK | None practical | High |
| B | Typed override struct on the CRD | Closed set of fields | Chosen fields | Full OpenAPI | Low, permanent field creep |
| C | Strategic merge over allowlisted paths on operator-emitted workloads | Allowlisted paths, open values | One workload object, GVK pinned | Allowlist, protected-path re-stamp, apply-time GVK assertion | Low |
| D | SSA field disownership | "Let X own this" only | Chosen paths | N/A | Very low |
| E | User-supplied `MutatingAdmissionPolicy` | High | Any object admission sees | CEL type-checked | Zero code |

**Mechanism reach is not a security boundary.** The column describes what each
mechanism can address, not what an authorized writer can ultimately achieve.
Per [§4.1](#41-threat-model), write access to option C is already
cluster-admin-equivalent. Reach matters because it determines what can be
*contained*, which is the subject of [§5.1](#51-why-not-kustomize-as-a-library).

**C is the proposal.** The reasoning for rejecting the others follows.

### 5.1 Why not kustomize as a library

Kustomize was the team's initial suggestion. It is rejected here not because it
is a poor tool but because its contract mismatches this operator on five axes.

**Unbounded transformation with no pruning.** A kustomization can rename,
delete, add, and re-kind resources. Combined with [§3.4](#3-constraints-from-the-current-design),
an overlay that renames a DaemonSet orphans the original permanently, and the
operator will happily recreate it on the next pass. There is no reconciliation
model that survives arbitrary resource-graph rewriting without a full pruning
and ownership-tracking implementation, which the operator does not have.

**GVK containment is impossible by construction.** This is the security
argument, and it is not the one it first appears to be. Per
[§4.1](#41-threat-model), write access to a bounded patch surface is *already*
cluster-admin-equivalent, so "kustomize grants more privilege" is not a
distinction that survives scrutiny. The real distinction is containment.

A patch surface can be forced to produce only `apps/v1` `Deployment` and
`DaemonSet` objects, by validation, by re-stamping, and by an assertion
immediately before apply ([§8.5](#85-apply-time-assertion)). An attacker holding
write access is then confined to node compromise, which is severe but requires a
chain. An overlay engine cannot be constrained that way, because selecting group,
version, and kind is precisely what it exists to do. Constraining kustomize to
two GVKs would remove the reason to adopt it.

The difference is therefore between an attacker who must pivot through a
compromised node and one who writes a `ClusterRoleBinding` directly, using the
`escalate` and `bind` verbs the operator holds
([§3.7](#3-constraints-from-the-current-design)). Both are bad. Only one is
containable.

**The internal manifest layout becomes public API.** Overlays reference base
resources by group, kind, name, and namespace. Adopting kustomize would freeze
the file layout under `deploy/*/rendered/` and every object name inside it as a
compatibility surface. That is a far larger commitment than a documented patch
target, and it is one the project would break routinely.

**Input shape mismatch.** Kustomize consumes a filesystem of files. Delivering
that into a cluster means either a ConfigMap holding multiple interdependent
files with no schema and no ordering guarantees, or a git reference, which puts
network access, credential handling, and supply-chain trust inside the operator.
Neither is attractive.

**Error attribution.** A failed `krusty` build yields a string. Turning that into
a per-component Site condition that tells a user which entry was wrong is
awkward, and the operator's status model
(`internal/operator/component/result.go`) is built around per-component
attribution.

Adopting it would also promote `sigs.k8s.io/kustomize/api` from a transitive
dependency of `k8s.io/cli-runtime` (`go.mod:336-337`) to a direct one, pulling
kyaml's YAML stack into the operator alongside the existing one.

### 5.2 Why not a typed override struct

A closed set of fields (`resources`, `tolerations`, `nodeSelector`, `affinity`,
`priorityClassName`) is safe, fully validated by the API server, and
discoverable through `kubectl explain`. It fails on scope: sidecar containers,
extra volumes, environment variables, command arguments, and images are all in
the requirement table in [§1](#1-problem-statement), and a closed set covering
them is not meaningfully closed. The well-trodden path is that projects starting
here add a free-form pod template later and then carry two overlapping surfaces
with an awkward precedence rule. Starting at C avoids that.

This is not contradicted by the allowlist in
[§8.1](#81-permitted), which is a closed set of *paths*, not of fields. The two
differ in what they can express:

| | Typed struct | Path allowlist over a structural merge |
|---|---|---|
| Adding a knob | A named Go field plus CRD schema per knob | Already covered if the path is permitted |
| Values | Constrained by the declared type | Open |
| Addressing something the operator never enumerated, such as a new sidecar or volume | Impossible | Natural, by merge key |
| CRD size | Grows with every knob | Unaffected |

`containers[*].env` as an allowlisted path permits any environment variable on
any container, including one the user added. As a typed field it would permit
exactly what the struct declared.

### 5.3 Why not SSA field disownership

Having the operator stop declaring selected paths so another field manager can
own them is elegant, requires no new API, and is the correct answer to "let VPA
manage resources". It is not a general mechanism: it cannot add a sidecar or a
volume, it requires users to understand field managers, and it makes the
operator's declared field set the configuration surface. It remains a good
complement and is listed in [§17](#17-open-questions).

### 5.4 Why not user-supplied mutating admission

`MutatingAdmissionPolicy` genuinely works with this operator: admission mutates
the operator's own apply request, so the result is attributed to the operator's
field manager and does not fight on reconcile. It requires zero code. It is
rejected as *the* answer because it is invisible in the operator's status,
unsupportable when a user files a bug, cluster-wide rather than per-component,
and requires users to write CEL against object shapes the project does not
document. It remains a legitimate path for users who want it and will be
mentioned in the reference documentation.

## 6. Format

Overrides live in a ConfigMap named `unbounded-component-overrides` in the
operator's namespace. The operator **only reads it**: it is never created, never
seeded, and never written. Absence means no overrides.

A ConfigMap rather than a field on `Site` because:

- It is cluster-scoped by nature, which dissolves the singleton ambiguity in
  [§3.5](#3-constraints-from-the-current-design). No "primary Site" concept is
  needed.
- It keeps the operator's CRD surface free of a large free-form field and avoids
  a conversion obligation when `Site` moves to `v1beta1`.
- It matches the existing component-configuration pattern, where user-owned
  ConfigMaps in the same namespace carry component configuration.

### 6.1 Document schema

Every key in `.data` is parsed as an independent overrides document. This lets
users split by concern or ownership (`kubectl create configmap --from-file`)
without inventing a merge tool.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: storage
    kind: DaemonSet
    sites: [edge-west, edge-east]
    extraArgs:
      run: ["--verbose"]
    patch:
      spec:
        template:
          spec:
            tolerations:
              - key: edge
                operator: Exists
            containers:
              - name: run
                resources:
                  limits:
                    memory: 512Mi
```

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | `overrides.unbounded-cloud.io/v1alpha1`. Missing or unrecognized is a hard error for that key. |
| `component` | yes | One of `net`, `machina`, `gantry`, `metalman`, `storage`. Matched against `ClusterComponent.Name()` / `SiteComponent.Name()`. |
| `kind` | yes | `Deployment` or `DaemonSet`. |
| `sites` | no | Per-Site components only. Absent matches every Site. An explicitly empty list is a validation error. Rejected on `net`, `machina`, and `gantry`. |
| `extraArgs` | no | Map of container name to arguments appended after the patch merges. See [§7.2](#72-extraargs). |
| `patch` | no | Strategic merge patch applied to the whole workload object. See [§7](#7-merge-semantics). |

`component` plus `kind` uniquely identifies every workload the operator emits
today: only `net` emits two workloads and they differ by kind. Users never have
to reconstruct derived names such as `unbounded-storage-<site>`.

At least one of `patch` and `extraArgs` must be present.

### 6.2 Resolution

An entry resolves to zero or more concrete objects:

- For `net`, `machina`, `gantry`: the single named workload of that kind.
- For `metalman`, `storage`: one object per matched Site, named by the
  component's own derivation (`metalman.go:100`, `storage.go:241`).

A name in `sites` that matches no existing Site is **reported, not fatal**.
Writing the ConfigMap before creating the Site is legitimate, and deleting a
Site must not retroactively invalidate an unrelated override. Unmatched names
appear in the component's Site condition message and in
`kubectl unbounded overrides list`.

### 6.3 Multiple entries and conflicts

The normal model is **one entry per workload carrying all of its changes**.
Strategic merge patch is structural, not sequential: a single patch document can
set resources, add a sidecar, add a toleration, and add a volume at once. That
also keeps the entry directly usable with `kubectl patch --type=strategic` for a
dry run.

Multiple entries may nonetheless resolve to the same object, which supports
splitting by ownership (a platform team owning one ConfigMap key, a security
team owning another). When they do:

1. Contributors are grouped **by resolved object**, not by `component` plus
   `kind`. Two entries with `sites: [a, b]` and `sites: [b, c]` are contributors
   to site `b`'s object only.
2. They are composed in deterministic order: sorted ConfigMap key, then document
   order within a key.
3. Composition is rejected only on **true conflict**, meaning two contributors
   assign different values to the same leaf path. Disjoint concerns compose
   silently; genuine disagreement fails with both contributing entries named.
4. A conflict fails only the affected object. In the `[a, b]` and `[b, c]`
   example, site `b`'s workload is not applied and sites `a` and `c` reconcile
   normally.
5. Every contributor is recorded on the workload and in the condition message,
   so overlap is visible even when it composes cleanly.

## 7. Merge semantics

### 7.1 Strategic merge patch

Merging uses `k8s.io/apimachinery/pkg/util/strategicpatch` with patch metadata
from `strategicpatch.NewPatchMetaFromStruct(&appsv1.DaemonSet{})` or the
`Deployment` equivalent. `apimachinery` is already a direct dependency, so this
adds nothing to `go.mod`.

Strategic merge is schema-aware through the `patchStrategy` and `patchMergeKey`
struct tags on the core types. The table below is **raw strategic-merge
behaviour**, which the mechanism follows except where
[§8.4](#84-additive-only-scheduling) deliberately departs from it:

| Field | Raw behaviour | Mechanism |
|---|---|---|
| `containers`, `initContainers` | Merge by `name`. An unknown name adds a container. | As raw |
| `volumes` | Merge by `name`, `retainKeys` | As raw, except operator-declared volumes ([§8.3](#83-protected)) |
| `env` | Merge by `name` | As raw |
| `volumeMounts` | Merge by `mountPath` | As raw |
| `imagePullSecrets` | Merge by `name` | As raw |
| `args`, `command` | Replace the whole list | As raw. See [§7.2](#72-extraargs) |
| `labels`, `annotations` | Map merge | As raw, minus the reserved prefix |
| `tolerations` | **Replace the whole list** | **Appended, never replaced** |
| `nodeSelector` | Map merge | Merged, but overwriting an operator-set key is rejected |
| `affinity.nodeAffinity` `nodeSelectorTerms` | **Replace the whole list** | **User expressions ANDed into each operator term** |

The three departures exist because raw semantics would silently destroy the
mandatory Site affinity that `metalman` and `storage` depend on. See
[§8.4](#84-additive-only-scheduling).

The patch targets the **whole workload object**, not just the pod template, so
`metadata.labels`, `metadata.annotations`, `spec.replicas` and
`spec.updateStrategy` are reachable through the same field as
`spec.template.spec.*`. One field, one code path.

For everything except scheduling, the value of `patch` is a
`kubectl patch --type=strategic` body, so a user can rehearse it against a live
object. That equivalence does **not** hold for `tolerations`, `nodeSelector`, or
`affinity`, where `kubectl patch` replaces and the mechanism appends. Rehearsing
a scheduling patch with `kubectl` will therefore show a more destructive result
than the operator produces. `overrides validate` ([§12](#12-cli)) is the
accurate check.

### 7.2 extraArgs

`args` and `command` carry no `patchMergeKey`, so strategic merge replaces them
wholesale. A user who patches `args` to add one flag silently drops every
operator-injected flag, including `--config-file` and
`--leader-elect-resource-namespace` on the `net` controller
(`deploy/net/controller/03-deployment.yaml.tmpl:114-118`), and will not receive
new ones added in later releases.

`metalman` makes the hazard concrete: its `args` begin with the `serve-pxe`
subcommand followed by `--site=<name>` (`metalman.go:108`). A patch that
replaces `args` drops both, and the container fails to start.

`extraArgs` exists to make the common case safe. It is a map of container name
to a list of arguments, appended to that container's `args` **after** the patch
merges:

```yaml
- component: machina
  kind: Deployment
  extraArgs:
    machina-controller: ["--max-concurrent-reconciles=20"]
```

Precedence is defined and documented: if an entry sets both `patch` (replacing
`args`) and `extraArgs` for the same container, the result is the replaced list
followed by the appended arguments. Naming a container that does not exist in
the generated workload is a **resolution** failure, not a schema failure: it is
scoped to the affected object and reported as Degraded
([§9.4](#94-failure-scope)), because container names are release-specific and
the syntactic compatibility promise in [§2](#2-goals-and-non-goals) must not be
contradicted by one.

## 8. Permitted and protected fields

Each object passes through a fixed pipeline:

```
component mutator            (image resolution, config hash, Site scoping)
  -> Env.RetargetNamespace   (env.go:229)
  -> validate against allowlist
  -> user patch
  -> re-stamp protected paths
  -> assert apps/v1 Deployment|DaemonSet
  -> Env.ApplyObject         (env.go:240, SSA with ForceOwnership)
```

The surface is an **allowlist**: top-level paths not enumerated below are
rejected. An earlier revision of this design used a denylist, which fails open
against fields nobody thought to enumerate.

The allowlist is **fail-closed at the path level and fail-open within a
permitted subtree**. Where a path is marked as a subtree, every descendant is
permitted, including fields added by future Kubernetes releases. That is a
deliberate choice, not an oversight: per
[§4.2](#42-the-allowlist-is-an-integrity-control-not-a-security-control) the
allowlist is an integrity control, and a new field under `securityContext` is
not a new capability for a principal who can already replace the container
image. Enumerating leaves would mean revisiting the list on every Kubernetes
minor release for no security benefit.

Per that same section, these restrictions prevent an authorized operator from
accidentally severing the operator's ability to reconcile a workload. They do
not contain an adversary, with the two exceptions noted below.

### 8.1 Permitted

Paths marked **subtree** permit all descendants. Paths marked **leaf** permit
only the named field.

Within `spec.template.spec`:

| Path | Kind | Notes |
|---|---|---|
| `containers[*].image` | leaf | Breaks version lockstep, reported per [§11](#11-drift-visibility-and-observability) |
| `containers[*].args`, `.command` | leaf | Replace semantics, see [§7.2](#72-extraargs) |
| `containers[*].env`, `.envFrom` | subtree | Merged by name |
| `containers[*].resources` | subtree | |
| `containers[*].volumeMounts` | subtree | Except operator-declared mounts, see [§8.3](#83-protected) |
| `containers[*].securityContext` | subtree | |
| `containers[*].livenessProbe`, `.readinessProbe`, `.startupProbe` | subtree | |
| `volumes` | subtree | Except operator-declared volumes |
| `imagePullSecrets` | subtree | |
| `nodeSelector`, `tolerations`, `affinity` | subtree | Additive only, see [§8.4](#84-additive-only-scheduling) |
| `topologySpreadConstraints` | subtree | |
| `priorityClassName` | leaf | |
| `dnsPolicy`, `dnsConfig` | subtree | |
| `terminationGracePeriodSeconds` | leaf | |

At the workload level:

| Path | Kind | Notes |
|---|---|---|
| `spec.replicas` | leaf | Deployments only |
| `spec.strategy`, `spec.updateStrategy` | subtree | |
| `metadata.labels`, `metadata.annotations` | subtree | Excluding the reserved prefix |
| `spec.template.metadata.labels`, `.annotations` | subtree | Excluding the reserved prefix and selector keys |

### 8.2 Adding containers requires explicit intent

Strategic merge cannot distinguish an intended sidecar from a typo. Both are
"this name is not in the workload", and merging by name silently creates a
container in either case. A patch meaning to raise the memory limit on
`machina-controller` but spelling it `machina-contoller` would add an image-less
container and leave the real limit untouched.

Container names are also release-specific
([§2](#2-goals-and-non-goals)), so a name that was correct at authoring time can
stop matching after an upgrade, with the same silent outcome.

The default is therefore **modify-only**: every container named under
`containers` or `initContainers` in a `patch` must already exist in the
generated workload. Adding one requires naming it explicitly:

```yaml
- component: net
  kind: DaemonSet
  addContainers: [log-shipper]
  patch:
    spec:
      template:
        spec:
          containers:
            - name: log-shipper
              image: fluent/fluent-bit:3.1
            - name: node
              resources:
                limits: {memory: 512Mi}
```

A name in `addContainers` that already exists in the workload is a validation
error, since the intent was to create rather than modify. A name in the patch
that is neither present in the workload nor listed in `addContainers` is a
resolution failure ([§9.4](#94-failure-scope)). The two rules together mean
neither a typo nor a renamed operator container can silently become a sidecar.

### 8.3 Protected

Rejected at validation **and** re-stamped after the merge. Validation gives a
clear error; the re-stamp means correctness does not depend on validation being
exhaustive.

| Path | Reason |
|---|---|
| `apiVersion`, `kind` | **Security.** Apply is GVK-directed and the operator holds `escalate` and `bind` ([§3.7](#3-constraints-from-the-current-design)). |
| `metadata.name`, `.namespace` | Identity. A rename orphans the original, and the operator does not prune ([§3.4](#3-constraints-from-the-current-design)). |
| `metadata.ownerReferences`, `.finalizers` | Garbage collection and Site-scoped cleanup. |
| `spec.selector` | The API server rejects a workload whose template labels do not satisfy its selector. |
| `spec.template.metadata.labels` keys referenced by the selector | Same. |
| `spec.template.spec.serviceAccountName` | **Security.** Retargeting borrows another identity's API permissions. Also detaches the component from its RBAC. |
| `hostNetwork`, `hostPID`, `hostIPC` | Deliberate per-component decisions. `gantry` runs `hostNetwork: false` by design; `net-node` cannot function without `hostNetwork: true`. |
| Labels and annotations under `unbounded-cloud.io/` | Carry config hashes, Site scoping, and override visibility ([§11](#11-drift-visibility-and-observability)). |
| Operator-declared `volumes`, by name | `Volumes` uses `patchStrategy:"merge,retainKeys"` (`k8s.io/api/core/v1/types.go:4145`), so a partial patch silently drops sibling fields of the volume it names. |
| Operator-declared `volumeMounts`, by **container plus `mountPath`** | See below. |

**Mount identity is `(container, mountPath)`, not name.** `volumeMounts` merges
on `patchMergeKey:"mountPath"` (`k8s.io/api/core/v1/types.go:3029`), not on
`name`. Protecting operator mounts by name would therefore be bypassable: a
patch supplying `{name: attacker-volume, mountPath: /host}` merges on the
matching `mountPath` and repoints an operator mount at a different volume while
never colliding on the protected name. Protection is keyed on the merge key that
strategic merge actually uses.

Also rejected:

- **Any `$`-prefixed key** anywhere in the patch. Restricting only `$patch` and
  `$setElementOrder` was incomplete; the directive namespace is open and
  directives can delete operator-managed content.
- **Explicit `null` values.** Strategic merge treats null as deletion, which is
  a second route to removing managed content that a path allowlist alone does
  not catch.

### 8.4 Additive-only scheduling

`metalman` and `storage` place their workloads with a mandatory Site affinity
(`metalman.go:170`, `storage.go:261-268`) built by `SiteNodeAffinity`
(`env.go:503-514`), which is `RequiredDuringSchedulingIgnoredDuringExecution`.

`NodeSelectorTerms` carries no `patchMergeKey`
(`k8s.io/api/core/v1/types.go:3778`), so a patch supplying `nodeSelectorTerms`
**replaces** the operator's terms outright. A user adding an unrelated node
constraint would silently remove Site isolation and allow two Sites' workloads
onto the same nodes, violating the no-retarget goal in
[§2](#2-goals-and-non-goals).

Scheduling constraints are therefore combined rather than patched.

**Node affinity requires a Cartesian product, not appending.**
`SiteNodeAffinity` emits **two** terms, matching the canonical and the
deprecated Site label, and `NodeSelectorTerms` are ORed. Appending the user's
expressions to each operator term is only correct when the user supplies a
single term. Given operator terms `O = [O1, O2]` and user terms `U = [U1, U2]`,
the required semantics are:

```
(O1 OR O2) AND (U1 OR U2)
  = (O1 AND U1) OR (O1 AND U2) OR (O2 AND U1) OR (O2 AND U2)
```

which is the Cartesian product of the two term lists. Each product term carries
the concatenation of both sides' `matchExpressions` **and** both sides'
`matchFields`; `NodeSelectorTerm` has both (`k8s.io/api/core/v1/types.go:3789`,
`:3793`) and dropping `matchFields` would silently discard a user constraint.

Operator terms are never removed or replaced. The remaining constraints:

- `affinity.nodeAffinity.preferredDuringScheduling...`, `podAffinity`, and
  `podAntiAffinity`: appended, since the operator sets none today. If a
  component later sets them, the same conjunction rule applies.
- `nodeSelector`: keys are merged; overwriting an operator-set key is rejected.
- `tolerations`: appended, never replaced, despite strategic merge's default
  replace semantics for that list.

### 8.5 Apply-time assertion

Independently of validation and re-stamping, the object is asserted to be
`apps/v1` `Deployment` or `DaemonSet` immediately before apply, and `ApplyObject`
itself gains a defensive check for objects carrying an override annotation.

Three layers for one property is deliberate. GVK escape is the difference
between a damaged workload and a cluster-admin binding
([§4.2](#42-the-allowlist-is-an-integrity-control-not-a-security-control)), so
it should not depend on any single check being correct.

## 9. Failure and update semantics

An override document is user-authored input that the API server accepts without
validation. The operator must therefore define what happens when it is wrong,
and in particular must never let a bad edit silently undo a good one.

### 9.1 Render, then validate, then apply

Validation is atomic because rendering is separated from applying. Each pass:

```
1. render    every enabled component produces its objects, no writes
2. validate  parse, schema, allowlist, protected paths, resolution, merge
3. apply     write everything, or nothing for the affected components
```

This ordering is the reason [§14](#14-implementation-plan) sequences the
render/apply split as a prerequisite. Without it, atomicity is unachievable:
components write before they reach a workload apply. `storage.go:64` and
`net.go:64` create a ConfigMap before `ApplyManifestFS`; `metalman.go:45`
applies its entire RBAC set before the Deployment at `:49`; and
`applyManifestData` writes object by object in a loop (`env.go:202-237`). A
resolution failure discovered at the DaemonSet would follow several completed
writes, and components earlier in the registry would already have been applied
with their new overrides.

Resolution belongs in step 2 and not earlier, because it needs the rendered
object: whether container `run` exists, and whether a `mountPath` collides with
an operator mount, are not answerable from the ConfigMap alone.

Splitting render from apply also requires splitting `ensureConfig`, which today
both creates the component ConfigMap and returns the hash stamped into the pod
template. It becomes a read-only hash computation used during render, and a
separate write performed in the apply phase.

### 9.2 Invalid overrides skip, they do not revert

The operator distinguishes three states, because conflating them turns a typo
into an uninstall:

| ConfigMap state | Behaviour | Rationale |
|---|---|---|
| **Absent** | Apply vanilla manifests | Removing overrides is deliberate. Reverting to defaults is the requested outcome. |
| **Present and valid** | Apply with overrides | |
| **Present and invalid** | **Skip applying Deployments and DaemonSets.** All other objects reconcile normally. | The user tried to express something and failed. Their intent was not "remove my configuration". |
| **Unreadable** (API error) | Skip workload applies, return the error, requeue with backoff | Transient. Indistinguishable from invalid for safety purposes. |

Applying vanilla manifests on invalid input was considered and rejected. It is
not a safe fallback, because defaults are not the current state: falling back
rewrites running infrastructure. A single mis-indented line would strip
resources, tolerations, sidecars, and pinned images from every component and
roll all of them at once, with a zero-available window on the two host-networked
workloads that use `maxSurge: 0`
([§13](#13-operational-notes)). A typo must not be able to cause that.

Skipping leaves the fleet exactly as it is. The cluster holds the last good
state because the operator does not write, which makes the behaviour
**restart-safe by construction**. This matters concretely: the operator runs
`replicas: 1` with `strategy: Recreate`
(`deploy/unbounded-operator/04-deployment.yaml.tmpl:13-16`), so it restarts on
every upgrade. An earlier revision cached last-known-good in memory, which that
restart discards; the next apply would then have stripped every override. There
is now no in-memory state to lose.

The cost, stated plainly: while overrides are invalid, drift on Deployments and
DaemonSets is not corrected. A workload someone edited by hand stays edited
until the override document is fixed. That is recoverable and non-disruptive,
which the alternative is not.

### 9.3 Failure is loud

Skipping is not silent. Every invalid-override pass produces:

- An **error-level log** naming the ConfigMap key, the entry index, and the
  specific failure.
- A **Degraded condition** on every affected component, so `kubectl wait` and
  any condition-based alerting fire.
- An **Event** on the Site.
- A **requeue with backoff**, repeating until the document is fixed.
- `kubectl unbounded overrides status` reporting the parse error and which
  workloads are consequently unmanaged.

This is louder than applying vanilla manifests would be, which produces a
rollout, one log line, and a Site still reporting `Ready=True`.

### 9.4 Failure scope

| Failure | Scope | Result |
|---|---|---|
| Malformed YAML, missing or unknown `apiVersion`, schema violation | Whole ConfigMap | Skip all workload applies, every component Degraded |
| Path outside the allowlist, protected path, `$` directive, explicit null | Whole ConfigMap | As above |
| Resolution: container absent and not in `addContainers`, name in `addContainers` that already exists, `mountPath` collision | That object only | Object not applied; other objects reconcile normally |
| True conflict between contributors to one resolved object | That object only | As above |
| `sites` naming a Site that does not exist | Nothing | Inert, reported ([§6.2](#62-resolution)) |

Document-level failures are ConfigMap-wide because a document that does not
parse cannot be attributed to a component. Resolution and conflict failures are
object-scoped because they can be, and because step 2 of
[§9.1](#91-render-then-validate-then-apply) knows exactly which object failed
before anything is written.

### 9.5 Desired versus applied

Divergence must be observable rather than inferred, but the two values have to
be comparable to mean anything.

Both are computed over the **same canonical resolved contributor set for a
single object**: the ordered list of entries that resolve to that workload,
canonicalized and hashed. An earlier revision hashed the applied set per object
against a desired hash covering the entire ConfigMap, which differ whenever more
than one workload is targeted, so the signal was always on.

| Value | Where | Meaning |
|---|---|---|
| `unbounded-cloud.io/override-hash` | Annotation on the workload | Hash of the contributor set actually merged into this object |
| Desired hash | Computed in `SiteStatus.Overrides` and by the CLI | Same computation over the current ConfigMap for the same object |

Desired is **computed, not annotated**. Writing it would require touching an
object the operator has just decided not to apply, contradicting
[§9.2](#92-invalid-overrides-skip-they-do-not-revert). Comparison happens in
status and in `overrides status`, both of which read the live annotation and
recompute the desired value.

## 10. Wiring

### 10.1 Component contract

The render/apply split gives components one new capability:

```go
// Renderer returns the objects a component would apply, without applying them.
// Implementations must not write. Cluster components receive every Site,
// because enablement is resolved as "any Site enables it"; per-Site components
// receive the single Site they are reconciling.
type ClusterRenderer interface {
    Render(ctx context.Context, env *Env, sites []unboundedv1alpha3.Site) ([]*unstructured.Unstructured, error)
}

type SiteRenderer interface {
    Render(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) ([]*unstructured.Unstructured, error)
}
```

Two signatures rather than one, because a single-Site signature is wrong for
`net`, `machina`, and `gantry`, which render from the full Site set
(`machina.go:47`, `gantry.go:81`).

`metalman` satisfies this by converting its typed `appsv1.Deployment`
(`metalman.go:98-196`) to unstructured, so both generation paths
([§3.6](#3-constraints-from-the-current-design)) converge before the merge
rather than at apply.

### 10.2 Pass structure

```
runComponents
  |
  +- read overrides ConfigMap                      once per pass
  +- parse, schema, allowlist validation           pure, atomic
  |
  +- for each enabled component: Render(...)       no writes
  +- resolve and merge overrides into workloads    per object
  +- re-stamp protected paths, assert GVK
  |
  +- for each component: write ConfigMaps, then ApplyObject each rendered object
```

The merge therefore happens between render and apply rather than inside
`ApplyObject`. `ApplyObject` retains the `apps/v1` assertion from
[§8.5](#85-apply-time-assertion) as a defensive layer, not as the mechanism.

`Env` still gains `ForComponent(name, site)` returning a shallow copy, so the
merge step knows which component and Site an object belongs to. `r.env()`
constructs a fresh `Env` per Reconcile (`reconciler.go:90-96`), so there is no
shared mutable state.

### 10.3 Watch and fan-out

The watch is registered centrally in `SetupWithManager` (`reconciler.go:279-311`)
rather than per component, since the ConfigMap spans components.

The obvious wiring, reusing `RequestSingletonAndAllSites` (`watch.go:35`), is
**wrong**. That handler lists Sites at event-delivery time and, when the List
fails, logs and returns only the singleton request (`watch.go:44-49`). There is
no retry: the event is consumed and the per-Site fan-out is lost permanently.
The singleton pass does not compensate, because Site components run only when
`site != nil` (`reconciler.go:179`).

```go
// The ConfigMap watch enqueues only the synthetic singleton request. Fan-out
// happens inside Reconcile, which can return an error and be retried.
b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
    builder.WithPredicates(env.ManagedConfigPredicate(
        env.InNamespaceNamed(component.OverridesConfigMapName))))
```

**Fan-out is synchronous, inside the Site-less pass.** `Reconcile` has no queue
handle; its signature returns only its own `ctrl.Result` (`reconciler.go:112`),
so it cannot enqueue other Sites. Rather than introduce a `source.Channel` and
its attendant acknowledgement and backpressure questions, the Site-less pass
runs per-Site components inline for every Site it lists.

This is sound because the controller is single-threaded.
`MaxConcurrentReconciles` is not configured anywhere in the operator, so it
takes controller-runtime's default of 1. Consequently:

| Concern | Resolution |
|---|---|
| Concurrent update | Impossible. One reconcile runs at a time. |
| Acknowledgement | Not needed. Work completes before the pass returns. |
| Backpressure | Not needed. No queue is involved. |
| Restart | The pass either completed or it did not; the next reconcile redoes it. Idempotent under SSA. |
| Partial failure | Errors aggregate into the existing `errors.Join` (`reconciler.go:191`) and the whole pass requeues with backoff. |

The Site-less pass already lists Sites and already returns that error to the
caller (`reconciler.go:145-148`), so a transient List failure retries rather
than silently dropping the fan-out.

It also gains a responsibility it does not have today: publishing Site status
for the per-Site components it runs. `runComponents` currently gates status
patching on `site != nil` (`reconciler.go:179-189`); that gate moves so the
inline fan-out reports conditions for each Site it touches.

The cost is an O(Sites) pass on override change. Sites are per-location and
few, so this is acceptable; `source.Channel` with explicit backpressure is the
documented fallback if that stops holding.

`ManagedConfigPredicate` (`watch.go:107`) is reused unchanged. It matches on
namespace, name, and payload change with no ownership requirement, so it works
on a user-owned ConfigMap.

**Rollout.** No config-hash annotation is needed for the override itself. The
merge changes the pod template, SSA writes the change, and the workload's own
rollout strategy takes over.

## 11. Drift visibility and observability

An override is invisible in the generated manifests and survives operator
upgrades. A pinned image or a replaced argument list silently freezes a
component at an old version, and that is the state support will most often be
handed without being told about it.

Three pieces of plumbing this design assumes **do not exist today** and are
implementation work, not free:

| Assumption | Reality |
|---|---|
| Conditions visible in `kubectl get site` | `Site` declares no condition printer columns (`site_types.go:25-33`). Its columns are node CIDRs, pod CIDR assignments, per-component enabled booleans at `priority=1`, node count, slice count, and age. Conditions require `-o yaml` or `-o json`. |
| The reconciler can emit Events | `SiteReconciler` has no recorder. Only `LegacyReaper` does, wired at `main.go:223`. |
| `Ready=True` means the component is healthy | It means server-side apply succeeded. No component inspects rollout status, so a workload wedged by a bad override reports `Ready=True` until something else notices. |

**On the workload:**

| Annotation | Meaning |
|---|---|
| `unbounded-cloud.io/override-hash` | Hash of the canonical resolved contributor set merged into this object ([§9.5](#95-desired-versus-applied)) |
| `unbounded-cloud.io/override-source` | Contributing ConfigMap keys and entry indices |
| `unbounded-cloud.io/version-drift` | Present only when a patch changed a container image. Value is `<container>=<image>`. |

The desired hash is deliberately **not** an annotation. Writing it to an object
the operator has decided not to apply would contradict
[§9.2](#92-invalid-overrides-skip-they-do-not-revert).

**On the Site.** `SiteStatus` gains one field:

```go
// OverrideStatus summarizes user-supplied workload overrides for one Site.
type OverrideStatus struct {
    // Phase is the aggregate state of override processing for this Site.
    // +kubebuilder:validation:Enum=None;Applied;Degraded
    Phase string `json:"phase"`

    // DesiredHash is the hash of the override set currently in the ConfigMap,
    // recomputed each pass. Empty when no overrides target this Site.
    // +optional
    DesiredHash string `json:"desiredHash,omitempty"`

    // Workloads lists each overridden workload and the hash actually applied,
    // read back from the object. Sorted by kind then name for stability.
    // +optional
    // +listType=map
    // +listMapKey=name
    Workloads []OverriddenWorkload `json:"workloads,omitempty"`

    // Message explains a Degraded phase: the ConfigMap key, entry index, and
    // failure. Empty otherwise.
    // +optional
    Message string `json:"message,omitempty"`
}

type OverriddenWorkload struct {
    Name        string `json:"name"`
    Kind        string `json:"kind"`
    AppliedHash string `json:"appliedHash,omitempty"`
    VersionDrift string `json:"versionDrift,omitempty"`
}
```

Semantics:

| Phase | Meaning |
|---|---|
| `None` | No override entry resolves to any workload for this Site. Also the value when the ConfigMap is absent. |
| `Applied` | Every resolved workload carries an `appliedHash` equal to `desiredHash`. |
| `Degraded` | The document is invalid, or a resolved object failed, or any `appliedHash` differs from `desiredHash`. `Message` explains which. |

Aggregation is per Site, not per component, because the ConfigMap is
cluster-scoped and a single document routinely targets several components.
Component-level detail stays in the existing conditions. With zero Sites the
field is never written, since there is no Site to write it to; the Site-less
pass reports override failures through logs and the reconcile error only.

`Ready` on a component condition still means the apply succeeded, not that the
workload is healthy. `Phase: Applied` likewise means the override merged and was
written, not that the resulting pods run.

**A printer column** sourced from `.status.overrides.phase`, so a customized or
degraded install is visible in `kubectl get site` without `-o yaml`. Adding
condition columns generally is a larger change and is left alone.

**An Event recorder** is added to `SiteReconciler` and wired in `main.go`
alongside the reaper's. Events fire on override hash change and on entry into
`Degraded`, not on every reconcile.

**Through the CLI**, see [§12](#12-cli).

Images are permitted with no additional gate. The signalling above is the whole
of the friction: users asked for image overrides, and per
[§4](#4-security-model) anyone able to set one could already run arbitrary code
on every node by other means.

## 12. CLI

Overrides are cluster-scoped, so they get a new top-level noun under
`kubectl unbounded` rather than living under `site`.

**`kubectl unbounded overrides list`** reads the ConfigMap and every Site and
prints each entry, the objects it resolved to, any Site names that matched
nothing, and drift flags read back from workload annotations. This is the
first thing to run on a support case.

**`kubectl unbounded overrides validate`** parses and validates without
applying, reusing the operator's validation path from
`internal/operator/component`. This narrows the weakest property of ConfigMap
storage: the API server accepts any ConfigMap, so a malformed document is only
rejected later, in reconcile, as a Site condition. `validate` moves that
feedback to authoring time, and can run against a file before it is applied.

**`kubectl unbounded overrides status`** reports, per resolved object, the
applied and desired override hashes ([§9.5](#95-desired-versus-applied)),
whether any workload is being skipped and why, image drift, and the
component conditions. It is authoritative because every value is read back from
what the operator actually did.

### 12.1 Why there is no `overrides diff`

An earlier revision proposed `overrides diff`, rendering the pre-patch workload
client-side and diffing the override against it. It is dropped for three
reasons.

**It cannot be authoritative.** A `kubectl unbounded` plugin is versioned
independently of the running operator. Under version skew the plugin renders
manifests from its own embedded copy and shows a diff against a workload the
cluster will never produce. A tool whose output is wrong precisely when an
install is unusual is worse than no tool.

**The signature was wrong.** The proposed `Renderer` accepted a single Site, but
cluster components render from the full Site set (`machina.go:47`,
`gantry.go:81`) because enablement is "any Site enables it".

**Render was not separable from side effects.** Components write before they
render: `ensureConfig` creates the component ConfigMap and returns the hash that
the render then stamps (`net.go:64`, `gantry.go:109`, `machina.go:71`,
`storage.go:136`). A pure `Render` would either skip the hash, producing output
that does not match reality, or perform writes, which a read-only CLI command
must not do. [§9.1](#91-render-then-validate-then-apply) resolves this by
splitting `ensureConfig` into a read-only hash and a separate write, but that
fixes the operator, not the skew problem above.

The correct shape is an operator-side dry-run: the operator renders and merges
using its own code and reports the result, and the CLI displays it. That keeps
rendering authority in the running operator, where it belongs.

That option is now cheap. [§9.1](#91-render-then-validate-then-apply) requires
the render/apply split for atomicity, so the machinery a dry-run needs exists as
a side effect. It remains [§17](#17-open-questions) rather than being designed
here, but it is no longer blocked on a refactor.

## 13. Operational notes

**Access control.** Write access to the overrides ConfigMap is
cluster-admin-equivalent ([§4.1](#41-threat-model)). It must be granted only to
cluster administrators and audited like an RBAC change. The machina RBAC
narrowing and its admission policy
([§4.4](#44-residual-risk-and-how-it-is-closed)) are blocking prerequisites, so
by the time this ships no component ServiceAccount can write the object.

**Unmanaged workloads during an invalid override.** Skipping
([§9.2](#92-invalid-overrides-skip-they-do-not-revert)) means Deployments and
DaemonSets are not reconciled while the override document is invalid. Drift
introduced by hand, or by another controller, persists until the document is
fixed. Everything else, including RBAC, Services, and component ConfigMaps,
continues to reconcile. This is the deliberate cost of never letting a typo roll
the fleet.

**Argument replacement.** Covered in [§7.2](#72-extraargs). It is the sharpest
edge in the design and must be prominent in user documentation, not only in the
API reference.

**Zero-available windows.** The `net` controller Deployment uses `maxSurge: 0`
with `maxUnavailable: 1` (`deploy/net/controller/03-deployment.yaml.tmpl:23-24`)
and `metalman` does the same (`metalman.go:141-142`), because both are
host-networked. An override that makes pods unschedulable or crash-looping
therefore produces a window with no available replica, not a stalled rollout
behind a healthy old pod. Because `Ready=True` only means the apply succeeded
([§11](#11-drift-visibility-and-observability)), the Site will not report the
problem. Overrides touching `resources`, `nodeSelector`, `affinity`, or
`tolerations` on those two workloads deserve particular care.

**Validation timing.** The API server will accept any ConfigMap. A malformed or
conflicting document is only detected in reconcile, so there is a window where
the user believes the change landed. `overrides validate` is the mitigation, and
skipping rather than reverting
([§9.2](#92-invalid-overrides-skip-they-do-not-revert)) makes the window inert
rather than destructive. What admission can and cannot add is
[§17](#17-open-questions).

**Upgrade behavior.** A patch referencing a container or volume that a later
release renames becomes a resolution failure, which is loud and object-scoped
([§9.4](#94-failure-scope)); the modify-only default in
[§8.2](#82-adding-containers-requires-explicit-intent) is what stops it becoming
a silently added sidecar instead. A patch that merges cleanly but is
semantically wrong for the new version is not detectable by the operator. Pinned
images survive upgrades indefinitely and are the most likely cause of an install
that behaves unlike its reported version.

The operator restarts on every upgrade: it runs `replicas: 1` with
`strategy: Recreate` (`deploy/unbounded-operator/04-deployment.yaml.tmpl:13-16`).
Because the failure model holds no in-memory state
([§9.2](#92-invalid-overrides-skip-they-do-not-revert)), that restart changes
nothing about override behaviour.

**Ordering.** The ConfigMap may be created before or after the Sites it names,
and before or after the components it patches. Unmatched entries are inert and
reported ([§6.2](#62-resolution)).

## 14. Implementation plan

| PR | Scope | Depends on |
|---|---|---|
| 1 | This design document | - |
| 2 | **Security hardening.** Narrow the `machina-controller` Role to `resourceNames` for `get`, `update`, `patch`, `delete` (`deploy/machina/02-rbac.yaml.tmpl:15-17`), and add a `ValidatingAdmissionPolicy` restricting its ConfigMap `create` by name, modelled on `deploy/net/controller/09-vap.yaml.tmpl`. **Blocking**: this feature widens the exposure that this PR removes ([§4.4](#44-residual-risk-and-how-it-is-closed)). | 1 |
| 3 | **Render/apply split** across `net`, `machina`, `gantry`, `metalman`, `storage`. Split `ensureConfig` into a read-only hash computation and a separate write. Behaviour-preserving, verified by golden-object tests. Prerequisite for atomicity ([§9.1](#91-render-then-validate-then-apply)). | 1 |
| 4 | **Override engine**: `internal/operator/component/override.go`. Load, document validation, allowlist, resolution, `addContainers` intent, composition, conflict detection, strategic merge, Cartesian affinity, protected-path re-stamp, GVK lockdown, hashing. Pure functions plus unit tests; no reconciler changes. | 1 |
| 5 | **Wiring**: render-merge-apply pass structure, `Env.ForComponent`, apply-time GVK assertion, skip-on-invalid with the absent/invalid distinction, synchronous fan-out with Site status publication, `SiteStatus.Overrides`, `EventRecorder`, printer column. | 3, 4 |
| 6 | `kubectl unbounded overrides list`, `validate`, `status` | 5 |
| 7 | Documentation: new reference page under `docs/content/reference/`, amend `architecture.md:189`, amend the `SiteComponentSpec` comment at `site_types.go:141-143`, update `cli.md`, add `deploy/unbounded-operator/examples/component-overrides.example.yaml` as a plain `.yaml` so neither `render-manifests` nor `go:embed` picks it up. The access-control guidance in [§4.3](#43-required-posture-for-cluster-operators) is part of this, not an afterthought. | 5, 6 |

PR 5 requires `make generate` for `SiteStatus.Overrides` and the printer column.
Nothing else changes the CRD schema, and there is no API version churn.

PRs 2, 3 and 4 are independent of one another and can land in parallel.

## 15. Testing

**Security.** These are the highest-value tests in the plan and are written
first. GVK escape attempts, including `kind: ClusterRoleBinding` with
`escalate`-requiring content, `kind: Secret`, and a mismatched `apiVersion`;
each must be rejected at validation, neutralized by the re-stamp, and caught by
the apply-time assertion independently. `serviceAccountName` retargeting.
Host-namespace changes. `$`-prefixed directive smuggling at every nesting depth.
Explicit `null` deletion of managed content. Reserved-prefix annotation and
label writes. Allowlist bypass through unenumerated top-level paths.

**Mount identity.** A patch supplying a volumeMount with a **different `name`
but a colliding `mountPath`** must be rejected. This is the bypass that
protecting mounts by name would have allowed
([§8.3](#83-protected)).

**Site isolation.** The Cartesian product with two operator terms and two user
terms must yield four terms, each carrying both sides' `matchExpressions` and
`matchFields`. A regression test asserts two Sites' workloads cannot be
scheduled onto the same nodes through any permitted override.

**Add versus modify.** A patch naming a container that does not exist and is not
in `addContainers` fails resolution rather than adding a container. A name in
`addContainers` that already exists is rejected. A deliberately misspelled
operator container name (`machina-contoller`) must fail rather than produce an
image-less sidecar.

**Atomicity.** No write occurs anywhere in the pass when any document is
invalid, asserted against a real API server by counting writes. Specifically:
component ConfigMaps and RBAC must not be written either, which is the failure
the pre-split design could not prevent.

**Failure semantics.** The three states in
[§9.2](#92-invalid-overrides-skip-they-do-not-revert) behave distinctly: absent
applies vanilla, invalid skips Deployments and DaemonSets while other objects
reconcile, unreadable skips and returns an error. An operator restart holding an
invalid document leaves workloads untouched, which is the destructive path the
in-memory design had. Failure scoping per [§9.4](#94-failure-scope).

**Hash comparability.** With one ConfigMap targeting three workloads, each
object's applied hash equals its desired hash when healthy. The earlier
per-object-versus-whole-ConfigMap scheme would have reported permanent
divergence, so this is a regression test for a specific defect.

**Merge semantics.** Container merge by name; sidecar addition via
`addContainers`; volume and volumeMount addition; `retainKeys` sibling
preservation on a partial volume patch; env merge by name; additive toleration
and nodeSelector merge; workload level labels and annotations; `spec.replicas`;
`extraArgs` append; `extraArgs` combined with a patch that replaces `args`.

**Validation.** Missing, empty, and unrecognized `apiVersion`; unknown
`component`; unsupported `kind`; `sites` on a cluster singleton; explicitly
empty `sites`; neither `patch` nor `extraArgs` present; malformed YAML.

**Resolution and composition.** Absent `sites` matching every Site; partial
overlap (`[a,b]` and `[b,c]`) failing only site `b`; disjoint concerns composing
cleanly; true conflict rejected with both entries named; deterministic ordering
across ConfigMap keys; unmatched Site names reported without failing.

**Watch and fan-out.** A ConfigMap change while the Site List fails must still
reach Site components once the List recovers; this must fail against the
`RequestSingletonAndAllSites` wiring. Synchronous fan-out publishes Site status
for every Site it touches.

**Render parity.** Golden-object tests asserting each component's `Render`
output equals what the pre-split apply path produced, for all five components.
Plus: an identical override produces an identical result on `metalman`'s typed
path and on `net`'s unstructured path.

**Integration (envtest, real API server).** Override applied through SSA and
observable in `managedFields`; ConfigMap deleted and the default restored, with
the revert scope in [§2](#2-goals-and-non-goals) asserted against real managed
fields rather than assumed; a field owned by a competing field manager surviving
override removal, documenting the limit.

**End to end (`e2e/operator/`).** A resources override rolls the target
DaemonSet; removing it reverts; an image override sets `version-drift` and moves
`SiteStatus.Overrides.Phase`.

**Documentation examples must resolve.** A test parses every YAML override
example in `designs/` and `docs/` and resolves it against the embedded
manifests, failing on any container, volume, or mountPath that does not exist.

This test earns its place. [§7.2](#72-extraargs) warns that container names are
release-specific, and the first two revisions of this document violated that
rule in their own examples: the storage example named a container `supervisor`
when the containers are `install` and `run`, and the machina example named
`controller` when it is `machina-controller`. A design document that cannot keep
its own examples resolvable is evidence that users will not either.

## 16. Prior art

Operators that manage workloads on a user's behalf have converged on bounded
patching rather than on embedding a templating or overlay engine.

- **Elastic Cloud on Kubernetes** exposes `podTemplate` as a
  `corev1.PodTemplateSpec` with `x-kubernetes-preserve-unknown-fields`, merged
  into a generated template. It is widely regarded as the best-in-class
  ergonomics for this problem and is the closest analogue to this proposal.
- **Strimzi** exposes a trimmed `template` type covering pod, container, and
  metadata, deliberately smaller than the full `PodSpec` to keep the CRD
  tractable.
- **Prometheus Operator** inlines a curated subset of `PodSpec` fields
  individually, and has added to that set steadily over years, which is the
  field-creep failure mode described in [§5.2](#52-why-not-a-typed-override-struct).

None of them embed kustomize, Helm, or a templating engine in the operator. The
consistent pattern is a bounded target: patches apply to workloads the operator
already generates, never to an arbitrary resource graph.

**This design is deliberately more restrictive than ECK on content.** ECK
accepts an essentially open `PodTemplateSpec`; [§8](#8-permitted-and-protected-fields)
accepts only allowlisted paths. The reason is
[§3.8](#3-constraints-from-the-current-design): the workloads here run
`hostNetwork`, `hostPID`, and `privileged: true` with hostPath mounts of the
host root, and two of them carry mandatory per-Site node affinity that raw
strategic merge would silently destroy. ECK's managed workloads are ordinary
StatefulSets, so open content costs it far less. The prior art supports the
bounded target; the content restriction is specific to this operator's
circumstances and is argued on its own terms in
[§4.2](#42-the-allowlist-is-an-integrity-control-not-a-security-control).

The project's own precedent points the same way. `deploy/gantry/README.md`
already ships a hardening overlay as an example for users to copy into their own
tooling rather than as something the operator consumes, and
`designs/gantry-unbounded-integration.md` states that raw manifests must remain
directly applicable for users who want full control.

## 17. Open questions

Two questions carried by earlier revisions are now **resolved** and are recorded
here so the reasoning is not lost:

- *Dedicated resource type instead of a ConfigMap.* Resolved in favour of the
  ConfigMap. The argument for a dedicated resource rested entirely on RBAC being
  unable to scope `create` by name. Admission policy can, the repository already
  relies on this for `unbounded-net-controller`, and the same pattern is a
  blocking prerequisite here. See
  [§4.4](#44-residual-risk-and-how-it-is-closed).
- *Last-known-good across restarts.* No longer applicable. The failure model
  holds no in-memory state
  ([§9.2](#92-invalid-overrides-skip-they-do-not-revert)).

Remaining:

1. **Admission-time validation, scoped realistically.** An earlier revision
   claimed a `ValidatingAdmissionPolicy` could reject schema errors, protected
   paths, `$` directives, and nulls in the overrides ConfigMap. That was
   overstated: CEL has no YAML parser, so a VAP cannot inspect structure inside
   `ConfigMap.data`. It **can** enforce writer identity, restrict `create` by
   name, cap `data` size, and constrain key naming. Deeper validation needs a
   webhook, which is a new serving path and certificate to maintain. Is the
   shallow coverage worth an installed policy, given `overrides validate`
   already exists?
2. **Operator-side dry-run.** [§12.1](#121-why-there-is-no-overrides-diff)
   rejects a client-side `diff` on version-skew grounds, but the render/apply
   split required by [§9.1](#91-render-then-validate-then-apply) makes an
   operator-side dry-run cheap. Worth a surface, or is `overrides status`
   sufficient?
3. **`spec.replicas` and `metalman.replicas`.** `Site` already has a typed,
   supported `spec.components.metalman.replicas` (`site_types.go:167`). An
   override can also set `spec.replicas`. Should the typed field win, the
   override win, or should overrides reject `spec.replicas` on `metalman`?
4. **`siteSelector`.** Label-based Site matching would express "all edge sites"
   without enumeration. Deferred because it needs a `Site` labelling convention
   that does not exist today. It can be added alongside `sites` later with a
   documented precedence.
5. **ServiceAccount annotations.** `serviceAccountName` is protected
   ([§8.3](#83-protected)), but workload identity integrations normally require
   annotating the ServiceAccount. The operator applies component ServiceAccounts
   with `ForceOwnership`, so user annotations on them are reverted. This is a
   real gap that neither this mechanism nor the existing ConfigMap escape hatch
   covers.
6. **SSA field disownership as a complement.** For letting VPA own `resources`,
   having the operator stop declaring that path is more correct than patching it
   to a fixed value. Offer as a separate opt-in?
7. **Reserved container names.** `addContainers`
   ([§8.2](#82-adding-containers-requires-explicit-intent)) stops a typo becoming
   a sidecar, but not a user-added sidecar colliding with a container a future
   release introduces. A documented naming convention is probably sufficient.
8. **Fan-out cost at scale.** Synchronous fan-out
   ([§10.3](#103-watch-and-fan-out)) is O(Sites) per override change and relies
   on `MaxConcurrentReconciles` remaining 1. Both assumptions should be revisited
   if Site counts grow or the controller is parallelized.
