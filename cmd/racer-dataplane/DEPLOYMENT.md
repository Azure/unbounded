# Racer dataplane build and deployment

Run build commands from the repository root. This packages the Rust dataplane;
the existing `racer-controller-build`, `racer-test`, `racer-generate`, and
`racer-manifests` targets remain available. The Go controller in this tree is a
scaffold: its bootstrap authentication/enrollment methods return `pending`
(`internal/racer/bootstrap.go:25-33`). Rendering those manifests alone does not
provide an operational enrollment/control service. Supply a compatible service
implementing [CONTROL_API.md](CONTROL_API.md) before starting the dataplane.

## Release artifacts

Use Linux, Rust/Cargo 1.96.0 (the image toolchain), and a C toolchain for the Rust
`ring` dependency. No verbs development package is needed for a Rust-only build.

```sh
make racer-dataplane-build
# bin/racer-dataplane; Cargo intermediates in bin/racer-cargo/

# Enable the dynamically loaded adapter interface in the Rust binary:
make racer-dataplane-build RACER_NATIVE_RDMA=true
# Separately compile the actual libibverbs adapter:
make racer-dataplane-native-build
# Stage an installation without writing system directories:
make racer-dataplane-native-install DESTDIR="$PWD/bin/racer-stage"
```

All Cargo build/check/test recipes in the Racer targets use `--locked`. The
release target selects only the binary and disables default features explicitly;
`RACER_NATIVE_RDMA=true` adds `--features rdma`. It does not run serving tests or
compile/install the native library implicitly. `RACER_CARGO`,
`RACER_CARGO_TARGET_DIR`, and `RACER_DATAPLANE_BIN` can override tool/output paths.
These are host-native builds; do not set `CARGO_BUILD_TARGET` when using the
release recipe's host output layout.

The native targets require `cc` and `pkg-config` plus actual libibverbs headers
and libraries (Debian/Ubuntu: `libibverbs-dev`). Output is
`bin/libracer_rdma.so.1`. Install uses `DESTDIR` plus `RACER_LIBDIR`, defaulting to
`/usr/local/lib` under `RACER_PREFIX=/usr/local`. A system installation also needs
the runtime `libibverbs1` and matching provider libraries (`ibverbs-providers` on
Debian), plus an administrator-managed loader search path/cache. Run `ldconfig`
after a system installation, or set `LD_LIBRARY_PATH` to a trusted installation
directory. Staging does not run `ldconfig`. The library filename stays
`libracer_rdma.so.1` while its private ABI is version 2, checked by the loader
(`src/rdma/backend.rs:91-123`). Rebuild the adapter with its paired Rust release.

```sh
make image-racer-dataplane-local CONTAINER_ENGINE=docker VERSION=dev
make image-racer-dataplane-local CONTAINER_ENGINE=docker VERSION=dev-rdma RACER_NATIVE_RDMA=true
```

See [image assets](../../images/racer-dataplane/README.md) for base-image overrides
and reproducibility limits. The optional image compiles against real libibverbs,
ships the adapter and provider packages, and enables the Rust loader feature.
Neither image target pushes. Building can download dependencies/base images;
use dry runs when downloads are not permitted. No cluster credentials are build
arguments. The runtime entry point uses environment configuration, with no CLI
subcommand or `--help`/`--version` handler (`src/main.rs:5-17`).

## Kernel, filesystem, and process permissions

- Deploy on Linux with working `io_uring_setup`, `io_uring_enter`, and
  `io_uring_register` access. The reactor creates a ring and uses socket, timer,
  cancellation, and file operations, including `openat2`, `statx`, mkdir, rename,
  unlink, and fsync (`src/runtime/reactor.rs`, `src/runtime/filesystem.rs`). A
  modern kernel such as Linux 6.1 or newer is a deployment baseline, not a
  substitute for checking the actual kernel/filesystem combination. Container
  seccomp, LSM, and host `kernel.io_uring_disabled` policy must permit these
  operations. Use a reviewed runtime seccomp profile allowing the required calls;
  there is no buffered/synchronous fallback for a denied reactor.
