---
title: "Workload Overrides"
weight: 6
description: "How to customize the Deployments and DaemonSets the Unbounded operator generates by adding workload override documents to the unbounded-component-overrides ConfigMap."
---

## Overview

A workload override is a small YAML document that tells the Unbounded operator how to change one of the Deployments or DaemonSets it generates. You write the document as a value in a ConfigMap, and the operator merges it into the workload before the workload is written to the API server. Overrides let you adapt an installation to its environment without forking manifests or taking ownership of objects the operator reconciles.

There is no `WorkloadOverride` custom resource. The term in this page refers to an entry inside an overrides document, and the only interface is the `unbounded-component-overrides` ConfigMap. The operator reads that ConfigMap, validates every document in it, resolves each entry against the workloads it plans for the current pass, and applies the surviving entries, using a strategic merge patch for `patch` and an append for `extraArgs`. Nothing about the override is stored as a separate API object, so a document is the whole record of what you asked for; the workloads and Site status are the record of what happened.

The mental model has three layers. The **document** is one ConfigMap value and carries the API version, an optional `images` block, and a list of entries. An **entry** selects workloads by component, kind, optional name, and optional Site, then describes changes through `patch`, `extraArgs`, and container addition declarations. The **patch** is a strategic merge patch whose reachable fields are restricted to an allowlist. Once you know which layer a given piece of YAML belongs to, most error messages become self-explanatory.

{{< callout type="warning" >}}
Write access to the overrides ConfigMap is equivalent to root on every node in every affected Site, and therefore to cluster-admin. The workloads it patches already run privileged and host-networked with host path mounts, so changing an image, its arguments, its environment, or adding a sidecar is arbitrary code execution. Grant write access to cluster administrators only and audit changes the same way you audit RBAC changes.
{{< /callout >}}

## The overrides ConfigMap

The operator reads exactly one ConfigMap, named `unbounded-component-overrides`, in the namespace where the operator runs. The default install namespace is `unbounded-system`. The operator's `--namespace` flag sets the namespace it reconciles components into, and the flag defaults to the value of `POD_NAMESPACE` that the manifests inject through the Downward API, falling back to `unbounded-system` when that variable is unset. An installation in a custom namespace reads the ConfigMap from that custom namespace, and a `--namespace` override takes precedence over `POD_NAMESPACE`.

### Name and namespace

The name is fixed. You cannot configure a different ConfigMap name, and a second ConfigMap with a similar name is ignored. Use the following command to create or replace the ConfigMap from a manifest file:

```bash
kubectl apply -f component-overrides.yaml
```

A minimal ConfigMap wrapper looks like this:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-component-overrides
  namespace: unbounded-system
data:
  resources.yaml: |
    apiVersion: overrides.unbounded-cloud.io/v1alpha1
    overrides:
      - component: metalman
        kind: Deployment
        patch:
          spec:
            template:
              spec:
                containers:
                  - name: metalman
                    resources:
                      limits:
                        memory: 512Mi
```

The `data` keys are chosen by you. Each key becomes the origin shown in error messages, annotations, and status, so name keys for the concern they carry.

### The ConfigMap is optional

The operator never creates the ConfigMap. It is not seeded during installation and it is not written to during reconciliation. An absent ConfigMap unambiguously means "no overrides"; it does not mean "not initialized yet". Deleting the ConfigMap is therefore a deliberate request to return every managed workload to its default rendering on the next pass.

An existing ConfigMap with no usable entries is a different state. It is still read, and an entry that fails parsing or validation is still reported, so deleting a broken document is not the same as fixing it. The distinction matters in troubleshooting: absence applies defaults, while a rejected document leaves the affected workloads untouched.

### One document per key

Each ConfigMap key holds exactly one overrides document. The operator rejects a key that contains more than one YAML document or trailing content after a `---` separator, because silently applying half of what you wrote is worse than refusing the key. An empty or whitespace-only value is ignored, which makes an accidental empty file harmless.

Keeping one document per key is what lets a mistake stay contained. Keys are parsed and validated independently, so a malformed key does not prevent the other keys from being read. The operator attributes the failure to the key or entry that caused it and withholds only the workloads that failure puts in doubt. A key that cannot be parsed at all names no targets, so the operator has to treat every overridable workload as in doubt for that pass.

### Splitting documents by concern

Because keys are independent, split overrides by concern or by owning team. A platform team might own a `scheduling.yaml` key, a storage team a `volumes.yaml` key, and an observability team a `sidecars.yaml` key. Each team can then edit its own key without reviewing the others, and a bad edit in one key does not discard the entries in the rest.

The operator reads keys in sorted order and entries within a document in the order written. That ordering is deterministic, but it is not a precedence mechanism. When two contributors disagree about the same value, the operator reports a conflict rather than letting the alphabet decide which team wins. Use separate keys for independent concerns, not as a way to stack competing values.

## Document format

Every document uses the same top-level fields. Unknown fields are rejected at every level, including inside a patch, so a misspelled field is an error rather than a silent no-op.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `apiVersion` | string | Yes | Must be `overrides.unbounded-cloud.io/v1alpha1`. |
| `images` | mapping | No | Images that no emitted workload declares. Currently only `kubeProxy`. |
| `overrides` | list | Yes | Entries that each target one workload or one workload per Site. |

### apiVersion

The only accepted API version is `overrides.unbounded-cloud.io/v1alpha1`. The field is a gate, not documentation: a version the release does not recognize is rejected so a document written against a different schema is never reinterpreted. The error names the version you supplied and the version the operator expects.

The API version belongs to the overrides document, not to the ConfigMap. The outer object remains a core `v1` ConfigMap.

### The overrides list

The `overrides` field is a list of entries. A single document can declare up to 256 entries. Each entry has the fields described in [Override fields](#override-fields), and an entry must change something: it needs a non-empty `patch`, a non-empty `extraArgs`, or an internally generated kube-proxy image entry. Declaring a container in `addContainers` or `addInitContainers` alone is not enough, because the declaration only says that a name in the patch is intentional.

### The images block

`images` exists for images that cannot be reached by a patch. The managed kube-proxy DaemonSets are created at runtime by the net controller, not by the operator, and the controller takes their image as a flag on its own container. Setting `images.kubeProxy.image` is shorthand for passing that flag. The setting is validated, hashed, reported, and withheld exactly like an entry targeting the net controller Deployment.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
images:
  kubeProxy:
    image: myregistry.example.com/kube-proxy:v1.34.2
```

