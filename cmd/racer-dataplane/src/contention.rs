//! Payload-free, discrete-event pressure model. Only admission is production code;
//! queues, service times, placement, and page ownership are explicit abstractions.

use crate::{
    model::{identity::CacheId, limits::ResourceClass, range::PAGE_BYTES},
    runtime::admission::{Admission, Reservation},
};
use std::{
    cmp::Reverse,
    collections::{BTreeMap, BinaryHeap, VecDeque},
    sync::Arc,
};

const PLAIN: usize = PAGE_BYTES as usize;
const CIPHER: usize = PLAIN + 16;
const CONTEXT: usize = 256;
const CLASSES: [ResourceClass; 11] = [
    ResourceClass::Plaintext,
    ResourceClass::Ciphertext,
    ResourceClass::DirtyCiphertext,
    ResourceClass::Registered,
    ResourceClass::RequestContext,
    ResourceClass::Flight,
    ResourceClass::Waiter,
    ResourceClass::Connection,
    ResourceClass::Pipe,
    ResourceClass::ControlProgress,
    ResourceClass::Relay,
];

#[derive(Clone, Debug)]
struct Config {
    nodes: usize,
    workers: usize,
    pages_per_worker: usize,
    dirty_pages: usize,
    pipes: usize,
    queue_entries: usize,
    window: usize,
    network_bytes_per_tick: u64,
    disk_bytes_per_tick: u64,
    reader_bytes_per_tick: u64,
    origin_bytes_per_tick: u64,
    completion_ticks: u64,
    retry_ticks: u64,
    max_attempts: usize,
    max_events: usize,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            nodes: 2000,
            workers: 1,
            pages_per_worker: 8,
            dirty_pages: 2,
            pipes: 8,
            queue_entries: 64,
            window: 2,
            network_bytes_per_tick: 1024 * 1024,
            disk_bytes_per_tick: 512 * 1024,
            reader_bytes_per_tick: 1024 * 1024,
            origin_bytes_per_tick: 16 * 1024 * 1024,
            completion_ticks: 2,
            retry_ticks: 64,
            max_attempts: 3,
            max_events: 2_000_000,
        }
    }
}

#[derive(Clone, Debug)]
struct Request {
    at: u64,
    node: usize,
    cache: usize,
    object: u64,
    first_page: u64,
    pages: usize,
    reader_bytes_per_tick: Option<u64>,
    cancel_after: Option<u64>,
    deadline: u64,
}
impl Request {
    fn new(node: usize, object: u64) -> Self {
        Self {
            at: 0,
            node,
            cache: 0,
            object,
            first_page: 0,
            pages: 4,
            reader_bytes_per_tick: None,
            cancel_after: None,
            deadline: 1_000_000,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct Report {
    submitted: usize,
    completed: usize,
    failed: usize,
    canceled: usize,
    retries: usize,
    hits: usize,
    fills: usize,
    joined: usize,
    persistence_skips: usize,
    evictions: usize,
    origin_bytes: u64,
    peer_bytes: u64,
    delivered_bytes: u64,
    disk_bytes: u64,
    events: usize,
    peak_events: usize,
    peak_requests: usize,
    peak_logical_bytes: u64,
    peak_worker: [usize; 11],
    rejections: [usize; 11],
    final_used: [usize; 11],
    latency_ticks: Vec<u64>,
    end_tick: u64,
    trace_hash: u64,
}

// Implementation follows in the next commit. Scenario and fidelity tests live
// in separate child modules so the entire scaffold stays out of release builds.