- Mount a persistent local ext4 filesystem at `/var/lib/racer/slabs`. Ext4 on a
  kernel reporting direct-I/O alignment is the recommended deployment choice.
  The code checks capabilities, not the filesystem name: it opens each worker
  slab with `O_DIRECT`, requires `statx(STATX_DIOALIGN)` with valid alignment, and
  rejects unsupported storage (`src/store/slab.rs:143-184,278-300`). Avoid tmpfs,
  the container writable overlay, and unverified network filesystems for slabs.
  There is no buffered-I/O fallback. Allow regular-file read/write, flock, statx,
  and directory creation; checkpoints also require rename and file/directory
  fsync. Check free physical space as sparse files fill.
- `RACER_SLAB_BYTES` is **per worker slab**, despite node-wide memory budgets.
  Each worker opens `worker-<id>-slab-0.dat` at the configured size
  (`src/app.rs:556-562`, `src/store/slab.rs:143-179`). Defaults are 1 GiB per worker,
  64 MiB segments, and two free reserve segments. Existing nonempty files with a
  different size are rejected. Plan capacity for the actual worker count and
  checkpoint files; do not share slabs between running processes.
- The image runs as UID/GID **65532**. Preprovision writable host/PVC mounts for
  that UID (or the explicit UID you select). A root-owned bind mount hides the
  image's prepared ownership. Identity directories must be `0700`, owned by the
  effective UID, with private regular files `0600` and one link. The runtime
  checks these properties and rejects symlink traversal for private directories
  (`src/control/async_files.rs:29-89,144-161`). Do not use `fsGroup` to make the
  private identity directory group-readable. The process can run with a
  read-only root filesystem when all required writable paths below are mounted.
- Make `/proc/self/fd` available for descriptor-anchored client UDS binding
  (`src/client/listener.rs:454-475`). Permit affinity calls for the process's own
  threads and reads of CPU/NUMA topology, cgroup CPU limits, and NIC locality in
  `/proc`, `/sys/devices/system/cpu`, `/sys/fs/cgroup`, and `/sys/class/infiniband`.
  Do not add broad privileged mode merely to enable ordinary HTTP operation.

## CPU, threads, and memory

`RACER_MAX_THREADS=8` means up to four I/O/crypto pairs, not eight workers.
Allowed physical cores, affinity/cpuset, and effective CPU quota constrain pair
count. Odd caps round down; one allowed CPU still supports a pair sharing that
CPU (`src/runtime/affinity.rs:88-112`). Control and diagnostics use those I/O
roles. Kernel io_uring workers are separate from the userspace thread cap;
account for them when setting container PID limits.

Aggregate budgets are divided among workers; insufficient per-worker page
progress reserves reduce the pair count (`src/app.rs:176-194,226-263`). Defaults
are 256 MiB plaintext, 256 MiB ciphertext, 128 MiB dirty, 128 MiB registered,
and 16 MiB request context. These admission dimensions are not a process RSS
limit. Allow headroom for stacks, TLS/control state, indexes, queues, buffers,
kernel socket/pipe/ring resources, and allocator overhead. Measure the deployment
before choosing a cgroup memory limit. See [CONFIGURATION.md](CONFIGURATION.md)
for every bounded resource and progress floor.

Native provisioning charges registered storage and staging together, including
4 KiB rounding (`src/app_native.rs:19-26`, `native/INTEGRATION.md:28-33`). The
default 128 MiB registered budget split four ways does not fund even one paired
slot per worker: a full 16 MiB page plus AEAD tag rounds above 16 MiB and is
charged twice. Fewer pairs or a larger `RACER_REGISTERED_BYTES` budget is needed
for native slots. This is additional to the activation prerequisites below.

## Configuration and mount contract

