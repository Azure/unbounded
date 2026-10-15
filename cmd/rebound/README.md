# rebound

rebound lets a namespace take a short, exclusive grant on a set of nodes.
While a grant is active, only pods from that namespace can be placed on those nodes.

It runs as an aggregated API server. It adds no CRDs, webhooks, or mutating policies.

## Usage

```yaml
apiVersion: rebound.unbounded-cloud.io/v1alpha1
kind: NodeGrant
metadata:
  name: train
  namespace: team-a
spec:
  nodeSelector:
    matchLabels:
      pool: gpu
  ttl: 30m
```

```sh
kubectl create -f grant.yaml
kubectl get nodegrants        # or: kubectl get ngr
kubectl delete ngr train
```

- **Atomic:** a grant gets all matching nodes or none. If any node is held by another
  grant, create fails with `409 Conflict` and names the busy nodes.
- **TTL:** `spec.ttl` is optional. It defaults to `--max-ttl` (1h) and may not exceed it.
- **Nodes:** `spec.nodeSelector` is required and must not be empty. It must match
  at least one node and at most `--max-nodes-per-grant` nodes (500 by default).
  The selector is resolved once at create time into `status.nodes`. Later label
  changes do not change the grant.
- **Immutable:** grants cannot be changed or renewed. Update and patch succeed
  only when they change nothing, so `kubectl apply` of an unchanged manifest
  works; any change fails with `422 Invalid`. Server-side apply is not
  supported. Delete and create again, but note
  that deleting a grant deletes its pods (see below).
- **Metadata:** labels and annotations are stored. Create rejects `generateName`,
  owner references, finalizers, deletion timestamps, and deletion grace periods.
- **Status:** the server sets `status.nodes` (sorted node names), `status.holder`
  (the user who created the grant), and `status.expiresAt`.
- **Access:** plain RBAC. The `view`, `edit`, and `admin` roles can read grants.
  Creating and deleting grants needs the `rebound-nodegrants-edit` ClusterRole,
  which is not added to any built-in role. A grant can take any node and deletes
  other namespaces' pods on it, so bind the role only where it is needed:

  ```sh
  kubectl create rolebinding nodegrants --namespace team-a \
    --clusterrole rebound-nodegrants-edit --user alice
  ```

## How it works

- All grants live in one ConfigMap, `rebound-ledger`, in the rebound namespace.
  Each node key maps to `<namespace>/<grant>`. Writes use the ConfigMap
  resourceVersion, so two racing creates cannot both win.
- The `rebound-pod-placement` ValidatingAdmissionPolicy reads the ledger as its
  param. It denies pods and bindings that target a granted node from any other
  namespace. Namespaces in `ExcludedNamespaces` and mirror pods created by the
  node's own kubelet are always allowed.
- The leader removes expired grants and grants whose namespace is gone. It also
  marks granted nodes with:
  - label `rebound.unbounded-cloud.io/namespace=<namespace>`
  - label `rebound.unbounded-cloud.io/grant=<grant>`
  - taint `rebound.unbounded-cloud.io/granted=<namespace>:PreferNoSchedule`
- The leader deletes pods that a grant does not allow:
  - While a grant is active, pods from other namespaces on its nodes. This runs
    when the grant is created and every 15 seconds after.
  - When a grant expires or is deleted, pods from its namespace that were created
    or scheduled onto its nodes while it was active. Pods that were there before
    the grant stay.

  Pods are deleted with a zero grace period, and PodDisruptionBudgets do not apply.
  The grant's TTL is the grace period. Pods in `ExcludedNamespaces`, mirror pods,
  and finished pods are never deleted. DaemonSet pods from other namespaces are
  deleted, and the DaemonSet will recreate them only if the pod placement policy
  allows it.

  To find these pods after a grant is gone, the ledger keeps a release record
  for 10 minutes. The leader checks those nodes every 15 seconds until the
  record is dropped, so it also finds pods that were still being created.
- The `rebound-node-keys` ValidatingAdmissionPolicy stops other users from
  changing those labels and taint. The rebound service account and `AdminGroups`
  are exempt.
- The leader keeps a self-signed CA and serving certificate in the
  `rebound-serving` Secret, renews them, and updates the APIService `caBundle`.
  All replicas reload the certificate without restart.

