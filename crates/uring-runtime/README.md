# uring-runtime

A small, explicitly driven, thread-per-core [io_uring] runtime for Linux.

- Explicitly driven: no background threads; the application runs the loop.
- Ownership-based I/O: buffers, addresses and paths are moved into each
  operation and returned with its result, so the kernel never touches freed
  memory.
- Pluggable backend: the `Backend` and `Host` traits separate the reactor from
  the kernel, so the same application code runs under the deterministic
  simulator in `uring-runtime-sim`.
- No application policy: no retries, hedging or hardcoded tuning.

Requires Linux 5.6 or newer (5.15 for `mkdirat`). Minimum supported Rust
version: 1.88.

Licensed under the Apache License, Version 2.0.

[io_uring]: https://kernel.dk/io_uring.pdf
