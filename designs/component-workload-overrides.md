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
10. [Wiring and execution](#10-wiring)
11. [Drift visibility and observability](#11-drift-visibility-and-observability)
12. [CLI](#12-cli)
13. [Operational notes](#13-operational-notes)
14. [Implementation](#14-implementation)
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

**3.9 Apply is not transactional.** `applyManifestData` writes object by object
in a loop (`env.go:202-237`), and each `ApplyObject` is an independent API call.
There is no batch, no transaction, and no rollback. An API or admission failure
on the fifth object leaves the first four updated. Any design that promises
all-or-nothing across objects is promising something Kubernetes does not offer.

**3.10 Components do more than apply objects.** Reconciliation is not reducible
to "produce objects, apply them". The existing components perform at least five
other kinds of operation:

| Operation | Example |
|---|---|
| Create-if-absent, preserving user data | `ensureConfig` (`storage.go:115-154`, `net.go:156-187`) |
| Adopt via optimistic-lock merge patch | `adoptConfig` (`storage.go:156-170`) |
| Delete | `Cleanup` (`storage.go:78-90`), `cleanupLegacyNodeConfig` (`gantry.go:170-178`) |
| Conditional retention | `resourcesExist` (`machina.go:57-69`) |
| Shared objects rendered per Site | metalman RBAC (`metalman.go:45`), identical for every Site |

Ordering also matters: `ensureConfig` returns the hash that the workload's pod
template carries, so the ConfigMap must exist before the workload that
references it.

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
| `machina-controller` | `configmaps`, all verbs, no `resourceNames` (`deploy/machina/02-rbac.yaml.tmpl:15-17`) | **Over-granted, and unused.** See below. |
| `metalman-controller` | `configmaps` `["get","list","watch"]` in the operator namespace; `resourceNames: ["cluster-info","aks-cluster-metadata"]` with `["get"]` in `kube-public` (`06-metalman-rbac.yaml.tmpl:59-61`, `:132-133`) | Read-only. Not a vector. |
| `unbounded-net-controller` | `configmaps` `["create"]` namespace-wide, plus `resourceNames: ["unbounded-net-serving-ca"]` for `["get","update","patch"]` (`net/controller/02-rbac.yaml.tmpl:169-176`) | Constrained at admission. Not a vector. |

`unbounded-net-controller` holds namespace-wide `create` because RBAC cannot
scope that verb: the authorizer has no object name to match against on a create
request. It is constrained instead by
`deploy/net/controller/09-vap.yaml.tmpl`, a `ValidatingAdmissionPolicy` with
`failurePolicy: Fail` that rejects any ConfigMap create from that ServiceAccount
not named `unbounded-net-serving-ca`. It cannot create the overrides ConfigMap.

**The machina grant is dead permission.** The machina controller makes exactly
one ConfigMap API call in the entire codebase:

```
cmd/machina/machina/controller/cluster_info.go:63
    k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "kube-root-ca.crt", ...)
```

That is a read, in `kube-public`, and it is already served by machina's
**ClusterRole**, which grants `configmaps: ["get"]` cluster-wide
(`02-rbac.yaml.tmpl:77-80`). Nothing uses the namespaced Role's ConfigMap verbs:

- `machina-config` reaches the pod as a **volume mount**. The kubelet performs
  the mount; the pod's ServiceAccount needs no RBAC for it.
- Leader election uses **Leases**, which have their own rule in the same Role.

The fix is therefore to **delete the grant**, not to constrain it. That is
strictly better than the alternatives:

| Option | Cost |
|---|---|
| **Delete the unused grant** | None. Removes machinery. |
| Narrow to `resourceNames`, drop `create` | Keeps a grant nothing uses. |
| Keep the grant, add a machina VAP | Requires `ValidatingAdmissionPolicy`, stable only in Kubernetes 1.30, against a documented baseline of 1.24+. |

With the grant removed, no component ServiceAccount can create or modify the
overrides ConfigMap. Write access is then held only by principals a cluster
administrator has granted it to, which is the posture
[§4.3](#43-required-posture-for-cluster-operators) requires. This is a blocking
prerequisite ([§14](#14-implementation)), because the feature widens an
exposure that removing the grant eliminates.

**Verification is required before deletion.** Removing RBAC fails at runtime,
not at build or test time, so the hardening change must be exercised against a
live cluster in `e2e/` rather than justified by a code search alone. If
something does depend on the grant, narrowing plus a VAP is the documented
fallback, and the Kubernetes baseline question returns with it.

**This resolves the ConfigMap versus dedicated-resource question.** An earlier
revision argued that only a dedicated resource type could scope `create`, and
recorded the choice as an open question on that basis. That argument was wrong
twice over: admission policy scopes `create` by name and the repository already
relies on it, and in this specific case no `create` grant needs to exist at all.
The ConfigMap is retained.

**Out of scope, but worth raising separately.** The documented Kubernetes
baseline of 1.24+
(`docs/content/reference/networking/operations.md:15`) is already stale
independently of this design: the operator installs `09-vap.yaml.tmpl`
unconditionally, and `ValidatingAdmissionPolicy` is not stable before 1.30. That
is a pre-existing documentation defect and should be filed on its own.

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
| `addContainers` | no | Names of containers the entry intends to **create** rather than modify. See [§8.2](#82-adding-containers-requires-explicit-intent). |
| `addInitContainers` | no | As `addContainers`, for `initContainers`. The two lists are separate because the merge keys are separate. |
| `extraArgs` | no | Map of container name to arguments appended after the patch merges. Names must resolve to a container that exists or is being added. See [§7.2](#72-extraargs). |
| `patch` | no | Strategic merge patch applied to the whole workload object. See [§7](#7-merge-semantics). |

`component` plus `kind` uniquely identifies every workload the operator emits
today: only `net` emits two workloads and they differ by kind. Users never have
to reconstruct derived names such as `unbounded-storage-<site>`.

At least one of `patch` and `extraArgs` must be present.

### 6.2 Parsing rules

The `apiVersion` gate is only meaningful if parsing is deterministic, so the
rules are fixed rather than inherited from whichever YAML library is used:

| Rule | Behaviour |
|---|---|
| Unknown fields | **Rejected.** Strict decoding at every level, including inside `patch`, where an unknown field means a path outside the allowlist. |
| Duplicate keys | **Rejected.** YAML permits them and most decoders silently take the last; a duplicate `patch` key would silently discard the first. |
| Documents per key | **Exactly one.** A `---` separator producing a second document is rejected rather than the remainder being ignored. |
| Trailing content | **Rejected**, for the same reason. |
| YAML merge keys (`<<`) | **Rejected.** Anchor and alias expansion would let a document reference content the allowlist walker never visits. |
| Anchors and aliases | Permitted only where expansion is fully resolved before validation, so the allowlist sees the expanded document. |
| Empty or whitespace-only key | Ignored, not an error. `kubectl create configmap --from-file` on an empty file is a plausible accident, not an intent. |

All of these are document-level failures per
[§9.5](#95-failure-scope): they fail preflight and no override is applied
anywhere.

### 6.3 Resolution

An entry resolves to zero or more concrete objects:

- For `net`, `machina`, `gantry`: the single named workload of that kind.
- For `metalman`, `storage`: one object per matched Site, named by the
  component's own derivation (`metalman.go:100`, `storage.go:241`).

A name in `sites` that matches no existing Site is **reported, not fatal**.
Writing the ConfigMap before creating the Site is legitimate, and deleting a
Site must not retroactively invalidate an unrelated override. Unmatched names
appear in the component's Site condition message and in
`kubectl unbounded overrides list`.

### 6.4 Multiple entries and conflicts

The normal model is **one entry per workload carrying all of its changes**.
Strategic merge patch is structural, not sequential: a single patch document can
set resources, add a sidecar, add a toleration, and add a volume at once.

Multiple entries may nonetheless resolve to the same object, which supports
splitting by ownership (a platform team owning one ConfigMap key, a security
team owning another). When they do:

1. Contributors are grouped **by resolved object**, not by `component` plus
   `kind`. Two entries with `sites: [a, b]` and `sites: [b, c]` are contributors
   to site `b`'s object only.
2. They are composed in deterministic order: sorted ConfigMap key, then document
   order within a key.
3. A conflict fails only the affected object. In the `[a, b]` and `[b, c]`
   example, site `b`'s workload is not applied and sites `a` and `c` reconcile
   normally.
4. Every contributor is recorded on the workload and in the condition message,
   so overlap is visible even when it composes cleanly.

### 6.5 What counts as a conflict

"Two contributors assigning different values to the same leaf" is not a
sufficient definition, because several parts of this mechanism are not leaf
assignments. Tolerations append, affinity takes a Cartesian product,
`extraArgs` concatenates, and `addContainers` declares intent. Comparing raw
patch leaves would report conflicts where none exist and miss conflicts that do.

Conflict detection therefore operates on the **normalized operation set** for an
object, after each contributor has been reduced to typed operations but before
they are combined:

| Operation | Composition across contributors | Conflicts when |
|---|---|---|
| Scalar set (`image`, `replicas`, `priorityClassName`, a `resources` leaf) | Last writer would win | Two contributors set the same path to **different** values. Identical values do not conflict. |
| Map merge (`nodeSelector`, labels, annotations) | Keys union | Two contributors set the same key to different values |
| List append (`tolerations`, `topologySpreadConstraints`) | Concatenate in contributor order | Never. Duplicates are permitted; the scheduler treats them as idempotent. |
| `extraArgs` | Concatenate per container in contributor order | Never |
| Cartesian affinity ([§8.4](#84-additive-only-scheduling)) | Product of all contributors' term lists with the operator's | Never. The product is associative and order-independent. |
| Merge-by-key list entry (`containers[name]`, `volumes[name]`, `env[name]`, `volumeMounts[mountPath]`) | Recurse into the entry and apply the rules above | Per the nested rule that applies |
| `addContainers` / `addInitContainers` | Union of names | Two contributors declare the same name with **non-identical** container definitions |
| `args`, `command` replace | Last writer would win | Two contributors supply different lists |

Two consequences worth stating explicitly:

- **Identical values never conflict.** Two teams independently setting the same
  memory limit is not an error, and failing it would make the ownership-split
  use case unusable.
- **Order-dependence is a conflict, not a resolution.** Where the table says
  "last writer would win", that outcome is rejected rather than accepted.
  Deterministic ordering exists so composition is reproducible, not so that
  silent precedence can be inferred from ConfigMap key names.

A conflict is reported with both contributors named by ConfigMap key and entry
index, and the normalized operation and path that disagree.

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
([§9.5](#95-failure-scope)), because container names are release-specific and
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

The authoritative list is `override.PermittedPaths()`, which the CLI and the
user reference both render rather than restate. The table below is a summary and
has drifted from the code before; `containers` and `initContainers` are now held
in step by a test rather than by whoever edits the list.

Values are also checked against the type Kubernetes fixes for the path.
Strategic merge does not police this: writing `containers:` as a mapping rather
than a list, one missing `-`, merged cleanly and produced an object whose
`containers` was no longer an array, which the operator then hashed, reported
`Applied`, and handed to an API server that rejected it with a message naming a
Go type.

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
| `containers[*].lifecycle`, `.ports` | subtree | |
| `containers[*].imagePullPolicy`, `.workingDir`, `.terminationMessagePath`, `.terminationMessagePolicy` | leaf | |
| `initContainers[*]` | as `containers[*]` | Identical surface, plus `restartPolicy`, which is what declares a native sidecar and is rejected by Kubernetes on an ordinary container |
| `volumes` | subtree | Except operator-declared volumes |
| `imagePullSecrets` | subtree | |
| `nodeSelector`, `tolerations`, `affinity` | subtree | Additive only, see [§8.4](#84-additive-only-scheduling) |
| `topologySpreadConstraints` | subtree | |
| `securityContext` | subtree | Pod-level |
| `priorityClassName`, `runtimeClassName`, `schedulerName` | leaf | |
| `dnsPolicy` | leaf | |
| `dnsConfig` | subtree | |
| `terminationGracePeriodSeconds` | leaf | |

At the workload level:

| Path | Kind | Notes |
|---|---|---|
| `spec.replicas` | leaf | Deployments only |
| `spec.strategy`, `spec.updateStrategy` | subtree | |
| `spec.minReadySeconds`, `spec.revisionHistoryLimit` | leaf | |
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
resolution failure ([§9.5](#95-failure-scope)). The two rules together mean
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
| Operator-declared `volumes`, by name | Two reasons. `Volumes` uses `patchStrategy:"merge,retainKeys"` (`k8s.io/api/core/v1/types.go:4145`), so a partial patch silently drops sibling fields of the volume it names. More seriously, volumes merge on `name`, so redefining one repoints **every** mount that uses it while naming no `mountPath` anywhere, which is the mount protection below bypassed from the other side. Adding volumes under new names is unrestricted. |
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

**The product is bounded.** It is multiplicative, and any number of documents
can target one workload, so three contributors with four terms each already
produce sixty-four. Compounded this is a denial of service against etcd rather
than a merely large object, and the resulting affinity would be unreadable to
anyone debugging a scheduling failure. Combining is refused past a fixed
ceiling, with an error explaining that required terms are ORed and that fewer,
broader terms are wanted.

**An empty `nodeSelectorTerms` list is refused rather than treated as the
identity.** An empty list matches nothing, so combining with it should yield
nothing; treating it as the identity quietly resolved to whichever side was
non-empty, leaving the operator's own constraint as the entire result while the
user's affinity was reported `Applied`.

Operator terms are never removed or replaced. The remaining constraints:

- `affinity.nodeAffinity.preferredDuringScheduling...`, `podAffinity`, and
  `podAntiAffinity`: appended, since the operator sets none today. If a
  component later sets them, the same conjunction rule applies.
- `nodeSelector`: keys are merged; overwriting an operator-set key is rejected.
- `tolerations`: appended, never replaced, despite strategic merge's default
  replace semantics for that list.
- `topologySpreadConstraints`: appended. Conflict detection treats it as
  additive, so the merge has to be additive too, or two contributors sharing a
  `topologyKey` would overwrite each other while conflict detection reported no
  disagreement.

A scheduling value of the wrong type is refused rather than dropped. Lifting
scheduling out of the patch before the merge and then failing a type assertion
silently discarded it, leaving the override hashed and reported `Applied` while
doing nothing at all.

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

### 9.1 Preflight atomicity, not transactional apply

An earlier revision claimed the pass would "apply everything, or nothing".
That is not achievable. Kubernetes offers no multi-object transaction and no
rollback; `applyManifestData` issues one API call per object
([§3.9](#3-constraints-from-the-current-design)), so a failure on the fifth
leaves the first four written.

What is achievable is **preflight atomicity**: everything that can be decided
without touching the cluster is decided before anything is written.

```
1. snapshot   read the overrides ConfigMap once, record its resourceVersion
2. preflight  parse, schema, allowlist, protected paths, directives, nulls
              pure function of the snapshot; no cluster access, no writes
3. plan       every enabled component produces an operation plan; no writes
4. resolve    merge overrides into Overridable operations, per object
5. execute    run the plan; NOT atomic, see below
```

Steps 1 to 4 are side-effect free. A failure in any of them means nothing was
written, which is a real guarantee. Step 5 is where the honest limits are.

### 9.2 Execution semantics

Execution is explicitly non-transactional. The rules exist so that partial
outcomes are predictable rather than arbitrary:

| Property | Behaviour |
|---|---|
| **Ordering** | Order is **inferred from the kind**, not declared: removals, then namespaces, schema (CRDs, priority and storage classes, webhooks), identity (ServiceAccounts and RBAC), config (ConfigMaps, Secrets, Services), custom resources, and workloads last. Declared `DependsOn` is honoured on top, for orderings that do not follow from the kinds involved. Within a tier the order is deterministic: component registry order, then the order that component emitted its operations. |
| **Continuation** | A failed operation does not abort the pass. Operations that do not depend on it still execute. Dependents are **skipped**, not attempted, so a failure does not cascade into a half-configured workload. |
| **Gating** | Three things gate an operation, and only the first is declared: a failed `DependsOn`; a failed earlier operation on the same object; and a failed earlier tier **for the same component and Site**. The last is scoped deliberately, because skipping every workload in the cluster over one component's ConfigMap would turn a contained failure into an outage. A failed Namespace is the one exception and gates everything in it, whichever component planned it, since nothing can be written into a namespace that does not exist. |
| **No rollback** | Completed operations are never undone. There is no compensating action, and none is attempted. |
| **Attribution** | Each failed operation is reported with its component, Site, object identity, and error. Aggregated into the existing `errors.Join` (`reconciler.go:191`). |
| **Retry** | The pass returns an error and controller-runtime requeues with backoff. Because every operation is idempotent under server-side apply, the retry re-executes the whole plan rather than resuming from a checkpoint. |
| **Partial status** | Objects that were applied carry their override annotations; objects that were skipped do not. `AppliedHash` is reported only for operations the executor completed, so status reflects what reached the cluster rather than what was intended ([§11](#11-drift-visibility-and-observability)). |
| **Skipped is not Ready** | A component whose operations were all skipped reports `DependencyNotWritten` rather than `Reconciled`. It did not write what it planned, and the component that actually failed already reports the underlying error, so repeating it here would bury the cause. |

Ordering is inferred rather than declared because declaring it puts a
correctness requirement on every component author in a place where getting it
wrong is invisible. A DaemonSet applied before its ConfigMap exists **succeeds**;
nothing fails and nothing is reported, and the symptom is a crash-looping pod
that cannot mount, until some later pass happens to order the two the other way.
The dependencies are a property of the Kubernetes object model rather than of
any component, so inferring them makes the ordering correct by construction for
every component, including ones that never consider it.

A pass that fails partway is a normal, recoverable state: the next reconcile
recomputes the plan from current cluster state and re-executes it.

### 9.3 Invalid overrides skip, they do not revert

The operator distinguishes four states, because conflating them turns a typo
into an uninstall:

| Snapshot state | Behaviour | Rationale |
|---|---|---|
| **Absent** | Execute the full plan with no overrides | Removing overrides is deliberate. Reverting to defaults is the requested outcome. |
| **Present and valid** | Execute the full plan with overrides merged | |
| **Present and invalid** | Drop every `Overridable` operation from the plan; execute the rest | The user tried to express something and failed. Their intent was not "remove my configuration". |
| **Unreadable** (API error) | As invalid, plus return the error so the pass requeues | Transient. Treated as invalid for safety. |

**The blast radius is the workloads, not the pass.** Only operations marked
`Overridable`, meaning the Deployments and DaemonSets an override could target,
are dropped. RBAC, Services, component ConfigMaps, adoptions and deletes all
continue to reconcile normally. An override typo must not stop the operator
doing its other work.

Applying vanilla manifests on invalid input was considered and rejected. It is
not a safe fallback, because defaults are not the current state: falling back
rewrites running infrastructure. A single mis-indented line would strip
resources, tolerations, sidecars, and pinned images from every component and
roll all of them at once, with a zero-available window on the two host-networked
workloads that use `maxSurge: 0` ([§13](#13-operational-notes)). A typo must not
be able to cause that.

Dropping the operations instead leaves those workloads exactly as they are. The
cluster holds the last good state because the operator does not write, which
makes the behaviour **restart-safe by construction**. This matters concretely:
the operator runs `replicas: 1` with `strategy: Recreate`
(`deploy/unbounded-operator/04-deployment.yaml.tmpl:13-16`), so it restarts on
every upgrade. An earlier revision cached last-known-good in memory, which that
restart discards; the next apply would then have stripped every override. There
is now no in-memory state to lose.

The cost, stated plainly: while overrides are invalid, drift on Deployments and
DaemonSets is not corrected. A workload someone edited by hand stays edited
until the override document is fixed. That is recoverable and non-disruptive,
which the alternative is not.

### 9.4 Failure is loud

Dropping operations is not silent. Every invalid-override pass produces:

- An **error-level log** naming the ConfigMap key, the entry index, and the
  specific failure.
- A **Degraded condition** on every affected component, so `kubectl wait` and
  any condition-based alerting fire.
- An **Event on the overrides ConfigMap itself**, which exists whenever
  overrides do and is the object the user edited. Events on Sites are emitted
  too, but the ConfigMap is the durable observation point when no Site exists
  ([§11](#11-drift-visibility-and-observability)).
- A **requeue with backoff**, repeating until the document is fixed.
- `kubectl unbounded overrides status` reporting the parse error and which
  workloads are consequently unmanaged.

This is louder than applying vanilla manifests would be, which produces a
rollout, one log line, and a Site still reporting `Ready=True`.

### 9.5 Failure scope

| Failure | Scope | Result |
|---|---|---|
| Parse: malformed YAML, duplicate keys, multiple documents, trailing content, merge keys ([§6.2](#62-parsing-rules)) | Whole snapshot | Preflight fails. Every `Overridable` operation dropped, every component Degraded. |
| Missing or unknown `apiVersion`, schema violation | Whole snapshot | As above |
| Path outside the allowlist, protected path, `$` directive, explicit null | Whole snapshot | As above |
| Resolution: container absent and not in `addContainers`, name in `addContainers` that already exists, `mountPath` collision | That object only | That object's operation dropped; every other operation executes |
| Conflict between contributors ([§6.5](#65-what-counts-as-a-conflict)) | That object only | As above |
| `sites` naming a Site that does not exist | Nothing | Inert, reported ([§6.3](#63-resolution)) |
| API or admission error during execution | That operation, its declared dependents, later operations on the same object, and that component's later tiers for that Site | Per [§9.2](#92-execution-semantics). A failed Namespace additionally gates every namespaced object in it. |
| An object the plan expected to create already exists | That pass | The write succeeds, since an existing payload surviving is the point of `OpCreateIfAbsent`, but the object is refreshed from the cluster and the pass re-plans: everything computed from the earlier read is stale, including the config hash stamped on the workload that mounts it. |

Preflight failures are snapshot-wide because a document that does not parse
cannot be attributed to a component. Resolution and conflict failures are
object-scoped because step 4 knows exactly which object failed before anything
is written.

### 9.6 Snapshot and consistency

Each pass reads the overrides ConfigMap **once** and records the observed
`resourceVersion`. Every decision in that pass derives from that single
snapshot, so a result is always traceable to a specific input version.

This matters because passes are not serialized against user edits. A user can
write the ConfigMap while a pass is executing, and different passes routinely
observe different versions. Without a recorded version, two Sites could carry
results computed from different inputs with nothing to indicate it.

| Question | Answer |
|---|---|
| What version produced this result? | `SiteStatus.Overrides.ObservedResourceVersion` |
| Can two Sites disagree? | Yes, transiently, when a pass for one Site precedes an edit and a pass for another follows it. Both record which version they saw. |
| How does it converge? | The ConfigMap watch enqueues a pass on every payload change ([§10.3](#103-watch-and-fan-out)), so a newer version always triggers reconciliation of every Site. |
| Is a stale snapshot harmful? | No. It produces a correct result for an older input, superseded by the pass the newer version triggers. |

The operator does **not** attempt read-modify-write consistency against the
ConfigMap, because it never writes it. There is no lost-update hazard, only a
convergence delay bounded by watch delivery.

### 9.7 Desired versus applied

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
| `SiteStatus.Overrides.Workloads[].DesiredHash` | Site status, written by the operator | Same computation over the snapshot, for the same object |
| `SiteStatus.Overrides.Workloads[].AppliedHash` | Site status, from the object the operator successfully wrote | Empty when the write failed, was skipped, or never ran |

`AppliedHash` is read from the merged object in the plan, because that is the
only place it exists, but **only for operations the executor completed**. Taking
it from the plan alone described intent rather than outcome: a workload whose
apply the API server rejected still reported its desired hash as applied, so the
Site said `Applied` for an override that had never reached the cluster, which is
the one case this comparison exists to catch.

Desired is **computed by the operator and persisted in status**, never written
as an annotation. Annotating an object the operator has just decided not to
apply would contradict [§9.3](#93-invalid-overrides-skip-they-do-not-revert),
and persisting it in status is what allows the CLI to report divergence without
recomputing anything ([§12](#12-cli)).

## 10. Wiring

### 10.1 The operation plan

An earlier revision proposed components returning `[]*unstructured.Unstructured`.
That cannot represent what components actually do
([§3.10](#3-constraints-from-the-current-design)): create-if-absent, owner-
reference adoption, deletion, conditional retention, shared objects, or
ordering. A list of objects has no way to say "create this only if absent,
preserving its data" or "this RBAC is the same for every Site".

Components therefore produce an **operation plan**, side-effect free:

```go
// OpKind is what the executor should do with an operation's object.
type OpKind int

const (
    // OpApply server-side applies, taking ownership of declared fields.
    OpApply OpKind = iota
    // OpCreateIfAbsent creates only when the object does not exist, and never
    // overwrites an existing payload. Models ensureConfig.
    OpCreateIfAbsent
    // OpAdoptOwnerRef adds an owner reference under optimistic lock, leaving
    // all other fields untouched. Models adoptConfig.
    OpAdoptOwnerRef
    // OpDelete removes the object, treating absence as success. Models
    // Cleanup and legacy reaping.
    OpDelete
)

type Operation struct {
    Kind   OpKind
    Object *unstructured.Unstructured

    // Component and Site attribute results and scope override resolution.
    // Site is empty for cluster-scoped operations.
    Component string
    Site      string

    // Overridable marks the Deployments and DaemonSets that overrides may
    // target. Only these are dropped when preflight fails
    // (§9.3), and only these are merge candidates.
    Overridable bool

    // SharedKey, when non-empty, identifies an operation that is identical
    // across Sites and must execute once per pass (§10.4).
    SharedKey string

    // DependsOn declares ordering. The executor runs dependencies first and
    // skips dependents when a dependency fails (§9.2).
    DependsOn []ObjectRef
}

type Plan struct {
    Operations []Operation
}
```

Cluster components plan from the full Site set, per-Site components from one
Site, because enablement for the singletons is resolved as "any Site enables it"
(`machina.go:47`, `gantry.go:81`):

```go
type ClusterPlanner interface {
    Plan(ctx context.Context, env *Env, sites []unboundedv1alpha3.Site) (*Plan, Result, error)
}

type SitePlanner interface {
    Plan(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) (*Plan, Result, error)
    CleanupPlan(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) (*Plan, Result, error)
}
```

Each returns a `Result` alongside the plan, because planning reaches verdicts
execution cannot: `Disabled` when no Site enables a component, `NoSites` when
net is retained with none, and the retained-singleton decisions. `(*Plan, error)`
alone had nowhere to carry them, and they are the conditions users
`kubectl wait` on. The driver folds execution outcomes into the verdict, so a
component that planned successfully but failed to write reports failure.

Planning may **read** cluster state, since decisions like machina's retention
check (`machina.go:57-69`) and storage's create-versus-adopt branch
(`storage.go:115-154`) depend on it. It may not write.

**Config hashes are computed at plan time**, from either the observed ConfigMap
or the embedded default about to be created, with the create-or-patch emitted as
a separate operation in the same plan. Four of the five components stamp a hash
into a pod template that they derive from a ConfigMap they may also be creating,
so this is what lets planning stay side-effect free.

The consequence is a bounded race: if another writer creates a different payload
between planning and execution, `OpCreateIfAbsent` correctly declines to
overwrite it, and the workload briefly carries a hash for content that is not
there. The config watch fires on that create and the next pass corrects it.

**Within a component, operations execute in the order the component planned
them.** An earlier draft sorted by kind and name, which would have silently
reordered deliberate sequencing: gantry removes its legacy node config before
applying anything, and storage writes a ConfigMap before the DaemonSet that
carries its hash. Components plan deterministically by walking sorted manifest
lists, so preserving their order is still stable across passes.

`metalman` satisfies this by converting its typed `appsv1.Deployment`
(`metalman.go:98-196`) to unstructured, so both generation paths
([§3.6](#3-constraints-from-the-current-design)) converge before the merge
rather than at apply.

Mapping the existing behaviour onto operation kinds:

| Existing code | Operation |
|---|---|
| `ApplyManifestFS` per object | `OpApply` |
| `ensureConfig` create branch (`storage.go:136`, `net.go:174`) | `OpCreateIfAbsent` |
| `ensureConfig` endpoint merge (`machina.go:155-205`) | `OpMergePatch` |
| `adoptConfig` (`storage.go:156-170`) | `OpMergePatch` |
| `Cleanup` (`storage.go:78-90`) | `OpDelete` |
| `cleanupLegacyNodeConfig` (`gantry.go:170-178`) | `OpDelete` |
| metalman support RBAC (`metalman.go:45`) | `OpApply` with `SharedKey` |
| Workload apply | `OpApply` with `Overridable: true` |

### 10.2 Pass structure

```
runComponents
  |
  +- snapshot overrides ConfigMap, record resourceVersion    §9.6
  +- preflight: parse, schema, allowlist                     §9.1, pure
  |
  +- for each enabled component: Plan(...)                   no writes
  +- deduplicate SharedKey operations                        §10.4
  +- resolve and merge overrides into Overridable operations
  +- re-stamp protected paths, assert GVK
  |
  +- execute the plan in dependency order                    §9.2, not atomic
```

The merge happens between planning and execution rather than inside
`ApplyObject`. `ApplyObject` retains the `apps/v1` assertion from
[§8.5](#85-apply-time-assertion) as a defensive layer, not as the mechanism.

`Env` gains `ForComponent(name, site)` returning a shallow copy, so planning and
merging know which component and Site an operation belongs to. `r.env()`
constructs a fresh `Env` per Reconcile (`reconciler.go:90-96`), so there is no
shared mutable state.

### 10.3 Watch and fan-out

The watch is registered centrally in `SetupWithManager` (`reconciler.go:279-311`)
rather than per component, since the ConfigMap spans components.

The obvious wiring, reusing `RequestSingletonAndAllSites`, is **wrong**, for two
reasons.

It lists Sites at event-delivery time and, when the List fails, logs and returns
only the singleton request. There is no retry: the event is consumed and the
per-Site fan-out is lost permanently.

It is also redundant. The singleton pass already reconciles every Site, because
the Site-less pass fans out to all of them (see below), so enqueuing the Sites
as well produced N extra full passes for one ConfigMap edit, each re-planning
and re-applying every component for a Site that had just been done.

That helper has been deleted rather than left available. An earlier revision of
this section claimed the singleton pass "does not compensate, because Site
components run only when `site != nil`", which contradicted the fan-out
described three paragraphs below it and was the stated justification for a
hazard that does not exist. The same wiring is now used by every component
watch, not only this one.

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
plans and executes per-Site components inline for every Site it lists.

**Concurrency is pinned explicitly.** The controller is configured with
`MaxConcurrentReconciles: 1` rather than inheriting controller-runtime's
default, so the guarantee is a property of this operator rather than of an
upstream default that could change:

```go
ctrl.NewControllerManagedBy(mgr).
    WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
    For(&unboundedv1alpha3.Site{}, ...)
```

What that does and does not buy:

| Concern | Resolution |
|---|---|
| Two reconciles racing each other | Prevented. One pass runs at a time. |
| Acknowledgement | Not needed. Work completes before the pass returns. |
| Backpressure | Not needed. No queue is involved. |
| Restart mid-pass | The next reconcile replans from current state. Every operation is idempotent. |
| Partial failure | Per [§9.2](#92-execution-semantics). |
| **External concurrent writes** | **Not prevented.** Users, other controllers, and admission all write the ConfigMap and the workloads while a pass runs. Single-threading says nothing about them. |

That last row corrects a claim an earlier revision made. Concurrent update is
not impossible; it is merely not self-inflicted. The snapshot model in
[§9.6](#96-snapshot-and-consistency) is what makes external concurrency safe:
each pass derives from one recorded `resourceVersion`, a newer version always
triggers another pass through the watch, and server-side apply makes
re-execution idempotent.

The cost is an O(Sites) pass on override change. Sites are per-location and
few. If Site counts grow past the low hundreds, or if the controller is ever
parallelized, `source.Channel` with explicit backpressure is the documented
fallback; both assumptions are recorded in [§17](#17-open-questions).

`ManagedConfigPredicate` (`watch.go:107`) is reused unchanged. It matches on
namespace, name, and payload change with no ownership requirement, so it works
on a user-owned ConfigMap.

**The cache is scoped to the operator's namespace.** A predicate filters events
after delivery; it does not stop the informer existing. Unscoped, the operator
ran informers over every ConfigMap, Deployment and DaemonSet in every namespace
in the cluster, which for ConfigMaps means a `kube-root-ca.crt` per namespace
plus whatever Helm and other operators have left, none of it ever read.
`DefaultNamespaces` applies only to namespaced kinds, so Sites, Nodes and CRDs
stay cluster-wide as they must. The legacy reaper is the one component that
legitimately reads other namespaces, and it already goes through `APIReader`
precisely so those reads bypass the cache.

### 10.4 Shared operation deduplication

Per-Site planning produces duplicate operations for objects that are not
per-Site. `metalman.Reconcile` applies the machina support manifests on every
pass for every Site (`metalman.go:45`), and `mutateSupportObject`
(`metalman.go:90-96`) keeps only metalman RBAC, which is byte-identical
regardless of Site. With N Sites that is N applies of the same objects; the
synchronous fan-out in [§10.3](#103-watch-and-fan-out) multiplies an
inefficiency that already exists.

Operations carrying a `SharedKey` are therefore collapsed before execution:

1. Index all planned operations by `SharedKey`.
2. Where several operations share a key, compare their objects semantically.
3. **Identical**: execute once, attribute the result to every contributing
   component and Site.
4. **Not identical**: reject the pass with a conflict error naming the
   contributors. A shared object that differs by Site is a planning bug, not a
   user error, and silently letting the last writer win would make the result
   depend on Site iteration order.

Deduplication happens after planning and before merging, so a shared object is
never a merge candidate for more than one Site.

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
| `unbounded-cloud.io/override-hash` | Hash of the canonical resolved contributor set merged into this object ([§9.7](#97-desired-versus-applied)) |
| `unbounded-cloud.io/override-source` | Contributing ConfigMap keys and entry indices |
| `unbounded-cloud.io/version-drift` | Present only when a patch changed a container image. Value is `<container>=<image>`. |

The desired hash is deliberately **not** an annotation. Writing it to an object
the operator has decided not to apply would contradict
[§9.3](#93-invalid-overrides-skip-they-do-not-revert).

**On the Site.** `SiteStatus` gains one field:

```go
// OverrideStatus summarizes user-supplied workload overrides for one Site.
type OverrideStatus struct {
    // Phase is the aggregate state of override processing for this Site.
    // +kubebuilder:validation:Enum=None;Applied;Degraded
    Phase string `json:"phase"`

    // ObservedResourceVersion is the resourceVersion of the overrides
    // ConfigMap this status was computed from (§9.6). Empty when the
    // ConfigMap is absent.
    // +optional
    ObservedResourceVersion string `json:"observedResourceVersion,omitempty"`

    // Workloads carries desired and applied hashes per workload. Both are
    // per-workload because contributors differ per workload; a Site-wide
    // desired hash would not be comparable to any of them.
    // +optional
    // +listType=map
    // +listMapKey=kind
    // +listMapKey=name
    Workloads []OverriddenWorkload `json:"workloads,omitempty"`

    // Message explains a Degraded phase: the ConfigMap key, entry index, and
    // failure. Empty otherwise.
    // +optional
    Message string `json:"message,omitempty"`
}

type OverriddenWorkload struct {
    // Kind and Name together identify the workload. Name alone is not unique:
    // a Deployment and a DaemonSet may share a name.
    Kind string `json:"kind"`
    Name string `json:"name"`

    // DesiredHash is computed by the operator from the observed snapshot.
    // AppliedHash is read back from the object's annotation. They are equal
    // when the override is in effect, and differ when the operation was
    // dropped or execution failed.
    // +optional
    DesiredHash string `json:"desiredHash,omitempty"`
    // +optional
    AppliedHash string `json:"appliedHash,omitempty"`

    // VersionDrift is set when the applied override changed a container image,
    // formatted as `<container>=<image>`.
    // +optional
    VersionDrift string `json:"versionDrift,omitempty"`
}
```

Both hashes are per workload. An earlier revision paired one Site-wide
`DesiredHash` against many per-workload `AppliedHash` values, which are
incomparable by construction whenever a document targets more than one workload.
The list is keyed on `kind` **and** `name`, because a Deployment and a DaemonSet
can legitimately share a name.

| Phase | Meaning |
|---|---|
| `None` | No override entry resolves to any workload for this Site. Also the value when the ConfigMap is absent. |
| `Applied` | Every resolved workload has `AppliedHash == DesiredHash`. |
| `Degraded` | Preflight failed, or a resolved object failed, or any workload has `AppliedHash != DesiredHash`. `Message` explains which. |

Aggregation is per Site, not per component, because the ConfigMap is
cluster-scoped and a single document routinely targets several components.
Component-level detail stays in the existing conditions.

Cluster-singleton workloads carry no Site but are reported on **every** Site,
because every Site depends on `net`, `machina` and `gantry`. Filtering them out
would hide the most likely case, an override of `net`, from `kubectl get site`
entirely.

`Ready` on a component condition still means the apply succeeded, not that the
workload is healthy. `Phase: Applied` likewise means the override merged and was
written, not that the resulting pods run.

**Zero Sites is still observable.** `SiteStatus` cannot be written when no Site
exists, yet cluster singletons are deliberately retained in that state
(`machina.go:57-69`), so overrides can be both configured and failing with
nowhere to report it. Two mechanisms cover this:

- **Events on the overrides ConfigMap.** It exists whenever overrides do, is the
  object the user edited, and is the natural place to look. All override
  outcomes emit here, not only Site-scoped ones.
- **Error-level logs** naming the ConfigMap key and entry index.

The promise is scoped accordingly: `SiteStatus.Overrides` reports overrides
affecting Sites that exist; ConfigMap Events and logs report everything,
including singleton workloads with no Site.

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

Every command reports what the operator did or would do, and is explicit about
which of the two it is. The authority problem that rules out client-side
diffing ([§12.1](#121-why-there-is-no-overrides-diff)) applies to the other
commands too, so each is scoped to what it can answer correctly.

**`kubectl unbounded overrides validate [-f FILE]`** performs **offline syntax
validation only**: parsing rules ([§6.2](#62-parsing-rules)), schema,
`apiVersion`, allowlist paths, protected paths, `$` directives, and nulls. Every
one of these is a pure function of the document, so a client can evaluate them
correctly regardless of operator version.

It deliberately does **not** attempt resolution. Whether container `run` exists,
or a `mountPath` collides, depends on the workload the running operator renders,
and a plugin built from a different commit would answer from its own embedded
manifests. The command says so in its output rather than implying full
validation:

```
$ kubectl unbounded overrides validate -f overrides.yaml
ok: 3 entries, syntax and allowlist valid
note: container and volume names are resolved by the operator; run
      `kubectl unbounded overrides status` after applying to confirm
```

Scoping it this way is what makes it useful: an offline check that is always
correct beats an online check that is correct only when versions match.

`-f` accepts both shapes users have: a bare overrides document, or the ConfigMap
manifest they would apply. A manifest is unwrapped and each `data` key checked
separately, which is how the operator reads it. This matters because the example
shipped with the operator is a ConfigMap manifest whose own comments recommend
this command. Keys must be unique across all inputs, since two files cannot
become one ConfigMap key; supplying two that collide is refused rather than
silently reduced to the last one.

**`kubectl unbounded overrides status`** reports resolution and application, and
**reads persisted state without recomputing it**. Desired hashes come from
`SiteStatus.Overrides.Workloads[].DesiredHash`, which the operator computed
([§9.7](#97-desired-versus-applied)); applied hashes and drift come from the
workload annotations. The CLI performs no rendering, no merging, and no hashing.

That is the whole reason [§9.7](#97-desired-versus-applied) persists the desired
hash in status rather than leaving it implicit. A CLI that recomputed it would
reintroduce exactly the skew that rules out `diff`: under version mismatch it
would report divergence that does not exist, or miss divergence that does.

**`kubectl unbounded overrides list`** shows the document as authored: each
entry, the objects it resolved to according to status, any Site names that
matched nothing, and the observed `resourceVersion` the operator last acted on.
Also read, not recomputed.

### 12.1 Why there is no `overrides diff`

An earlier revision proposed `overrides diff`, rendering the pre-patch workload
client-side and diffing the override against it. It is dropped for three
reasons.

**It cannot be authoritative.** A `kubectl unbounded` plugin is versioned
independently of the running operator. Under version skew the plugin renders
manifests from its own embedded copy and shows a diff against a workload the
cluster will never produce. A tool whose output is wrong precisely when an
install is unusual is worse than no tool.

**The signature was wrong.** The proposed renderer accepted a single Site, but
cluster components plan from the full Site set (`machina.go:47`,
`gantry.go:81`) because enablement is "any Site enables it". The operation plan
in [§10.1](#101-the-operation-plan) splits this into `ClusterPlanner` and
`SitePlanner` for that reason.

**Rendering was not separable from side effects.** Components write before they
render: `ensureConfig` creates the component ConfigMap and returns the hash that
the render then stamps (`net.go:64`, `gantry.go:109`, `machina.go:71`,
`storage.go:136`). A pure render would either skip the hash, producing output
that does not match reality, or perform writes, which a read-only CLI command
must not do. [§10.1](#101-the-operation-plan) resolves this by expressing the
ConfigMap as an `OpCreateIfAbsent` operation and computing the hash read-only,
but that fixes the operator, not the skew problem above.

The correct shape is an operator-side dry-run: the operator renders and merges
using its own code and reports the result, and the CLI displays it. That keeps
rendering authority in the running operator, where it belongs.

That option is now cheap. [§9.1](#91-preflight-atomicity-not-transactional-apply)
requires the operation plan for preflight atomicity, so a dry-run reduces to
plan, merge, report, and do not execute. It remains
[§17](#17-open-questions) rather than being designed here, but it is no longer
blocked on a refactor.

## 13. Operational notes

**Access control.** Write access to the overrides ConfigMap is
cluster-admin-equivalent ([§4.1](#41-threat-model)). It must be granted only to
cluster administrators and audited like an RBAC change. The machina RBAC
narrowing and its admission policy
([§4.4](#44-residual-risk-and-how-it-is-closed)) are blocking prerequisites, so
by the time this ships no component ServiceAccount can write the object.

**Unmanaged workloads during an invalid override.** Skipping
([§9.3](#93-invalid-overrides-skip-they-do-not-revert)) means Deployments and
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
([§9.3](#93-invalid-overrides-skip-they-do-not-revert)) makes the window inert
rather than destructive. What admission can and cannot add is
[§17](#17-open-questions).

**Upgrade behavior.** A patch referencing a container or volume that a later
release renames becomes a resolution failure, which is loud and object-scoped
([§9.5](#95-failure-scope)); the modify-only default in
[§8.2](#82-adding-containers-requires-explicit-intent) is what stops it becoming
a silently added sidecar instead. A patch that merges cleanly but is
semantically wrong for the new version is not detectable by the operator. Pinned
images survive upgrades indefinitely and are the most likely cause of an install
that behaves unlike its reported version.

The operator restarts on every upgrade: it runs `replicas: 1` with
`strategy: Recreate` (`deploy/unbounded-operator/04-deployment.yaml.tmpl:13-16`).
Because the failure model holds no in-memory state
([§9.3](#93-invalid-overrides-skip-they-do-not-revert)), that restart changes
nothing about override behaviour.

**Ordering.** The ConfigMap may be created before or after the Sites it names,
and before or after the components it patches. Unmatched entries are inert and
reported ([§6.3](#63-resolution)).

## 14. Implementation

The design and the implementation land on one branch, so this section records
what was built rather than a plan for building it. Commits, in order:

| Commit | Scope |
|---|---|
| `aa4c2b3c` | **Security hardening.** Delete the unused ConfigMap grant from the `machina-controller` Role. Verified against the controller's actual API calls and its cache configuration, not by inspection alone ([§4.4](#44-residual-risk-and-how-it-is-closed)). |
| `abfc49ea` | **Operation plan and executor.** `Plan`, `Operation`, `OpKind`, dependency ordering, continuation, shared-key deduplication. No component changes. |
| `8139c1e7` | **Plan-then-execute conversion** of all five components, with golden plan tests pinning the exact operations each produces. Behaviour-preserving. |
| `570587b4` | Golden plan ordering fix for gantry. |
| `832cb381` | **Override schema, parsing and validation.** Everything that is a pure function of the document. |
| `9d4082e7` | **Resolution, merge and hashing.** Everything that needs the rendered workload. |
| `af17e680` | **Pass wiring.** Snapshot, preflight, skip-on-invalid, ConfigMap watch, explicit `MaxConcurrentReconciles: 1`. |
| `f5da47e1` | **Status reporting.** `SiteStatus.Overrides`, printer column, `EventRecorder`. |
| `e718dfe5` | **Fan-out**, so per-Site components see override changes. |
| `e0434f4e` | **CLI**: `overrides validate`, `list`, `status`. |
| `19876112` | **Real-API-server e2e** ([§15](#15-testing)). |

Only the status commit changes the CRD schema, through
`go generate ./api/machina/...`. There is no API version churn.

### What implementation changed about this design

Five things were wrong or missing, and are corrected above rather than left for
a reader to discover:

- `OpAdoptOwnerRef` was too narrow. machina merges config **content**, not just
  an owner reference, so the operation is `OpMergePatch` carrying observed and
  desired state.
- `Plan` returning `(*Plan, error)` had nowhere to carry `Disabled`, `NoSites`
  or the retained-singleton verdicts, which are the conditions users
  `kubectl wait` on. It returns a `Result` too.
- Config-hash timing was unspecified. Hashes are computed at plan time, with a
  bounded self-healing race documented in [§10.1](#101-the-operation-plan).
- Within-component operation order has to be preserved rather than sorted, or
  gantry's legacy cleanup and storage's ConfigMap-before-DaemonSet sequencing
  silently reorder. (Superseded in part: order is now inferred from the kind,
  and emission order is preserved *within* a tier. See
  [§9.2](#92-execution-semantics).)
- The testing section assumed envtest, which does not exist in this repository.
  Coverage extends the existing kind harness instead ([§15](#15-testing)).

Four defects were found by tests rather than by review, which is recorded here
because it is evidence about where the risk in this feature actually sits:

- Sorting operations by kind and name inside a component reordered deliberate
  sequencing.
- The reserved `unbounded-cloud.io/` prefix check ran against the parent path,
  so a permitted subtree let label keys through: a patch could have forged a
  config hash the reaper gates on.
- `Client.Get` strips TypeMeta, so converting a fetched object to unstructured
  produced no GVK and an opaque apiserver error. The executor now rejects such
  an operation by name.
- yaml.v3 decodes whole numbers as `int` while apimachinery accepts only
  `int64` and **panics** otherwise, so the first user to write `spec.replicas`
  would have crashed the operator. Parsing normalizes decoded values.

### What review changed after implementation

A review of the implemented feature raised twenty-five findings. The sections
above are corrected rather than annotated, so this is only a record of where the
remaining risk turned out to sit. Two of them crashed the operator from a
document that passed validation, and both were reproduced before being fixed:

- yaml.v3 resolves an unquoted date to `time.Time`, which apimachinery's
  `DeepCopyJSONValue` panics on exactly as it does on a plain `int`. The
  normalization added above covered numbers but not this.
- strategicpatch compares merge keys with `==`, which panics at runtime on an
  uncomparable type. `containers[*].env` is a permitted subtree, so
  `env: [{name: [oops], value: x}]` reached that comparison. Merge keys are now
  type-checked before the merge runs.

The rest cluster into four themes, which is the useful part:

- **Intent reported as outcome.** Applied hashes came from the plan, so a write
  the API server rejected was still reported `Applied` ([§9.7](#97-desired-versus-applied)).
  A component whose operations were all skipped still reported `Reconciled`
  ([§9.2](#92-execution-semantics)). Malformed scheduling and wrongly typed
  values were dropped silently while the override was hashed and reported
  applied ([§8.4](#84-additive-only-scheduling)).
- **Correctness left to component authors.** Execution order was declared per
  component, and getting it wrong was invisible ([§9.2](#92-execution-semantics)).
  Every component reconciled its own copy of the Namespace, under one field
  owner and with labels they did not agree on, so the label flipped on every
  pass; under server-side apply that is a write loop, not a race that settles.
- **Tables drifting apart.** The permitted-path list claimed init containers
  had the same surface as containers and gave them eight fields fewer; the
  merge-key table named a path no patch could reach. Five invariant tests now
  hold them together ([§8.1](#81-permitted)).
- **Unbounded or amplified work.** The affinity product had no ceiling
  ([§8.4](#84-additive-only-scheduling)); the cache was cluster-wide and the
  component watches enqueued N redundant passes per edit
  ([§10.3](#103-watch-and-fan-out)).

Two findings were about the tooling rather than the operator. `overrides
validate` rejected a ConfigMap manifest, which is the shape users have and which
the shipped example is, with comments in it recommending the command that failed
on it; and it keyed files by base name, so two files called `overrides.yaml`
overwrote each other and it reported success for a document it never read. A
test now runs the command against the shipped example.

The e2e suite was rebuilding a simplified copy of the reconcile pass rather than
running `SiteReconciler`, and that copy had already drifted: it dropped only the
first overridable operation on an unusable document where the operator drops all
of them. It now drives the real reconciler ([§15](#15-testing)).

## 15. Testing

**Security.** These are the highest-value tests in the plan and are written
first. GVK escape attempts, including `kind: ClusterRoleBinding` with
`escalate`-requiring content, `kind: Secret`, and a mismatched `apiVersion`;
each must be rejected at validation, neutralized by the re-stamp, and caught by
the apply-time assertion independently. `serviceAccountName` retargeting.
Host-namespace changes. `$`-prefixed directive smuggling at every nesting depth.
Explicit `null` deletion of managed content. Reserved-prefix annotation and
label writes. Allowlist bypass through unenumerated top-level paths.

**RBAC removal (PR 2).** An `e2e/` run exercising machina registration, CSR
approval and cluster-info resolution with the ConfigMap grant deleted. This is
the only test that can prove the grant is unused; a code search cannot.

**Mount identity.** A patch supplying a volumeMount with a **different `name`
but a colliding `mountPath`** must be rejected. This is the bypass that
protecting mounts by name would have allowed ([§8.3](#83-protected)).

**Site isolation.** The Cartesian product with two operator terms and two user
terms must yield four terms, each carrying both sides' `matchExpressions` and
`matchFields`. A regression test asserts two Sites' workloads cannot be
scheduled onto the same nodes through any permitted override.

**Add versus modify.** A patch naming a container that does not exist and is not
in `addContainers` fails resolution rather than adding a container. A name in
`addContainers` that already exists is rejected. A deliberately misspelled
operator container name (`machina-contoller`) must fail rather than produce an
image-less sidecar. `addInitContainers` is honoured separately from
`addContainers`.

**Parsing rules ([§6.2](#62-parsing-rules)).** Unknown fields at every nesting
level; duplicate keys; a second document after `---`; trailing content; YAML
merge keys; unresolved aliases; empty keys ignored rather than failing.

**Preflight atomicity.** No write of any kind occurs when preflight fails,
asserted against a real API server by counting writes. Specifically: component
ConfigMaps, RBAC and adoptions must all be absent, since preflight precedes the
whole plan.

**Execution semantics ([§9.2](#92-execution-semantics)).** Dependency ordering:
a ConfigMap is written before the workload that hashes it. Continuation: a
failed operation does not prevent independent operations from executing.
Dependents of a failed operation are skipped, not attempted. No rollback:
operations completed before a failure remain. Attribution: each failure names
its component, Site and object. Idempotent replay: re-running a partially failed
plan converges.

**Invalid-input blast radius ([§9.3](#93-invalid-overrides-skip-they-do-not-revert)).**
With an invalid document, `Overridable` operations are dropped **and every other
operation still executes**: RBAC is applied, component ConfigMaps are created,
deletes happen. This replaces an earlier test that demanded zero writes, which
contradicted the stated policy. The four snapshot states behave distinctly, and
an operator restart holding an invalid document leaves workloads untouched.

**Shared operation deduplication ([§10.4](#104-shared-operation-deduplication)).**
With three Sites, metalman's support RBAC is applied **once** per pass, not
three times. Unequal operations sharing a key are rejected rather than
last-writer-wins.

**Snapshot and consistency ([§9.6](#96-snapshot-and-consistency)).** A pass
records the observed `resourceVersion`; a ConfigMap edit mid-pass does not
corrupt the in-flight result; the subsequent watch-triggered pass converges.

**Conflict normalization ([§6.5](#65-what-counts-as-a-conflict)).** Per operation
type: identical scalar values do not conflict; differing ones do; appended
tolerations never conflict; Cartesian affinity never conflicts; `extraArgs`
concatenate in contributor order; `addContainers` conflicts only on non-identical
definitions of the same name. Conflict messages name both contributors by
ConfigMap key and entry index.

**Hash comparability.** With one ConfigMap targeting three workloads, each
workload's desired hash equals its applied hash when healthy. The earlier
per-object-versus-whole-ConfigMap scheme would have reported permanent
divergence, so this is a regression test for a specific defect. Status list
entries are distinguishable when a Deployment and a DaemonSet share a name.

**Zero-Site observability ([§11](#11-drift-visibility-and-observability)).** With
no Sites and a retained singleton, an invalid document still produces an Event
on the overrides ConfigMap and an error log.

**Merge semantics.** Container merge by name; sidecar addition via
`addContainers`; volume and volumeMount addition; `retainKeys` sibling
preservation on a partial volume patch; env merge by name; additive toleration
and nodeSelector merge; workload level labels and annotations; `spec.replicas`;
`extraArgs` append; `extraArgs` combined with a patch that replaces `args`.

**Resolution.** Absent `sites` matching every Site; partial overlap (`[a,b]` and
`[b,c]`) failing only site `b`; deterministic ordering across ConfigMap keys;
unmatched Site names reported without failing.

**Watch and fan-out.** A ConfigMap change while the Site List fails must still
reach Site components once the List recovers; this must fail against the
`RequestSingletonAndAllSites` wiring. Synchronous fan-out publishes Site status
for every Site it touches.

**Plan parity.** Golden-plan tests asserting each component's `Plan` output
matches the operations the pre-refactor code performed, for all five components.
Plus: an identical override produces an identical result on `metalman`'s typed
path and on `net`'s unstructured path.

**CLI authority ([§12](#12-cli)).** `overrides validate` performs no resolution
and its output says so. `overrides status` issues no rendering or hashing calls,
asserted by fake-client call counting, so a version-skewed plugin cannot
misreport.

**Integration (real API server, kind).** There is no envtest in this repository
and adding it would introduce a second real-API-server mechanism alongside the
kind harness `e2e/operator/` already provides, so this coverage extends that
suite instead. It is guarded by `//go:build e2e` and runs in CI through the
existing `operator-e2e-kind` workflow, whose path filters already cover
`internal/operator` and `e2e/operator`.

**It drives the real `SiteReconciler`.** The first implementation rebuilt a
simplified copy of the reconcile pass, which meant the parts most likely to be
wrong were checked against a reimplementation that could drift from the
original, and had: the copy dropped only the first overridable operation on an
unusable document where the operator drops all of them.

It covers what the fake client cannot, because its Apply is a stub, its
validation is negligible, and server-side apply ownership and `managedFields`
are real apiserver behaviour:

- An override applied and then removed restores the operator's own value, with
  the operator asserted present in `managedFields`, since that ownership is the
  mechanism the revert guarantee rests on.
- An invalid document leaves a running workload byte-identical rather than
  reverting it, while an object overrides cannot target still reconciles.
- A field owned by a competing field manager survives override removal,
  documenting the limit of the guarantee.
- **Order is inferred, not declared.** A component plans its DaemonSet and
  ConfigMap before the namespace they live in, with no `DependsOn` anywhere. The
  fake client accepts a write into a namespace that does not exist; a real
  apiserver returns `NotFound`, so the pass only succeeds if the namespace was
  hoisted ahead of them.
- **A rejected write is not reported as applied.** The document used is valid by
  every rule the operator enforces: allowlisted path, real container, string
  value. Only the apiserver knows `cpu: banana` is not a quantity. This is the
  end-to-end form of [§9.7](#97-desired-versus-applied), and nothing short of a
  real apiserver can produce it.
- The namespace is created by the operator rather than by the test, so every
  case above also depends on there being exactly one owner for it.

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

Implementation settled several of these. They are recorded rather than deleted,
so the reasoning survives:

- *Operation taxonomy, hash timing and planning verdicts.* All three were gaps
  found while implementing and are corrected in
  [§10.1](#101-the-operation-plan) and [§14](#14-implementation).
- *envtest.* Not present in this repository; coverage extends the existing kind
  harness ([§15](#15-testing)).
- *Where singleton override state is reported.* On every Site, since every Site
  depends on the singletons ([§11](#11-drift-visibility-and-observability)).

Three questions carried by earlier revisions were **resolved** before
implementation began:

- *Dedicated resource type instead of a ConfigMap.* Resolved in favour of the
  ConfigMap. The argument rested on RBAC being unable to scope `create` by name.
  Admission policy can, the repository already relies on it, and in the one case
  that mattered no `create` grant needs to exist at all. See
  [§4.4](#44-residual-risk-and-how-it-is-closed).
- *Last-known-good across restarts.* No longer applicable. The failure model
  holds no in-memory state
  ([§9.3](#93-invalid-overrides-skip-they-do-not-revert)).
- *Whether to add a machina `ValidatingAdmissionPolicy`.* Not needed; the grant
  it would have constrained is unused and is deleted instead, which also avoids
  raising the Kubernetes baseline to 1.30.

Remaining:

1. **Admission-time validation, scoped realistically.** An earlier revision
   claimed a `ValidatingAdmissionPolicy` could reject schema errors, protected
   paths, `$` directives, and nulls in the overrides ConfigMap. That was
   overstated: CEL has no YAML parser, so a VAP cannot inspect structure inside
   `ConfigMap.data`. It **can** enforce writer identity, restrict `create` by
   name, cap `data` size, and constrain key naming. Deeper validation needs a
   webhook, which is a new serving path and certificate to maintain. Is the
   shallow coverage worth an installed policy, given `overrides validate` covers
   syntax offline?
2. **Operator-side dry-run.** [§12.1](#121-why-there-is-no-overrides-diff)
   rejects a client-side `diff` on version-skew grounds, and
   [§12](#12-cli) scopes `validate` to syntax for the same reason. An
   operator-side dry-run would close the resolution gap authoritatively, and the
   operation plan makes it cheap: plan, merge, report, do not execute. Worth a
   surface?
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
8. **Scaling assumptions.** Synchronous fan-out
   ([§10.3](#103-watch-and-fan-out)) is O(Sites) per override change and
   `MaxConcurrentReconciles` is now pinned to 1 explicitly. Both are recorded
   assumptions rather than permanent choices; `source.Channel` with explicit
   backpressure is the fallback if Site counts grow past the low hundreds or the
   controller is parallelized.
9. **Retention of overrides for deleted Sites.** Per-Site components clean up on
   Site deletion (`storage.go:78-90`), but an override entry naming a deleted
   Site becomes inert and is merely reported
   ([§6.3](#63-resolution)). Should stale entries be surfaced more strongly, for
   example as a Degraded condition after some period, or is inert-and-reported
   the right behaviour?
