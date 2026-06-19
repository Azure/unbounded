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
deterministic order (disks closed first, then fabric / pool drops).

## Configuration file

The schema is defined by `api/unbounded-storage/config.proto` and is the source of
truth; the daemon can load strict TOML or a raw binary protobuf wire
message with a `.binpb` extension. The schema is deliberately
proto3-native rather than idiomatic TOML:

- Enum fields are plain integers (the enum discriminant), not strings.
- Byte-size fields are plain integer byte counts, with no K/M/G
  suffixes.
- Any field left at its proto3 zero value is treated as "unset" and
  filled with the documented default after load. Unknown keys are
  rejected: the TOML loader is strict, so a typo fails loudly at parse
  time, and enum integers outside their defined values are rejected by
  validation. (The protobuf wire path stays forward-compatible by
  protobuf's own unknown-field semantics.)

Every section, and the table itself, is optional; omitted values fall
back to the documented defaults. The config holds both the dynamically
reloadable cluster state and the startup-fixed knobs (the fabric
endpoint and threads, the fabric in-flight cap, memory sizing, CPU
topology). The latter live in the `[startup]` section; they are read
once at process start and cannot change without a restart, so they are
deliberately excluded from the live-reload diff. The daemon tracks two
config versions independently: the applied config version (advances as
reloadable state is applied) and the startup config version (pinned to
the config realized at process start). A later config that only changes
a `[startup]` field bumps the applied version but leaves the startup
version behind until the daemon restarts.

```toml
# unbounded-storage.toml

[p2p]
local_node_id   = 1              # u64; this daemon's node id (required with peers).
fingers_per_node = 100           # routing finger-table fanout per node.

# Optional. Disjoint discovery: configure this node with ONLY its direct
# routing neighbors instead of the full cluster. When present, the global
# finger-table build is bypassed and these ids are used verbatim. Every id
# must reference a [[peers]] entry below and must not be local_node_id. The
# resulting routes are identical to the global build fed the same neighbors,
# so a controller with global view can plan these per node (see
# designs/storage-disjoint-routing-parity.md).
# [p2p.routing_plan]
# fingers     = [2, 5, 9, 17]    # ids of this node's finger neighbors.
# successor   = 2                # id of the nearest forward neighbor on the ring.
# predecessor = 64               # id of the nearest backward neighbor on the ring.

[[peers]]                        # repeat per remote peer; ids must be unique.
id        = 1                    # u64, unique within the daemon.
address   = { socket = "10.0.0.1:9000" }
                                  # numeric fabric listen address. On InfiniBand
                                  # this is usually IPoIB; tcp fallback uses TCP.
                                  # Use { native = "hex:..." } only for
                                  # controller-generated provider-native addresses.
hca_numa  = 0                    # optional u16; pin connection setup to this NUMA node.

[[disks]]                        # repeat per local device; paths must be unique.
path        = "/dev/nvme0n1"     # required.
kind        = 0                  # 0 = nvme (default), 1 = block, 2 = file.
numa        = 0                  # optional u16; biases the open onto a CPU on this node.
queue_depth = 32                 # optional u32; per-disk io_uring depth.

[startup.memory]                 # startup-fixed; read once at process start.
no_hugepages   = false           # true allocates per-shard backing from the heap.
memory_total_bytes = 134217728   # u64 bytes (no K/M/G suffix). Total backing pool split
                                 #   evenly across serving shards. 0 -> 128 MiB.

[startup.fabric]
listen_addr         = "0.0.0.0:0" # fabric listen address; :0 picks a free port.
                                 # Use "hex:..." only for provider-native binds.
progress_threads    = 2          # libfabric progress threads per shard.
progress_poll_us    = 10         # progress-thread busy-poll budget (us).
rpc_worker_threads  = 4          # fabric RPC worker threads per shard.
max_inflight        = 1024       # max in-flight fabric ops per shard (back-pressure).

[startup.topology]
serving_cores         = 0        # serving shards; 0 = auto-fill every usable CPU.
nic_workers           = 4        # fabric CPUs pinned per active HCA (0 -> 4).
hcas_per_numa_node    = 1        # max HCAs used per NUMA node (0 -> 1).
use_smt_siblings      = false    # also place shards on SMT sibling CPUs.
ignore_isolated       = false    # also schedule onto isolcpus-isolated CPUs.
include_node_cpu0     = false    # allow placing a shard on each NUMA node's CPU 0.
allow_inactive_port   = false    # use HCA ports not in the active state.
disable_rdma          = false    # disable RDMA and force the libfabric tcp provider.
```
