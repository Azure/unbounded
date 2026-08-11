---
title: "Workload Overrides"
weight: 6
description: "Customize the Deployments and DaemonSets the unbounded operator generates."
---

## Overview

The operator generates and reconciles the workloads for its components. Because
it applies them with server-side apply and takes ownership of the fields it
declares, editing one of those workloads directly does not work: the change is
reverted on the next reconcile, and a GitOps controller managing the same object
will fight the operator indefinitely.

Workload overrides are the supported way to customize them. You write strategic
merge patches into a ConfigMap, and the operator merges them into the workloads
it generates before applying.

## Security: treat write access as cluster-admin

**Write access to the overrides ConfigMap is equivalent to root on every node in
every affected Site, and therefore to cluster-admin.**

This is a property of the mechanism, not a limitation that could be tightened.
The workloads being patched already run with `hostNetwork`, `hostPID`,
`privileged: true` containers and hostPath mounts of the host root filesystem.
Against pods in that state, changing a container image, changing its arguments,
injecting an environment variable, or adding a sidecar is arbitrary code
execution on every node. Rejecting `privileged: true` or new `hostPath` volumes
would achieve nothing, because those are already present.

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
| `component` | yes | `net`, `machina`, `gantry`, `metalman` or `storage` |
| `kind` | yes | `Deployment` or `DaemonSet`. With `component` this identifies every workload the operator emits, so you never write a derived per-Site name. |
| `sites` | no | Per-Site components only. **Omit it to match every Site.** An empty list is an error, since it is far likelier to be a mistake than an intent to match nothing. |
| `patch` | no | A strategic merge patch against the whole workload object, so `metadata`, `spec.replicas` and the pod template are all reachable. |
| `extraArgs` | no | Arguments to append, keyed by container name. See below. |
| `addContainers` | no | Names of containers this entry intends to create rather than modify. |
| `addInitContainers` | no | As `addContainers`, for init containers. |

At least one of `patch` and `extraArgs` must be present.

### Always use `extraArgs` to add arguments

`args` and `command` carry no strategic merge key, so a patch that sets `args`
**replaces the whole list**, dropping every argument the operator injected and
never receiving new ones added in later releases. `metalman` makes this concrete:
its arguments begin with the `serve-pxe` subcommand, so a replacing patch stops
the container starting at all.

```yaml
      - component: machina
        kind: Deployment
        extraArgs:
          machina-controller: ["--max-concurrent-reconciles=20"]
```

`extraArgs` appends after any patch, so if you do both, the result is the
replaced list followed by the appended arguments.

### Adding a sidecar requires saying so

Strategic merge cannot tell a sidecar from a typo: both are "this name is not
present", and merging by name would silently create a container either way. A
patch meaning to raise a limit on `machina-controller` but spelling it
`machina-contoller` would add an image-less container and leave the real limit
untouched.

Entries are therefore modify-only unless you name the addition:

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

### Scheduling is added to, not replaced

`nodeSelector`, `tolerations` and `affinity` are combined with the operator's own
constraints rather than replacing them, because the operator relies on them:
`metalman` and `storage` place their workloads with a mandatory per-Site node
affinity, and replacing it would let two Sites' workloads run on the same nodes.

Node affinity terms are ORed by Kubernetes, so combining yours with the
operator's produces the product of both, and every resulting term carries both
constraints. Tolerations are appended. A `nodeSelector` key the operator already
sets cannot be overwritten.

One consequence worth knowing: because scheduling is combined rather than
merged, rehearsing a scheduling patch with `kubectl patch --type=strategic` shows
a **more destructive** result than the operator produces. Use
`kubectl unbounded overrides validate` instead.

### What you cannot change

Some fields are rejected, and are also restored after the merge so correctness
does not depend on the check being exhaustive:

| Field | Why |
|---|---|
| `apiVersion`, `kind` | The object's group, version and kind decide what resource is written. The operator holds `escalate` and `bind` on ClusterRoleBindings, so this is a genuine security boundary rather than an integrity one. |
| `metadata.name`, `.namespace` | Renaming orphans the original; the operator does not prune. |
| `metadata.ownerReferences`, `.finalizers` | Per-Site garbage collection depends on them. |
| `spec.selector`, and template labels it matches | A workload whose template labels stop satisfying its selector is rejected by the API server. |
| `spec.template.spec.serviceAccountName` | Retargeting borrows another identity's API permissions. |
| `hostNetwork`, `hostPID`, `hostIPC` | Deliberate per-component decisions. |
| Labels and annotations under `unbounded-cloud.io/` | They carry config hashes, Site scoping and override visibility. |
| Operator-declared mounts | Mount identity is `(container, mountPath)`, because `volumeMounts` merge on `mountPath` rather than on name, so protecting them by name would be bypassable. |
| Operator-declared volumes | `volumes` merge on `name`, so redefining one repoints every mount that uses it without naming a `mountPath` anywhere. Adding volumes under new names is fine. |

Strategic merge directives (any `$`-prefixed key) and explicit `null` values are
rejected everywhere, because both can delete operator-managed content.

Values are checked against the type Kubernetes requires. Writing `containers:`
as a mapping rather than a list, or `nodeSelector:` as a list rather than a
mapping, is reported against the field rather than merged into a workload the
API server will later refuse.

Anything not listed as overridable is rejected. Within a permitted subtree such
as `resources` or `securityContext`, fields added by future Kubernetes releases
are accepted.

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

`validate` deliberately does **not** check whether a container or volume you
named exists. That depends on the workload the running operator renders, and the
plugin may be a different version. Run `overrides status` after applying to
confirm resolution.

Override state also appears on each Site:

```bash
kubectl get sites -o wide
kubectl get site edge-west -o jsonpath='{.status.overrides}'
```

## When an override is wrong

If a document cannot be parsed, fails validation, or cannot be read, the operator
**leaves the affected workloads exactly as they are**. It does not fall back to
its default manifests.

That is deliberate. Defaults are not the current state, so falling back would
rewrite running infrastructure: a single mis-indented line would strip resources,
tolerations, sidecars and pinned images from every component at once and roll all
of them, including a window with no available replica on the host-networked
components. Leaving them alone is recoverable; reverting them is not.

The cost is that while a document is unusable, drift on those workloads is not
corrected. Everything else, including RBAC, Services and component ConfigMaps,
keeps reconciling. The Site reports `Degraded` with the offending key and entry.

A failure affecting one workload is scoped to that workload. If two entries
select overlapping Sites and disagree, only the Site they both select fails; the
others reconcile normally.

## Overriding a container image

You can, and it is reported loudly, because it breaks the version-lockstep
invariant: components otherwise run the operator's own version, and a pinned
image **survives operator upgrades indefinitely**. That makes it the likeliest
cause of an install behaving unlike its reported version.

An overridden image is recorded on the workload as an
`unbounded-cloud.io/version-drift` annotation, on the Site status, and called out
by `kubectl unbounded overrides status`.

## Multiple entries for one workload

Normally one entry carries all of a workload's changes, since a strategic merge
patch is structural and can set resources, add a sidecar and add a toleration at
once.

Multiple entries may target one workload, which is useful when different teams
own different ConfigMap keys. They compose in a deterministic order, and two
entries setting the same value identically is fine. Where they genuinely
disagree, the result is rejected rather than resolved by ConfigMap key ordering,
with both contributors named.