Set `RACER_CLUSTER_ID` to the deployed non-nil lowercase cluster UUID and
`RACER_CONTROL_ENDPOINT` to its HTTPS authority, for example
`https://racer-controller.unbounded-system.svc:7443`. The endpoint certificate
must match that name/IP. All five configured credential/state paths must be
absolute and mutually non-nested (`src/config.rs:91-113,199-218`).

| Container path (default) | Mount and ownership |
| --- | --- |
| `/etc/racer/trust/ca.crt` | Read-only deployment server-trust PEM CA bundle, readable by the runtime UID. `RACER_TRUST_BUNDLE` selects this file. |
| `/var/run/secrets/racer-control/token` | Read-only rotating projected ServiceAccount token for audience `racer-control`; `RACER_SERVICE_ACCOUNT_TOKEN` selects this file. |
| `/etc/racer/keys` | Read-only **whole** shared SecretBundle projection directory containing `..data/bundle.json` via a relative generation link; selected by `RACER_SECRET_DIRECTORY`. |
| `/var/lib/racer/identity` | Writable node-private persistent directory, `0700`; selected by `RACER_IDENTITY_DIRECTORY`. Contains locally generated `pending.json` and `identity.json`. |
| `/var/lib/racer/slabs` | Writable dedicated direct-I/O-capable persistent storage; selected by `RACER_SLAB_DIRECTORY`. Keep separate from identity and projections. |
| `/run/racer` | Writable shared UDS directory tree for accepted caches. This path is fixed, not an environment override. |

The server-trust file authenticates the controller's HTTPS server; the transport
loads only that configured trust store (`src/control/transport.rs:223-242`).
Installing system CA certificates does not supply it. Peer trust roots and
cache encryption keys come from the distinct shared `bundle.json`. The common
bundle does not contain node signing private keys. The process generates those
locally and persists enrollment/identity records
(`src/control/enrollment.rs:107-123,216-231`). Preserve identity across restarts
of the same node; do not clone it to another node or place it in the common
SecretBundle.

Mount the entire Kubernetes Secret volume at `/etc/racer/keys`, not a `subPath`
mount of `bundle.json`. The reader opens `..data` once and reads that generation
coherently (`src/control/async_files.rs:163-182`). A non-Kubernetes deployment
must implement the same directory layout and atomic generation switch. Trust
and token readers support ordinary projection symlinks. Keep projected files
readable by UID 65532 without exposing them through client/origin mounts.

The control protocol requires a live bound Pod token from the authorized
dataplane ServiceAccount/workload on its assigned Node. Keep token projection
rotation working for enrollment and renewal; do not bake a token into the
image. Node UID, shares, and rails come from authenticated enrollment/membership.
`RACER_NODE_UID`, `RACER_NODE_ID`, `RACER_SHARES`, `RACER_RAILS`, and
`RACER_ALIGNED_RAILS` are rejected even when empty (`src/config.rs:70-80`).

### Client and origin sockets

For each accepted cache name the only paths are:

- `/run/racer/<cache-name>/client/socket`: the dataplane owns this listener.
- `/run/racer/<cache-name>/origin/socket`: the application origin adapter owns it.

Mount each authorized client directory into its consumers and the corresponding
origin directory into the adapter. Racer needs both endpoint paths visible.
Use directory mounts, not socket-file bind mounts, so listener replacement works.
Precreate directories with suitable traversal/write permissions; accepted
`socket_mode` governs the client socket. The dataplane neither changes the
adapter's ownership nor creates its origin listener
(`src/control/caches.rs:55-95`, `src/client/listener.rs:498-514`). Avoid symlinked
ancestors. An existing socket is not blindly unlinked; after a crash, establish
that its owner is gone before removing a stale client socket.

## Networking, readiness, and shutdown

Allow outbound HTTPS to control and bidirectional peer TCP on the published
port. Default peer listen is `0.0.0.0:7443`; the controller's advertised endpoint
must be reachable by peers and agree with `RACER_PEER_LISTEN`. The image's
`EXPOSE` is metadata, not a port mapping. Origin/client traffic is local UDS.

