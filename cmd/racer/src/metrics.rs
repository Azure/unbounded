//! The prometheus endpoint.
//!
//! Counters are per-core plain `u64` adds, no atomics. Each worker publishes its own row
//! of `AtomicU64` from `Handler::tick`; the scrape thread only sums rows and never touches
//! the runtime, so it cannot block a worker or deadlock at shutdown. Every slot sums, so
//! core 0 alone writes process-wide values (server.rs).

use std::fmt::Write as _;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::Mutex;
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

/// One worker's counters, filled in each tick (server.rs).
///
/// The subsystems already keep exactly these counters per core, so a tick hands over the
/// structs it has rather than copying them field by field. [`NodeStats`] carries what no
/// subsystem owns: values derived at sample time, and the node-wide ones core 0 alone
/// reports.
#[derive(Clone, Copy, Default)]
pub(crate) struct Sample {
    pub(crate) paxos: crate::paxos::Stats,
    pub(crate) heal: crate::paxos::heal::Stats,
    pub(crate) cache: crate::cache::Stats,
    pub(crate) alloc: crate::alloc::Stats,
    pub(crate) node: NodeStats,
}

/// The counters no subsystem keeps. Arrays are indexed by page class, small then huge, to
/// match the `per` arrays of the subsystem stats.
#[derive(Clone, Copy, Default)]
pub(crate) struct NodeStats {
    pub(crate) heal_replaying: u64,
    pub(crate) heal_shedding: u64,
    pub(crate) cache_tail_bytes: u64,
    pub(crate) cache_unused_bytes: u64,
    pub(crate) slots: [u64; 2],
    pub(crate) free: [u64; 2],
    /// Cores voting low, then cores voting critical.
    pub(crate) pressure: [u64; 2],
    pub(crate) quarantined: u64,
    pub(crate) unbacked: u64,
    pub(crate) store_throttle_us: u64,
    pub(crate) config_generation: u64,
    pub(crate) config_rejected: u64,
    pub(crate) broadcast_stalls: u64,
    pub(crate) broadcast_wait_us: u64,
    pub(crate) topology_epoch: u64,
    pub(crate) node_id: u64,
    pub(crate) workers: u64,
    pub(crate) universes: u64,
    pub(crate) devices: u64,
    pub(crate) extents: u64,
    pub(crate) peers: u64,
}

/// The metric table. Rows sharing a name must stay adjacent, so the encoder emits one
/// `# HELP`/`# TYPE` pair per metric while the label sets differ.
///
/// The first column is the path into [`Sample`] the row reads. A metric naming its own
/// source is what keeps the sampler short: there is no second list to keep in step.
macro_rules! metrics {
    ($(($($path:tt)+) $name:literal $labels:literal $kind:ident $help:literal,)*) => {
        const TABLE: &[(&str, &str, Kind, &str)] = &[$(($name, $labels, Kind::$kind, $help),)*];

        impl Sample {
            fn as_array(&self) -> [u64; N] {
                [$(self.$($path)+,)*]
            }
        }
    };
}

