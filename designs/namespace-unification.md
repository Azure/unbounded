# Namespace Unification (`unbounded-system`)

## Summary

Historically, unbounded components installed into several namespaces:

| Component(s) | Old namespace |
| --- | --- |
| machina, metalman, machine-ops, orca, storage-supervisor, inventory | `unbounded-kube` |
| net controller + node | `unbounded-net` |
| gantry | `gantry-system` |
| inventory aggregator Service (stray) | `machina-system` |

All first-party components now install into a single namespace that defaults to
**`unbounded-system`** and is configurable. External dependencies are out of
scope: the inventory PostgreSQL dependency still resolves via
`postgres.inventory.svc.cluster.local`.

Genuine reads of Kubernetes system objects remain in `kube-system`
(`kube-proxy`, `kube-dns`, the bootstrap-token Secret, and the
`extension-apiserver-authentication` ConfigMap).

## Configuration

A single Makefile variable drives every component's namespace:

```make
UNBOUNDED_NAMESPACE ?= unbounded-system
```

Each component var (`MACHINA_NAMESPACE`, `NET_NAMESPACE`, `ORCA_NAMESPACE`,
`GANTRY_NAMESPACE`, `MACHINE_OPS_NAMESPACE`, `INVENTORY_NAMESPACE`,
`UNBOUNDED_STORAGE_SUPERVISOR_NAMESPACE`) derives from it, so overriding
`UNBOUNDED_NAMESPACE` moves everything at once while an individual component can
still be overridden:

```bash
# Everything into a custom namespace.
make machina-manifests net-manifests gantry-manifests UNBOUNDED_NAMESPACE=acme-system

# Just one component elsewhere.
make net-manifests NET_NAMESPACE=acme-net
```

Manifest templates default the namespace via
`{{ default "unbounded-system" .Namespace }}`, so rendering with no `--set
Namespace` still produces `unbounded-system`.

## Migration runbook (existing clusters)

Kubernetes namespaces cannot be renamed in place, and this change is
forward-only: new installs land in `unbounded-system`, and existing clusters
must be migrated by re-deploying. The high-level procedure:

1. **Inventory current state.** Record the components installed and any
   operator-managed secrets/config you will need to recreate:

   ```bash
   kubectl get all,secret,configmap -n unbounded-kube
   kubectl get all,secret,configmap -n unbounded-net
   kubectl get all,secret,configmap -n gantry-system
   ```

2. **Back up stateful data first.** orca/garage object storage and any
   PersistentVolumes are the only data that cannot be regenerated. Snapshot or
   back these up before deleting anything. The inventory PostgreSQL lives in the
   separate `inventory` namespace and is unaffected.

3. **Re-create operator-provided secrets in the new namespace.** These are not
   templated and must be reapplied (names are unchanged):

   - machina SSH key Secrets (`ssh-<site>`).
   - machine-ops credential Secrets referenced by `MachineOperationCredential`.
   - orca credentials, registry pull secrets, etc.

   ```bash
   kubectl create namespace unbounded-system
   kubectl get secret ssh-mysite -n unbounded-kube -o yaml \
     | sed 's/namespace: unbounded-kube/namespace: unbounded-system/' \
     | kubectl apply -f -
   ```

4. **Deploy each component into the new namespace** using the render targets
   (they default to `unbounded-system`):

   ```bash
   make machina-manifests machine-ops-manifests net-manifests gantry-manifests inventory-manifests
   kubectl apply -f deploy/machina/rendered/
   make -C hack/net deploy
   kubectl apply -f deploy/gantry/rendered/
   ```

5. **Validate** rollout and health in the new namespace:

   ```bash
   kubectl -n unbounded-system get pods
   kubectl -n unbounded-system rollout status deploy/machina-controller
   kubectl -n unbounded-system rollout status deploy/unbounded-net-controller
   kubectl -n unbounded-system rollout status ds/gantry
   ```

6. **Decommission the old namespaces** once traffic and reconciliation are
   confirmed healthy in `unbounded-system`:

   ```bash
   kubectl delete namespace unbounded-kube unbounded-net gantry-system
   ```

> Note: leader-election leases follow the deploy namespace (via the Downward API
> `POD_NAMESPACE` for net and metalman). Deleting the old namespaces releases
> the old leases automatically.
