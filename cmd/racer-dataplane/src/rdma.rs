//! Optional verbs transport. Unsupported isolation or rail mapping selects HTTP.
//!
//! Public contracts always compile; native FFI implementation is gated by `rdma`.
//! Attach the bounded lifecycle endpoints, activate `Devices` against publication,
//! and run `WithNative` on the existing crypto role. Serving I/O turns only consume
//! mailboxes and drive `Sessions::progress`. See `native/INTEGRATION.md`.
mod backend;
pub mod device;
pub mod lifecycle;
pub mod permission;
pub mod registered;
pub mod session;
pub mod transfer;
pub mod verbs;
