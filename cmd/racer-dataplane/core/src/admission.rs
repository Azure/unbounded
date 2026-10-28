//! Racer resource classes and the pure parts of the admission policy.
//!
//! The dataplane and simulator admission policies both delegate to [`Rules`] so they cannot drift;
//! rejection reporting stays with each caller.

use std::num::NonZeroUsize;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ResourceClass {
    Plaintext,
    Ciphertext,
    DirtyCiphertext,
    Registered,
    RequestContext,
    Flight,
    Waiter,
    Connection,
    Pipe,
    ControlProgress,
    Relay,
    IngressConnection,
    OutboundConnection,
    ControlConnection,
    /// Outbound sockets other than origin metadata, kept below the outbound
    /// limit so metadata lookups never queue behind page bodies.
    OrdinaryOutboundConnection,
}

impl ResourceClass {
    pub const ALL: [Self; 15] = [
        Self::Plaintext,
        Self::Ciphertext,
        Self::DirtyCiphertext,
        Self::Registered,
        Self::RequestContext,
        Self::Flight,
        Self::Waiter,
        Self::Connection,
        Self::Pipe,
        Self::ControlProgress,
        Self::Relay,
        Self::IngressConnection,
        Self::OutboundConnection,
        Self::ControlConnection,
        Self::OrdinaryOutboundConnection,
    ];

    /// Stable snake_case name for reports.
    pub fn name(self) -> &'static str {
        match self {
            Self::Plaintext => "plaintext",
            Self::Ciphertext => "ciphertext",
            Self::DirtyCiphertext => "dirty_ciphertext",
            Self::Registered => "registered",
            Self::RequestContext => "request_context",
            Self::Flight => "flight",
            Self::Waiter => "waiter",
            Self::Connection => "connection",
            Self::Pipe => "pipe",
            Self::ControlProgress => "control_progress",
            Self::Relay => "relay",
            Self::IngressConnection => "ingress_connection",
            Self::OutboundConnection => "outbound_connection",
            Self::ControlConnection => "control_connection",
            Self::OrdinaryOutboundConnection => "ordinary_outbound_connection",
        }
    }
}

impl flow_control::Class for ResourceClass {
    const COUNT: usize = 15;
    fn index(self) -> usize {
        self as usize
    }

    fn requires_wipe(self) -> bool {
        // Only Ciphertext backs AEAD output and disk records sent to peers.
        // Keep every other class conservative, including mixed-use Registered.
        !matches!(self, Self::Ciphertext)
    }
}

/// Outbound slots only origin metadata may use once a worker has at least two.
/// A single-slot worker keeps no reserve so ordinary traffic still progresses.
pub const METADATA_OUTBOUND_RESERVE: usize = 1;

const MIB: usize = 1024 * 1024;

/// Independent bounded-resource dimensions. Validation precedes resource creation.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Limits {
    pub plaintext_bytes: NonZeroUsize,
    pub ciphertext_bytes: NonZeroUsize,
    pub dirty_bytes: NonZeroUsize,
    pub registered_bytes: NonZeroUsize,
    pub request_context_bytes: NonZeroUsize,
    pub flights: NonZeroUsize,
    pub waiters_per_flight: NonZeroUsize,
    pub queue_entries: NonZeroUsize,
    pub connections_per_neighbor: NonZeroUsize,
    pub client_connections: NonZeroUsize,
    pub pipes: NonZeroUsize,
    pub range_window_pages: NonZeroUsize,
    pub header_bytes: NonZeroUsize,
    pub placement_cache_bytes: NonZeroUsize,
    pub cached_paths: NonZeroUsize,
    pub path_cache_bytes: NonZeroUsize,
    /// Per-worker active searches, independent of cache retention and worker count.
    pub active_path_searches: NonZeroUsize,
    /// Old live membership generations in addition to current; cache-only
    /// publications reuse a generation and do not consume another slot.
    pub retained_snapshots: NonZeroUsize,
    pub metadata_entries: NonZeroUsize,
    pub relay_transfers: NonZeroUsize,
}