Diagnostics default to `127.0.0.1:9090`. Probe from the same network namespace,
or explicitly configure `RACER_DIAGNOSTICS_LISTEN` to a reachable numeric address
and restrict that access. Startup must first enroll/recover identity, open slabs,
recover state, and accept compatible membership; diagnostics and peer listeners
are started afterward (`src/app.rs:781-878`). Do not treat an image build or
container creation as a readiness check. Use HTTP GET `/healthz`, `/readyz`, and
`/metrics` as documented in `src/telemetry/INTEGRATION.md:46-50`, and allow startup
time for control availability.

SIGTERM/SIGINT initiate lifecycle shutdown (`src/app.rs:373-404`). Configure
termination grace longer than `RACER_SHUTDOWN_TIMEOUT_MS` (default 30000) and
allow completion fencing after admission stops. Do not delete identity, slabs,
or socket directories while a process is draining.

## Optional native RDMA: packaging versus activation

Native operation requires all of:

1. Rust `rdma` feature, compatible `libracer_rdma.so.1`, libibverbs, and the actual
   hardware provider libraries.
2. `RACER_ENABLE_RDMA=true` (default is `false`).
3. Assigned `/dev/infiniband` uverbs devices with device-cgroup and Unix access,
   host RDMA drivers/fabric configuration, usable active ports, GID index zero,
   4 KiB base pages, and type-2B memory-window support.
4. Adequate `RLIMIT_MEMLOCK` for pinned registered memory and provider resources.
   Set the limit in the service/container runtime; `CAP_IPC_LOCK` may be needed
   under the host's policy, but is not a replacement for device access. The
   image cannot grant host devices, change host limits, or configure a fabric.
5. Authenticated controller rail/alignment membership plus trusted local
   `FabricPort` associations and sufficient registered slot budget.

Supply trusted local associations with exactly one of these environment options:

- `RACER_FABRIC_PORTS`: inline JSON array, at most 4096 bytes, no literal control
  characters. Example (replace with verified local values):
  `[{"fabric":"fabric-a","device":"mlx5_0","port":1,"gid":"fe800000000000000000000000001234"}]`.
- `RACER_FABRIC_PORTS_FILE`: absolute path to an operator-managed read-only regular
  JSON file, for example `/etc/racer/native/ports.json`, at most 65536 bytes.
  Mount a ConfigMap/projected file readable by UID 65532; projection symlinks and
  JSON newlines are supported. Keep it separate from writable identity/slab paths.
  The file is read once before startup; restart to apply changes.

Both default to absent; both set together or an empty value/file fails startup.
`[]` explicitly supplies no associations. At most 64 entries are accepted, with
required `fabric`, `device`, and integer `port` (1-255), plus optional `gid`
(omitted/null or 32 lowercase hex digits, nonzero and nonmulticast). Device names
are 1-63 ASCII bytes, start alphanumeric, and contain only alphanumerics, `_`,
`.`, or `-`, without `..`. Fabric labels are opaque UTF-8, 1-4096 bytes, without
controls or surrounding ASCII spaces. Duplicate fabrics/physical ports and unknown
or duplicate fields fail startup. See [CONFIGURATION.md](CONFIGURATION.md) for
the full validation and trust contract.

The executable forwards these associations to `Application::with_fabric_ports`.
They constrain discovered physical ports against authenticated published rails;
they never supply rail IDs/alignment or infer fabric labels from device names.
An optional GID pins the discovered value, not a configurable GID table index.
HTTP remains available when native prerequisites are absent or incompatible.

Follow [native/README.md](native/README.md) for explicitly gated provider tests.
A real-libibverbs compile or no-device test validates linkage/fallback only.
Neither proves DMA, invalidation/fencing, multi-host routing, or NUMA behavior on
deployment hardware. Record those hardware results separately from build checks.
