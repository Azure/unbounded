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
  nodes: [gpu-1, gpu-2]
  ttl: 30m
```

```sh
kubectl create -f grant.yaml
kubectl get nodegrants        # or: kubectl get ngr
kubectl delete ngr train
```

- **Atomic:** a grant gets all listed nodes or none. If any node is held by another
  grant, create fails with `409 Conflict` and names the busy nodes.
- **TTL:** `spec.ttl` is optional. It defaults to `--max-ttl` (1h) and may not exceed it.
- **Immutable:** grants cannot be updated or renewed. Delete and create again, but
  note that deleting a grant deletes its pods (see below).
- **Status:** the server sets `status.holder` (the user who created the grant) and
  `status.expiresAt`.
- **Access:** plain RBAC. The `view` role can read grants. `edit` and `admin` can
  also create and delete them.

## How it works

- All grants live in one ConfigMap, `rebound-ledger`, in the rebound namespace.
  Each node key maps to `<namespace>/<grant>`. Writes use the ConfigMap
  resourceVersion, so two racing creates cannot both win.
- The `rebound-pod-placement` ValidatingAdmissionPolicy reads the ledger as its
  param. It denies pods and bindings that target a granted node from any other
  namespace. Namespaces in `ExcludedNamespaces` and mirror pods are always allowed.
- The leader removes expired grants and grants whose namespace is gone. It also
  marks granted nodes with:
  - label `rebound.unbounded-cloud.io/namespace=<namespace>`
  - label `rebound.unbounded-cloud.io/grant=<grant>`
  - taint `rebound.unbounded-cloud.io/granted=<namespace>:PreferNoSchedule`
- The leader deletes pods that a grant does not allow:
  - While a grant is active, pods from other namespaces on its nodes. This runs
    when the grant is created and every 15 seconds after.
  - When a grant expires or is deleted, pods from its namespace that were created
    on its nodes while it was active. Pods that were there before the grant stay.

  Pods are deleted with a zero grace period, and PodDisruptionBudgets do not apply.
  The grant's TTL is the grace period. Pods in `ExcludedNamespaces`, mirror pods,
  and finished pods are never deleted. DaemonSet pods from other namespaces are
  deleted, and the DaemonSet will recreate them only if the pod placement policy
  allows it.

  To find these pods after a grant is gone, the ledger keeps a release record
  for 10 minutes.
- The `rebound-node-keys` ValidatingAdmissionPolicy stops other users from
  changing those labels and taint. The rebound service account and `AdminGroups`
  are exempt.
- The leader keeps a self-signed CA and serving certificate in the
  `rebound-serving` Secret, renews them, and updates the APIService `caBundle`.
  All replicas reload the certificate without restart.

## Deploy

```sh
make rebound-manifests
kubectl apply -f deploy/rebound/rendered/
```

Template values (set with `--set` on `render-manifests`):

| Value | Default |
|---|---|
| `Namespace` | `unbounded-system` |
| `Image` | `ghcr.io/azure/rebound:latest` |
| `Replicas` | `2` |
| `MaxTTL` | `1h` |
| `ExcludedNamespaces` | `kube-system,unbounded-system` |
| `AdminGroups` | `system:masters` |

## Limits

- The ledger must fit in one ConfigMap (about 900 KiB).
- Watch history covers the last 100 ledger changes. Older resource versions get `410 Gone`.
- Pod placement enforcement is eventual: the policy reads the ledger through the
  API server's parameter cache. For a short time after a grant is created
  (typically seconds), pods from other namespaces may still be placed on its nodes.
- The reaper enforces expiry shortly after `expiresAt`, so node restrictions may
  linger briefly, and pods are deleted a little later than `expiresAt`.
- Pod `creationTimestamp` has one-second precision and comes from the API server
  clock. A pod created in the same second a grant starts counts as created during
  the grant.
- If the leader is down for more than 10 minutes after a grant ends, its pods are
  not deleted.
