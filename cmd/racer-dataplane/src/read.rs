//! One acquisition coordinator per worker, shared by client and peer entry points.
pub mod candidates;
pub mod dispatch;
pub(crate) mod drivers;
pub mod fill;
pub mod flight;
pub mod metadata;
pub mod range_stream;
#[cfg(test)]
mod remote_tests;
pub mod serve;
