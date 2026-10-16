# uring-runtime-sim

Deterministic simulation backend for [`uring-runtime`](../uring-runtime).

`SimBackend` implements the `uring_runtime::Backend` and `Host` traits on top
of an in-memory `World`, so the same application code that runs on io_uring
runs unchanged under simulation, and an execution can be replayed exactly
from a seed.

- Filesystem: Linux `openat2` resolution (`RESOLVE_*`, symlinks, `O_PATH`,
  `O_NOFOLLOW`, trailing slashes), sparse files, `flock`, `renameat2`
  flags, `O_DIRECT` alignment, configurable reported owner, link count and
  umask.
- Durability: separate volatile and durable images; `fsync` of a file or a
  directory makes exactly that inode durable; `crash`, `crash_under(subtree)`
  and `corrupt` for power-loss and bit-rot testing.
- Network: Unix and Internet streams, listeners, Internet datagrams, pipes,
  Linux `poll` readiness, half-close, partitions between endpoint labels, and
  forced disconnects.
- Faults: typed, first-in first-out fault injection (`Errno`, `Short`,
  `Delay`, `HoldCompletion`) matched by `OpKind` for asynchronous operations
  or `SyncOp` for synchronous calls; rejected submissions, small transfer
  chunks and cancel completion ordering knobs.
- Determinism: a manually advanced `SimClock` (`Send + Sync`), a SHA-256
  counter-mode `SimEntropy` stream, ordered tables and a bounded event trace
  for comparing runs.

```rust
use uring_runtime::backend::BackendConfig;
use uring_runtime::Backend;
use uring_runtime_sim::{DEFAULT_ENTROPY_DOMAIN, SimBackend, SimClock, SimEntropy, World};

let world = World::new();
let clock = SimClock::new(42);
let mut backend = SimBackend::new(&world, clock, SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 42, 0));
backend.init(&BackendConfig::new(256)).unwrap();
world.write_file("/etc/config", b"key = value").unwrap();
```

The simulator never sleeps and never touches the host filesystem, network,
clock or random number generator. It is a separate crate, not a feature
flag, so it can never change production behavior.

The `tests/differential.rs` suite runs the same operations against the host
Linux kernel and the simulator and requires identical results.

Minimum supported Rust version: 1.88.

Licensed under the Apache License, Version 2.0.
