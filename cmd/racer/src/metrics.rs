//! The prometheus endpoint.
//!
//! Counters are per-core plain `u64` adds, no atomics. Each worker publishes its own row
//! of `AtomicU64` from `Handler::tick`; the scrape thread only sums rows and never touches
//! the runtime, so it cannot block a worker or deadlock at shutdown. Every slot sums, so
//! core 0 alone writes process-wide values (server.rs).

use std::fmt::Write as _;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::OnceLock;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use crate::config::MAX_EXTENTS;

#[derive(Clone, Copy)]
enum Kind {
    Counter,
    Gauge,
}

impl Kind {
    fn as_str(self) -> &'static str {
        match self {
            Kind::Counter => "counter",
            Kind::Gauge => "gauge",
        }
    }
}

/// The metric table. Rows sharing a name must stay adjacent, so the encoder emits one
/// `# HELP`/`# TYPE` pair per metric while the label sets differ.
macro_rules! metrics {
    ($($field:ident: $name:literal $labels:literal $kind:ident $help:literal,)*) => {
        /// One worker's counters, filled in each tick (server.rs).
        #[derive(Clone, Copy, Default)]
        pub struct Sample {
            $(pub $field: u64,)*
        }

        const TABLE: &[(&str, &str, Kind, &str)] = &[$(($name, $labels, Kind::$kind, $help),)*];

        impl Sample {
            fn as_array(&self) -> [u64; N] {
                [$(self.$field,)*]
            }
        }
    };
}

