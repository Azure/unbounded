//! Optional verbs transport. Unsupported isolation or rail mapping selects HTTP.
//!
//! Public contracts always compile; native FFI implementation is gated by `rdma`.
//! Configure `Devices` explicitly, exchange signed setup via `Sessions::prepare`,
//! and drive `Sessions::progress` from the owning I/O reactor. See
//! `native/README.md` for the versioned adapter and integration contract.
pub mod device;
pub mod permission;
pub mod registered;
pub mod session;
pub mod transfer;
pub mod verbs;