fn nz(value: usize) -> NonZeroUsize {
    NonZeroUsize::new(value).expect("nonzero default")
}

impl Default for Limits {
    /// Per-worker defaults used for every dimension the environment leaves unset. Byte and count
    /// dimensions are stated per worker and scaled by the I/O worker count during sizing.
    fn default() -> Self {
        Self {
            plaintext_bytes: nz(64 * MIB),
            ciphertext_bytes: nz(128 * MIB),
            dirty_bytes: nz(160 * MIB),
            registered_bytes: nz(96 * MIB),
            request_context_bytes: nz(32 * MIB),
            flights: nz(64),
            waiters_per_flight: nz(64),
            queue_entries: nz(256),
            connections_per_neighbor: nz(2),
            client_connections: nz(64),
            pipes: nz(16),
            range_window_pages: nz(2),
            header_bytes: nz(32 * 1024),
            placement_cache_bytes: nz(16 * MIB),
            cached_paths: nz(128),
            path_cache_bytes: nz(8 * MIB),
            active_path_searches: nz(8),
            retained_snapshots: nz(2),
            metadata_entries: nz(4096),
            relay_transfers: nz(16),
        }
    }
}

/// Per-charge minimum amounts for the byte classes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Floors {
    pub plaintext: usize,
    pub ciphertext: usize,
    pub sealed: usize,
    /// One sealed page plus its largest record header, unpadded.
    pub record: usize,
}

/// Slab allocation unit; must match `page_alloc::buffer_capacity`.
const ALLOCATION_UNIT: usize = 64 * 1024;

impl Floors {
    /// Floors for `page_bytes` pages and `max_header_bytes` record headers.
    pub fn for_page(page_bytes: usize, max_header_bytes: usize) -> Self {
        let record = page_bytes + 16 + max_header_bytes + 4096;
        Self {
            plaintext: page_bytes,
            ciphertext: record.div_ceil(ALLOCATION_UNIT) * ALLOCATION_UNIT,
            sealed: page_bytes + 16,
            record: page_bytes + 16 + max_header_bytes,
        }
    }
}

/// The pure admission rules: per-class limits, floors, and key bound.
#[derive(Clone, Debug)]
pub struct Rules {
    pub limits: Limits,
    pub floors: Floors,
}

impl Rules {
    pub fn new(limits: Limits, floors: Floors) -> Self {
        Self { limits, floors }
    }

    pub fn limit(&self, class: ResourceClass) -> usize {
        let limits = &self.limits;
        match class {
            ResourceClass::Plaintext => limits.plaintext_bytes.get(),
            ResourceClass::Ciphertext => limits.ciphertext_bytes.get(),
            ResourceClass::DirtyCiphertext => limits.dirty_bytes.get(),
            ResourceClass::Registered => limits.registered_bytes.get(),
            ResourceClass::RequestContext => limits.request_context_bytes.get(),
            ResourceClass::Flight => limits.flights.get(),
            ResourceClass::Waiter => limits
                .flights
                .get()
                .saturating_mul(limits.waiters_per_flight.get()),
            ResourceClass::Connection => limits.client_connections.get(),
            // Snapshot, renewal, and keyring delivery make independent progress.
            ResourceClass::ControlConnection => (limits.client_connections.get() / 4).min(3),
            ResourceClass::OutboundConnection => limits.client_connections.get() / 4,
            ResourceClass::OrdinaryOutboundConnection => {
                let outbound = self.limit(ResourceClass::OutboundConnection);
                outbound - usize::from(outbound >= 2) * METADATA_OUTBOUND_RESERVE
            }
            ResourceClass::IngressConnection => limits.client_connections.get().saturating_sub(
                self.limit(ResourceClass::ControlConnection)
                    + self.limit(ResourceClass::OutboundConnection),
            ),
            ResourceClass::Pipe => limits.pipes.get(),
            ResourceClass::ControlProgress => limits.queue_entries.get(),
            ResourceClass::Relay => limits.relay_transfers.get(),
        }
    }