metrics! {
    // consensus
    paxos_accept_ok:         "racer_paxos_accept_total"           r#"{result="ok"}"#           Counter "Guarded accepts, by outcome.",
    paxos_accept_rejected:   "racer_paxos_accept_total"           r#"{result="rejected"}"#     Counter "Guarded accepts, by outcome.",
    paxos_one_shot:          "racer_paxos_one_shot_total"         ""                           Counter "Writes that committed without a prepare phase.",
    paxos_guard_conflicts:   "racer_paxos_guard_conflicts_total"  ""                           Counter "Accepts refused because the guard did not match.",
    paxos_prepares:          "racer_paxos_prepare_total"          ""                           Counter "Prepare phases run after a one-shot accept failed.",
    paxos_term_bumps:        "racer_paxos_term_bumps_total"       ""                           Counter "Term increments.",
    paxos_lww_retries:       "racer_paxos_lww_retry_total"        ""                           Counter "Last-writer-wins rounds retried after losing a race.",
    paxos_repairs:           "racer_paxos_repair_total"           ""                           Counter "Pages rewritten to a lagging replica.",
    paxos_read_matched:      "racer_paxos_read_total"             r#"{result="matched"}"#      Counter "Quorum reads, by outcome.",
    paxos_read_remote_match: "racer_paxos_read_total"             r#"{result="remote_match"}"# Counter "Quorum reads, by outcome.",
    paxos_read_failed:       "racer_paxos_read_total"             r#"{result="failed"}"#       Counter "Quorum reads, by outcome.",
    paxos_learn_stale:       "racer_paxos_learn_stale_total"      ""                           Counter "Learn frames dropped as older than the local value.",
    paxos_seals:             "racer_paxos_seal_total"             ""                           Counter "Shards sealed for handover.",
    paxos_groups_unavailable: "racer_paxos_groups_unavailable"    ""                           Counter "Rounds abandoned because no quorum was reachable.",
    paxos_gateway_retries:   "racer_gateway_fallback_total"       r#"{reason="retry"}"#        Counter "Cross-zone operations that fell through the gateway ring, by outcome. A retry went on to an answer; unavailable means no gateway of the zone answered.",
    paxos_zones_unavailable: "racer_gateway_fallback_total"       r#"{reason="unavailable"}"#  Counter "Cross-zone operations that fell through the gateway ring, by outcome.",
    paxos_warms_sent:        "racer_warm_total"                   r#"{result="sent"}"#         Counter "Cross-zone cache warming frames, by outcome. Sent counts what a commit fanned out, taken what a receiver acted on, dropped what it declined.",
    paxos_warms_taken:       "racer_warm_total"                   r#"{result="taken"}"#        Counter "Cross-zone cache warming frames, by outcome.",
    paxos_warms_dropped:     "racer_warm_total"                   r#"{result="dropped"}"#      Counter "Cross-zone cache warming frames, by outcome.",

    // anti-entropy
    heal_sweeps:             "racer_heal_sweep_total"             ""                           Counter "Anti-entropy sweeps started.",
    heal_buckets_diff:       "racer_heal_buckets_diff_total"      ""                           Counter "Digest buckets found to differ from a peer.",
    heal_repairs:            "racer_heal_repair_total"            r#"{result="ok"}"#           Counter "Pages repaired by anti-entropy, by outcome.",
    heal_failed:             "racer_heal_repair_total"            r#"{result="failed"}"#       Counter "Pages repaired by anti-entropy, by outcome.",
    heal_oversized:          "racer_heal_oversized_total"         ""                           Counter "Sweeps cut short because the divergence was too large.",
    heal_dropped:            "racer_heal_dropped_total"           ""                           Counter "Registers given back after a group moved off this node.",
    heal_stalled:            "racer_heal_stalled_total"           ""                           Counter "Anti-entropy sweeps abandoned for running too long. Nonzero means maintenance is waiting on something that never answers.",
    heal_replaying:          "racer_heal_groups_replaying"        ""                           Gauge   "Groups this node is replaying into. Nonzero means a group is running two of three.",
    heal_shedding:           "racer_heal_groups_shedding"         ""                           Gauge   "Groups this node still holds registers for but is no longer a member of.",

    // cooperative cache. Split by page class throughout: the two classes differ by three
    // orders of magnitude in bytes per entry, so unlabelled totals say nothing.
    cache_hits_small:        "racer_cache_lookup_total"           r#"{class="small",result="hit"}"#   Counter "Cache lookups, by class and outcome.",
    cache_hits_huge:         "racer_cache_lookup_total"           r#"{class="huge",result="hit"}"#    Counter "Cache lookups, by class and outcome.",
    cache_misses_small:      "racer_cache_lookup_total"           r#"{class="small",result="miss"}"#  Counter "Cache lookups, by class and outcome.",
    cache_misses_huge:       "racer_cache_lookup_total"           r#"{class="huge",result="miss"}"#   Counter "Cache lookups, by class and outcome.",
    cache_served_small:      "racer_cache_served_total"           r#"{class="small"}"#         Counter "Cache hits that then passed confirmation against the quorum, by class.",
    cache_served_huge:       "racer_cache_served_total"           r#"{class="huge"}"#          Counter "Cache hits that then passed confirmation against the quorum, by class.",
    cache_admits_small:      "racer_cache_admit_total"            r#"{class="small"}"#         Counter "Pages admitted to the cache, by class.",
    cache_admits_huge:       "racer_cache_admit_total"            r#"{class="huge"}"#          Counter "Pages admitted to the cache, by class.",
    cache_evictions_small:   "racer_cache_evict_total"            r#"{class="small"}"#         Counter "Pages evicted from the cache to make room for a hotter one, by class.",
    cache_evictions_huge:    "racer_cache_evict_total"            r#"{class="huge"}"#          Counter "Pages evicted from the cache to make room for a hotter one, by class.",
    cache_dropped_small:     "racer_cache_dropped_total"          r#"{class="small"}"#         Counter "Pages lost because their chunk went to the other class or back to the allocator, by class.",
    cache_dropped_huge:      "racer_cache_dropped_total"          r#"{class="huge"}"#          Counter "Pages lost because their chunk went to the other class or back to the allocator, by class.",
    cache_stale_small:       "racer_cache_stale_total"            r#"{class="small"}"#         Counter "Cache hits that confirmation found stale, by class.",
    cache_stale_huge:        "racer_cache_stale_total"            r#"{class="huge"}"#          Counter "Cache hits that confirmation found stale, by class.",
    cache_shed_small:        "racer_cache_shed_total"             r#"{class="small"}"#         Counter "Cache work declined because the store was under pressure, by class.",
    cache_shed_huge:         "racer_cache_shed_total"             r#"{class="huge"}"#          Counter "Cache work declined because the store was under pressure, by class.",
    cache_reject_policy_small: "racer_cache_reject_total"         r#"{class="small",reason="policy"}"# Counter "Admissions refused, by class and reason: policy is the extent's cache_admit, victim is a hotter incumbent. For 4 KiB pages the policy count lands on the group member that computes the width, not on the node that would have cached.",
    cache_reject_policy_huge:  "racer_cache_reject_total"         r#"{class="huge",reason="policy"}"#  Counter "Admissions refused, by class and reason: policy is the extent's cache_admit, victim is a hotter incumbent. For 4 KiB pages the policy count lands on the group member that computes the width, not on the node that would have cached.",
    cache_reject_victim_small: "racer_cache_reject_total"         r#"{class="small",reason="victim"}"# Counter "Admissions refused, by class and reason: policy is the extent's cache_admit, victim is a hotter incumbent. For 4 KiB pages the policy count lands on the group member that computes the width, not on the node that would have cached.",
    cache_reject_victim_huge:  "racer_cache_reject_total"         r#"{class="huge",reason="victim"}"#  Counter "Admissions refused, by class and reason: policy is the extent's cache_admit, victim is a hotter incumbent. For 4 KiB pages the policy count lands on the group member that computes the width, not on the node that would have cached.",
    cache_bytes_small:       "racer_cache_bytes"                  r#"{class="small"}"#         Gauge   "Media the cache holds, by class.",
    cache_bytes_huge:        "racer_cache_bytes"                  r#"{class="huge"}"#          Gauge   "Media the cache holds, by class.",
    cache_borrowed_small:    "racer_cache_borrowed_bytes"         r#"{class="small"}"#         Gauge   "Cache media on loan from the allocator's free list rather than the store's tail, by class.",
    cache_borrowed_huge:     "racer_cache_borrowed_bytes"         r#"{class="huge"}"#          Gauge   "Cache media on loan from the allocator's free list rather than the store's tail, by class.",
    cache_tail_bytes:        "racer_cache_tail_bytes"             ""                           Gauge   "Store bytes past the end of the layout, the space the cache is carved from.",
    cache_unused_bytes:      "racer_cache_unused_bytes"           ""                           Gauge   "Tail the cache is not holding because policy.cache_index_bytes will not pay to index it.",

    // allocator
    alloc_slots_small:       "racer_alloc_slots"                  r#"{class="small"}"#         Gauge   "Page slots on this node, by slab class.",
    alloc_slots_huge:        "racer_alloc_slots"                  r#"{class="huge"}"#          Gauge   "Page slots on this node, by slab class.",
    alloc_free_small:        "racer_alloc_free_slots"             r#"{class="small"}"#         Gauge   "Unallocated page slots, by slab class.",
    alloc_free_huge:         "racer_alloc_free_slots"             r#"{class="huge"}"#          Gauge   "Unallocated page slots, by slab class.",
    alloc_pressure_low:      "racer_alloc_cores_pressured"        r#"{level="low"}"#           Gauge   "Workers whose shards are short of free slots, by watermark.",
    alloc_pressure_critical: "racer_alloc_cores_pressured"        r#"{level="critical"}"#      Gauge   "Workers whose shards are short of free slots, by watermark.",
    alloc_quarantined:       "racer_alloc_quarantined_blocks"     ""                           Gauge   "Metadata blocks that failed both copies at startup.",
    alloc_unbacked:          "racer_alloc_unbacked_pages"         ""                           Gauge   "Pages the configuration asks for that the store has no slots for. Nonzero until a restart grows the store.",
    mblock_commits_small:     "racer_mblock_commits_total"         r#"{class="small"}"#         Counter "Metadata mutations staged, by slab class.",
    mblock_commits_huge:      "racer_mblock_commits_total"         r#"{class="huge"}"#          Counter "Metadata mutations staged, by slab class.",
    mblock_flushes_small:     "racer_mblock_flushes_total"         r#"{class="small"}"#         Counter "Metadata block writes issued, by slab class.",
    mblock_flushes_huge:      "racer_mblock_flushes_total"         r#"{class="huge"}"#          Counter "Metadata block writes issued, by slab class.",
    mblock_flush_batch_small: "racer_mblock_flush_batch_total"     r#"{class="small"}"#         Counter "Metadata mutations covered by block writes, by slab class.",
    mblock_flush_batch_huge:  "racer_mblock_flush_batch_total"     r#"{class="huge"}"#          Counter "Metadata mutations covered by block writes, by slab class.",
    commit_parks_small:       "racer_commit_park_total"            r#"{class="small"}"#         Counter "Commit waits behind an in-flight metadata block write, by slab class.",
    commit_parks_huge:        "racer_commit_park_total"            r#"{class="huge"}"#          Counter "Commit waits behind an in-flight metadata block write, by slab class.",
    flush_busy_us_small:      "racer_flush_busy_us_total"          r#"{class="small"}"#         Counter "Aggregate microseconds metadata block writes were in flight, by slab class.",
    flush_busy_us_huge:       "racer_flush_busy_us_total"          r#"{class="huge"}"#          Counter "Aggregate microseconds metadata block writes were in flight, by slab class.",
    swept_epoch_small:        "racer_sweep_reclaimed_total"        r#"{class="small",reason="epoch"}"#     Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    swept_epoch_huge:         "racer_sweep_reclaimed_total"        r#"{class="huge",reason="epoch"}"#      Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    swept_uncovered_small:    "racer_sweep_reclaimed_total"        r#"{class="small",reason="uncovered"}"# Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    swept_uncovered_huge:     "racer_sweep_reclaimed_total"        r#"{class="huge",reason="uncovered"}"#  Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",

    // store rate budget
    store_throttle_us:       "racer_store_throttle_us_total"     ""                           Counter "Time transfers were held back to keep the store within its configured rate.",

    // node and configuration
    config_generation:       "racer_config_generation"            ""                           Gauge   "Generation of the configuration in force.",
    config_rejected:         "racer_config_rejected_total"        ""                           Counter "Configurations rejected since start.",
    topology_epoch:          "racer_topology_epoch"               ""                           Gauge   "Highest topology epoch in force across this node's universes.",
    node_id:                 "racer_node_id"                      ""                           Gauge   "This node's id.",
    workers:                 "racer_workers"                      ""                           Gauge   "Worker threads, one per physical core.",
    universes:               "racer_universes"                    ""                           Gauge   "Universes this node participates in.",
    devices:                 "racer_devices"                      ""                           Gauge   "Block devices this node exports.",
    extents:                 "racer_extents"                      ""                           Gauge   "Extents this node's configuration names.",
    peers:                   "racer_peers"                        ""                           Gauge   "Peers this node holds a fabric link to.",
}