A reference with a tag or digest is used as written. A reference without one takes the tag of the image that `kube-system/kube-proxy` runs, so a mirror keeps the provider's version; if that DaemonSet does not exist, the controller searches the registry for a tag matching the API server's major.minor.patch version. Omit the tag only when you want that resolution to happen at runtime.

## Targeting workloads

An entry selects workloads with four fields. The operator matches them against the operations its components planned for the pass, and only operations a component marked as overridable are candidates. That restriction is what confines overrides to Deployments and DaemonSets; a document cannot reach a Service, a ConfigMap, a Role, or a custom resource even if the component plans one.

### Components and kinds

The component names the subsystem that generates the workload, and the kind names the object type. The pair must be one the component actually emits. An entry naming `machina` with kind `DaemonSet` is rejected, because machina emits no DaemonSet and the entry could never match anything.

| Component | Kinds it emits | Per-Site |
| --- | --- | --- |
| `net` | `Deployment`, `DaemonSet` | No |
| `machina` | `Deployment` | No |
| `gantry` | `DaemonSet` | No |
| `token-refresher` | `Deployment` | No |
| `racer` | `DaemonSet`, `Deployment` | No |
| `metalman` | `Deployment` | Yes |

Only `metalman` is a per-Site component in the override model. Its Deployment exists once per Site and carries the Site's name on the planned operation. Every other component in the table is a cluster singleton, so an entry targeting it must omit the `sites` selector.

### Exact workload names

The `name` field is optional. When it is empty, the entry matches every workload of that component and kind. When it is set, it must match the workload's name exactly and must be a valid DNS1123 subdomain. This is the field to use when a component emits more than one workload and you want to change only one of them.

The following names are useful starting points. They reflect the manifests in this release; confirm them against your cluster with `kubectl get deployment,daemonset -A` if you are unsure.

| Component | Workload | Kind |
| --- | --- | --- |
| `net` | `unbounded-net-controller` | `Deployment` |
| `net` | `unbounded-net-node` | `DaemonSet` |
| `machina` | `machina-controller` | `Deployment` |
| `gantry` | `gantry` | `DaemonSet` |
| `gantry` | `gantry-overlaybd-config` | `DaemonSet` |
| `token-refresher` | `token-refresher` | `Deployment` |
| `racer` | `racer` | `DaemonSet` |
| `metalman` | `metalman` | `Deployment` |

### Site selection

The `sites` field selects which Sites a per-Site entry affects. Omit it to match every Site. An explicitly empty list is rejected, because it is far more likely to be a mistake than an intent to match nothing. Listing a Site name more than once is also rejected.

Naming a Site that does not exist is inert and reported, not fatal. A document may legitimately be written before its Site is created, and deleting a Site must not retroactively invalidate an unrelated override. You will see the name in the operator log and in the warning printed by `kubectl unbounded overrides list`.

For a cluster singleton such as the net controller, `sites` must be omitted. The component runs once and has no Site to select. The operator rejects the selector rather than accepting an entry that can never match.

### What an entry can never target

Entries are matched on component, kind, name, and Site against planned operations. An entry naming a component that is disabled, or that is not installed on the cluster, resolves to nothing. That is reported as an inert entry rather than as a failure, because the document is valid even when its target is absent. The operator still records the verdict, so an entry that silently matches nothing does not look identical to an entry that was applied.

## Override fields

One entry describes both what to target and what to change. The targeting fields (`component`, `kind`, `name`, `sites`) are read before the change fields, which is why a broken patch can still be attributed to the workloads it would have touched.

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `component` | string | Yes | One of `net`, `machina`, `gantry`, `token-refresher`, `racer`, or `metalman`. |
| `kind` | string | Yes | `Deployment` or `DaemonSet`, and a kind the component emits. |
| `name` | string | No | Exact workload name. Omit to match every workload of that component and kind. |
| `sites` | list of strings | No | Sites to affect. Only for `metalman`; omit to match every Site. |
| `patch` | mapping | No | Strategic merge patch applied to the whole workload object. |
| `extraArgs` | mapping of string to list | No | Arguments appended to a container, keyed by container name. |
| `addContainers` | list of strings | No | Names in the patch that are intended as new containers. |
| `addInitContainers` | list of strings | No | Names in the patch that are intended as new init containers. |

### component

`component` is required and must be one of the six names in the table. Validation rejects an unknown name and lists the accepted values. It also rejects a known component paired with a kind that component does not emit, so a typo in either field cannot produce an entry that validates but overrides zero workloads.

### kind

`kind` is required and must be `Deployment` or `DaemonSet`. Capitalization matters. The pair check described above then narrows it to the kinds the component actually plans. `Deployment` and `DaemonSet` are the only object types the override mechanism accepts; there is no kind for a Pod, a Job, or a custom resource.

### name

`name` optionally restricts the entry to one exact workload. Leaving it empty is the common case for a component that emits one workload, and it is also the correct choice when you want the same change on every workload of that kind in the component. Setting it is how you distinguish `gantry` from `gantry-overlaybd-config`, or `unbounded-net-controller` from `unbounded-net-node`.

