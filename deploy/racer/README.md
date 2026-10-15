# Racer startup

The operator installs both `ClusterCache` and `ClusterVolume` CRDs before starting
its watches. Either kind triggers installation. Only volumes with `spec.type:
Cache` enter the cache catalog; each resource keeps its Kubernetes UID as its
cache identity. Cache names must be unique across both kinds. A duplicate name
or UID rejects the whole catalog update rather than choosing one resource.
Deleting the last resource retains the installation and maintains its serving
TLS; it does not delete workloads or reset durable state.

`make racer-generate` generates both CRDs under `api/racer/v1alpha1/crd` and
`deploy/racer/crd`. `make racer-manifests` includes both in standalone output.
Install the CRDs before starting standalone controllers. Controller and operator
RBAC grants read-only access to both resource kinds.

The operator deploys `racer-controller` after reserving its permanent installation
claim and marker and establishing serving TLS, configuration, admission policy,
and RBAC. No initialization Job or `initialize` CLI command is used. Dataplanes
remain gated on valid, installation-UID-bound version state; normal serving
readiness still requires validated replicated state.

New operator claims also use `initialization_protocol: staged-v1`. The operator
creates a marker in `operator-pending`, which controller startup rejects. It then
freezes the claim with the marker's Kubernetes UID in `marker_uid`, and finally
promotes that exact marker to `fresh` through resource-version CAS. Each boundary
is restartable, including uncertain API responses. Workloads and serving TLS are
not provisioned until the UID-bound marker is promoted. A delayed stale Create can
leave only a non-startable orphan, never a replacement installation. Once the
claim is consumed, a missing or different marker UID fails closed. Legacy operator
claims without this protocol retain one-shot claim-before-Create ordering and
cannot be upgraded to recover ambiguous missing markers. Preserve both objects;
never reset claims or rewrite immutable UID commitments.

Every controller replica runs the same startup guard before recovery and manager
startup. A valid consumed, immutable marker and valid version record require no
writes. Initialization requires
`initialization_protocol: staged-v1` in the marker's data. The
controller first creates an uncommitted, installation-bound version candidate,
then consumes and freezes the marker with that candidate's Kubernetes UID in
`version_uid`. A restart can finish the CAS after a crash or uncertain Create
response. Competing installers converge using Create and resource-version CAS.
The candidate cannot serve until the immutable marker binds its UID.

A consumed marker with missing, replaced, or corrupt version state never authorizes
creation. Markers without `initialization_protocol: staged-v1` do not authorize
initialization. Never add the protocol field to repair an existing installation,
reset a marker, or recreate lost counters under the same cluster identity.
Restore consistent durable state or explicitly rebootstrap with a new cluster UUID.

For standalone manifests, use `InitializationState=fresh` only with a genuinely new
cluster UUID and absent version state. Supply `ClusterID` as a nonzero lowercase
UUID; the renderer checks its form, not whether it has been used before. Create
the fresh marker once, after installing the admission policy and RBAC, and before
starting the controller. Do not use apply to replace an existing marker.

Default rendering omits the installation object entirely, including when
`InitializationState=consumed`. After startup, remove `InitializationState=fresh`
from render inputs and render again. Preserve the actual controller-finalized
immutable marker, including its Kubernetes UID, `version_uid`, protocol field,
and version record. Do not generate a replacement consumed manifest or reapply a
fresh template. Exclude the finalized marker from GitOps pruning and replacement;
omitting it from later renders is not permission to delete it. Constructors do
not initialize state; the running controller owns this guard.

Standalone template inputs `InstallationConfigMapName`, `VersionConfigMapName`,
and `CredentialsSecretName` propagate to runtime configuration, RBAC, and the
write-restriction admission policy. The marker also records the version name.
Use valid Kubernetes resource names; never rename durable state in an existing
installation. Operator-managed installations use fixed names and restore this
wiring when reconciling configuration.

The controller Pod runs as non-root UID 65532 with RuntimeDefault seccomp, all
capabilities dropped, privilege escalation disabled, and a read-only root
filesystem. Each controller requests 100m CPU and 128Mi memory, with limits of
2 CPUs and 1Gi memory. These are deployment defaults, not a capacity guarantee;
size operator workload overrides for the installation's topology and request load.

The Deployment has three replicas and a rolling update budget of zero unavailable
and one surge. The standalone `controller-pdb.yaml` requires two available replicas
during voluntary evictions. The operator also manages this object. Preferred
anti-affinity spreads replicas across hosts when possible without blocking small
clusters. This does not guarantee separate hosts or protect against node failure;
check placement and capacity before maintenance.