    pub fn floor(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => self.floors.plaintext,
            ResourceClass::Ciphertext => self.floors.ciphertext,
            ResourceClass::DirtyCiphertext | ResourceClass::Registered => self.floors.sealed,
            _ => 1,
        }
    }

    /// Per-cache working allowance of a weighted class (`Policy::progress`). Plaintext is request
    /// working memory; ciphertext is both working and retained memory. Sized for one range window
    /// and shared by all requests on the cache; the equal-share cap can shrink it, so this limits
    /// how far low priority shrinks the share but does not guarantee progress.
    pub fn progress(&self, class: ResourceClass) -> usize {
        let window = self.limits.range_window_pages.get();
        match class {
            ResourceClass::Plaintext => window
                .saturating_add(1)
                .saturating_mul(self.floors.plaintext),
            ResourceClass::Ciphertext => window
                .saturating_mul(self.floors.sealed)
                .saturating_add(self.floors.record),
            _ => 0,
        }
    }

    /// Priority weights page-memory budgets (`Policy::weighted`). Other request-serving classes
    /// keep an equal split so a priority-0 cache can still serve requests.
    pub fn weighted(class: ResourceClass) -> bool {
        matches!(
            class,
            ResourceClass::Plaintext | ResourceClass::Ciphertext | ResourceClass::DirtyCiphertext
        )
    }

    pub fn max_keys(&self) -> usize {
        self.limits.metadata_entries.get()
    }

    pub fn wakes(class: ResourceClass) -> bool {
        matches!(
            class,
            ResourceClass::Connection | ResourceClass::IngressConnection
        )
    }

    pub fn allows_stopped(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::ControlProgress)
    }

    pub fn covers(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::Ciphertext)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flow_control::Class;

    #[test]
    fn indices_follow_all_and_names_are_unique() {
        for (i, class) in ResourceClass::ALL.iter().enumerate() {
            assert_eq!(class.index(), i);
        }
        let mut names: Vec<_> = ResourceClass::ALL.iter().map(|c| c.name()).collect();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), ResourceClass::COUNT);
    }

    #[test]
    fn connection_split_reserves_metadata_and_control() {
        let mut limits = Limits::default();
        let rules = |limits: &Limits| Rules::new(limits.clone(), Floors::for_page(16 * MIB, 0));
        let r = rules(&limits);
        assert_eq!(r.limit(ResourceClass::ControlConnection), 3);
        assert_eq!(r.limit(ResourceClass::OutboundConnection), 16);
        assert_eq!(r.limit(ResourceClass::OrdinaryOutboundConnection), 15);
        assert_eq!(r.limit(ResourceClass::IngressConnection), 45);
        assert_eq!(r.limit(ResourceClass::Waiter), 4096);
        limits.client_connections = nz(4);
        let r = rules(&limits);
        assert_eq!(r.limit(ResourceClass::OutboundConnection), 1);
        assert_eq!(r.limit(ResourceClass::OrdinaryOutboundConnection), 1);
        assert_eq!(r.limit(ResourceClass::ControlConnection), 1);
        assert_eq!(r.limit(ResourceClass::IngressConnection), 2);
        limits.client_connections = nz(1);
        let r = rules(&limits);
        assert_eq!(r.limit(ResourceClass::OrdinaryOutboundConnection), 0);
        assert_eq!(r.limit(ResourceClass::IngressConnection), 1);
        limits.flights = nz(usize::MAX);
        assert_eq!(rules(&limits).limit(ResourceClass::Waiter), usize::MAX);
    }

    #[test]
    fn floors_round_ciphertext_to_allocation_unit() {
        let floors = Floors::for_page(MIB, 20480);
        assert_eq!(floors.plaintext, MIB);
        assert_eq!(floors.sealed, MIB + 16);
        assert_eq!(floors.ciphertext, MIB + 64 * 1024);
        assert_eq!(floors.record, MIB + 16 + 20480);
        assert_eq!(Floors::for_page(0, 0).ciphertext, ALLOCATION_UNIT);
    }
}