The value is checked as a DNS1123 subdomain. A name that does not exist in the current plan does not fail validation, because the plugin cannot know which workloads the running operator renders. It makes the entry inert for that pass, which the operator reports.

### sites

`sites` is meaningful only for per-Site components, which currently means `metalman`. A nil selector matches every Site, including the empty Site of a cluster singleton. An explicitly empty list is a validation error. Duplicates and empty strings inside the list are errors too.

An entry may name a Site that does not exist yet. Resolution reports it as an unmatched Site, and the entry is inert until the Site appears. This is deliberate: writing the override in advance is a normal workflow, and a Site deletion should not invalidate a document that also targets other Sites.

### patch

`patch` is a strategic merge patch applied to the whole workload object. It can reach workload labels and annotations, `spec.replicas`, rollout strategy, the pod template metadata, and the fields inside the pod template that the allowlist permits. The patch must be a mapping; a scalar or a list in that position is rejected.

Strategic merge is used instead of a JSON merge patch because it understands Kubernetes list semantics. Containers merge by `name`, environment entries by `name`, volume mounts by `mountPath`, ports by `containerPort`, and volumes and image pull secrets by `name`. That means a patch can raise a limit on one container without restating the others.

Two list types do not merge and are therefore dangerous in a patch. `args` and `command` carry no patch merge key, so a patch that sets either replaces the entire list. Use `extraArgs` for arguments. Replacing `command` is occasionally legitimate but drops any operator-supplied entrypoint, and replacing `args` drops every operator-injected flag and prevents new flags from arriving on upgrade.

### extraArgs

`extraArgs` appends arguments to a container's `args` list after all patches have been merged. It is keyed by container name, and each value is a list of strings. The field exists because a patch cannot append to `args`, and because appending is what you want for almost every customization: the operator's own flags survive, and so do flags added by a future operator upgrade.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: metalman
    kind: Deployment
    extraArgs:
      metalman: ["--operation-max-concurrent-machines=20"]