Install `create-restriction.yaml` and `node-restriction.yaml`, including both
bindings, and verify that the policies are active with no type-check errors
**before** applying RBAC or any controller workload. RBAC cannot restrict
Secret and ConfigMap creation by name or limit Node patches to specific fields.
These fail-closed deny policies supply those restrictions.

## Network access

`RACER_HANDSHAKE_TIMEOUT` bounds controller TLS handshakes and defaults to `5s`.
Set the `HandshakeTimeout` template input to override it. Keep a positive duration;
this deadline is separate from request admission and long-poll limits.

Limit controller HTTPS ingress (TCP 8443 by default) to the clients and dataplanes
that use the control API and controller replicas that replicate state. Replicas
need direct Pod-to-Pod HTTPS access as well as Service access. Allow kubelet probes
on TCP 8081 and authorized monitoring on TCP 8080. Preserve DNS and Kubernetes API
access for identity checks and reconciliation. Dataplane peers and clients also
need access to their configured peer listener (TCP 8082 in managed workloads).

No universal NetworkPolicy is shipped: client locations, cross-cluster routes,
and CNI enforcement differ. Use namespace/Pod selectors or source ranges that fit
the installation, and test bootstrap, replication, renewal, and data transfer.
Host-network traffic may bypass Pod policy or appear under node addresses. For
host networking or external clients, enforce the required limits with the CNI,
host firewall, or upstream network controls; do not assume a Pod selector alone
protects those paths.

The controller logs its version, Git commit, and build time before loading runtime
configuration. `make racer-controller-build` stamps `VERSION`, `GIT_COMMIT`, and
`BUILD_TIME`; the controller image accepts the same build arguments. Unstamped
development builds report `dev` and `unknown` metadata.

## Atomic credentials

The controller owns one `racer-credentials` Secret containing `issuer.json`
(private issuer keys and certificates), `bundle.json` (public roots and cache
keys), and `rotation.json` (rotation deadlines and issuer roles). Configure its
name in standalone templates with `CredentialsSecretName`, which also sets
`RACER_CREDENTIALS_SECRET_NAME` and the matching RBAC and admission policy.
Dataplanes never receive or mount this Secret.
They receive only the validated public-root/cache-key bundle through the control
API. Every rotation publishes all three entries in one resource-version CAS.
Only an authoritative postwrite reread can install serving trust.

Secret get/list/watch/update/patch permissions name only the credentials Secret.
The controller's Secret informer includes the matching `metadata.name` field
selector required by RBAC for list/watch. It cannot read unrelated Secrets or the
serving Secret through the API, including its `ca.key`; kubelet projects only the
serving certificate, leaf key, and public CA bundle into the Pod. Secret creation
is necessarily namespace-wide in RBAC, so the fail-closed admission policy must
be installed before the RoleBinding and controller workload.

`make racer-admission-envtest KUBEBUILDER_ASSETS=/path/to/envtest/assets` runs the
real API-server operator component regressions for admission, RBAC, identity
recovery, dataplane apply, and update strategy with a five-minute command bound.
CI provisions assets and includes this target in `make racer-envtest-ci`.

The permanent credentials annotation on the version ConfigMap records
`secretName/initialRootFingerprint`. For staged installations the controller first
creates the complete, installation-bound credentials Secret, then commits the
claim and `racer.unbounded-cloud.io/credentials-uid` together in one version CAS.
Unclaimed staged material cannot be used by credential readers. Restart adopts
only a complete generation-one candidate with matching installation and issuer
identity. No private material is written to ConfigMaps or a second Secret.
Deletion before commit can discard only never-authorized candidates; deletion or
replacement after commit never authorizes regeneration. A delayed stale Create
can at most leave an unusable orphan with a different UID, never restore authority.
Initialization requires the staged protocol; missing protocol fields and
ambiguous legacy gaps fail closed. Claimed missing, corrupt, or mismatched
credentials never authorize regeneration. Preserve the claim and
consistent durable state. The persisted format is intentionally breaking; there
is no compatibility reader or migration from the former split Secrets.

The Node policy scopes updates to `racer-controller` in the configured namespace.
It permits adding, changing, or removing only these annotations:

- `racer.unbounded-cloud.io/enrolled-shares`
- `racer.unbounded-cloud.io/enrolled-rdma-nics`
- `racer.unbounded-cloud.io/last-admitted-member`