## Build

```sh
make rebound              # lint, test, and build bin/rebound
make rebound-build        # build only
make rebound-oci-push     # build and push the image
```

## Deploy

```sh
make rebound-manifests
kubectl apply -f deploy/rebound/rendered/
```

Each release also ships these manifests, rendered for that release's image,
in the `rebound/` directory of the `unbounded-manifests-<version>.tar.gz`
archive.

Set these Make variables to change the rendered manifests:

| Make variable | Template value | Default |
|---|---|---|
| `UNBOUNDED_NAMESPACE` | `Namespace` | `unbounded-system` |
| `REBOUND_IMAGE` | `Image` | `ghcr.io/azure/rebound:<version>` |
| `REBOUND_REPLICAS` | `Replicas` | `2` |
| `REBOUND_MAX_TTL` | `MaxTTL` | `1h` |
| `REBOUND_MAX_NODES_PER_GRANT` | `MaxNodesPerGrant` | `500` |
| `REBOUND_EXCLUDED_NAMESPACES` | `ExcludedNamespaces` | `kube-system,unbounded-system` |
| `REBOUND_ADMIN_GROUPS` | `AdminGroups` | `system:masters` |

`ExcludedNamespaces` is used by both the pod placement policy and the pod
deletion controller. The rebound namespace is always exempt, even if it is not
in `ExcludedNamespaces`. `AdminGroups` may change rebound node labels and taints.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--namespace` | `unbounded-system` | Namespace of the ledger, serving Secret, and leader lease |
| `--max-ttl` | `1h` | Default and maximum grant TTL |
| `--max-nodes-per-grant` | `500` | Maximum nodes a grant's selector may match |
| `--excluded-namespaces` | `kube-system,unbounded-system` | Namespaces whose pods are never deleted |
| `--leader-elect` | `true` | Run controllers and certificate rotation on one replica |
| `--secure-port` | `8443` | HTTPS port for the API |
| `--metrics-bind-address` | `:8080` | Metrics address |
| `--health-probe-bind-address` | `:8081` | Controller health probe address |

The standard Kubernetes API server flags for delegated authentication and
authorization are also accepted. Run `rebound --help` for the full list.

## Limits

- A selector must match 1 to `--max-nodes-per-grant` nodes. Empty selectors are rejected.
- The ledger must fit in one ConfigMap. Writes that add a grant, including
  ones that also remove an expired grant, are limited to 900 KiB of data; cleanup writes may use up to 1000 KiB.
- Watch history covers the last 100 ledger changes. Older resource versions get `410 Gone`.
- Pod placement enforcement is eventual: the policy reads the ledger through the
  API server's parameter cache. For a short time after a grant is created
  (typically seconds), pods from other namespaces may still be placed on its nodes.
- The reaper enforces expiry shortly after `expiresAt`, so node restrictions may
  linger briefly, and pods are deleted a little later than `expiresAt`. Until then,
  `get`, `list`, and `watch` still return the grant, with `expiresAt` in the past.
- Pod `creationTimestamp` has one-second precision and comes from the API server
  clock. The scheduled time has the same precision. A pod created or scheduled in
  the same second a grant starts counts as during the grant. Pods created in the
  same second the grant ends are kept.
- Release records are kept for 10 minutes from when they are written, even if the
  leader removes an expired grant late. Pods created after expiry are not deleted
  by that release. If the leader cannot process a release before its record is
  dropped, pod deletions can be missed. A record is also kept while its removal
  is unconfirmed and until its handoff cooldown ends.
- A node cannot be granted again for 10 seconds after its grant expires, or after
  the removal of a deleted grant is confirmed. The API server confirms a removal
  with a second ledger write once the removal has been stored; if that write is
  lost, the next create on the node or the next reaper sweep confirms it. Creating
  a grant on the node before then fails with `409 Conflict` and a
  `retryAfterSeconds` hint. The leader checks the ledger right before each pod
  delete and gives up on any delete it cannot finish within 6 seconds of that
  check. A check that still saw the old grant ran before the removal was stored,
  so before it was confirmed, however slow the write was. Deletes allowed by the
  old grant are therefore done before the next grant starts. This assumes rebound
  replica clocks agree to within a few seconds.