const N: usize = TABLE.len();

/// One worker's slots, cacheline padded; only its owner stores, so no `fetch_add`.
#[repr(align(64))]
struct Row([AtomicU64; N]);

#[repr(align(64))]
struct ExtRow([AtomicU64; MAX_EXTENTS * EXT_SERIES.len()]);

/// Everything a scrape sums, in one place. A node has exactly one of these.
struct Tables {
    rows: OnceLock<Box<[Row]>>,
    exts: OnceLock<Box<[ExtRow]>>,
    /// The `(universe, extent)` each per-extent row stands for, `(0, 0)` when unused.
    /// Extent ids are not dense, so a row index is the extent's configuration position,
    /// which every core agrees on. These do not sum; only core 0 writes them.
    ext_ids: [(AtomicU64, AtomicU64); MAX_EXTENTS],
}

impl Tables {
    const fn new() -> Tables {
        Tables {
            rows: OnceLock::new(),
            exts: OnceLock::new(),
            ext_ids: [const { (AtomicU64::new(0), AtomicU64::new(0)) }; MAX_EXTENTS],
        }
    }
}

/// This node's tables. One process is one node, so they are process-wide.
#[cfg(not(feature = "sim"))]
fn tables<R>(f: impl FnOnce(&Tables) -> R) -> R {
    static TABLES: Tables = Tables::new();
    f(&TABLES)
}