metrics! {
    // consensus
    (paxos.accept_ok)              "racer_paxos_accept_total"           r#"{result="ok"}"#           Counter "Guarded accepts, by outcome.",
    (paxos.accept_rejected)        "racer_paxos_accept_total"           r#"{result="rejected"}"#     Counter "Guarded accepts, by outcome.",
    (paxos.one_shot)               "racer_paxos_one_shot_total"         ""                           Counter "Writes that committed without a prepare phase.",
    (paxos.guard_conflicts)        "racer_paxos_guard_conflicts_total"  ""                           Counter "Accepts refused because the guard did not match.",
    (paxos.prepares)               "racer_paxos_prepare_total"          ""                           Counter "Prepare phases run after a one-shot accept failed.",
    (paxos.term_bumps)             "racer_paxos_term_bumps_total"       ""                           Counter "Term increments.",
    (paxos.repairs)                "racer_paxos_repair_total"           ""                           Counter "Pages rewritten to a lagging replica.",
    (paxos.read_matched)           "racer_paxos_read_total"             r#"{result="matched"}"#      Counter "Quorum reads, by outcome.",
    (paxos.read_remote_match)      "racer_paxos_read_total"             r#"{result="remote_match"}"# Counter "Quorum reads, by outcome.",
    (paxos.read_failed)            "racer_paxos_read_total"             r#"{result="failed"}"#       Counter "Quorum reads, by outcome.",
    (paxos.learn_stale)            "racer_paxos_learn_stale_total"      ""                           Counter "Learn frames dropped as older than the local value.",
    (paxos.seals)                  "racer_paxos_seal_total"             ""                           Counter "Shards sealed for handover.",
    (paxos.groups_unavailable)     "racer_paxos_groups_unavailable"     ""                           Counter "Rounds abandoned because no quorum was reachable.",
    (paxos.gateway_retries)        "racer_gateway_fallback_total"       r#"{reason="retry"}"#        Counter "Cross-zone operations that fell through the gateway ring, by outcome. A retry went on to an answer; unavailable means no gateway of the zone answered.",
    (paxos.zones_unavailable)      "racer_gateway_fallback_total"       r#"{reason="unavailable"}"#  Counter "Cross-zone operations that fell through the gateway ring, by outcome.",
    (paxos.warms_sent)             "racer_warm_total"                   r#"{result="sent"}"#         Counter "Cross-zone cache warming frames, by outcome. Sent counts what a commit fanned out, taken what a receiver acted on, dropped what it declined.",
    (paxos.warms_taken)            "racer_warm_total"                   r#"{result="taken"}"#        Counter "Cross-zone cache warming frames, by outcome.",
    (paxos.warms_dropped)          "racer_warm_total"                   r#"{result="dropped"}"#      Counter "Cross-zone cache warming frames, by outcome.",

    // anti-entropy
    (heal.sweeps)                  "racer_heal_sweep_total"             ""                           Counter "Anti-entropy sweeps started.",
    (heal.buckets_diff)            "racer_heal_buckets_diff_total"      ""                           Counter "Digest buckets found to differ from a peer.",
    (heal.repairs)                 "racer_heal_repair_total"            r#"{result="ok"}"#           Counter "Pages repaired by anti-entropy, by outcome.",
    (heal.failed)                  "racer_heal_repair_total"            r#"{result="failed"}"#       Counter "Pages repaired by anti-entropy, by outcome.",
    (heal.oversized)               "racer_heal_oversized_total"         ""                           Counter "Sweeps cut short because the divergence was too large.",
    (heal.dropped)                 "racer_heal_dropped_total"           ""                           Counter "Registers given back after a group moved off this node.",
    (heal.stalled)                 "racer_heal_stalled_total"           ""                           Counter "Anti-entropy sweeps abandoned for running too long. Nonzero means maintenance is waiting on something that never answers.",
    (node.heal_replaying)          "racer_heal_groups_replaying"        ""                           Gauge   "Groups this node is replaying into. Nonzero means a group is running two of three.",
    (node.heal_shedding)           "racer_heal_groups_shedding"         ""                           Gauge   "Groups this node still holds registers for but is no longer a member of.",

    // cooperative cache. One class: only immutable blocks are cacheable, and every entry
    // is one 4 KiB block.
    (cache.hits)                   "racer_cache_lookup_total"           r#"{result="hit"}"#          Counter "Cache lookups, by outcome.",
    (cache.misses)                 "racer_cache_lookup_total"           r#"{result="miss"}"#         Counter "Cache lookups, by outcome.",
    (cache.served)                 "racer_cache_served_total"           ""                           Counter "Cache hits that then passed confirmation against the quorum.",
    (cache.admits)                 "racer_cache_admit_total"            ""                           Counter "Blocks admitted to the cache.",
    (cache.evictions)              "racer_cache_evict_total"            ""                           Counter "Blocks evicted from the cache to make room for a hotter one.",
    (cache.dropped)                "racer_cache_dropped_total"          ""                           Counter "Blocks lost because their chunk went back to the free pool.",
    (cache.stale)                  "racer_cache_stale_total"            ""                           Counter "Cache hits that confirmation found stale.",
    (cache.shed)                   "racer_cache_shed_total"             ""                           Counter "Cache work declined because the store was under pressure.",
    (cache.rejected_policy)        "racer_cache_reject_total"           r#"{reason="policy"}"#       Counter "Admissions refused, by reason: policy is a mutable extent or the extent's cache_admit, victim is a hotter incumbent. The policy count lands on the group member that computes the width, not on the node that would have cached.",
    (cache.rejected_victim)        "racer_cache_reject_total"           r#"{reason="victim"}"#       Counter "Admissions refused, by reason: policy is a mutable extent or the extent's cache_admit, victim is a hotter incumbent.",
    (cache.bytes)                  "racer_cache_bytes"                  ""                           Gauge   "Media the cache holds.",
    (node.cache_tail_bytes)        "racer_cache_tail_bytes"             ""                           Gauge   "Store bytes past the end of the layout, the space the cache is carved from.",
    (node.cache_unused_bytes)      "racer_cache_unused_bytes"           ""                           Gauge   "Tail the cache is not holding because policy.cache_index_bytes will not pay to index it.",

    // allocator
    (node.slots[0])                "racer_alloc_slots"                  r#"{class="mutable"}"#         Gauge   "Page slots on this node, by slab class.",
    (node.slots[1])                "racer_alloc_slots"                  r#"{class="immutable"}"#          Gauge   "Page slots on this node, by slab class.",
    (node.free[0])                 "racer_alloc_free_slots"             r#"{class="mutable"}"#         Gauge   "Unallocated page slots, by slab class.",
    (node.free[1])                 "racer_alloc_free_slots"             r#"{class="immutable"}"#          Gauge   "Unallocated page slots, by slab class.",
    (node.pressure[0])             "racer_alloc_cores_pressured"        r#"{level="low"}"#           Gauge   "Workers whose shards are short of free slots, by watermark.",
    (node.pressure[1])             "racer_alloc_cores_pressured"        r#"{level="critical"}"#      Gauge   "Workers whose shards are short of free slots, by watermark.",
    (node.quarantined)             "racer_alloc_quarantined_blocks"     ""                           Gauge   "Metadata blocks that failed both copies at startup.",
    (node.unbacked)                "racer_alloc_unbacked_blocks"         ""                           Gauge   "Pages the configuration asks for that the store has no slots for. Nonzero until a restart grows the store.",
    (alloc.per[0].commits)         "racer_mblock_commits_total"         r#"{class="mutable"}"#         Counter "Metadata mutations staged, by slab class.",
    (alloc.per[1].commits)         "racer_mblock_commits_total"         r#"{class="immutable"}"#          Counter "Metadata mutations staged, by slab class.",
    (alloc.per[0].flushes)         "racer_mblock_flushes_total"         r#"{class="mutable"}"#         Counter "Metadata block writes issued, by slab class.",
    (alloc.per[1].flushes)         "racer_mblock_flushes_total"         r#"{class="immutable"}"#          Counter "Metadata block writes issued, by slab class.",
    (alloc.per[0].flush_batch)     "racer_mblock_flush_batch_total"     r#"{class="mutable"}"#         Counter "Metadata mutations covered by block writes, by slab class.",
    (alloc.per[1].flush_batch)     "racer_mblock_flush_batch_total"     r#"{class="immutable"}"#          Counter "Metadata mutations covered by block writes, by slab class.",
    (alloc.per[0].parks)           "racer_commit_park_total"            r#"{class="mutable"}"#         Counter "Commit waits behind an in-flight metadata block write, by slab class.",
    (alloc.per[1].parks)           "racer_commit_park_total"            r#"{class="immutable"}"#          Counter "Commit waits behind an in-flight metadata block write, by slab class.",
    (alloc.per[0].busy_us)         "racer_flush_busy_us_total"          r#"{class="mutable"}"#         Counter "Aggregate microseconds metadata block writes were in flight, by slab class.",
    (alloc.per[1].busy_us)         "racer_flush_busy_us_total"          r#"{class="immutable"}"#          Counter "Aggregate microseconds metadata block writes were in flight, by slab class.",
    (alloc.per[0].swept_epoch)     "racer_sweep_reclaimed_total"        r#"{class="small",reason="epoch"}"#     Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    (alloc.per[1].swept_epoch)     "racer_sweep_reclaimed_total"        r#"{class="huge",reason="epoch"}"#      Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    (alloc.per[0].swept_uncovered) "racer_sweep_reclaimed_total"        r#"{class="small",reason="uncovered"}"# Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",
    (alloc.per[1].swept_uncovered) "racer_sweep_reclaimed_total"        r#"{class="huge",reason="uncovered"}"#  Counter "Registers the sweep destroyed, by slab class and reason. Epoch means the extent's tombstone epoch moved past the row, uncovered means the configuration stopped naming an extent for its address. Both are asked for by the control plane and neither takes a consensus round, so a mistaken configuration empties an extent here and nowhere else.",

    // store rate budget
    (node.store_throttle_us)       "racer_store_throttle_us_total"      ""                           Counter "Time transfers were held back to keep the store within its configured rate.",

    // node and configuration
    (node.config_generation)       "racer_config_generation"            ""                           Gauge   "Generation of the configuration in force.",
    (node.config_rejected)         "racer_config_rejected_total"        ""                           Counter "Configurations rejected since start.",
    (node.broadcast_stalls)        "racer_control_broadcast_stalled_total" ""                        Counter "Control-thread broadcasts that ran past their warning deadline. Nonzero means reconfiguration stalled waiting on a worker.",
    (node.broadcast_wait_us)       "racer_control_broadcast_wait_us"    ""                           Gauge   "Microseconds the control thread has been waiting in its current broadcast; 0 when it is not waiting.",
    (node.topology_epoch)          "racer_topology_epoch"               ""                           Gauge   "Highest topology epoch in force across this node's universes.",
    (node.node_id)                 "racer_node_id"                      ""                           Gauge   "This node's id.",
    (node.workers)                 "racer_workers"                      ""                           Gauge   "Worker threads, one per physical core.",
    (node.universes)               "racer_universes"                    ""                           Gauge   "Universes this node participates in.",
    (node.devices)                 "racer_devices"                      ""                           Gauge   "Block devices this node exports.",
    (node.extents)                 "racer_extents"                      ""                           Gauge   "Extents this node's configuration names.",
    (node.peers)                   "racer_peers"                        ""                           Gauge   "Peers this node holds a fabric link to.",
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

/// This node's tables.
///
/// One process is one node on real hardware, and the first entry is the only one ever
/// asked for. Under simulation a process is a cluster, so there is a set per node: shared
/// tables would sum unrelated nodes and contend on one line. They are leaked rather than
/// owned because a `Row` is read by every worker of a node and outlives any one of them,
/// and there are as many as the cluster has nodes.
fn tables<R>(f: impl FnOnce(&Tables) -> R) -> R {
    static NODES: Mutex<Vec<&'static Tables>> = Mutex::new(Vec::new());
    let node = crate::kernel::node();
    let t = {
        let mut nodes = NODES.lock().expect("metrics tables");
        while nodes.len() <= node {
            nodes.push(Box::leak(Box::new(Tables::new())));
        }
        nodes[node]
    };
    f(t)
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
        "racer_extent_live_blocks",
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

/// One connection, served start to finish. One at a time; prometheus reconnects per scrape.
///
/// Split out of [`serve`] so a caller that owns the loop can take a turn at a time: a
/// scrape is a whole unit of work, and this is where it begins and ends.
pub fn serve_turn(l: &TcpListener) {
    match l.accept() {
        Ok((s, _)) => handle(s),
        // Descriptor exhaustion is transient: back off, never leave the loop.
        Err(e) if e.kind() != std::io::ErrorKind::Interrupted => {
            crate::kernel::sleep_blocking(Duration::from_millis(100));
        }
        Err(_) => {}
    }
}

/// The `racer-metrics` thread.
pub fn serve(l: TcpListener) -> ! {
    loop {
        serve_turn(&l);
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