All other Node fields must stay unchanged, except API-server field ownership
bookkeeping (`metadata.managedFields`). Node creation and deletion are not granted
by RBAC. The policy does not restrict other accounts, including node agents.
Install its binding before the ClusterRoleBinding and retain both policies while
the controller has write access.

Bundle generation is a publication version, not a rotation count. Catalog
admission, preparation, activation, and root retirement each consume one version
only when state changes. RKG1 key IDs bind their creation publication, which never
exceeds the containing bundle generation. Unchanged reconciliation does not write,
and exhausted generations cannot wrap or publish changes.

Preparation publishes exact-ID active and prepared cache keys. Activation removes
replaced symmetric keys immediately; already-held dataplane leases may finish.
Only issuer root fingerprints have retirement deadlines. Roots overlap for the
retention period, and expired roots and their private keys are removed atomically.
Interval, preparation, retention, and leaf-lifetime policy settings remain in
effect. Catalog admission reserves two symmetric key generations plus all
overlapping roots, retains admitted UIDs first, and never evicts existing caches
to admit new ones.

## Native RDMA deployment

Release and nightly dataplane images include native RDMA by default
(`RACER_NATIVE_RDMA=true`). The image builds the private verbs adapter against
`libibverbs-dev` and includes `libibverbs1` and `ibverbs-providers` at runtime.
`RACER_ENABLE_RDMA=auto` discovers eligible hardware; missing or ineligible
hardware leaves HTTP available. Building a development image with
`RACER_NATIVE_RDMA=false` omits the native transport, not the workload's privileges.

The operator's dataplane runs **unconditionally privileged as root**, with
`allowPrivilegeEscalation: true`, and mounts host `/dev/infiniband` at the same
path. This is hostPath access, not device-plugin or CDI allocation: there is no
per-Pod device reservation or isolation. Admission policy and the container
runtime must allow privileged Pods and hostPath volumes. Treat dataplane image
and workload configuration writers as trusted host administrators. Privileged
mode grants capabilities and bypasses normal container security restrictions;
neither a capability-drop list nor `allowPrivilegeEscalation: false` would be a
meaningful privilege boundary here.

The root filesystem stays read-only. The RDMA mount is also read-only to protect
directory entries against ordinary writes, **not** to prohibit character-device
I/O or confine a privileged process. Identity, slab, and socket hostPaths remain
writable. The RDMA volume uses `DirectoryOrCreate`: on a node without
`/dev/infiniband`, kubelet creates an empty directory with mode 0755 and kubelet's
ownership. This intentionally mutates the host's `/dev` but creates no hardware
or device nodes, and avoids a missing-directory mount failure on HTTP-only nodes.
The directory must be creatable by kubelet; a read-only host `/dev` or an existing
non-directory path still fails mounting. Host drivers/udev must populate real
`uverbs*` devices. Driver loading and device provisioning are not done by Racer.

### Namespace, sysfs, and RoCE requirements

Host networking is **not** enabled implicitly by RDMA access. Existing workload
networking configuration remains authoritative. Check from the dataplane's
actual namespaces, not just a host shell:

- Verbs must enumerate and open the intended device/port through `/dev/infiniband`.
  Host kernel drivers, firmware, and image userspace providers must be compatible.
  The port must be active and advertise type-2B memory windows (MW2B); allocation,
  registration, binding, and connection setup can still fail after discovery.
- The container must see the relevant `/sys/class/infiniband` and
  `/sys/class/infiniband_verbs` entries and their `/sys/devices` targets. Racer
  reads device topology through sysfs for PCI ordering and NUMA locality. No host
  `/sys` mount is added: exposing host sysfs alone would not assign an RDMA device
  or its Ethernet interface to the Pod's network namespace.