/// This node's tables, under simulation. The simulator runs a whole cluster in one thread
/// and a test may run several side by side, so process-wide tables would sum unrelated
/// clusters and contend on one line.
#[cfg(feature = "sim")]
fn tables<R>(f: impl FnOnce(&Tables) -> R) -> R {
    thread_local! {
        static TABLES: Tables = const { Tables::new() };
    }
    TABLES.with(|t| f(t))
}

/// One row per worker in both tables. Called once, from the first configuration.
pub(crate) fn init(cores: usize) {
    tables(|t| {
        let rows = (0..cores)
            .map(|_| Row(std::array::from_fn(|_| AtomicU64::new(0))))
            .collect();
        let _ = t.rows.set(rows);
        let exts = (0..cores)
            .map(|_| ExtRow(std::array::from_fn(|_| AtomicU64::new(0))))
            .collect();
        let _ = t.exts.set(exts);
    })
}

/// Publish this worker's row. Relaxed: a mid-update scrape mixes two adjacent ticks.
pub(crate) fn publish(core: usize, s: &Sample) {
    tables(|t| {
        let Some(row) = t.rows.get().and_then(|r| r.get(core)) else {
            return;
        };
        for (slot, v) in row.0.iter().zip(s.as_array()) {
            slot.store(v, Ordering::Relaxed);
        }
    })
}

fn collect() -> [u64; N] {
    tables(|t| {
        let mut out = [0u64; N];
        for row in t.rows.get().map(|r| &r[..]).unwrap_or(&[]) {
            for (o, slot) in out.iter_mut().zip(row.0.iter()) {
                *o = o.saturating_add(slot.load(Ordering::Relaxed));
            }
        }
        out
    })
}

