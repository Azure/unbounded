# uring-runtime

A worker-local, explicitly driven io_uring runtime. It does not create an
executor or choose application placement and admission policies.

- `reactor::Reactor<S, B>` owns submissions through their completion fences.
  `Scope` supplies cancellation/deadline policy; `Budget` supplies retained byte
  charges. Callers drive `poll_budgeted` and `wait` explicitly.
- `group` runs pinned local services and optional shared helper services with
  explicit startup, drain, fence, shutdown, and join phases. The caller supplies
  the plan, service factory, and teardown scope.
- `affinity` discovers effective CPU/cgroup/NUMA/NIC constraints. It does not
  choose lane-to-helper ratios or device placement.
- `channel`, `deadline`, and `environment` provide bounded SPSC handoff,
  cancellation registration, and time/entropy sources.

I/O buffers implement unsafe stable-storage contracts: accepted operations can
outlive their futures, and storage must remain valid until the kernel fence.
Dropping an operation is abandonment, not proof that its resources are reusable.

The nondefault `simulation` feature provides deterministic clock, entropy, and
I/O backends. Racer enables it only as a development dependency. Its entropy
domain is retained for compatibility with existing replay seeds.

Racer-specific request IDs, candidate deadlines, admission classes, crypto
queues, page ownership hashing, TLS time conversion, and placement policy stay
in the parent crate's `runtime` adapters.

From the dataplane directory, run `cargo test -p uring-runtime --features simulation`
to test this crate, including the public reserved-capacity lifecycle scenario and
the simulated short-write request/reply workflow. Simulation scenarios need no
io_uring permissions; real-kernel regression tests remain separate and may require
io_uring support. Run `cargo test --workspace --all-features` for workspace coverage.
Build the production binary with:

```sh
cargo build --release --bin racer-dataplane --no-default-features
```