```

Every argument for one container must come from one entry. Two contributors appending to the same container is a conflict even when the lists are identical, because arguments concatenate and the result would depend on ConfigMap key ordering. The operator cannot tell whether a component accepts a flag, so a typo in `extraArgs` usually becomes a crash loop rather than a validation error. Check the component's `--help` before adding a flag.

### addContainers and addInitContainers

Strategic merge cannot distinguish a sidecar from a misspelled container name. Both look like "this name is not present", and merging by name silently creates an image-less container either way. `addContainers` and `addInitContainers` declare that the named containers in the patch are intentional additions rather than typos.

Names listed in `addContainers` must appear as containers in the patch, and names listed in `addInitContainers` must appear as init containers. A declaration without a matching definition is rejected, since it would create nothing. A name may not appear in both lists, because Kubernetes requires container names to be unique across the two. At resolution time, a name declared as an addition must not already exist on the workload, and a name in the patch that is neither existing nor declared is rejected with an instruction to declare it or correct it.

### images.kubeProxy.image

This field sets the image of the managed kube-proxy DaemonSets. It is not an entry in the `overrides` list, and writing it inside an entry is rejected as an unknown field. The value is validated with the same parser the net controller uses, so the operator and the controller cannot disagree about what a valid image reference is.

The setting resolves to the net controller Deployment and its `controller` container. At merge time the operator appends `--managed-kube-proxy-image=<value>` to that container after `extraArgs` have been appended. Passing the same flag yourself through `extraArgs` or a replaced `args` list is a conflict, because the controller would honor whichever came last.

## What a patch may change

The allowlist is an integrity control rather than a privilege boundary. An authorized writer can already replace an image or add a sidecar, which is arbitrary code execution on a privileged node agent. The allowlist exists to stop an accidental edit from severing the operator's ability to reconcile a workload: detaching it from its selector, its ServiceAccount, or its per-Site node affinity. Two exceptions are genuine security controls, the object's group, version, and kind, and `serviceAccountName`, because both escape the workload rather than merely damaging it.

### Permitted fields

A field marked as a subtree permits every descendant, including fields added by future Kubernetes releases. A leaf permits exactly that field. Container and init container fields are identical except that init containers also accept `restartPolicy`, which is how a native sidecar is declared.

| Path | Extent |
| --- | --- |
| `metadata.labels` | Subtree |
| `metadata.annotations` | Subtree |
| `spec.replicas` | Leaf |
| `spec.strategy` | Subtree |
| `spec.updateStrategy` | Subtree |
| `spec.minReadySeconds` | Leaf |
| `spec.revisionHistoryLimit` | Leaf |
| `spec.template.metadata.labels` | Subtree |
| `spec.template.metadata.annotations` | Subtree |
| `spec.template.spec.containers.*.name` | Leaf |
| `spec.template.spec.containers.*.image` | Leaf |
| `spec.template.spec.containers.*.imagePullPolicy` | Leaf |
| `spec.template.spec.containers.*.args` | Subtree |
| `spec.template.spec.containers.*.command` | Subtree |
| `spec.template.spec.containers.*.env` | Subtree |
| `spec.template.spec.containers.*.envFrom` | Subtree |
| `spec.template.spec.containers.*.resources` | Subtree |
| `spec.template.spec.containers.*.volumeMounts` | Subtree |
| `spec.template.spec.containers.*.securityContext` | Subtree |
| `spec.template.spec.containers.*.livenessProbe` | Subtree |
| `spec.template.spec.containers.*.readinessProbe` | Subtree |
| `spec.template.spec.containers.*.startupProbe` | Subtree |
| `spec.template.spec.containers.*.lifecycle` | Subtree |
| `spec.template.spec.containers.*.ports` | Subtree |
| `spec.template.spec.containers.*.terminationMessagePath` | Leaf |
| `spec.template.spec.containers.*.terminationMessagePolicy` | Leaf |
| `spec.template.spec.containers.*.workingDir` | Leaf |
| `spec.template.spec.initContainers.*.restartPolicy` | Leaf |
| `spec.template.spec.volumes` | Subtree |
| `spec.template.spec.imagePullSecrets` | Subtree |
| `spec.template.spec.nodeSelector` | Subtree |
| `spec.template.spec.tolerations` | Subtree |
| `spec.template.spec.affinity` | Subtree |
| `spec.template.spec.topologySpreadConstraints` | Subtree |
| `spec.template.spec.priorityClassName` | Leaf |
| `spec.template.spec.dnsPolicy` | Leaf |
| `spec.template.spec.dnsConfig` | Subtree |
| `spec.template.spec.terminationGracePeriodSeconds` | Leaf |
| `spec.template.spec.runtimeClassName` | Leaf |
| `spec.template.spec.schedulerName` | Leaf |
| `spec.template.spec.securityContext` | Subtree |

The init container list accepts the same container paths with `initContainers` in place of `containers`. The parity is intentional: Kubernetes uses the same container type for both, and a native sidecar runs for the life of the pod, so it needs probes, lifecycle hooks, and ports just as an ordinary container does.

`spec.strategy` applies to a Deployment and `spec.updateStrategy` applies to a DaemonSet. Both paths are permitted because the entry's `kind` decides which workload is patched, so match the field to the kind in the same entry; the other field does not describe that workload's rollout and will not have the effect you intend.

### Protected fields

Protected fields are rejected by name with the reason attached. They are also re-stamped after the merge, so correctness does not depend on the rejection list being exhaustive. The reason matters because most of these are valid Kubernetes fields that would appear to work if the operator accepted them.

| Path | Why it is protected |
| --- | --- |
| `apiVersion` | The group and version decide what resource is written. |
| `kind` | The kind decides what resource is written. |
| `metadata.name` | Renaming a workload orphans the original, and the operator does not prune. |
| `metadata.namespace` | Renaming a workload orphans the original. |
| `metadata.ownerReferences` | Owner references drive per-Site garbage collection. |
| `metadata.finalizers` | Finalizers drive deletion ordering the operator relies on. |
| `metadata.resourceVersion` | The operator applies rather than updates, so a resource version here is always wrong. |
| `spec.selector` | A template that stops satisfying its selector is rejected by the API server. |
| `spec.template.spec.serviceAccountName` | Retargeting it borrows another identity's API permissions and detaches the component from its RBAC. |
| `spec.template.spec.hostNetwork` | Host namespace membership is a deliberate per-component decision. |
| `spec.template.spec.hostPID` | Host namespace membership is a deliberate per-component decision. |
| `spec.template.spec.hostIPC` | Host namespace membership is a deliberate per-component decision. |
| `status` | Status is owned by the workload controller, not by the operator. |

One documented exception exists. The racer dataplane needs the host network namespace for RDMA devices that connect queue pairs through the RDMA connection manager, so a `racer` entry may set `spec.template.spec.hostNetwork` as a boolean. No other component and no other host namespace field has that exception.

### Values an override cannot express

An override can add and change, but it cannot delete. Explicit null is rejected everywhere, because strategic merge treats null as deletion, and deletion is how operator-managed content would be removed. A patch containing `resources: null`, for example, does not clear the resources; it is refused with an explanation that deletion is not available.

Keys beginning with `$` are rejected anywhere in a patch. They address the strategic merge directive namespace, which is open-ended and can delete operator-managed content. YAML merge keys (`<<`) are rejected during parsing for the same underlying reason: they would hide content from the allowlist walk.

A value in `metadata.labels`, `metadata.annotations`, `spec.template.metadata.labels`, `spec.template.metadata.annotations`, or `spec.template.spec.nodeSelector` must be a string. A non-string makes Kubernetes treat the whole map as absent, so the operator would replace every label or annotation with its own bookkeeping instead of merging into yours.

Labels and annotations under the `unbounded-cloud.io/` prefix are reserved. They carry config hashes, Site scoping, and override visibility, so a patch that wrote them could hide the fact that an override is in effect.

### Typed Site fields take precedence

`Site.spec` is the supported customization surface, and an override is the escape hatch for everything it does not cover. Where the two describe the same thing, the typed field wins. Today the only such conflict is `spec.replicas` on `metalman`, which is owned by `spec.components.metalman.replicas`; a patch that sets it is rejected with the name of the field to use instead.

The same rule is enforced twice. Validation rejects the path, and the merge captures the typed value before applying patches and restores it afterward. The second pass is what keeps the rule true if a future typed field is added without a matching validation rule. Accepting the patch instead would leave you editing the typed field to no effect, or force the operator to recompute it every pass and fight the override forever.

## Validation

Validation has two halves, and only one of them can run offline. Schema and allowlist validation is a pure function of the document, so the operator runs it before anything is written and the CLI can run the same checks without a cluster. Resolution validation needs the rendered workload, because whether a container or mount path exists depends on what the running operator emits, and a client built from a different commit cannot answer that correctly.

### Parse limits

Parsing is strict and bounded. Strictness exists so that the document the operator applies is the document you wrote. The limits exist because YAML duplicate-key checking is quadratic in the size of one mapping, and the operator reconciles with a single worker; an unbounded document could pin the controller.

| Limit | Value | Scope |
| --- | --- | --- |
| One ConfigMap value | 256 KiB | Per key |
| All values together | 1 MiB | Whole ConfigMap |
| Entries per document | 256 | `overrides` list |
| Keys in one mapping | 1,024 | Any mapping |
| Nodes in one document | 20,000 | Whole document |
| Nesting depth | 32 | Whole document |

The parser rejects unknown fields at every level, duplicate mapping keys, more than one YAML document per key, YAML merge keys, YAML timestamps, and values that cannot be represented as JSON. Quoting a value such as `2026-08-11` is required if you mean a string, because an unquoted date decodes to a timestamp and is refused instead of being silently coerced.

### Schema and allowlist checks

Entry validation reports every problem it finds, not just the first, and attributes each problem to its source. A source is rendered either as `<key>[<index>]` for an entry in the `overrides` list or as `<key>:images.kubeProxy` for the image shorthand. This is the string you will see in error messages, in the source annotation, and in Site status.

The checks are:

1. `component` is present and known, and `kind` is present, supported, and emitted by that component.
2. `sites` is absent or valid for the component, with no empty or duplicate entries.
3. `name`, when set, is a DNS1123 subdomain.
4. The entry changes something other than declaring a container addition.
5. Every `extraArgs` container name and argument list is non-empty.
6. `addContainers` and `addInitContainers` have no empty or duplicate names, do not overlap, and each declared name has a matching definition in the patch.
7. Every patch path is permitted, no protected path is written, no `$` directive appears, and no null is used.
8. Labels, annotations, and node selector values are strings, list and map shapes match the Kubernetes schema, and merge keys have the correct type.
9. Required node affinity is structurally valid, with at least one term, every term a mapping, and expression lists where Kubernetes expects them.
10. No patch sets a path owned by a typed Site field.

### Resolution checks

Resolution runs against the workload the operator actually rendered. These checks cannot be expressed in a patch alone because they depend on what is already there.

| Check | What it rejects |
| --- | --- |
| Container names | A patch targeting a container the workload does not have, unless the name is declared as an addition; declaring an addition that already exists. |
| `extraArgs` targets | A container name that is neither present nor declared. |
| kube-proxy target | A net controller Deployment with no `controller` container to receive the image flag. |
| Volume mounts | A mount that reuses an operator mount path in the same container but names a different volume. |
| Volumes | A patch that redefines a volume the operator already declares. |
| Environment sources | Setting `value` on an environment variable the operator defines with `valueFrom`, or the reverse. |
| Deployment strategy | Setting `type: Recreate` while a `rollingUpdate` block is present. |
| Selector labels | Changing a template label that the workload's selector matches. |
| Racer dataplane | Changing the canonical container count, `envFrom`, `volumeMounts`, `volumes`, `serviceAccountName`, `automountServiceAccountToken`, or operator-owned environment. |

Adding a new volume is allowed and is often necessary for a sidecar. Only redefining an existing name is refused. The distinction matters because volumes merge on name, so redefining a volume the operator declares would silently repoint every container that mounts it without any patch naming a mount path. Adding a new name has no such effect.

The selector label rule allows you to set a selector-matched label to the value it already has. That changes nothing and refusing it would fail a document that merely restates the workload. Changing it would be silently undone by the operator's restore step, so the error tells you to add a label under a different key instead.

## How the operator applies overrides

The operator folds overrides into its plan after every component has planned its operations and before anything is written. A pass reads the ConfigMap once, parses and validates it, and then applies the usable entries. The single read is important: every decision in a pass derives from the same snapshot, and the snapshot's `resourceVersion` is recorded so a result can be traced to the exact input that produced it.

### One pass, in order

1. Read the ConfigMap. A missing ConfigMap produces an empty snapshot.
2. Parse every key. A key-level failure is attributed to that key.
3. Validate the entries that parsed, and drop the entries that validation rejects.
4. Decide the quarantine: which workloads must not be written because an override that could have shaped them is unusable.
5. Resolve the remaining entries against overridable operations.
6. Apply the entries to a copy of each target workload.
7. Write the surviving operations and publish status and events.

Parsing and validation are completed before any write, so a deterministic document error is never half-applied. The operator does not retry unchanged invalid input; it waits for the ConfigMap watch to fire, because retrying the same bytes cannot make progress. An API read failure is different and is retried with backoff.

### Merge order

For each target workload, contributors are merged in a fixed order that does not depend on which one happens to be processed first:

1. Capture the operator's identity, the paths owned by typed Site fields, and the operator's scheduling constraints.
2. Strategic-merge each contributor's patch, with scheduling removed first.
3. Combine scheduling additively so the operator's constraints survive.
4. Append `extraArgs`.
5. Append the flag that `images.kubeProxy` stands for.
6. Restore typed Site fields.
7. Restore identity: API version, kind, name, namespace, owner references, finalizers, selector, selector-matched template labels, and reserved metadata.

Steps 6 and 7 are backstops. They make the group, version, and kind unbreakable rather than merely rejected, which matters because the apply path is directed by the object's group, version, and kind and the operator holds escalate and bind on ClusterRoleBindings. The same restore keeps a typed Site field authoritative over anything a patch says about it.

### Scheduling merge semantics

Scheduling fields are lifted out of each patch before the strategic merge, because `nodeSelectorTerms` carries no patch merge key and a raw merge would replace the operator's terms outright. A metalman Deployment relies on a mandatory per-Site node affinity, so replacing it would let two Sites' workloads schedule onto the same nodes.

| Field | Combination |
| --- | --- |
| `nodeSelector` | Union of keys. A key the operator sets to one value and the override sets to another is an error. |
| `tolerations` | Operator tolerations first, then user tolerations. |
| `topologySpreadConstraints` | Operator constraints first, then user constraints. |
| Required node affinity | Cartesian product of the term lists, because required terms are ORed and both constraints must hold. |
| Preferred node affinity, pod affinity, pod anti-affinity | Concatenated, because none is a hard constraint the operator relies on. |

The Cartesian product is bounded at 128 terms. The operator itself contributes at most two terms, matching the canonical and the deprecated Site label, so the bound leaves room for genuinely complex constraints while preventing an unbounded product from becoming a denial of service against the API server. Combining terms is also refused when the result would be a term with no constraints, because such a term matches every node and would widen scheduling rather than narrow it.

### extraArgs merge semantics

`extraArgs` is applied after all patches, which makes its precedence predictable. A patch may legitimately replace `args` wholesale; `extraArgs` then appends to whatever the patch left behind. Arguments from multiple containers are gathered first, sorted by container name and then by entry order, and appended to the matching container in both `containers` and `initContainers`.

The operator does not parse the arguments and cannot know whether a component accepts them. A flag removed in a new release, or a misspelled flag, will be passed through, and the component's own parser will exit non-zero. That is a crash loop rather than a validation error, so check the component's help output before adding anything.

### Conflicts between contributors

Two entries that resolve to the same workload are contributors to it. The operator does not resolve their disagreement by ordering. Deterministic ordering exists so composition is reproducible, not so that ConfigMap key names can be used as precedence.

| Situation | Outcome |
| --- | --- |
| Two patches set the same leaf to the same value | Accepted. Setting one value twice is the same as setting it once. |
| Two patches set the same leaf to different values | Rejected, naming both sources and the path. |
| Two entries append `extraArgs` to the same container | Rejected even if the lists are identical. Arguments concatenate, so two identical appends add the argument twice. |
| Two entries add the same container with the same definition | Accepted. |
| Two entries add the same container with different definitions | Rejected. |
| Two documents set different kube-proxy images | Rejected. |
| A document sets `images.kubeProxy` and another passes the flag directly | Rejected. Put the image in one place. |

Additive paths never conflict by value, because concatenation does not overwrite. They can still fail a later check: two tolerations that are individually valid are concatenated, but two node selectors that disagree on the same key are rejected because the key can hold only one value.

### Failure handling

A failed resolution, a contributor conflict, or a merge error withholds the target operation. The operator does not apply the workload un-overridden, because defaults are not the current state. Reverting would rewrite running infrastructure over a user's mistake and would strip resources, tolerations, sidecars, and pinned images from workloads that are already running, including host-networked workloads that use `maxSurge: 0` and cannot tolerate a rolling replacement.

The blast radius of a failure depends on what is knowable. An entry that failed validation names its component, kind, name, and Sites, so only those workloads are withheld. A key that failed to parse names nothing, so every overridable workload is withheld for that pass. This is what keeps a mis-indented line in one key from discarding the entries in every other key.

## Observability

An override that merged successfully is not necessarily an override that reached the cluster. Admission can reject the write, the API server can fail it, or the operation can be skipped because a dependency failed. The operator therefore reports both what it intended and what it wrote, in annotations, Site status, events, and the CLI.

### Annotations on the workload

Every overridden workload carries annotations under the operator's reserved prefix. The hash and source annotations are always written; the drift annotation appears only when an override changed an image.

| Annotation | Meaning |
| --- | --- |
| `unbounded-cloud.io/override-hash` | SHA-256 of the contributor set that shaped this object. |
| `unbounded-cloud.io/override-source` | ConfigMap keys and entry indices that shaped this object. |
| `unbounded-cloud.io/version-drift` | Present only when an override changed a container image, formatted as `<container>=<image>`. |

The hash is computed from the contributors alone, before the merge is attempted. That means a workload whose merge failed still reports a hash rather than looking like a workload with no override. The matching desired hash lives in Site status, not on the object, because writing it would mean touching an object the operator may have just decided not to apply.

The source annotation is a convenience and is bounded at 8 KiB. If a workload has enough contributors to exceed that, the value is truncated with a `+N more` suffix that points at the ConfigMap. The hash annotation is the authoritative record.

### Site status

Each Site carries an `overrides` status block when an override targets any of its workloads. Cluster singletons are reported on every Site, so `kubectl get site` does not hide the commonest case.

| Status field | Meaning |
| --- | --- |
| `phase` | `None`, `Applied`, or `Degraded`. `Applied` means every resolved workload carries the desired hash. |
| `observedResourceVersion` | The ConfigMap version this status was computed from. Empty when no ConfigMap exists. |
| `workloads` | One row per overridden workload, with kind, name, hashes, drift, and state. |
| `message` | Explains a `Degraded` phase and names the ConfigMap key and entry at fault. |

The per-workload `state` field distinguishes four outcomes. `Applied` means the merged workload was written and carries the desired hash. `Pending` means the write was deferred to the next pass because the cluster moved under this one; nothing is wrong. `Withheld` means the operator declined to write the workload because an override that could have shaped it could not be used. `Failed` means the override could not be resolved, merged, or written.

`phase: Applied` reports that the merge was written, not that the resulting pods are healthy. Check the component's own conditions or the workload rollout for health.

### Events on the ConfigMap

The operator records the verdict of the document against the ConfigMap itself, because the ConfigMap is cluster-scoped and a document-level failure does not belong to any one Site. When no Site exists yet, the event is the only verdict you get.

| Reason | Type | Meaning |
| --- | --- | --- |
| `OverridesApplied` | Normal | Workloads were overridden, possibly with inert entries that matched nothing. |
| `OverridesRejected` | Warning | The document is wrong and no workload was written. |
| `OverridesPartiallyRejected` | Warning | Some entries applied and some were rejected. |
| `OverridesPartiallyApplied` | Warning | A per-workload apply failed after a successful merge. |
| `OverridesNotWritten` | Warning | The merge succeeded but the executor did not write the workload. |

Events fire only when the observed `resourceVersion` or the verdict changes, so a broken document that keeps requeueing does not produce an event per pass. Inspect them with:

```bash
kubectl describe configmap unbounded-component-overrides -n unbounded-system
```

### CLI commands

The plugin exposes three subcommands. All of them default to the `unbounded-system` namespace and accept `--namespace` when the operator is installed elsewhere, plus `--kubeconfig` for an explicit kubeconfig.

```bash
kubectl unbounded overrides validate -f component-overrides.yaml
kubectl unbounded overrides list
kubectl unbounded overrides status
```

`validate` is offline syntax and allowlist validation. It accepts either a bare overrides document or a ConfigMap manifest, and `-f` is repeatable. A bare document is keyed by its base name, which is what it would become as a ConfigMap key; `-f -` reads standard input. The command deliberately does not check whether a container or volume named by a patch exists, because that depends on the workload the running operator renders.

`list` shows the entries as authored, in a table with source, component, kind, Site selector, and a summary of changes. It also warns about Site names that match nothing. It is the answer to "what did I ask for", not "what happened".

`status` shows the override state the operator recorded on each Site. It performs no rendering, merging, or hashing of its own, so its answer stays correct when the plugin and the operator are different versions. It exits non-zero when any Site reports `Degraded`, which makes it usable as a CI gate after applying a document.

## Examples

The examples below are complete documents. Each one can be placed under its own key in the ConfigMap. The comments in the shipped `deploy/unbounded-operator/examples/component-overrides.example.yaml` follow the same rules and are worth reading alongside this page.

### Raise a resource limit

Resources are a permitted subtree, and containers merge by name, so you can change one limit without restating the container.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: metalman
    kind: Deployment
    sites: [edge-west, edge-east]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: metalman
                resources:
                  limits:
                    memory: 512Mi
```

