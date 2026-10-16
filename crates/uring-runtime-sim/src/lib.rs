//! Deterministic simulation backend for [`uring_runtime`].
//!
//! This crate will provide `SimBackend`, an implementation of
//! [`uring_runtime::Backend`] and [`uring_runtime::Host`] backed by an
//! in-memory world: a simulated network (streams, listeners, datagrams, pipes,
//! partitions), a simulated disk with crash and corruption injection, typed
//! fault injection keyed by [`uring_runtime::backend::OpKind`], a seeded clock
//! and a seeded entropy stream. Running the same program with the same seed
//! replays the same execution.
//!
//! The simulator is a separate crate rather than a cargo feature, so enabling
//! it can never change the behavior of the production backend.
//!
//! The crate is currently a placeholder; its API is not yet available.
