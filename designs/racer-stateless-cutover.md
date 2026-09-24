# Racer stateless dev/test cutover

> Historical pre-1.0 procedure. Do not use this migration procedure for Racer
> 1.0. The inventory script has been removed; 1.0 requires fresh CA state and
> slabs. See `cmd/racer-dataplane/CONTRACT.md` for the current format contract.

This is a **coordinated fresh trust-domain cutover**, not a rolling upgrade or a
backward-compatibility migration. Deploy matching CP and DP binaries together.
Placement changes to Node-UID HRW with 262,144 slots per universe; certificates
carry self-contained signed process claims; CA state changes to version 5.

The CA loader rejects older versions and unknown fields
(`cmd/racer-controlplane/src/security/state.rs:25`, `:92`). There is no automatic
old-format CA migration. If preserving the old CA is required, stop here and
implement a reviewed migration instead of resetting it. Exact old-process
renewal after live eligibility/selection loss is not preserved.

## 1. Stop writers and preserve the restart inputs

Choose one explicit Kubernetes context and state namespace. Suspend the
Unbounded operator and any GitOps reconciler that would restore Racer workloads.
Stop **all** old CP and DP processes, including standbys and manually launched
test processes. For a managed dev/test installation, scale the CP Deployment to
zero and remove the singleton DP DaemonSet with foreground deletion after saving
its desired configuration. Wait for graceful Pod termination and verify that no
process still has the slab open. Do not force-delete Pods or rely solely on a
missing Pod API object as proof that its process stopped.

Disabling Racer on Sites is insufficient: the operator retains and repairs an
existing installation (`internal/operator/components/racer/racer.go:65`). Keep
the operator suspended throughout cleanup/reset, and reinstall only the new
version. Preserve Sites, Nodes, P2PCaches (including their UIDs/generations),
storage annotations, and workload overrides. Save old CA/runtime state only to
an explicitly chosen protected backup location outside source control if rollback
is needed. Do not print private CA data into a shared run log.

Retain `/var/lib/racer/cache-v5.slab` and its compatible storage files. The
managed profile mounts `/var/lib/racer` at `/cache`; its bootstrap does not
reformat or wipe it (`internal/operator/components/racer/resources.go:236`,
`:281`). Do not remove the cache directory, PVCs/PVs, or Node data as part of
runtime cleanup. Keep a single slab writer per node.

## 2. Inventory and remove only obsolete runtime ConfigMaps

The following helper is **read-only**. It takes an explicit context/namespace,
prints `kind/name`, UID, and resourceVersion, and performs no deletion:

```bash
CONTEXT=your-dev-context
STATE_NAMESPACE=unbounded-system
bash hack/scripts/racer-obsolete-runtime.sh "$CONTEXT" "$STATE_NAMESPACE"
```

Its allowlist requires both the exact old name shape and the old ownership marker:

| Obsolete objects | Required marker |
| --- | --- |
| `racer-v4-topology-<64 hex>`, `racer-v4-storage-<64 hex>` | `racer.unbounded-cloud.io/rust-state=pointer` |
| `racer-v4-chunk-<64 hex>` | `racer.unbounded-cloud.io/rust-state=chunk` |
| `racer-v4-store-gate` | `racer.unbounded-cloud.io/rust-state=gate` |
| `racer-pki-<16 hex>-<64 hex>` | `racer.unbounded-cloud.io/pki-participants=v4` |
| `racer-replica-<Pod UID>` | Controller ownerReference to that exact v1 Pod UID |

Review the inventory. With writers still stopped, delete each reviewed object
by its exact name and the observed UID/resourceVersion. For example, substitute
one inventory row into this explicit API deletion, which rejects a changed or
recreated object instead of deleting it by name alone:

```bash
NAME=racer-v4-store-gate
UID_FROM_INVENTORY=replace-with-observed-uid
RV_FROM_INVENTORY=replace-with-observed-resource-version
jq -n --arg uid "$UID_FROM_INVENTORY" --arg rv "$RV_FROM_INVENTORY" \
  '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Orphan",
    preconditions:{uid:$uid,resourceVersion:$rv}}' |
  kubectl --context="$CONTEXT" delete \
    --raw="/api/v1/namespaces/$STATE_NAMESPACE/configmaps/$NAME" -f -
```

Do not pipe the entire inventory into deletion, use wildcard/prefix deletion, or
delete all ConfigMaps in the namespace. A conflict requires reinspection, not an
unconditional retry. The helper deliberately excludes trust, revision checkpoint,
Secrets, Leases, workload ConfigMaps, CRs, and cache data.

Older Go-era `racer-desired-*` objects or objects labeled
`racer.unbounded-cloud.io/state=commit` may also prevent fresh CA bootstrap
(`cmd/racer-controlplane/src/security/kubernetes.rs:81`). They are **not** covered
by the helper's old-Rust allowlist. Inspect their exact names, owner references,
and payload schema against the version that created them; remove only individually
confirmed obsolete runtime objects using the same UID/RV-precondition workflow.
Do not broaden the helper to arbitrary `racer-*` objects.

## 3. Explicit old-format CA reset, only if chosen

Obsolete runtime cleanup does not make an old CA compatible. Reset is a separate
destructive choice that invalidates every old leaf and discards the old trust
domain. Confirm all old CP/DP processes are stopped, and that any desired backup
is secured before proceeding.

For a deliberately fresh dev/test trust domain, individually review and explicitly
remove **only** these exact state-namespace objects using UID/RV preconditions as
above (Secrets use `/api/v1/namespaces/.../secrets/...`; the Lease uses
`/apis/coordination.k8s.io/v1/namespaces/.../leases/...`):

- `Secret/racer-ca`: explicit private CA format reset.
- `ConfigMap/racer-trust`: explicit old public trust reset.
- `ConfigMap/racer-runtime-revisions`, **if present from a prior stateless run**:
  explicit revision-domain reset, never routine cleanup.
- `Lease/racer-controlplane`: explicit stopped-election reset.

No script performs these deletions. Never delete only the CA Secret and expect
regeneration over remaining trust/runtime artifacts. Never recreate or zero the
checkpoint in an established domain. Fresh bootstrap is deliberately blocked by
prior artifacts; checkpoint loss on restart also fails closed
(`cmd/racer-controlplane/src/security/kubernetes.rs:190`, `:247`,
`cmd/racer-controlplane/src/revision.rs:180`). Recreate CP Pods rather than reuse
their old CSR/response annotations. Replace DP Pods so keys, leaves, and received
cursors start fresh; the host cache slab remains intact.

## 4. Start and verify the matching version

Install the new operator/configuration and matching CP/DP images. Resume
reconciliation. Confirm one fresh checkpoint, one trust ConfigMap, one CA Secret
with at most two roots, and one leader Lease. CP Pods exchange only bounded
current CSR/response annotations; no participant/replica/runtime-history
ConfigMaps should appear. The operator grants namespaced Pod patching but no
ConfigMap deletion (`internal/operator/components/racer/resources.go:82`).

Verify live enrollment, independent `/v4/config` convergence, leader failover,
storage capacity and compatible slab reuse, and reads through the cache. Exercise
rotation with the intended `--ca-overlap-delay`; overlap publication plus delay
and skew precedes issuer switch, and maximum issued expiry plus skew precedes
old-root retirement. Proof acknowledgments and offline members do not gate it.
Record actual validation results separately; the historical pre-cutover campaign
is not evidence for this deployment.
