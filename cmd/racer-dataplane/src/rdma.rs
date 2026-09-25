//! Optional verbs transport. Unsupported isolation or rail mapping selects HTTP.
//!
//! Public contracts always compile; native FFI implementation is gated by `rdma`.
pub mod device;
pub mod permission;
pub mod registered;
pub mod session;
pub mod transfer;
pub mod verbs;
