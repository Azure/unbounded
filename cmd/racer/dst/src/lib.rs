//! `racer-dst`: the unified deterministic simulation test harness (plan D12,
//! D14, section 9.2).
//!
//! One harness, one choice stream, one engine, one invariant set and one cost
//! model for the whole Racer application. Production code runs unmodified;
//! every nondeterministic decision is drawn from [`Choices`], so a run is a
//! pure function of `(scenario, choice stream)`.
//!
//! # Layout
//!
//! | Module | Role |
//! |---|---|
//! | [`authority`] | Full fidelity control plane: real `racer-authority` authorities and the fake API |
//! | [`choices`] | The choice stream: seeded PRNG (CI), fuzzer bytes, record/replay, shrinking, seed files |
//! | [`engine`] | Virtual time ([`engine::Nanos`]), event queue with deterministic ties, [`engine::TraceHash`] |
//! | [`costs`] | The checked-in `costs.toml` cost table and its parser |
//! | [`resources`] | FIFO cost model: NIC/disk/CPU/crypto servers, origin, per-site RTT matrix |
//! | [`world`] | Nodes, roles ([`world::SimRole`], [`world::LaneRole`]), the one-thread scheduler, faults |
//! | [`invariants`] | Safety/resource/liveness registration and built-ins |
//! | [`scenario`] | Builder API: nodes, roles, faults, invariants, bounds, fidelity |
//! | [`model`] | Model fidelity: byte-free interpreter over real `WorkerCore`s on the cost model |
//! | [`determinism`] | Double-run trace comparison and failure shrinking |
//! | [`fixture`] | A minimal racer-io node (two lanes, sim disk, timers, mailbox) |
//! | [`scenarios`] | Built-in scenario catalog run by `make racer-dst` |
//!
//! # Running
//!
//! `make racer-dst` runs `dst/seeds/ci.txt` (each entry twice, comparing
//! trace hashes). Environment: `DST_SEEDS` (a seed file path, or a count `N`
//! to run seeds `1..=N` of every scenario), `DST_STEPS` (step budget per
//! run), `DST_FILTER` (scenario name substring). A failure prints a shrunk
//! `words=` entry; check it into `dst/seeds/regressions.txt`.
//!
//! # Plugging in the real application (Full fidelity, Phase 1 onward)
//!
//! A scenario's node factory ([`Scenario::roles`]) receives a
//! [`world::NodeEnv`] and returns the node's roles. For Full fidelity the
//! factory assembles the real `racer` application through the same
//! `Application` path as `main`, with:
//!
//! - every reactor built by [`world::NodeEnv::reactor`] (or on
//!   `Driver::Sim(env.os.clone())` with [`world::NodeEnv::worker_ctx`]), so
//!   disk, sockets and timers are simulated and time and entropy come from the
//!   world;
//! - every thread of the production process returned as one role: I/O lanes
//!   wrap `racer_io::exec::Lane` in [`world::LaneRole`]; other roles
//!   (crypto helpers, RDMA owner, GPU executors, authority shell) implement
//!   [`world::SimRole`] over their racer-io parts;
//! - per-node context for former process globals, so crash/restart
//!   ([`world::Fault::Crash`], [`world::Fault::Restart`]) rebuilds a node
//!   through the real startup path on the surviving simulated disk.
//!
//! Application observations become invariants: roles report violations via
//! [`world::Reporter`] and fold observed data into the [`world::Trace`];
//! later phases add typed observers to [`invariants::Probe`] (delivered bytes
//! vs origin bytes, key exposure, authority terms).
//!
//! # Model fidelity (Phase 5)
//!
//! [`scenario::Fidelity::Model`] runs [`model`]: a byte-free interpreter over the real
//! `WorkerCore` machines and a real `topology::Placement` (with [`model::phantom`]
//! rankings). Each effect is priced by [`resources`] and completed as an
//! [`engine::Engine`] event instead of a reactor operation. A Model scenario carries a
//! [`model::ModelSpec`] (workload and report checks) instead of a node factory; choices
//! and the determinism double-run are shared. Interpreters are dispatch-only (no
//! decisions), which is what makes Full/Model overlap calibration meaningful. What is
//! modeled is listed in `dst/MODEL.md`.
//!
//! //! # Fuzzing (Phase 10)
//!
//! The non-workspace `fuzz/` crate feeds libFuzzer bytes to
//! [`Choices::from_bytes`] and runs a scenario; the shrinking contract in
//! [`choices`] keeps fuzzer-found streams minimizable.
//!
//! # Determinism rules
//!
//! No `std::collections::{HashMap, HashSet}` in this crate (enforced by
//! `tests/no_random_state.rs`); use `BTreeMap` or a fixed-seed hasher. No host
//! clock or OS entropy on behavior paths: use the node's `WorkerCtx`.
#![deny(missing_docs)]
#![recursion_limit = "512"]

pub mod authority;
pub mod choices;
pub mod cli;
pub mod costs;
pub mod determinism;
pub mod engine;
pub mod fixture;
pub mod invariants;
pub mod model;
// Copied from racer-sim with its documentation as is.
#[allow(missing_docs)]
pub mod resources;
pub mod scenario;
pub mod scenarios;
pub mod world;

pub use choices::Choices;
pub use scenario::Scenario;
pub use world::World;