Omit `sites` to apply the change to every Site. A `patch` that adds `resources.limits` merges into the operator's existing requests and limits rather than replacing them.

### Change a rollout strategy

Workload-level fields are reachable through the same patch. This example sets a Deployment's rolling update parameters:

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: machina
    kind: Deployment
    patch:
      spec:
        strategy:
          rollingUpdate:
            maxSurge: 1
            maxUnavailable: 0
```

Do not switch to `type: Recreate` while a `rollingUpdate` block is present. Kubernetes rejects a Deployment carrying both, and an override cannot delete the operator's block, so the operator rejects the attempt at resolution time.

### Pin a container image

Changing an image is a permitted leaf. The operator reports the change as version drift, because the component is no longer matched to the operator's own version and the pin survives operator upgrades.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: gantry
    kind: DaemonSet
    name: gantry
    patch:
      spec:
        template:
          spec:
            containers:
              - name: gantry
                image: myregistry.example.com/gantry:v0.9.0
```

Use the `images.kubeProxy` block, not a patch, for the managed kube-proxy DaemonSets. They are created by the net controller at runtime and are not present in the operator's plan.

### Add a node selector

Node selectors merge by key. Adding a key narrows where the workload may run without disturbing the operator's own keys.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: gantry
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              example.com/accelerator: a100
```

If the operator already sets the same key to a different value, the merge fails with an error naming the key. That is intentional: overriding it would change the set of nodes the workload is allowed to run on, which is exactly the constraint the operator relies on for per-Site placement.

### Add tolerations

Tolerations concatenate, so an override adds to the operator's tolerations rather than replacing them.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: metalman
    kind: Deployment
    patch:
      spec:
        template:
          spec:
            tolerations:
              - key: edge
                operator: Exists
```

