//! Focused test crate using the production reactor/admission and raw sockets
//! while unrelated application owners are integrating the full crate.
#![allow(dead_code)]
#[path = "../error.rs"]
mod error;
#[path = "../model/identity.rs"]
pub mod identity;
#[path = "../model/limits.rs"]
pub mod limits;
#[path = "../model/range.rs"]
pub mod range;
pub const MAX_FIELD_BYTES: usize = 8192;
pub const MAX_WIRE_INTEGER: u64 = i64::MAX as u64;
fn parse_decimal(value: &[u8]) -> error::Result<u64> {
    if value.is_empty() || value.len() > 19 || (value.len() > 1 && value[0] == b'0') {
        return Err(error::Error::InvalidRequest);
    }
    value.iter().try_fold(0u64, |n, b| {
        if !b.is_ascii_digit() {
            return Err(error::Error::InvalidRequest);
        }
        n.checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(*b - b'0')))
            .filter(|n| *n <= MAX_WIRE_INTEGER)
            .ok_or(error::Error::InvalidRequest)
    })
}
mod model {
    pub use crate::{identity, limits, range};
}
#[path = "../runtime/admission.rs"]
pub mod admission;
#[path = "../runtime/deadline.rs"]
pub mod deadline;
#[path = "../runtime/reactor.rs"]
pub mod reactor;
mod runtime {
    pub use crate::{admission, deadline, reactor};
}
mod test_support {
    pub mod cluster {
        use crate::model::limits::Limits;
        use std::num::NonZeroUsize;
        pub struct Fixture {
            pub limits: Limits,
        }
        pub fn config(_: bool) -> Fixture {
            let n = NonZeroUsize::new(16).unwrap();
            let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
            Fixture {
                limits: Limits {
                    plaintext_bytes: bytes,
                    ciphertext_bytes: bytes,
                    dirty_bytes: bytes,
                    registered_bytes: bytes,
                    request_context_bytes: bytes,
                    flights: n,
                    waiters_per_flight: n,
                    queue_entries: n,
                    connections_per_neighbor: n,
                    client_connections: n,
                    pipes: n,
                    range_window_pages: n,
                    replay_entries: n,
                    header_bytes: NonZeroUsize::new(4096).unwrap(),
                    route_search_work: n,
                    cached_rankings: n,
                    cached_paths: n,
                    retained_snapshots: n,
                    metadata_entries: n,
                    relay_transfers: n,
                },
            }
        }
    }
}
#[path = "../telemetry.rs"]
mod telemetry;
