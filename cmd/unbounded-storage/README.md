# unbounded-storage

`unbounded-storage` is the per-host storage daemon for Project
Unbounded. It owns a set of local block devices, presents them as
NUMA-pinned shards on top of a fabric (TCP today, RDMA when an HCA is
present), and reconciles its peer and disk topology in place from a
TOML config file. It is the only Rust crate in this repository; the
crate-local conventions live in
[`AGENTS.md`](AGENTS.md).

The runtime is configured by a single TOML file (default
`/etc/unbounded-storage/config.toml`). On every successful reparse of
that file the daemon swaps the peer set and the disk set in place
without restarting; backing allocations, the fabric, and the topology
plan are only built at startup.

## Build

The crate is built through the top-level `Makefile` so CI and local
runs stay aligned. Direct `cargo` invocations are discouraged.

```bash
# format, lint, test, release build (the "did I break it" target)
make unbounded-storage

# release build only
make unbounded-storage-build

# tests only (cargo test --locked --all-targets)
make unbounded-storage-test
```

The built binary is copied to `bin/unbounded-storage`.

## Running

```bash
# default: read /etc/unbounded-storage/config.toml; if absent, fall
# back to built-in defaults (a heap-backed, single-shard, no-peer,
# no-disk run that is mostly useful as a smoke test).
unbounded-storage

# explicit config path; missing or invalid here is fatal
unbounded-storage --config /etc/unbounded-storage/config.toml
```

Run `unbounded-storage --help` for the full surface. `--config` is the
only command-line option: the daemon reads everything else - both the
dynamically reloadable cluster state and the startup-fixed settings (the
fabric endpoint and thread pools, per-shard memory sizing, and
CPU-topology selection) - from the config file. The startup-fixed
settings live in the `[startup]` section and cannot change without a
restart.

| Flag | Default | Effect |
| --- | --- | --- |
| `--config <PATH>` | `/etc/unbounded-storage/config.toml` | TOML or `.binpb` config file. Missing default path is non-fatal; missing explicit path is fatal. |
| `-h, --help` | - | Print help. |
| `-V, --version` | - | Print version. |

The daemon traps `SIGINT` and `SIGTERM` and tears shards down in a
deterministic order: shards stop and join first, disk-channel publications are
cleared, and disks are drained last. At startup shards complete both readiness
phases while parked; initial disks are reconciled and published before RPC
servers and shard serving are activated. A startup disk-open failure retires
the prepared shard layer rather than reporting a partial startup configuration.

## Configuration file

The schema is defined by `api/unbounded-storage/config.proto` and is the source of
truth; the daemon can load strict TOML or a raw binary protobuf wire
message with a `.binpb` extension. The schema is deliberately
proto3-native rather than idiomatic TOML:

- Byte-size fields are plain integer byte counts, with no K/M/G
  suffixes.
- Any field left at its proto3 zero value is treated as "unset" and
  filled with the documented default after load. Unknown keys are
  rejected: the TOML loader is strict, so a typo fails loudly at parse
  time. (The protobuf wire path stays forward-compatible by protobuf's
  own unknown-field semantics.)

Every section, and the table itself, is optional; omitted values fall
back to the documented defaults. The config holds both the dynamically
reloadable cluster state and the startup-fixed knobs (the fabric
endpoint and threads, the fabric in-flight cap, memory sizing, CPU
topology). The latter live in the `[startup]` section; they are read
once at process start and cannot change without a restart, so they are
deliberately excluded from the live-reload diff. The top-level `self`
peer name is also startup-fixed because the process-wide fabric and ring
identities are derived from it. The daemon tracks two config versions
independently: the applied config version (advances as reloadable state
is applied) and the startup config version (pinned to the config realized
at process start). A later config that only changes a startup-fixed field
bumps the applied version but leaves the startup version behind until the
daemon restarts.

