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

# override individual storage knobs without touching the file
unbounded-storage --no-hugepages --bytes-per-shard=64M
```

Run `unbounded-storage --help` for the full surface. The CLI is
intentionally small: everything else lives in the config file so it
can be live-reloaded.

| Flag | Default | Effect |
| --- | --- | --- |
| `--config <PATH>` | `/etc/unbounded-storage/config.toml` | TOML config file. Missing default path is non-fatal; missing explicit path is fatal. |
| `--no-hugepages` | off | Override `[storage] backing_kind` to `heap`. |
| `--bytes-per-shard <BYTES>` | unset | Override `[storage] bytes_per_shard`. Bare integers are bytes; `K`, `M`, `G` suffixes are powers of 1024. Zero is rejected. |
| `-h, --help` | - | Print help. |
| `-V, --version` | - | Print version. |

The daemon traps `SIGINT` and `SIGTERM` and tears shards down in a
deterministic order (disks closed first, then fabric / pool drops).

## Configuration file

The file is TOML with `deny_unknown_fields` enforced at every level,
so typos in a key name fail loudly at parse time rather than being
silently ignored. Every section, and the table itself, is optional;
omitted values fall back to the documented defaults.

```toml
# unbounded-storage.toml

[fabric]
listen_addr      = "0.0.0.0:0"   # libfabric self-address bind. ":0" picks a free port.
max_inflight     = 1024          # per-fabric in-flight request cap.
progress_threads = 2             # libfabric progress threads per shard.
progress_poll_us = 10            # busy-poll budget before yielding (us).

[storage]
bytes_per_shard = "128M"         # int (bytes) or string with K/M/G (powers of 1024).
backing_kind    = "hugepage_2mb" # "hugepage_2mb" or "heap".

[topology]
rdma_progress_per_hca = 1        # progress threads scheduled per HCA.
rdma_handlers_per_hca = 4        # handler threads scheduled per HCA.
use_smt_siblings      = false    # allow placement on SMT sibling cores.
respect_isolated      = true     # skip isolcpus-isolated CPUs when scheduling.
exclude_node_cpu0     = true     # never place workers on CPU 0 of any NUMA node.
require_active_port   = true     # only consider HCAs with an active port.
tcp_fallback_threads  = 1        # progress threads used when no HCA is present.

[[peers]]                        # repeat per remote peer; ids must be unique.
id        = 1                    # u64, unique within the daemon.
transport = "tcp"                # "tcp" or "rdma".
address   = "10.0.0.1:9000"      # tcp: host:port, parsed as SocketAddr.
                                 # rdma: lowercase even-length ASCII hex
                                 #       (raw libfabric address).
hca_numa  = 0                    # optional u16; pin connection setup to this NUMA node.

[[disks]]                        # repeat per local device; paths must be unique.
path        = "/dev/nvme0n1"     # required.
kind        = "nvme"             # "nvme" (default) or "block".
numa        = 0                  # optional u16; biases the open onto a CPU on this node.
queue_depth = 32                 # optional u32; per-disk io_uring depth.
```

### Validation

Failed validation in any of the following categories is fatal at
load and is rejected (without applying) on reload:

- Duplicate `peers[].id`.
- Duplicate `disks[].path`.
- `transport = "tcp"` with an `address` that does not parse as a
  `SocketAddr` (note: hostnames are rejected; supply an IP literal).
- `transport = "rdma"` with an `address` that is empty, odd-length,
  or contains non-hex bytes.
- Empty `disks[].path`.
- Unknown keys at any level (top-level table, any `[...]` section, or
  inside an entry of `[[peers]]` / `[[disks]]`).

### Live reload

The daemon watches the config file via `notify` and reapplies any
update that parses and validates successfully. Each successful
update increments a monotonic `generation` counter that appears in
the daemon's log lines so an operator can correlate a TOML edit with
the reconciliation pass it produced.

Reconciliation is in-place where the runtime supports it:

- `[[peers]]` adds, removes, and address/NUMA changes are applied to
  every shard's fabric on the next update. Failures are logged
  per-shard per-peer; surviving peers stay applied.
- `[[disks]]` adds and removes are applied to the `DiskRegistry`,
  which opens or closes the underlying io_uring engine on its own
  progress thread. Open failures are logged and the disk is left out
  of the published topology.
- `[fabric]`, `[storage]`, and `[topology]` are not reloaded. They
  govern allocations and thread placements that are decided at
  startup; changing them requires a restart.

A reload that fails to parse (or fails any of the validation rules
above) is dropped with an error log and the previous configuration
stays in effect.
