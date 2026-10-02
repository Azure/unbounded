//! Strict fixed-length HTTP/1.1. The caller owns scheduling and resource policy.
mod codec;
pub mod connection;
pub use codec::{Codec, Error, Header, MessageHead, Opaque, StartLine};
