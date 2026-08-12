//! The prometheus endpoint.
//!
//! Counters live where the work happens: per-core state, plain `u64` adds, no atomics
//! and no shared lines. A worker cannot read another worker's state, so the flow is
//! inverted — each publishes its own row of `AtomicU64` from `Handler::tick` and the
//! scrape thread only sums rows. The HTTP thread never touches the runtime, so it can
//! neither block a worker nor deadlock at shutdown.
//!
//! One aggregation rule: **every slot sums**. A process-wide value is therefore written
//! by core 0 alone (server.rs), so its sum is the value itself.

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

/// The metric table: the fields a worker fills in and the exposition text they map to.
/// Rows sharing a name must stay adjacent, so the encoder can emit one `# HELP`/`# TYPE`
/// pair per metric while the label sets differ.
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

    // anti-entropy
    heal_sweeps:             "racer_heal_sweep_total"             ""                           Counter "Anti-entropy sweeps started.",
    heal_buckets_diff:       "racer_heal_buckets_diff_total"      ""                           Counter "Digest buckets found to differ from a peer.",
    heal_repairs:            "racer_heal_repair_total"            r#"{result="ok"}"#           Counter "Pages repaired by anti-entropy, by outcome.",
    heal_failed:             "racer_heal_repair_total"            r#"{result="failed"}"#       Counter "Pages repaired by anti-entropy, by outcome.",
    heal_oversized:          "racer_heal_oversized_total"         ""                           Counter "Sweeps cut short because the divergence was too large.",
    heal_dropped:            "racer_heal_dropped_total"           ""                           Counter "Registers given back after a group moved off this node.",
    heal_replaying:          "racer_heal_groups_replaying"        ""                           Gauge   "Groups this node is replaying into. Nonzero means a group is running two of three.",
    heal_shedding:           "racer_heal_groups_shedding"         ""                           Gauge   "Groups this node still holds registers for but is no longer a member of.",

    // cooperative cache
    cache_hits:              "racer_cache_lookup_total"           r#"{result="hit"}"#          Counter "Cache lookups, by outcome.",
    cache_misses:            "racer_cache_lookup_total"           r#"{result="miss"}"#         Counter "Cache lookups, by outcome.",
    cache_served:            "racer_cache_served_total"           ""                           Counter "Cache hits that then passed confirmation against the quorum.",
    cache_admits:            "racer_cache_admit_total"            ""                           Counter "Pages admitted to the cache.",
    cache_evictions:         "racer_cache_evict_total"            ""                           Counter "Pages evicted from the cache.",
    cache_stale:             "racer_cache_stale_total"            ""                           Counter "Cache hits that confirmation found stale.",
    cache_shed:              "racer_cache_shed_total"             ""                           Counter "Cache work declined because the store was under pressure.",

    // allocator
    alloc_slots_small:       "racer_alloc_slots"                  r#"{class="small"}"#         Gauge   "Page slots on this node, by slab class.",
    alloc_slots_huge:        "racer_alloc_slots"                  r#"{class="huge"}"#          Gauge   "Page slots on this node, by slab class.",
    alloc_free_small:        "racer_alloc_free_slots"             r#"{class="small"}"#         Gauge   "Unallocated page slots, by slab class.",
    alloc_free_huge:         "racer_alloc_free_slots"             r#"{class="huge"}"#          Gauge   "Unallocated page slots, by slab class.",
    alloc_pressure_low:      "racer_alloc_cores_pressured"        r#"{level="low"}"#           Gauge   "Workers whose shards are short of free slots, by watermark.",
    alloc_pressure_critical: "racer_alloc_cores_pressured"        r#"{level="critical"}"#      Gauge   "Workers whose shards are short of free slots, by watermark.",
    alloc_quarantined:       "racer_alloc_quarantined_blocks"     ""                           Gauge   "Metadata blocks that failed both copies at startup.",
    alloc_unbacked:          "racer_alloc_unbacked_pages"         ""                           Gauge   "Pages the configuration asks for that the store has no slots for. Nonzero until a restart grows the store.",

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

/// One worker's slots, padded so no two workers share a cacheline. A worker only ever
/// stores into its own row, so nothing is contended and no counter needs a `fetch_add`.
#[repr(align(64))]
struct Row([AtomicU64; N]);

static ROWS: OnceLock<Box<[Row]>> = OnceLock::new();

/// One row per worker in both tables. Called once, from the first configuration.
pub(crate) fn init(cores: usize) {
    let rows = (0..cores)
        .map(|_| Row(std::array::from_fn(|_| AtomicU64::new(0))))
        .collect();
    let _ = ROWS.set(rows);
    let exts = (0..cores)
        .map(|_| ExtRow(std::array::from_fn(|_| AtomicU64::new(0))))
        .collect();
    let _ = EXTS.set(exts);
}

/// Publish this worker's row. Relaxed: a scrape catching a row mid-update mixes two
/// adjacent ticks, indistinguishable from scraping a moment earlier and not worth a
/// fence per worker per millisecond.
pub(crate) fn publish(core: usize, s: &Sample) {
    let Some(row) = ROWS.get().and_then(|r| r.get(core)) else {
        return;
    };
    for (slot, v) in row.0.iter().zip(s.as_array()) {
        slot.store(v, Ordering::Relaxed);
    }
}