Required node affinity is different. If you supply required terms, they are ANDed with the operator's terms through a Cartesian product, so both constraints must hold. Preferred affinity and pod affinity are concatenated.

### Add extra arguments

Use `extraArgs` for any argument that should be appended. A patch that sets `args` replaces the whole list, which drops every operator-injected flag and prevents new flags from arriving on upgrade.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: metalman
    kind: Deployment
    extraArgs:
      metalman: ["--operation-max-concurrent-machines=20"]
```

Keep every argument for one container in one entry. Splitting them across entries is a conflict, because concatenation would make the result depend on ConfigMap key order.

### Add environment variables

Environment entries merge by name, so a patch can add a variable or change its value.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: net
    kind: Deployment
    name: unbounded-net-controller
    patch:
      spec:
        template:
          spec:
            containers:
              - name: controller
                env:
                  - name: HTTP_PROXY
                    value: http://proxy.example.com:3128
```

The component is `net`, not the workload name. Use `name` when you want to target the controller Deployment rather than every Deployment the component emits. An override cannot change an environment variable from `value` to `valueFrom`, or the reverse, when the operator defines the other one. Kubernetes permits exactly one source, and an override cannot remove the existing source.

### Add a sidecar

Adding a container requires declaring it in `addContainers` and defining it in the patch.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
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
                resources:
                  limits:
                    memory: 128Mi