```toml
# unbounded-storage.toml

self = "node-a"                 # startup-fixed local peer name.
fingers_per_node = 100           # routing finger-table fanout per node.

[[backends]]
name = "origin"

[backends.config.http]
url = "origin.example.com:80"    # host:port resolved for origin fetches.
stripe_size_bytes = 4194304      # optional; must be a power of two.

# Optional. Disjoint discovery: configure this node with ONLY its direct
# routing neighbors instead of the full cluster. When present, the global
# finger-table build is bypassed and these peer names are used verbatim. Every
# name must reference a [[peers]] entry below and must not equal self. The
# resulting routes are identical to the global build fed the same neighbors,
# so a controller with global view can plan these per process (see
# designs/storage-disjoint-routing-parity.md).
# [routing_plan]
# fingers     = ["node-b", "node-c"]
# successor   = "node-b"        # nearest forward neighbor on the ring.
# predecessor = "node-z"        # nearest backward neighbor on the ring.

[[peers]]                        # include self plus remote peers.
name      = "node-a"             # ring/fabric ids are derived from this name.
tags      = ["region-a", "rack-1"]

[peers.config.tcp]
addr      = "10.0.0.10:9000"     # advertised local fabric address.

[[peers]]
name      = "node-b"
tags      = ["region-a", "rack-2"]

[peers.config.tcp]
addr      = "10.0.0.1:9000"      # parsed as SocketAddr.

# Or, for RDMA peers:
# [peers.config.rdma]
# addr     = "hex:deadbeef"      # provider-native libfabric address bytes.

[[caches]]
name = "cache"
source = "origin"               # backend used for miss fills.

[[disks]]                        # repeat per local device; paths must be unique.
queue_depth = 32                 # optional u32; per-disk io_uring depth.
skip_recovery_scan = false       # only for fresh or benchmark disks.

[disks.config.block]
path        = "/dev/nvme0n1"     # required for block disks.
numa        = 0                  # optional u16; biases the open onto a CPU on this node.

# Or, for file-backed disks:
# [disks.config.file]
# path = "/var/lib/unbounded-storage/disk0.img"
# size = 1073741824              # required bytes for file-backed disks.

# Or replace all [[disks]] entries with automatic discovery:
# [disk_discovery]
# deny_paths = ["/dev/disk/by-id/example"] # optional exact device identities.
#
# [disk_discovery.fallback]
# path = "/var/lib/unbounded-storage/cache.disk"
# size = 2147483648
#
# Discovery continuously selects only completely unused whole block devices.
# Mounted, swap, partitioned, held, read-only, removable, virtual, and
# signature-bearing devices are excluded. Discovered devices are opened with
# O_EXCL and re-probed before use. If none are safe, the file fallback is used;
# its parent directory must exist. Hotplug changes rehash cached stripes and are
# not data preserving.

[[frontends]]
name = "http"
source = "cache"                # backend or cache component name.

[frontends.config.http]
addr = "0.0.0.0:9000"

[startup.memory]                 # startup-fixed; read once at process start.
no_hugepages   = false           # true allocates shard backings from the heap.
memory_total_bytes = 134217728   # u64 bytes (no K/M/G suffix). Node-wide data pool;
                                 #   partial 2 MiB pages are unused, then whole pages
                                 #   are split across serving shards. Unset -> 128 MiB;
                                 #   explicit 0 is invalid when shards are configured.
                                 #   RPC scratch adds 8 pages per fabric unit.

[startup.fabric]
progress_threads    = 2          # libfabric progress threads per fabric unit.
progress_poll_us    = 10         # progress-thread busy-poll budget (us).
rpc_worker_threads  = 4          # RPC worker threads per fabric unit.
max_inflight        = 1024       # max in-flight ops per fabric unit (back-pressure).

[startup.fabric.binds.tcp]
addr                = "0.0.0.0:0" # fabric listen address; :0 picks a free port.

# Or use automatic RDMA HCA binding:
# [startup.fabric.binds.auto_rdma]
# hcas_per_numa_node = 1        # max HCAs used per NUMA node.

[startup.topology]
# serving_cores       = 12       # optional serving-shard cap; omit to use every usable CPU.
nic_workers           = 4        # fabric CPUs pinned per active HCA (0 -> 4).
use_smt_siblings      = false    # also place shards on SMT sibling CPUs.
ignore_isolated       = false    # also schedule onto isolcpus-isolated CPUs.
include_node_cpu0     = false    # allow placing a shard on each NUMA node's CPU 0.
allow_inactive_port   = false    # use HCA ports not in the active state.
disable_rdma          = false    # disable RDMA and force the libfabric tcp provider.
```