/// The prometheus text exposition format, version 0.0.4.
fn encode() -> String {
    let values = collect();
    let mut out = String::with_capacity(N * 96);
    let mut last = "";
    for (&(name, labels, kind, help), v) in TABLE.iter().zip(values) {
        if name != last {
            let _ = writeln!(out, "# HELP {name} {help}");
            let _ = writeln!(out, "# TYPE {name} {}", kind.as_str());
            last = name;
        }
        let _ = writeln!(out, "{name}{labels} {v}");
    }
    encode_extents(&mut out);
    out
}

// ------------------------------------------------------------------------ per extent

/// Per-extent series, outside the fixed table because an extent exists only once named.
const EXT_SERIES: [(&str, &str); 2] = [
    (
        "racer_extent_live_pages",
        "Immutable pages still live in the extent, by extent.",
    ),
    (
        "racer_extent_tombstones",
        "Trimmed pages awaiting the extent's next epoch, by extent.",
    ),
];

/// Publish this worker's per-extent counts: `(universe, extent, live, tombstones)`, in
/// configuration order. Rows past the end are zeroed so a stale count stops contributing.
/// Only core 0 writes the names; a core behind on a reload would otherwise retire a series.
pub(crate) fn publish_extents(core: usize, rows: &[(u32, u32, u64, u64)]) {
    tables(|t| {
        let Some(row) = t.exts.get().and_then(|r| r.get(core)) else {
            return;
        };
        for slot in row.0.iter() {
            slot.store(0, Ordering::Relaxed);
        }
        for (s, &(universe, extent, live, tombs)) in rows.iter().enumerate() {
            if s >= MAX_EXTENTS {
                break;
            }
            if core == 0 {
                t.ext_ids[s].0.store(universe as u64, Ordering::Relaxed);
                t.ext_ids[s].1.store(extent as u64, Ordering::Relaxed);
            }
            row.0[s].store(live, Ordering::Relaxed);
            row.0[MAX_EXTENTS + s].store(tombs, Ordering::Relaxed);
        }
        // Clearing names past the end retires an extent the configuration dropped.
        if core == 0 {
            for slot in t.ext_ids.iter().skip(rows.len().min(MAX_EXTENTS)) {
                slot.0.store(0, Ordering::Relaxed);
                slot.1.store(0, Ordering::Relaxed);
            }
        }
    })
}

fn encode_extents(out: &mut String) {
    tables(|t| {
        for (i, (name, help)) in EXT_SERIES.iter().enumerate() {
            let _ = writeln!(out, "# HELP {name} {help}");
            let _ = writeln!(out, "# TYPE {name} gauge");
            for (slot, (universe, extent)) in t.ext_ids.iter().enumerate() {
                let extent = extent.load(Ordering::Relaxed);
                if extent == 0 {
                    continue;
                }
                let universe = universe.load(Ordering::Relaxed);
                let mut v = 0u64;
                for row in t.exts.get().map(|r| &r[..]).unwrap_or(&[]) {
                    v = v.saturating_add(row.0[i * MAX_EXTENTS + slot].load(Ordering::Relaxed));
                }
                let _ = writeln!(
                    out,
                    "{name}{{universe=\"{universe}\",extent=\"{extent}\"}} {v}"
                );
            }
        }
    })
}

// ------------------------------------------------------------------------------- http

/// Deadline on each socket read or write of a scrape.
const TIMEOUT: Duration = Duration::from_secs(5);
/// A scrape is one short GET. Anything larger is not one.
const MAX_HEAD: usize = 8 << 10;

const EXPOSITION: &str = "text/plain; version=0.0.4; charset=utf-8";
const PLAIN: &str = "text/plain; charset=utf-8";

/// Bind the endpoint. Separate from [`serve`] so a bad address fails at startup and early
/// scrapes queue in the backlog instead of being refused.
pub fn listen(addr: &str) -> std::io::Result<TcpListener> {
    match addr.strip_prefix(':') {
        Some(port) => TcpListener::bind(format!("0.0.0.0:{port}")),
        None => TcpListener::bind(addr),
    }
}

/// The `racer-metrics` thread. One connection at a time; prometheus reconnects per scrape.
pub fn serve(l: TcpListener) -> ! {
    loop {
        match l.accept() {
            Ok((s, _)) => handle(s),
            // Descriptor exhaustion is transient: back off, never leave the loop.
            Err(e) if e.kind() != std::io::ErrorKind::Interrupted => {
                std::thread::sleep(Duration::from_millis(100));
            }
            Err(_) => {}
        }
    }
}