```

Without the declaration, the operator would reject the unknown container name instead of creating an image-less container. The reverse mistake is declaring a name that is already present; that is rejected too, because the entry then intends to create something that already exists. Use `addInitContainers` for an init container and add `restartPolicy: Always` if you want it to run as a native sidecar.

### Set the managed kube-proxy image

The kube-proxy image is not reachable by a patch, so it has its own block.

```yaml
apiVersion: overrides.unbounded-cloud.io/v1alpha1
images:
  kubeProxy:
    image: myregistry.example.com/kube-proxy
```

Without a tag or digest, the controller resolves one at runtime from the cluster's `kube-system/kube-proxy` image or from the API server version. Add an explicit tag when you want a fixed version, and use the same registry path that the controller can pull from on every node.

## A safe editing workflow

Overrides change running infrastructure, so treat a document like a production change rather than a convenient patch. The following workflow keeps mistakes local and makes the result verifiable.

1. Write each concern in its own ConfigMap key and validate it offline with `kubectl unbounded overrides validate -f component-overrides.yaml`.
2. Apply the ConfigMap, or apply it with server-side dry run first when your change also touches the wrapper metadata.
3. Confirm the operator observed it with `kubectl unbounded overrides status` and with `kubectl describe configmap unbounded-component-overrides -n unbounded-system`.
4. Check that the workload rolled out as expected, and that the intended containers carry the change.
5. Record why the override exists. An override that outlives its reason becomes the least understood part of an installation.

The second step catches syntax and allowlist errors. The third catches resolution errors, which are the ones that depend on the rendered workload: a misspelled container, a missing volume, a selector label change, or an incompatible strategy. The fourth catches writes that merged but did not reach the cluster.

## Troubleshooting

Start with the phase and the message. `kubectl unbounded overrides status` prints the operator's own verdict per Site, and `kubectl describe configmap` prints the document-level verdict. If neither shows anything, confirm that the ConfigMap exists in the namespace the operator actually uses; a ConfigMap in the wrong namespace is invisible, not broken.

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| No overrides are in effect, and no ConfigMap error | The ConfigMap is in a different namespace, or has the wrong name. | Check the operator namespace with `kubectl get deploy -A` and use `--namespace`. |
| `OverridesRejected` event, every workload untouched | A key failed to parse, so the entries it held were never read. | Run `overrides validate` on the ConfigMap manifest or each key. |
| `OverridesPartiallyRejected` event, some workloads updated | One entry failed validation or resolution and its targets were withheld. | Read the status message; it names the key and entry. |
| Site reports `Degraded` with a container error | The patch targets a container name the workload does not have. | Correct the name, or declare a genuine addition with `addContainers`. |
| Site reports `Degraded` with a volume or mount error | A patch redefines an operator volume or repoints an operator mount. | Add a new volume under a different name instead. |
| Entry is listed but nothing changes | It resolved to no workload, so it is inert. | Check `component`, `kind`, `name`, and `sites`; check that the component is enabled. |
| Warning about Site names that match no Site | The Site does not exist yet, or the name has a typo. | Create the Site, or correct the name. |
| Workload runs but the flag is ignored | The argument was added with a `patch` that replaced `args`, or the component rejects it. | Move it to `extraArgs` and check the component's help output. |
| Workload crash loops after an argument change | The component does not accept the flag, or the value is malformed. | Read the container log, then correct or remove the argument. |
| `OverridesNotWritten` event | The merge succeeded but admission or the API server refused the write. | Read the event and the operator log for the API error. |
| Workload image differs from the operator version | An image override is in effect. | Expected drift; remove the override when the pin is no longer needed. |

### The ConfigMap is valid but nothing applied

An entry that resolves to no workload is inert, not invalid. The three common reasons are a component that is not enabled on any Site, a workload name that does not exist in this release, and a Site selector that names a Site that has not been created. The operator logs each inert source and the CLI warns about unknown Site names. Compare the entry against `kubectl unbounded overrides list`, which shows the parsed component, kind, and Site selector rather than the YAML you think you wrote.

If the component is enabled but the workload is absent, the component may be failing to plan for an unrelated reason. Check the component's condition on the Site before blaming the override.

### A patch is accepted but has no visible effect

The most common cause is a patch that sets a value the operator restores. Selector-matched template labels are restored, typed Site fields are restored, and reserved metadata is restored. Validation and resolution reject most of these, but a patch that sets a value equal to the current one is accepted and naturally has no effect.

The second cause is a strategic merge path that does not match the object's schema. A field spelled correctly but placed under the wrong parent is usually caught as "not an overridable field", which is why the error names the full dotted path. Read the path in the message against the workload's actual structure.

### The merge succeeds but the pods do not start

Overrides do not validate the semantics of the resulting pod. A sidecar with a missing command, an environment variable with a bad value, a volume mount that points at a volume with the wrong content, and an argument the binary does not recognize all merge successfully and fail later. The operator reports the merge and the write, not pod health, so the next step is the workload's own status and the container logs.

For a privileged node agent, a bad override can also make the node unhealthy in ways that are visible only in the agent's own metrics. Roll back by removing the entry or the ConfigMap key, wait for the operator to reconcile, and confirm that the workload returns to its previous generation.

### Rolling back

Deleting an entry removes it from the contributor set, the hash changes, and the operator re-renders the workload without that contribution. Deleting the whole ConfigMap does the same for every override at once. Neither operation is destructive by itself, but both cause a rollout, so plan for the disruption on components that run with tight availability budgets.

A rollback does not restore a pinned image or an added sidecar if another document still contributes it. Search all keys for the same target and for the same container before assuming a single deletion is enough. `kubectl unbounded overrides list` shows every entry and its source key, which makes this check straightforward.

## Security

The security model is deliberately blunt. Anyone who can write this ConfigMap can run arbitrary code as root on every node in every affected Site. The workloads it patches already use host networking, host PID, privileged containers, and host path mounts of the host root, so a changed image, a changed argument, an added sidecar, or a changed environment variable is equivalent to cluster-admin. Rejecting `privileged: true` or new host path volumes would achieve nothing against pods that are already in that state.

The allowlist is an integrity control, not a privilege boundary. It stops an authorized operator from accidentally breaking reconciliation by detaching a workload from its selector, its ServiceAccount, or its per-Site node affinity. Two restrictions are genuine security controls because they escape the workload: the group, version, and kind of the applied object, since the operator holds escalate and bind on ClusterRoleBindings, and `serviceAccountName`, since retargeting it borrows another identity's API permissions.

Treat the ConfigMap like a ClusterRoleBinding. Grant write access to cluster administrators only, audit changes, and prefer narrow entries over broad ones. An entry with no `name` and no `sites` applies to every workload of its component and kind, which is rarely what a targeted customization needs.

## Related

- [Architecture]({{< relref "reference/architecture" >}}) for how the operator plans and reconciles component workloads.
- [CRD Reference]({{< relref "reference/machina-crd" >}}) for the Site fields that remain the supported customization surface, including the typed fields an override cannot replace.
- [CLI Reference]({{< relref "reference/cli" >}}) for the full `kubectl unbounded` command surface.
- [Agent configuration]({{< relref "reference/agent/configuration" >}}) for the flags that `extraArgs` commonly targets.
- [Operations]({{< relref "guides/operations" >}}) for rollout and recovery procedures around the workloads these overrides change.