fn collect() -> [u64; N] {
    let mut out = [0u64; N];
    for row in ROWS.get().map(|r| &r[..]).unwrap_or(&[]) {
        for (o, slot) in out.iter_mut().zip(row.0.iter()) {
            *o = o.saturating_add(slot.load(Ordering::Relaxed));
        }
    }
    out
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

/// The per-extent series, beside the fixed table rather than in it because an extent
/// exists only once a configuration names it. Both sum across cores like everything
/// else.
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

/// The `(universe, extent)` each row stands for, `(0, 0)` when unused.
///
/// An extent id is assigned by the control plane and is not a small dense number, so the
/// row index is the extent's position in the configuration instead. Every core walks the
/// same configuration in the same order, so every core agrees on that position without
/// coordinating; these two do not sum, and only core 0 writes them.
static EXT_IDS: [(AtomicU64, AtomicU64); MAX_EXTENTS] =
    [const { (AtomicU64::new(0), AtomicU64::new(0)) }; MAX_EXTENTS];

#[repr(align(64))]
struct ExtRow([AtomicU64; MAX_EXTENTS * EXT_SERIES.len()]);

static EXTS: OnceLock<Box<[ExtRow]>> = OnceLock::new();

/// Publish this worker's per-extent counts: `(universe, extent, live, tombstones)`, in
/// configuration order. Rows past the end are zeroed, so an extent this core holds
/// nothing for stops contributing rather than sticking at its last value.
///
/// The names are core 0's alone. A core holding no page of an extent still lists it, but
/// a core that has not caught up to a reload would otherwise retire a series the others
/// are still filling.
pub(crate) fn publish_extents(core: usize, rows: &[(u32, u32, u64, u64)]) {
    let Some(row) = EXTS.get().and_then(|r| r.get(core)) else {
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
            EXT_IDS[s].0.store(universe as u64, Ordering::Relaxed);
            EXT_IDS[s].1.store(extent as u64, Ordering::Relaxed);
        }
        row.0[s].store(live, Ordering::Relaxed);
        row.0[MAX_EXTENTS + s].store(tombs, Ordering::Relaxed);
    }
    // A configuration that dropped an extent leaves its old row named but empty, which
    // would keep a stale series alive; clearing the names past the end retires it.
    if core == 0 {
        for slot in EXT_IDS.iter().skip(rows.len().min(MAX_EXTENTS)) {
            slot.0.store(0, Ordering::Relaxed);
            slot.1.store(0, Ordering::Relaxed);
        }
    }
}

fn encode_extents(out: &mut String) {
    for (i, (name, help)) in EXT_SERIES.iter().enumerate() {
        let _ = writeln!(out, "# HELP {name} {help}");
        let _ = writeln!(out, "# TYPE {name} gauge");
        for (slot, (universe, extent)) in EXT_IDS.iter().enumerate() {
            let extent = extent.load(Ordering::Relaxed);
            if extent == 0 {
                continue;
            }
            let universe = universe.load(Ordering::Relaxed);
            let mut v = 0u64;
            for row in EXTS.get().map(|r| &r[..]).unwrap_or(&[]) {
                v = v.saturating_add(row.0[i * MAX_EXTENTS + slot].load(Ordering::Relaxed));
            }
            let _ = writeln!(
                out,
                "{name}{{universe=\"{universe}\",extent=\"{extent}\"}} {v}"
            );
        }
    }
}

// ------------------------------------------------------------------------------- http

/// Deadline on each socket read or write of a scrape.
const TIMEOUT: Duration = Duration::from_secs(5);
/// A scrape is one short GET. Anything larger is not one.
const MAX_HEAD: usize = 8 << 10;

const EXPOSITION: &str = "text/plain; version=0.0.4; charset=utf-8";
const PLAIN: &str = "text/plain; charset=utf-8";

/// Bind the endpoint. Separate from [`serve`] so a bad address fails at startup, and so
/// early scrapes queue in the backlog rather than being refused before the dataplane is
/// up.
pub fn listen(addr: &str) -> std::io::Result<TcpListener> {
    match addr.strip_prefix(':') {
        Some(port) => TcpListener::bind(format!("0.0.0.0:{port}")),
        None => TcpListener::bind(addr),
    }
}

/// The `racer-metrics` thread. One connection at a time: a scrape is a sum over a few
/// hundred atomics per worker and prometheus opens a fresh connection each time, so
/// concurrency wins nothing.
pub fn serve(l: TcpListener) -> ! {
    loop {
        match l.accept() {
            Ok((s, _)) => handle(s),
            // Descriptor exhaustion is transient: back off rather than spin on it, and
            // never lose the endpoint.
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

    // Read the whole head before answering, so the peer is never writing into a socket
    // we have already closed.
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

    /// Every row must be reachable and same-named rows contiguous: the encoder emits
    /// `# HELP`/`# TYPE` only when the name changes.
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

    /// The per-extent block is the only view the control plane has of an extent's epoch
    /// readiness, and the one place a row is both dynamic and summed.
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

        // The same extent id in two universes is two series: the label pair is what
        // identifies a row, and partitioning reaches the metrics too.
        publish_extents(0, &[(1, 7, 1, 0), (5, 7, 2, 0)]);
        publish_extents(1, &[]);
        let text = encode();
        assert!(
            text.contains("\nracer_extent_live_pages{universe=\"5\",extent=\"7\"} 2\n"),
            "{text}"
        );
        // A configuration that dropped an extent retires its series rather than
        // freezing it at its last value.
        assert!(!text.contains("universe=\"2\""), "{text}");
    }
}