fn handle(mut s: TcpStream) {
    let _ = s.set_read_timeout(Some(TIMEOUT));
    let _ = s.set_write_timeout(Some(TIMEOUT));

    // Read the whole head before answering, so the peer never writes into a closed socket.
    let mut head = Vec::new();
    let mut buf = [0u8; 1024];
    loop {
        if head.windows(4).any(|w| w == b"\r\n\r\n") {
            break;
        }
        if head.len() > MAX_HEAD {
            return respond(&mut s, 431, "Request Header Fields Too Large", PLAIN, "");
        }
        match s.read(&mut buf) {
            Ok(0) => return,
            Ok(n) => head.extend_from_slice(&buf[..n]),
            Err(_) => return,
        }
    }

    let line = std::str::from_utf8(&head)
        .unwrap_or("")
        .lines()
        .next()
        .unwrap_or("");
    let mut words = line.split(' ');
    let method = words.next().unwrap_or("");
    let path = words.next().unwrap_or("").split('?').next().unwrap_or("");
    match (method, path) {
        ("GET", "/metrics") => respond(&mut s, 200, "OK", EXPOSITION, &encode()),
        ("GET", _) => respond(&mut s, 404, "Not Found", PLAIN, "try /metrics\n"),
        _ => respond(&mut s, 405, "Method Not Allowed", PLAIN, "only GET\n"),
    }
}

fn respond(s: &mut TcpStream, code: u16, reason: &str, ctype: &str, body: &str) {
    let head = format!(
        "HTTP/1.1 {code} {reason}\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    let _ = s.write_all(head.as_bytes());
    let _ = s.write_all(body.as_bytes());
    let _ = s.flush();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shorthand_address_binds_all_interfaces() {
        let listener = listen(":0").unwrap();
        assert!(listener.local_addr().unwrap().ip().is_unspecified());
    }

    /// Same-named rows must be contiguous: `# HELP`/`# TYPE` is emitted on name change.
    #[test]
    fn table_is_grouped_by_name() {
        let mut seen: Vec<&str> = Vec::new();
        let mut last = "";
        for &(name, _, _, help) in TABLE {
            if name != last {
                assert!(!seen.contains(&name), "{name} appears in two places");
                seen.push(name);
                last = name;
            }
            assert!(
                name.starts_with("racer_"),
                "{name} is missing the namespace"
            );
            assert!(help.ends_with('.'), "{name} help should be a sentence");
        }
    }

    #[test]
    fn exposition_covers_every_row() {
        let text = encode();
        for &(name, labels, _, _) in TABLE {
            assert!(
                text.contains(&format!("# TYPE {name} ")),
                "no TYPE for {name}"
            );
            assert!(
                text.contains(&format!("\n{name}{labels} 0\n")),
                "no sample for {name}{labels}"
            );
        }
    }

    /// The control plane's only view of extent epoch readiness; the one dynamic summed row.
    #[test]
    fn extent_rows_sum_and_skip_unnamed_slots() {
        init(2);
        publish_extents(0, &[(1, 7, 3, 1), (2, 9, 0, 0)]);
        publish_extents(1, &[(1, 7, 4, 6), (2, 9, 0, 0)]);
        let text = encode();
        assert!(
            text.contains("\nracer_extent_live_pages{universe=\"1\",extent=\"7\"} 7\n"),
            "{text}"
        );
        assert!(
            text.contains("\nracer_extent_tombstones{universe=\"1\",extent=\"7\"} 7\n"),
            "{text}"
        );
        // Named but held by neither core, so it is present and contributes nothing.
        assert!(
            text.contains("\nracer_extent_live_pages{universe=\"2\",extent=\"9\"} 0\n"),
            "{text}"
        );
        // A slot no configuration has named is not a series at all.
        assert!(!text.contains("extent=\"0\""), "{text}");

        // Same extent id in two universes is two series: the label pair identifies a row.
        publish_extents(0, &[(1, 7, 1, 0), (5, 7, 2, 0)]);
        publish_extents(1, &[]);
        let text = encode();
        assert!(
            text.contains("\nracer_extent_live_pages{universe=\"5\",extent=\"7\"} 2\n"),
            "{text}"
        );
        // A dropped extent retires its series rather than freezing at its last value.
        assert!(!text.contains("universe=\"2\""), "{text}");
    }
}
