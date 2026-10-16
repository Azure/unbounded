# uring-runtime-sim

Deterministic simulation backend for [`uring-runtime`](../uring-runtime).

`SimBackend` will implement the `uring_runtime::Backend` and `Host` traits on
top of an in-memory network and disk with seeded time, seeded entropy and
typed fault injection, so that a program's execution can be replayed exactly
from a seed. It is a separate crate, not a feature flag, so it can never change
production behavior.

Status: placeholder; the API is not yet available.

Licensed under the Apache License, Version 2.0.
