---
title: "Workload Overrides"
weight: 6
description: "Customize the Deployments and DaemonSets the unbounded operator generates."
---

## Overview

The operator generates and reconciles the workloads for its components. It
applies them with server-side apply and takes ownership of every field it
declares, so editing one of those fields directly does not work: the change is
reverted on the next reconcile, and a GitOps controller managing the same object
will fight the operator indefinitely.

Fields the operator does not declare are left alone, which is what makes
[annotating a ServiceAccount](#annotating-a-serviceaccount) work. That is a
narrow guarantee, though, and not one to build on for workloads: the set of
fields the operator declares grows between releases, so a field it ignores today
may be reclaimed tomorrow.

Workload overrides are the supported way to customize them. You write strategic
merge patches into a ConfigMap, and the operator merges them into the workloads
it generates before applying.

## Security: treat write access as cluster-admin

**Write access to the overrides ConfigMap is equivalent to root on every node in
every affected Site, and therefore to cluster-admin.**

This is a property of the mechanism, not a limitation that could be tightened.
`unbounded-net-node` and `unbounded-storage-supervisor` already run with
`hostNetwork`, `hostPID`, `privileged: true` containers and hostPath mounts of
the host root filesystem. Against pods in that state, changing a container
image, changing its arguments, injecting an environment variable, or adding a
sidecar is arbitrary code execution on every node in the Site. Rejecting
`privileged: true` or new `hostPath` volumes would achieve nothing, because
those are already present.

`gantry` and `machina-controller` are not in that state, but it makes no
difference to the conclusion: reaching any node is enough, and both `net` and
`storage` are reachable from a single ConfigMap key.

Consequently:

- Grant write access to cluster administrators only. Do not include it in
  namespace-wide ConfigMap grants for platform or application teams.
- Audit changes to it, and put it under the same review as RBAC changes.
- The field restrictions described below are **integrity controls**, not a
  privilege boundary. They stop an authorized operator from accidentally
  breaking the operator's ability to reconcile a workload. They are not
  containment.

## What is supported, and what is not

**The mechanism is supported.** The document schema, gated by `apiVersion`, the
merge semantics, the validation behaviour and the revert behaviour are all
maintained, and a document that is valid today keeps working within its declared
`apiVersion`.

**Your particular patch is not.** Container names, volume names and the shape of
generated workloads are implementation details that may change in any release. A
patch naming a container that a later release renames stops resolving, and is
reported rather than silently ignored. Nothing guarantees that an overridden
component functions correctly, passes its health checks, or upgrades cleanly.

## Writing overrides

Overrides live in a ConfigMap named `unbounded-component-overrides` in the
operator's namespace. The operator only reads it: it is never created for you,
and an absent ConfigMap means no overrides.

Every key in the ConfigMap is parsed as an independent document, so you can split
by concern or by ownership.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-component-overrides
  namespace: unbounded-system
data:
  overrides.yaml: |
    apiVersion: overrides.unbounded-cloud.io/v1alpha1
    overrides:
      - component: storage
        kind: DaemonSet
        sites: [edge-west]
        patch:
          spec:
            template:
              spec:
                containers:
                  - name: run
                    resources:
                      limits:
                        memory: 512Mi
```

### Entry fields

| Field | Required | Meaning |
|---|---|---|
| `component` | yes | `net`, `machina`, `gantry`, `metalman` or `storage`. |
| `kind` | yes | The kind that component emits. With `component` this identifies every workload the operator emits, so you never write a derived per-Site name. A pair the component cannot produce, such as `machina`/`DaemonSet`, is rejected rather than left to match nothing. |
| `sites` | no | **Per-Site components only**, meaning `metalman` and `storage`. Naming it on `net`, `machina` or `gantry` is an error, because those are cluster singletons and there is one of each for the whole cluster. **Omit it to match every Site.** An empty list is an error, since it is far likelier to be a mistake than an intent to match nothing. |
| `patch` | no | A strategic merge patch against the whole workload object, so `metadata.labels`, `metadata.annotations`, `spec.replicas` and the pod template are all reachable. |
| `extraArgs` | no | Arguments to append, keyed by container name. See below. |
| `addContainers` | no | Names of containers this entry intends to create rather than modify. |
| `addInitContainers` | no | As `addContainers`, for init containers. |

At least one of `patch` and `extraArgs` must be present.

Each component emits one kind, except `net`:

| Component | Kinds | Per-Site |
|---|---|---|
| `net` | `Deployment` and `DaemonSet` | no |
| `machina` | `Deployment` | no |
| `gantry` | `DaemonSet` | no |
| `metalman` | `Deployment` | yes |
| `storage` | `DaemonSet` | yes |

### Always use `extraArgs` to add arguments

`args` and `command` carry no strategic merge key, so a patch that sets `args`
**replaces the whole list**, dropping every argument the operator injected and
never receiving new ones added in later releases. `metalman` makes this concrete:
its arguments begin with the `serve-pxe` subcommand, so a replacing patch stops
the container starting at all.

```yaml
      - component: metalman
        kind: Deployment
        extraArgs:
          metalman: ["--operation-max-concurrent-machines=20"]
```

**The operator cannot check that a component accepts a flag.** It knows nothing
about any component's command line, and these components exit non-zero on an
unrecognised flag, so a typo here is a `CrashLoopBackOff` rather than a
validation error. Check the component's `--help` before adding anything, and
note that a setting exposed in a component's config file is often not exposed as
a flag at all.

`extraArgs` appends after any patch, so if you do both, the result is the
replaced list followed by the appended arguments.

### Adding a sidecar requires saying so

Strategic merge cannot tell a sidecar from a typo: both are "this name is not
present", and merging by name would silently create a container either way. A
patch meaning to raise a limit on `machina-controller` but spelling it
`machina-contoller` would add an image-less container and leave the real limit
untouched.

Entries are therefore modify-only unless you name the addition. The name must
also be defined in the same patch, or nothing would be created, and a name
cannot appear in both `addContainers` and `addInitContainers`, because
Kubernetes requires container names to be unique across the two lists:

```yaml
      - component: gantry
        kind: DaemonSet
        addContainers: [log-shipper]
        patch:
          spec:
            template:
              spec:
                containers:
                  - name: log-shipper
                    image: fluent/fluent-bit:3.1
```

If a later release adds an operator container with the same name as your
sidecar, the entry starts failing on upgrade: `addContainers` names something
that now exists, which is an error rather than a silent merge into the
operator's container. The workload keeps the spec it had, and the Site reports
`Degraded` naming the file, entry index and container. Rename your sidecar and
it reconciles again. Prefixing sidecar names with something of your own is the
cheapest way to never meet this.

### Scheduling is added to, not replaced

`nodeSelector`, `tolerations` and `affinity` are combined with the operator's own
constraints rather than replacing them, because the operator relies on them:
`metalman` and `storage` place their workloads with a mandatory per-Site node
affinity, and replacing it would let two Sites' workloads run on the same nodes.

Node affinity terms are ORed by Kubernetes, so combining yours with the
operator's produces the product of both, and every resulting term carries both
constraints. That product is capped at 128 terms: it is multiplicative, and the
operator already contributes two on every per-Site workload.

Tolerations and `topologySpreadConstraints` are appended, which means two
entries supplying constraints with the same `topologyKey` both land rather than
one replacing the other. A `nodeSelector` key the operator already sets cannot
be given a *different* value; setting it to the same value is accepted and does
nothing.

One consequence worth knowing: because scheduling is combined rather than
merged, rehearsing a scheduling patch with `kubectl patch --type=strategic` shows
a **more destructive** result than the operator produces, since it replaces
where the operator appends.

There is no faithful client-side rehearsal. `overrides validate` checks the
document but renders nothing, because the combined result depends on the
workload the running operator generates. Apply the change and read
`overrides status`.

### What you cannot change

Some fields are rejected, and are also restored after the merge so correctness
does not depend on the check being exhaustive:

| Field | Why |
|---|---|
| `apiVersion`, `kind` | The object's group, version and kind decide what resource is written. The operator holds `escalate` and `bind` on ClusterRoleBindings, so this is a genuine security boundary rather than an integrity one. |
| `metadata.name`, `.namespace` | Renaming orphans the original; the operator does not prune. |
| `metadata.ownerReferences`, `.finalizers` | Per-Site garbage collection depends on them. |
| `spec.selector` | A workload whose template labels stop satisfying its selector is rejected by the API server. |
| Template labels the selector matches | Same reason. Setting one to the value it already has is permitted, since it changes nothing. |
| `metadata.resourceVersion` | The operator applies rather than updates, so a `resourceVersion` here is always wrong. Watch for this when pasting from `kubectl get -o yaml`. |
| `status` | Owned by the workload controller, not by the operator. Also easy to paste in by accident. |
| `spec.template.spec.serviceAccountName` | Retargeting borrows another identity's API permissions. |
| `hostNetwork`, `hostPID`, `hostIPC` | Deliberate per-component decisions. |
| Labels and annotations under `unbounded-cloud.io/` | They carry config hashes, Site scoping and override visibility. |
| `spec.replicas` on `metalman` | The Site owns it: set `spec.components.metalman.replicas`. See below. |
| Repointing an operator-declared mount | Mount identity is `(container, mountPath)`, because `volumeMounts` merge on `mountPath` rather than on name, so protecting them by name would be bypassable. Mounting a *different* volume at a path the operator already mounts is refused; adjusting the same mount, for example `readOnly`, is not. |
| Operator-declared volumes | `volumes` merge on `name`, so redefining one repoints every mount that uses it without naming a `mountPath` anywhere. Adding volumes under new names is fine. |

Strategic merge directives (any `$`-prefixed key) and explicit `null` values are
rejected everywhere, because both can delete operator-managed content.

**An override cannot delete a field.** Explicit `null` is refused everywhere,
because that is how strategic merge removes operator-managed content. Replacing
a list wholesale is still possible, and `args` is the case where that matters:
see above. Where Kubernetes says two fields may not both be set, adding one is
not enough, so the change cannot be expressed at all. Two cases are detected and reported rather than left to fail at apply time:
setting `value` on an env variable the operator defines with `valueFrom` (or the
reverse), and setting `spec.strategy.type: Recreate` on a Deployment whose
`rollingUpdate` block the operator sets.

Values are checked against the type Kubernetes requires. Writing `containers:`
as a mapping rather than a list, or `nodeSelector:` as a list rather than a
mapping, is reported against the field rather than merged into a workload the
API server will later refuse.

### Typed Site fields win

`Site.spec` is the supported customization surface. Overrides are the escape
hatch for everything it does not cover, so where the two describe the same
thing, the typed field decides and the override is rejected with the name of
the field to use instead.

Today there is one such field:

| Override path | Set this instead |
|---|---|
| `spec.replicas` on `metalman` | `spec.components.metalman.replicas` |

`spec.replicas` remains available on `net` and `machina`, whose Deployments have
no typed replica count.

One case is **not** enforced. `spec.components.metalman.dhcpAutoInterface` adds
a command-line flag, and `extraArgs` appends flags, so an override can append
one that contradicts it. Detecting that would mean the operator understanding
each component's flag semantics. If you use `dhcpAutoInterface`, do not also
pass DHCP interface flags through `extraArgs`.

## The overridable surface

Anything not listed here is rejected. A path ending in `.*` is a subtree: every
field below it is permitted, including fields added by future Kubernetes
releases. That is deliberate rather than an oversight, since a new field under
`securityContext` is not a new capability for a principal who can already
replace the container image, and enumerating leaves would mean revisiting this
list every Kubernetes minor release for no security benefit.

The allowlist is therefore fail-closed at the path level and fail-open within a
permitted subtree. It is the same list the operator compiles, held against this
document by a test.

<!-- BEGIN GENERATED: permitted paths -->
- `metadata.annotations.*`
- `metadata.labels.*`
- `spec.minReadySeconds`
- `spec.replicas`
- `spec.revisionHistoryLimit`
- `spec.strategy.*`
- `spec.template.metadata.annotations.*`
- `spec.template.metadata.labels.*`
- `spec.template.spec.affinity.*`
- `spec.template.spec.containers.*.args.*`
- `spec.template.spec.containers.*.command.*`
- `spec.template.spec.containers.*.env.*`
- `spec.template.spec.containers.*.envFrom.*`
- `spec.template.spec.containers.*.image`
- `spec.template.spec.containers.*.imagePullPolicy`
- `spec.template.spec.containers.*.lifecycle.*`
- `spec.template.spec.containers.*.livenessProbe.*`
- `spec.template.spec.containers.*.name`
- `spec.template.spec.containers.*.ports.*`
- `spec.template.spec.containers.*.readinessProbe.*`
- `spec.template.spec.containers.*.resources.*`
- `spec.template.spec.containers.*.securityContext.*`
- `spec.template.spec.containers.*.startupProbe.*`
- `spec.template.spec.containers.*.terminationMessagePath`
- `spec.template.spec.containers.*.terminationMessagePolicy`
- `spec.template.spec.containers.*.volumeMounts.*`
- `spec.template.spec.containers.*.workingDir`
- `spec.template.spec.dnsConfig.*`
- `spec.template.spec.dnsPolicy`
- `spec.template.spec.imagePullSecrets.*`
- `spec.template.spec.initContainers.*.args.*`
- `spec.template.spec.initContainers.*.command.*`
- `spec.template.spec.initContainers.*.env.*`
- `spec.template.spec.initContainers.*.envFrom.*`
- `spec.template.spec.initContainers.*.image`
- `spec.template.spec.initContainers.*.imagePullPolicy`
- `spec.template.spec.initContainers.*.lifecycle.*`
- `spec.template.spec.initContainers.*.livenessProbe.*`
- `spec.template.spec.initContainers.*.name`
- `spec.template.spec.initContainers.*.ports.*`
- `spec.template.spec.initContainers.*.readinessProbe.*`
- `spec.template.spec.initContainers.*.resources.*`
- `spec.template.spec.initContainers.*.restartPolicy`
- `spec.template.spec.initContainers.*.securityContext.*`
- `spec.template.spec.initContainers.*.startupProbe.*`
- `spec.template.spec.initContainers.*.terminationMessagePath`
- `spec.template.spec.initContainers.*.terminationMessagePolicy`
- `spec.template.spec.initContainers.*.volumeMounts.*`
- `spec.template.spec.initContainers.*.workingDir`
- `spec.template.spec.nodeSelector.*`
- `spec.template.spec.priorityClassName`
- `spec.template.spec.runtimeClassName`
- `spec.template.spec.schedulerName`
- `spec.template.spec.securityContext.*`
- `spec.template.spec.terminationGracePeriodSeconds`
- `spec.template.spec.tolerations.*`
- `spec.template.spec.topologySpreadConstraints.*`
- `spec.template.spec.volumes.*`
- `spec.updateStrategy.*`
<!-- END GENERATED: permitted paths -->

These are refused outright, and are also restored after the merge so that
correctness does not depend on the check being exhaustive:

<!-- BEGIN GENERATED: protected paths -->
- `apiVersion`
- `kind`
- `metadata.finalizers`
- `metadata.name`
- `metadata.namespace`
- `metadata.ownerReferences`
- `metadata.resourceVersion`
- `spec.selector`
- `spec.template.spec.hostIPC`
- `spec.template.spec.hostNetwork`
- `spec.template.spec.hostPID`
- `spec.template.spec.serviceAccountName`
- `status`
<!-- END GENERATED: protected paths -->

The allowlist does not vary by kind. `spec.replicas` is permitted for a
`DaemonSet` entry as far as this list is concerned, and is then rejected by the
API server, which has no such field on a DaemonSet.

## Annotating a ServiceAccount

Overrides reach Deployments and DaemonSets only, so they cannot annotate a
ServiceAccount. You do not need them to: **annotations you add to a component
ServiceAccount are preserved.**

```bash
kubectl annotate serviceaccount -n unbounded-system unbounded-net-controller \
  azure.workload.identity/client-id=00000000-0000-0000-0000-000000000000
```

This is what workload identity needs on every cloud
(`azure.workload.identity/client-id`, `eks.amazonaws.com/role-arn`,
`iam.gke.io/gcp-service-account`), and it survives every reconcile.

The mechanism is worth knowing, because it tells you where the boundary is. The
operator applies its ServiceAccounts with server-side apply and declares a name,
a namespace and labels. It declares **no annotations**, so it owns no key in
that map, and an apply cannot remove a field it does not declare. Your
annotation is owned by whatever wrote it and is left alone.

The same reasoning says what is *not* safe: a label the operator declares is
reverted, because it owns that key. This guarantee is covered by an end-to-end
test against a real API server, so a future release cannot quietly take it away.

## Checking your work

```bash
# Offline: syntax, schema and allowlist. Correct regardless of operator version.
# Accepts either a bare overrides document or the ConfigMap manifest you apply.
kubectl unbounded overrides validate -f overrides.yaml

# What the ConfigMap currently declares.
kubectl unbounded overrides list

# What the operator actually did with it.
kubectl unbounded overrides status
```

`validate` is offline, so its answer stays correct under version skew, and that
is exactly why it cannot answer anything that depends on the workload the
running operator renders. It does **not** check:

- whether a container, init container or volume you named exists;
- whether a `volumeMount` would repoint one the operator declares;
- whether a volume name collides with one the operator declares;
- whether an `env` entry sets `value` where the operator uses `valueFrom`, or
  the reverse;
- whether `spec.strategy.type: Recreate` collides with a `rollingUpdate` block
  the operator sets;
- whether two entries targeting one workload disagree;
- whether a `nodeSelector` key collides with one the operator sets;
- whether the combined affinity exceeds the 128-term cap;
- whether a template label you set is one the selector matches.

All of those are checked by the operator, and reported on the Site and as an
Event on the ConfigMap. Run `overrides status` after applying to confirm.

It also does not check that a flag in `extraArgs` exists. Nothing can: the
operator knows nothing about any component's command line.

Override state also appears on each Site:

```bash
kubectl get sites -o wide
kubectl get site edge-west -o jsonpath='{.status.overrides}'
```

Each workload in `status.overrides.workloads` carries a `state`:

| State | Meaning |
|---|---|
| `Applied` | Merged and written. Not a statement about pod health. |
| `Pending` | The write was deferred to the next pass because the cluster moved under this one. Nothing is wrong. |
| `Withheld` | The operator declined to write it, because an override that could have shaped it could not be used. The running workload is untouched. |
| `Failed` | The override could not be resolved, merged, or written. |

### Events on the ConfigMap

The document's fate is also recorded against the ConfigMap you edited, which is
where `kubectl describe` will show it:

```bash
kubectl describe configmap -n unbounded-system unbounded-component-overrides
```

| Reason | Meaning |
|---|---|
| `OverridesApplied` | Normal. Names how many workloads were overridden, and how many entries matched nothing. |
| `OverridesRejected` | Warning. Nothing applied; workloads were left unchanged. |
| `OverridesPartiallyRejected` | Warning. Some entries applied and some workloads were withheld. |
| `OverridesPartiallyApplied` | Warning. Entries resolved but one or more workloads could not be merged. |
| `OverridesNotWritten` | Warning. The merge succeeded but the write did not reach the cluster. |

This matters most when no Site exists yet: the ConfigMap is cluster-scoped, so
there is no Site status to carry the verdict and the Event is the only report
you get. Events fire when the observed `resourceVersion` or the verdict changes,
so a steady state stays quiet.

## Limits

A document that exceeds any of these is rejected. The parsing limits are not
arbitrary: yaml.v3's duplicate-key check is quadratic in the number of keys in
one mapping and is on by default, so the API server's own 1 MiB ConfigMap limit
would otherwise permit several seconds of operator CPU on every pass.

| Limit | Value |
|---|---|
| Bytes in one ConfigMap key | 256 KiB |
| Bytes across every key | 1 MiB |
| Entries in one document | 256 |
| Keys in one YAML mapping | 1024 |
| Nodes in one document | 20000 |
| Nesting depth | 32 |
| Combined required node affinity terms | 128 |

Two things truncate rather than fail: the `override-source` annotation past
8 KiB, and the message copied into Site status past 2048 bytes. Both say so, and
the full text is in the operator log and the ConfigMap Events.

## Parsing is strict

The `apiVersion` gate is only meaningful if the document that passes it is the
document you wrote, so anything ambiguous is rejected rather than interpreted:

- **Unknown fields**, at every level, including inside `patch`. This applies to
  the override schema, not to the contents of a permitted subtree: a `patch` may
  still set fields added by future Kubernetes releases.
- **Duplicate keys**, which most YAML decoders silently resolve to the last one.
- **More than one YAML document per key**, and any trailing content after a
  `---`.
- **YAML merge keys** (`<<`), which would let content reach the merge that
  validation never inspected.
- **Unquoted timestamps.** `maintenance-window: 2026-08-11` is a date to YAML,
  not a string. Quote it.

An empty or whitespace-only value is ignored rather than rejected, because
`kubectl create configmap --from-file` on an empty file is a plausible accident.

## When an override is wrong

The operator **leaves the affected workloads exactly as they are**. It does not
fall back to its default manifests.

That is deliberate. Defaults are not the current state, so falling back would
rewrite running infrastructure: a single mis-indented line would strip resources,
tolerations, sidecars and pinned images and roll the workload, including a
window with no available replica on the host-networked components. Leaving them
alone is recoverable; reverting them is not.

### How much is affected

This depends on how much the operator can work out about the failure, and it is
worth knowing before an incident.

| Failure | What is withheld |
|---|---|
| An entry fails validation | Only the workloads that entry could have resolved to, matched on its `component`, `kind` and `sites`. |
| An entry names a component that is not recognised | Nothing. Entries resolve by component, so it could never have matched a workload. |
| A ConfigMap key fails to parse | **Every** overridable workload, on every Site. The entries in that key were never read, so there is no way to know what they would have changed. |
| The ConfigMap cannot be read at all | As above. |

Keys are independent, so a key that fails to parse does not stop the other keys
being read: the entries in them still apply. Entries are independent within a
key for the same reason.

Everything that is not a Deployment or a DaemonSet keeps reconciling in every
case, including RBAC, Services, component ConfigMaps and deletions. An override
typo does not stop the operator doing its other work.

### What you will see

- The Site reports `Degraded`, with the offending key and entry index, and lists
  the affected workloads in `status.overrides.workloads` with `state: Withheld`.
- **The component that owns a withheld workload reports `Ready=False`** with
  reason `OverrideNotApplied`. It did not write what it planned, so it is not
  reconciled. Any automation waiting on
  `kubectl wait --for=condition=NetReady` will fail while the document is
  broken, which is the intended signal.
- An Event is recorded on the ConfigMap itself, `OverridesRejected` when nothing
  applied or `OverridesPartiallyRejected` when some entries did.
- The pass requeues, so fixing the document is picked up without further action.

The cost, which is deliberate, is that drift on a withheld workload is not
corrected until the document is fixed.

## Overriding a container image

You can, and it is reported loudly, because it breaks the version-lockstep
invariant: components otherwise run the operator's own version, and a pinned
image **survives operator upgrades indefinitely**. That makes it the likeliest
cause of an install behaving unlike its reported version.

An overridden image is recorded on the workload as an
`unbounded-cloud.io/version-drift` annotation, on the Site status, and called out
by `kubectl unbounded overrides status`.

## Annotations on an overridden workload

Every overridden workload carries these, which is the fastest way to answer
"why does this DaemonSet look like this?" from the object itself:

| Annotation | Meaning |
|---|---|
| `unbounded-cloud.io/override-hash` | Hash of the entries merged into this object. The matching desired hash is in `Site.status.overrides`, computed over the same set, so the two are directly comparable. |
| `unbounded-cloud.io/override-source` | The ConfigMap keys and entry indices that shaped the object, for example `resources.yaml[0],sidecar.yaml[2]`. Truncated to `,+N more` past 8 KiB, since Kubernetes caps all annotations on an object at 256 KiB and the hash is the authoritative record. |
| `unbounded-cloud.io/version-drift` | Present only when an override changed a container image. |

These keys are reserved: a patch that writes anything under
`unbounded-cloud.io/` is rejected.

## Multiple entries for one workload

Normally one entry carries all of a workload's changes, since a strategic merge
patch is structural and can set resources, add a sidecar and add a toleration at
once.

Multiple entries may target one workload, which is useful when different teams
own different ConfigMap keys. They compose in a deterministic order, and two
entries setting the same value identically is fine. Where they genuinely
disagree, the result is rejected rather than resolved by ConfigMap key ordering,
with both contributors named.

Three things compose rather than conflict, because they are additive by
construction:

- `tolerations` and `topologySpreadConstraints` are concatenated.
- `affinity` is combined as described above, and required node affinity takes a
  Cartesian product with the operator's own terms. The product is capped at 128
  terms, since it is multiplicative and any number of entries can target one
  workload.
- `nodeSelector` keys are unioned. Two entries setting the same key to
  different values do conflict, and so does an entry setting a key the operator
  already sets.

`extraArgs` is the exception that looks additive and is not allowed to be. Two
entries appending to the *same container* are rejected, because the arguments
concatenate and which one a component honours would then depend on what the
ConfigMap keys happen to be called. Split by container, not by argument.
