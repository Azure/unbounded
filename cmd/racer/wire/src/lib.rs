//! Pure Racer wire contracts: control records, object protocol values, the
//! HTTP/1.1 head codec, bounded binary codecs, and route wire checks.
//!
//! This crate performs no I/O and depends on no Racer I/O crate. Transport,
//! scheduling, credential ownership, and application policy stay with callers.
//!
//! - [`control`]: bounded control records and canonical JSON.
//! - [`object`]: object identities, metadata, ranges, and client/origin heads.
//! - [`http`]: strict HTTP/1.1 head codec, MIME values, and byte ranges.
//! - [`codec`]: bounded binary readers and writers and canonical hexadecimal.
//! - [`route`]: wire-form route budget checks.

pub mod codec;
pub mod control;
#[warn(missing_docs)]
pub mod http;
pub mod object;
pub mod route;
