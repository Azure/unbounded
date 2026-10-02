# Racer startup

The operator deploys `racer-controller` after reserving its permanent installation
claim and marker and establishing serving TLS, configuration, admission policy,
and RBAC. No initialization Job or `initialize` CLI command is used. Dataplanes
remain gated on valid, installation-UID-bound version state; normal serving
readiness still requires validated replicated state.

Every controller replica runs the same startup guard before recovery and manager
startup. A valid consumed, immutable marker and valid version record require no
writes. For a fresh marker and absent version, one resource-version CAS winner
consumes and freezes the marker, then makes exactly one version Create attempt.
Concurrent losers only reread, waiting up to five seconds for the winner's gap.

A consumed marker with missing or corrupt version state never authorizes creation.
A crash or lost response after marker consumption can therefore leave an unusable
installation. This ambiguity is intentional: never reset or recreate a marker to
retry, and never recreate lost version counters under the same cluster identity.
Restore consistent durable state or explicitly rebootstrap with a new cluster UUID.

For standalone manifests, use `InitializationState=fresh` only with a genuinely new
cluster UUID and absent version state. After startup, retain `consumed` in
declarative configuration and preserve the marker and version record. Constructors
do not initialize state; the running controller owns this guard.

## Atomic credentials

The controller owns one `racer-credentials` Secret containing `issuer.json`
(private issuer keys and certificates), `bundle.json` (public roots and cache
keys), and `rotation.json` (rotation deadlines and issuer roles). Configure its
name with `RACER_CREDENTIALS_SECRET_NAME`; custom names also require matching RBAC
and create-restriction policy. Dataplanes never receive or mount this Secret.
They receive only the validated public-root/cache-key bundle through the control
API. Every rotation publishes all three entries in one resource-version CAS.
Only an authoritative postwrite reread can install serving trust.

The permanent credentials annotation on the version ConfigMap records
`secretName/initialRootFingerprint`. Its CAS authorizes exactly one Secret Create
attempt. Claimed missing, corrupt, or mismatched credentials never authorize
regeneration, even after an ambiguous Create response. Preserve the claim and
consistent durable state. The persisted format is intentionally breaking; there
is no compatibility reader or migration from the former split Secrets.

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
