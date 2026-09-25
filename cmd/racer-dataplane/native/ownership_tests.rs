//! Standalone compilation of the real adapter and error types permits ownership
//! verification while other component owners are editing the crate concurrently.
#[path = "../src/error.rs"]
mod error;
#[path = "../src/rdma/verbs.rs"]
mod verbs;
