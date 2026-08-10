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
9. [Wiring](#9-wiring)
10. [Drift visibility](#10-drift-visibility)
11. [CLI](#11-cli)
12. [Operational notes](#12-operational-notes)
13. [Implementation plan](#13-implementation-plan)
14. [Testing](#14-testing)
15. [Prior art](#15-prior-art)
16. [Open questions](#16-open-questions)

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

The **mechanism** is supported and versioned:

- The document schema, gated by a required `apiVersion`.
- The merge semantics, including patch strategy and `extraArgs` precedence.
- Validation behavior and the errors it reports.
- Revert behavior: removing an override restores the operator default.
- That a valid document continues to be accepted across operator releases within
  its declared `apiVersion`.

### What the project does not support

The **content of any individual user's patch** is not supported:

- That a given patch remains correct as generated manifests change between
  releases. Field paths, container names, and volume names are implementation
  details and may change in any release.
- That a component with an override applied functions correctly, meets its
  health checks, or upgrades cleanly.
- Any behavior of a component running an image other than the operator's own
  version (see [§10](#10-drift-visibility)).

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
  [§10](#10-drift-visibility) make cluster-side changes detectable, and the
  object itself should be under change control.
- Prefer managing it through the same review path as RBAC changes.

### 4.4 Residual risk

Several component ServiceAccounts currently hold namespace-wide ConfigMap write
in the operator namespace with no `resourceNames` restriction:

| Role | Location |
|---|---|
| `machina-controller` | `deploy/machina/02-rbac.yaml.tmpl:15-17` |
| `metalman-controller` | `deploy/machina/06-metalman-rbac.yaml.tmpl:59`, `:132` |
| `unbounded-net-controller` | `deploy/net/controller/02-rbac.yaml.tmpl:170`, `:173`, `:216` |

A compromised component holding one of these can write the overrides ConfigMap
and cause the operator to deploy arbitrary code on every node. This is a
pre-existing over-grant that this design makes materially more dangerous.

A parallel hardening change narrows these Roles to `resourceNames` for `get`,
`update`, `patch`, and `delete`. It is worth doing on its own merits and is
sequenced independently in [§13](#13-implementation-plan).

**It does not fully close the gap.** RBAC cannot scope `create` by
`resourceNames`, because the object name is not available to the authorizer for
create requests. A component retaining namespace-wide ConfigMap `create` can
therefore still seed the overrides ConfigMap when it does not already exist.

The only complete fix is a dedicated resource type, where `create` is scopable
by resource rather than by name. That was consciously not taken here in favour
of keeping the API surface small, and is recorded as an open question in
[§16](#16-open-questions) so it can be re-argued.

## 5. Alternatives considered

| | Mechanism | Expressiveness | Blast radius | Validation | Cost |
|---|---|---|---|---|---|
| A | Kustomize as a library | Unlimited | Whole cluster | None practical | High |
| B | Typed override struct on the CRD | Closed set | Chosen fields | Full OpenAPI | Low, permanent field creep |
| C | Strategic merge patch, target restricted to operator-emitted workloads | Everything within a workload | One object | Decode plus reserved-path checks | Low |
| D | SSA field disownership | "Let X own this" only | Chosen paths | N/A | Very low |
| E | User-supplied `MutatingAdmissionPolicy` | High | Cluster | CEL type-checked | Zero code |

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

**Privilege escalation.** `Site` is cluster-scoped and the operator's service
account is highly privileged: it installs CRDs, ClusterRoles, webhooks, and
host-networked privileged DaemonSets. An overlay is arbitrary object creation
executed with those credentials. Anyone able to write the overlay source can
mint a privileged workload or a ClusterRoleBinding. Patching an existing pod
template is a materially smaller grant: those workloads are already privileged,
so the delta is bounded, whereas arbitrary object creation is not.

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

### 5.3 Why not SSA field disownership

Having the operator stop declaring selected paths so another field manager can
own them is elegant, requires no new API, and is the correct answer to "let VPA
manage resources". It is not a general mechanism: it cannot add a sidecar or a
volume, it requires users to understand field managers, and it makes the
operator's declared field set the configuration surface. It remains a good
complement and is listed in [§16](#16-open-questions).

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
      supervisor: ["--verbose"]
    patch:
      spec:
        template:
          spec:
            tolerations:
              - key: edge
                operator: Exists
            containers:
              - name: supervisor
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
struct tags on the core types, which gives the behavior users expect:

| Field | Behavior |
|---|---|
| `containers`, `initContainers` | Merge by `name`. An unknown name adds a container. |
| `volumes` | Merge by `name`. |
| `env` | Merge by `name`. |
| `volumeMounts` | Merge by `mountPath`. |
| `imagePullSecrets` | Merge by `name`. |
| `tolerations` | Replace the whole list. |
| `args`, `command` | Replace the whole list. See [§7.2](#72-extraargs). |
| `nodeSelector`, `labels`, `annotations` | Map merge. |

The patch targets the **whole workload object**, not just the pod template, so
`metadata.labels`, `metadata.annotations`, `spec.replicas` and
`spec.updateStrategy` are reachable through the same field as
`spec.template.spec.*`. One field, one code path.

A useful consequence: the value of `patch` is exactly a
`kubectl patch --type=strategic` body. A user can validate a patch against a
live object before committing it, which is what makes this mechanism
debuggable in the field.

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
    controller: ["--max-concurrent-reconciles=20"]
```

Precedence is defined and documented: if an entry sets both `patch` (replacing
`args`) and `extraArgs` for the same container, the result is the replaced list
followed by the appended arguments. Naming a container that does not exist is a
validation error, since unlike an unknown Site name it cannot become valid
later.

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

The surface is an **allowlist**: paths not enumerated below are rejected.
An earlier revision of this design used a denylist, which fails open. Fields
added by future Kubernetes versions, and fields nobody thought to enumerate,
would have been silently patchable. Rejecting by default inverts that.

Per [§4.2](#42-the-allowlist-is-an-integrity-control-not-a-security-control),
these restrictions are integrity controls. They prevent an authorized operator
from accidentally severing the operator's ability to reconcile a workload. They
do not contain an adversary, with the two exceptions noted below.

### 8.1 Permitted

Within `spec.template.spec`:

| Path | Notes |
|---|---|
| `containers[*].image` | Breaks version lockstep, reported per [§10](#10-drift-visibility) |
| `containers[*].args`, `.command` | Replace semantics, see [§7.2](#72-extraargs) |
| `containers[*].env`, `.envFrom` | Merged by name |
| `containers[*].resources` | |
| `containers[*].volumeMounts` | Except mounts the operator declares, see [§8.2](#82-protected) |
| `containers[*].securityContext` | Except changes that would reduce an operator-set value |
| `containers[*].livenessProbe`, `.readinessProbe`, `.startupProbe` | |
| Added `containers`, `initContainers` | Names not present in the generated workload |
| `volumes` | Except volumes the operator declares |
| `imagePullSecrets` | |
| `nodeSelector`, `tolerations`, `affinity` | Additive only, see [§8.3](#83-additive-only-scheduling) |
| `topologySpreadConstraints` | |
| `priorityClassName` | |
| `dnsPolicy`, `dnsConfig` | |
| `terminationGracePeriodSeconds` | |

At the workload level:

| Path | Notes |
|---|---|
| `spec.replicas` | Deployments only |
| `spec.strategy`, `spec.updateStrategy` | |
| `metadata.labels`, `metadata.annotations` | Excluding the reserved prefix |
| `spec.template.metadata.labels`, `.annotations` | Excluding the reserved prefix and selector keys |

### 8.2 Protected

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
| Labels and annotations under `unbounded-cloud.io/` | Carry config hashes, Site scoping, and override visibility ([§10](#10-drift-visibility)). |
| Operator-declared `volumes` and `volumeMounts`, by name | `Volumes` uses `patchStrategy:"merge,retainKeys"` (`k8s.io/api/core/v1/types.go:4145`), so a partial patch silently drops sibling fields of the volume it names. |

Also rejected:

- **Any `$`-prefixed key** anywhere in the patch. Restricting only `$patch` and
  `$setElementOrder` was incomplete; the directive namespace is open and
  directives can delete operator-managed content.
- **Explicit `null` values.** Strategic merge treats null as deletion, which is
  a second route to removing managed content that a path allowlist alone does
  not catch.

### 8.3 Additive-only scheduling

`metalman` and `storage` place their workloads with a mandatory Site affinity
(`metalman.go:170`, `storage.go:261-268`) built by `SiteNodeAffinity`
(`env.go:503-514`), which is `RequiredDuringSchedulingIgnoredDuringExecution`.

`NodeSelectorTerms` carries no `patchMergeKey`
(`k8s.io/api/core/v1/types.go:3778`), so a patch supplying `nodeSelectorTerms`
**replaces** the operator's terms outright. A user adding an unrelated node
constraint would silently remove Site isolation and allow two Sites' workloads
onto the same nodes, violating the no-retarget goal in
[§2](#2-goals-and-non-goals).

Scheduling constraints are therefore merged additively rather than applied as a
raw patch:

- `affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`: user
  `matchExpressions` are appended to **each** operator term, so the result is
  the conjunction of the Site constraint and the user constraint. Operator terms
  are never removed or replaced.
- `affinity.nodeAffinity.preferredDuringScheduling...`, `podAffinity`, and
  `podAntiAffinity`: appended, since the operator sets none today. If a
  component later sets them, the same conjunction rule applies.
- `nodeSelector`: keys are merged; overwriting an operator-set key is rejected.
- `tolerations`: appended, never replaced, despite strategic merge's default
  replace semantics for that list.

### 8.4 Apply-time assertion

Independently of validation and re-stamping, the object is asserted to be
`apps/v1` `Deployment` or `DaemonSet` immediately before apply, and `ApplyObject`
itself gains a defensive check for objects carrying an override annotation.

Three layers for one property is deliberate. GVK escape is the difference
between a damaged workload and a cluster-admin binding
([§4.2](#42-the-allowlist-is-an-integrity-control-not-a-security-control)), so
it should not depend on any single check being correct.

## 9. Wiring

**Application point.** The patch is applied inside `Env.ApplyObject`
(`env.go:240`), gated on `kind` being `Deployment` or `DaemonSet`. This is the
only point the YAML path and `metalman`'s typed path share
([§3.1](#3-constraints-from-the-current-design)), so both get identical
semantics and **no component file changes are required**.

**Context propagation.** `ApplyObject` needs to know which component and Site it
is applying for. `Env` gains unexported `component` and `site` fields plus:

```go
// ForComponent returns a shallow copy of the Env scoped to one component and,
// for per-Site components, one Site. The reconciler sets this before handing
// the Env to a component so ApplyObject can resolve overrides.
func (e *Env) ForComponent(name, site string) *Env
```

`runComponents` already holds both values at its call sites
(`reconciler.go:175-182`), and `r.env()` constructs a fresh `Env` per Reconcile
(`reconciler.go:90-96`), so this introduces no shared mutable state.

**Load point.** The ConfigMap is read once per reconcile pass in
`runComponents`, alongside the existing `env.ListSites` call
(`reconciler.go:145`). Parse and validation failures are reported once against
every component rather than being attributed to whichever component applied
first.

**Watch.** Registered centrally in `SetupWithManager` (`reconciler.go:279-311`)
rather than per component, since the ConfigMap spans components:

```go
b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
    builder.WithPredicates(env.ManagedConfigPredicate(
        env.InNamespaceNamed(component.OverridesConfigMapName))))
```

Both helpers are reused unchanged. `RequestSingletonAndAllSites`
(`watch.go:35`) enqueues the Site-less singleton pass plus every Site, which is
exactly the fan-out a cross-component config change needs.
`ManagedConfigPredicate` (`watch.go:107`) matches on namespace, name, and
payload change with no ownership requirement, so it works on a user-owned
ConfigMap without modification.

**Rollout.** No config-hash annotation is needed for the override itself. The
patch changes the pod template, SSA writes the change, and the workload's own
rollout strategy takes over.

## 10. Drift visibility

An override is invisible in the generated manifests and survives operator
upgrades. A pinned image or a replaced argument list silently freezes a
component at an old version, and that is the state support will most often be
handed without being told about it. Deviation is therefore surfaced in four
places.

**On the workload:**

| Annotation | Meaning |
|---|---|
| `unbounded-cloud.io/workload-override-hash` | SHA-256 of the composed override. Changes when the override changes. |
| `unbounded-cloud.io/workload-override-source` | Contributing ConfigMap keys and entry indices. |
| `unbounded-cloud.io/version-drift` | Present only when a patch changed a container image. Value is `<container>=<image>`. |

**On the Site.** A new result constructor in
`internal/operator/component/result.go`:

```go
// ReconciledWithOverrides reports a component reconciled with user-supplied
// workload overrides applied. It is Ready, but carries the override summary so
// a customized install is visible in `kubectl get site`.
func ReconciledWithOverrides(message string) Result
```

`Ready` stays true, with `Reason: ReconciledWithOverrides` and a message listing
patched workloads, unmatched Site names, and any image drift. Validation and
conflict failures use the existing `component.Failed`, which already aggregates
into the reconcile error and requeues with backoff.

**As an Event** on the Site, gated on the override hash so it fires on change
rather than on every reconcile.

**Through the CLI**, see [§11](#11-cli).

Images are permitted with no additional gate. The signalling above is the whole
of the friction: users asked for image overrides, and the mechanism is an
acknowledged escape hatch rather than a supported configuration surface.

## 11. CLI

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

**`kubectl unbounded overrides diff`** renders the workload the operator would
generate, applies the override, and prints a unified diff. Operator-internal
noise (config-hash annotations, re-stamped identity fields) is excluded.

`diff` cannot work against the live object, because the live object already has
the override applied. It requires rendering the pre-patch object outside the
operator, and today rendering is fused into applying: the mutators are
unexported (`net.go:115`, `machina.go:123`, `gantry.go:187`, `storage.go:223`)
and `metalman`'s builder is an unexported typed function (`metalman.go:98`).

Supporting it therefore requires extracting a render capability from each
component:

```go
// Renderer is an optional component capability that returns the objects a
// component would apply for the given Env and Site, without applying them.
type Renderer interface {
    Render(ctx context.Context, env *Env, site *unboundedv1alpha3.Site) ([]*unstructured.Unstructured, error)
}
```

This is a real refactor across all five components and is sequenced as its own
behavior-preserving PR in [§13](#13-implementation-plan). It stands on its own
merits beyond `diff`: it makes components unit-testable without a client and
opens the door to an operator-internal dry-run mode later.

## 12. Operational notes

**Argument replacement.** Covered in [§7.2](#72-extraargs). It is the sharpest
edge in the design and must be prominent in user documentation, not only in the
API reference.

**Zero-available windows.** The `net` controller Deployment uses `maxSurge: 0`
with `maxUnavailable: 1` (`deploy/net/controller/03-deployment.yaml.tmpl:23-24`)
and `metalman` does the same (`metalman.go:141-142`), because both are
host-networked. An override that makes pods unschedulable or crash-looping
therefore produces a window with no available replica, not a stalled rollout
behind a healthy old pod. Overrides touching `resources`, `nodeSelector`,
`affinity`, or `tolerations` on those two workloads deserve particular care.

**Validation timing.** The API server will accept any ConfigMap. A malformed or
conflicting document is only detected in reconcile and reported as a Site
condition, so there is a window where the user believes the change landed.
`overrides validate` is the mitigation; moving validation to admission is
[§16](#16-open-questions).

**Upgrade behavior.** Overrides are not versioned against component releases. A
patch referencing a container or volume that a later release renames becomes a
validation error, which is loud. A patch that merges cleanly but is semantically
wrong for the new version is not detectable by the operator. Pinned images
survive upgrades indefinitely and are the most likely cause of an install that
behaves unlike its reported version.

**Ordering.** The ConfigMap may be created before or after the Sites it names,
and before or after the components it patches. Unmatched entries are inert and
reported ([§6.2](#62-resolution)).

## 13. Implementation plan

| PR | Scope | Depends on |
|---|---|---|
| 1 | This design document | - |
| 2 | Override engine: `internal/operator/component/override.go` (types, load, validate, resolve, compose, conflict detection, merge, re-stamp, drift detection), `Env.Overrides`, `Env.ForComponent`, the `ApplyObject` gate, `ReconciledWithOverrides` in `result.go`, load in `runComponents`, watch in `SetupWithManager`. No component file changes. | 1 |
| 3 | `kubectl unbounded overrides list` and `overrides validate` | 2 |
| 4 | Render/apply split across `net`, `machina`, `gantry`, `metalman`, `storage`. Behavior-preserving refactor. | 2 |
| 5 | `kubectl unbounded overrides diff` | 4 |
| 6 | Documentation: new reference page under `docs/content/reference/`, amend `architecture.md:189`, amend the `SiteComponentSpec` comment at `site_types.go:141-143`, update `cli.md`, add `deploy/unbounded-operator/examples/component-overrides.example.yaml` as a plain `.yaml` so neither `render-manifests` nor `go:embed` picks it up. | 2, 3, 5 |

No CRD change, no `make generate`, and no API version churn at any step.

## 14. Testing

**Merge semantics.** Container merge by name; sidecar addition; volume and
volumeMount addition; env merge by name; toleration list replacement; workload
level labels and annotations; `spec.replicas`; `extraArgs` append; `extraArgs`
combined with a patch that replaces `args`.

**Validation.** Missing, empty, and unrecognized `apiVersion`; unknown
`component`; unsupported `kind`; `sites` on a cluster singleton; explicitly
empty `sites`; `extraArgs` naming an absent container; `$patch` and
`$setElementOrder` directives; reserved paths; neither `patch` nor `extraArgs`
present; malformed YAML in one key not preventing other keys from loading.

**Resolution and composition.** Absent `sites` matching every Site; partial
overlap (`[a,b]` and `[b,c]`) failing only site `b`; disjoint concerns composing
cleanly; true conflict rejected with both entries named; deterministic ordering
across ConfigMap keys; unmatched Site names reported without failing.

**Invariants.** Patches attempting to change name, namespace, ownerReferences,
selector, or a selector-referenced template label do not survive the re-stamp
and are rejected at validation.

**Parity.** An identical override produces an identical result on `metalman`'s
typed path and on `net`'s unstructured path.

**Integration (envtest).** Override applied through SSA and observable on the
object; ConfigMap deleted and the default restored; ConfigMap payload change
triggering reconcile through the registered watch.

**End to end (`e2e/operator/`).** A resources override rolls the target
DaemonSet; removing it reverts; an image override sets `version-drift` and flips
the condition reason.

**Refactor safety (PR 4).** Golden-object tests asserting `Render` output equals
what the pre-refactor apply path produced, for all five components.

## 15. Prior art

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
consistent pattern is: bounded target, open content within it.

The project's own precedent points the same way. `deploy/gantry/README.md`
already ships a hardening overlay as an example for users to copy into their own
tooling rather than as something the operator consumes, and
`designs/gantry-unbounded-integration.md` states that raw manifests must remain
directly applicable for users who want full control.

## 16. Open questions

1. **`overrides diff` output format.** Unified diff of YAML is readable but
   noisy for deep pod specs. Worth considering a path-oriented summary as the
   default with full diff behind a flag.
2. **Admission-time validation.** A `ValidatingAdmissionPolicy` on the ConfigMap
   would close the window described in [§12](#12-operational-notes) without a
   webhook. It cannot do full validation, since resolving `component` and
   `sites` needs cluster state, but it could reject schema errors, unknown
   `apiVersion`, and reserved paths. Is the partial coverage worth the
   additional installed object?
3. **`spec.replicas` and `metalman.replicas`.** `Site` already has a typed,
   supported `spec.components.metalman.replicas` (`site_types.go:167`). An
   override patch can also set `spec.replicas`. Should the typed field win, the
   override win, or should overrides reject `spec.replicas` on `metalman`
   specifically?
4. **`siteSelector`.** Label-based Site matching would express "all edge sites"
   without enumeration and is more idiomatic than a name list. Deferred because
   it needs a `Site` labelling convention that does not exist today. It can be
   added alongside `sites` later with a documented precedence.
5. **SSA field disownership as a complement.** For the specific case of letting
   VPA own `resources`, having the operator stop declaring that path is more
   correct than patching it to a fixed value. Should that be offered as a
   separate opt-in, orthogonal to this mechanism?
6. **Reserved container names.** Should users be prevented from adding a sidecar
   whose name collides with a container the operator might add in a future
   release? A documented naming convention is probably sufficient.
