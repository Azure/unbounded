//! Real Racer node logic, payload-free, for many simulated nodes.
//!
//! Each simulated node runs the production decision code from `racer-core`,
//! `topology`, `flow-control` and `page-alloc`; only payloads, sockets, disks
//! and clocks are simulated. Per node:
//!
//! - one [`state::Worker`] per worker (default 2), each with a phantom
//!   `Placement` (shared ranking memo), a `PageFlights` singleflight table, a
//!   `TickLru` memory index and its own `flow_control::Quotas`; pages map to
//!   workers with `WorkerMap::select`;
//! - one phantom `Paths` route cache (shared route memo), link circuits
//!   (`flow_control::Circuits`, the state behind `LinkHealth`), a phantom disk
//!   (`Segments` + `SegmentClock` + `PhantomEntries`) and retention history
//!   (`SecondSight`, `Heat`).
//!
//! [`world::NodeWorld`] drives these with events: read arrival, peer request
//! hops (`decide_hop` at every hop), replies, origin completions, membership
//! publications and faults. See `designs/racer-core-simulation.md`, section
//! "Node integration".

pub mod admission;
pub mod config;
pub mod ids;
pub mod membership;
pub mod state;

pub use config::NodeConfig;