- Linux RDMA shared namespace mode can expose devices in pod network namespaces;
  exclusive mode restricts access to the namespace owning the device. See
  [`rdma_dev_access_netns` and `add_one_compat_dev` in Linux device.c](https://github.com/torvalds/linux/blob/v6.12/drivers/infiniband/core/device.c).
  A privileged Pod and device bind mount do not override this namespace contract.
  If your device/driver requires the host namespace, explicitly configure host
  networking and review peer/diagnostics port conflicts and network exposure.
- RoCE additionally needs the correct Ethernet netdevice, address, GID type,
  routing, and fabric configuration. RDMA device visibility does not establish
  any of these. libibverbs can query GIDs through kernel ioctls or sysfs fallback;
  the latter reads `ports/<port>/gids/<index>`, and extended GID queries can resolve
  `gid_attrs/ndevs/<index>` via namespace-local `if_nametoindex`. See
  [rdma-core GID query implementation](https://github.com/linux-rdma/rdma-core/blob/v44.0/libibverbs/cmd_device.c).
  Racer's native adapter currently queries GID index **0** and uses source GID
  index **0** for QP setup. The optional NIC `gid` is a matching constraint, not
  a GID-index selector; it does not configure a RoCE address or select RoCE v2.
  Deployments needing a different index must retain HTTP until supported.
- Ensure pinned-memory and RDMA resource budgets fit the node and runtime's
  limits. Privilege does not guarantee successful memory registration or MW2B
  support. Validate transfers, fallback, and recovery on your hardware before
  relying on RDMA. Source review and CPU-only tests are not hardware validation.

### NIC policy and coordinated upgrade

The Node annotation `racer.unbounded-cloud.io/rdma-nics` is a JSON array:

```json
[{"device":"mlx5_0","port":1,"rail":0},{"device":"mlx5_1","port":1,"rail":0,"gid":"00000000000000000000ffffc0000201","numa_node":1}]
```

Each entry requires a verbs device name, `port` in 1..255, and `rail` in
0..65535. Optional `gid` is exactly 32 lowercase hexadecimal digits; optional
`numa_node` is an unsigned 32-bit integer. At most 64 entries are accepted.
Device/port pairs must be unique; repeated rails are allowed. Canonical ordering
is rail, device, port. NUMA locality is autodetected when no override is supplied;
unknown locality is eligible for any worker. Workers prefer same-rail NUMA-local
candidates, then unknown locality, then remote locality, and spread choices
within that preference tier by worker ID.

An **absent** admin annotation uses the discovery report saved by the controller
in `racer.unbounded-cloud.io/enrolled-rdma-nics`. Leave this controller-managed
annotation alone: the dataplane submits its report through authenticated bootstrap
bound to its live Pod/Node identity, and the controller publishes the resulting
NIC details in member `rdma_nics`. Discovery initially considers active MW2B-capable
ports, orders them by PCI BDF then port, and assigns ordinal rails. The private
identity directory's `rdma-rails.json` journal preserves physical device/port
assignments across renewal and restart: missing ports retain their rail reservation
and new ports append instead of renumbering survivors. Do not delete that journal
independently of a coordinated topology change. An explicit admin
`[]` overrides the report and disables RDMA NIC eligibility on that node; removing
the annotation restores the report. An explicit nonempty list overrides automatic
rail assignment. Use it when PCI order differs from your intended rail topology.

Discovery refreshes at enrollment and certificate renewal. With
`RACER_ENABLE_RDMA=auto`, no usable startup ports means HTTP-only resource sizing
until restart. Use `RACER_ENABLE_RDMA=true` to reserve native capacity before
hardware becomes available. Missing libraries or discovery failures still fall
back to HTTP. Unknown NUMA is usable, but discovery cannot infer cabling: asymmetric
initial inventories, changed device names, or differing physical rail layouts
require explicit mappings. See the [inventory lifecycle details](../../cmd/racer-dataplane/RDMA.md).

Remote eligibility always requires the same **nonempty Site and rail**; there is
no per-rail fabric string or alignment toggle. Site comes only from the canonical
Node label `unbounded-cloud.io/site`. Local device names and NUMA IDs need not
match remote ones. Uneven NIC/rail counts or an unavailable rail keep HTTP for
that transfer; Racer does not remap the page hash or route to another rail.

This is a coordinated hard break, not a rolling mixed-version protocol:

- Upgrade controller and dataplane binaries together, and review workload security
  admission before rollout. Quiesce/drain affected traffic using your operational
  procedure; do not assume the DaemonSet's rolling update makes mixed protocols safe.
- Members and bootstrap requests now require `rdma_nics`; members also require
  explicit `site` (empty means HTTP-only). Wire schema version remains 1. Old
  `rails`/`alignment_enabled` payloads are not accepted by the network codecs.
- Old `racer.unbounded-cloud.io/rails` and `racer.unbounded-cloud.io/aligned-rails`
  annotations are ignored with diagnostics, never translated into NICs. Remove
  them and replace intended policy explicitly. Invalid new annotations retain
  last accepted values for admitted nodes; Site label changes still take effect.
- Remove legacy `RACER_RAILS`, `RACER_ALIGNED_RAILS`, `RACER_FABRIC_PORTS`, and
  `RACER_FABRIC_PORTS_FILE` environment settings, including empty values: these
  obsolete settings fail startup rather than silently changing policy. Move NIC
  policy to the Node annotation instead.
