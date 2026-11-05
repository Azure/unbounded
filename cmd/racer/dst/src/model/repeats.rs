//! Origin repeat classification: racer-sim's `classify_repeat` (`sim/src/node/world.rs`) over
//! Model acquisitions. Each origin request for a page that was fetched before is classified
//! against the page's previous request `A`: rank 0 moved, `A` failed, `A` still in flight
//! (concurrent), `A`'s copy is gone, or `A` still holds it (`copy_held`, with a cause). An owner
//! above rank 0 reaches origin only with evidence against rank 0; the last probe of rank 0
//! names why (`origin_skips_*`). A held repeat whose `A` was such a skip is that skip's orphan
//! (`origin_repeats_orphan_*`). A held repeat that is neither a skip, an orphan nor raced is
//! unexplained, a hard failure.

use std::collections::BTreeMap;

use serde_json::{Map, Value, json};

use super::Page;
use crate::engine::Nanos;

/// How one walk probe ended (racer-sim `Asked`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Asked {
    Copy,
    Miss,
    /// Transport failure or timeout, and why (`None`: timed out or failed otherwise).
    Unreachable(Option<Cut>),
    /// The destination answered `Unavailable` itself.
    Failed,
    /// A relay on the route answered a failure: the destination never saw the request.
    Relayed,
    /// Shed or behind.
    Busy,
    /// A terminal answer the walk does not continue from.
    Unfunded,
}

/// Why a walk probe failed in transport (racer-sim `Cut`). The Model has no member that does not
/// serve yet (`NotServing`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Cut {
    /// A partition or site cut on the way.
    Partition,
    /// A node on the way, or the destination, is down.
    Down,
    /// No route within the probe's links.
    Route,
    /// A link circuit was open.
    Circuit,
}

/// Skip causes with their `origin_skips_*` and `origin_repeats_orphan_*` counters, in
/// racer-sim's order (`SKIPS`); `copy` never orphans and is not reported.
pub const SKIPS: [(&str, &str, &str); 12] = [
    (
        "not_serving",
        "origin_skips_not_serving",
        "origin_repeats_orphan_not_serving",
    ),
    (
        "partition",
        "origin_skips_partition",
        "origin_repeats_orphan_partition",
    ),
    ("down", "origin_skips_down", "origin_repeats_orphan_down"),
    ("route", "origin_skips_route", "origin_repeats_orphan_route"),
    (
        "circuit",
        "origin_skips_circuit",
        "origin_repeats_orphan_circuit",
    ),
    (
        "timeout",
        "origin_skips_timeout",
        "origin_repeats_orphan_timeout",
    ),
    (
        "failed",
        "origin_skips_failed",
        "origin_repeats_orphan_failed",
    ),
    ("relay", "origin_skips_relay", "origin_repeats_orphan_relay"),
    ("busy", "origin_skips_busy", "origin_repeats_orphan_busy"),
    (
        "copy_only",
        "origin_skips_copy_only",
        "origin_repeats_orphan_copy_only",
    ),
    (
        "unfunded",
        "origin_skips_unfunded",
        "origin_repeats_orphan_unfunded",
    ),
    ("copy", "origin_skips_copy", "origin_repeats_orphan_copy"),
];

fn skip_counters(skip: &str) -> (&'static str, &'static str) {
    let (_, s, o) = SKIPS.iter().find(|e| e.0 == skip).unwrap_or(&SKIPS[10]);
    (s, o)
}

/// Why rank 0 did not serve an owner above it: its last probe of rank 0.
pub fn skip_of(last: Option<Asked>) -> &'static str {
    match last {
        Some(Asked::Unreachable(Some(cut))) => match cut {
            Cut::Partition => "partition",
            Cut::Down => "down",
            Cut::Route => "route",
            Cut::Circuit => "circuit",
        },
        Some(Asked::Unreachable(None)) => "timeout",
        Some(Asked::Failed) => "failed",
        Some(Asked::Relayed) => "relay",
        Some(Asked::Busy) => "busy",
        Some(Asked::Miss) => "copy_only",
        Some(Asked::Copy) => "copy",
        Some(Asked::Unfunded) | None => "unfunded",
    }
}

/// The last origin request for a page.
#[derive(Clone, Copy, Debug)]
struct LastOrigin {
    node: u32,
    rank0: u32,
    ok: bool,
    /// When it completes; in flight before.
    done: Nanos,
    skip: Option<&'static str>,
}

/// One origin request about to be issued.
pub struct Request<'a> {
    pub now: Nanos,
    pub node: u32,
    pub page: Page,
    /// The walked ranking's rank 0 and this node's rank in it.
    pub rank0: u32,
    pub rank: Option<usize>,
    /// When the acquisition started, and its probes so far.
    pub started: Nanos,
    pub asked: &'a [(u32, Asked)],
    /// When the request completes and whether it succeeds.
    pub done: Nanos,
    pub ok: bool,
}

/// Repeat counters and the last origin request per page.
#[derive(Debug, Default)]
pub struct Repeats {
    pub counters: BTreeMap<&'static str, u64>,
    last: BTreeMap<Page, LastOrigin>,
    failure_retries: BTreeMap<Page, u64>,
}

impl Repeats {
    fn incr(&mut self, name: &'static str) {
        *self.counters.entry(name).or_default() += 1;
    }

    fn get(&self, name: &str) -> u64 {
        self.counters.get(name).copied().unwrap_or(0)
    }

    /// Classify `r`; `held(node)` says whether `node` still holds the page. Returns an
    /// explanation when the repeat is unexplained (a hard failure).
    pub fn origin(&mut self, r: &Request<'_>, held: impl Fn(u32) -> bool) -> Option<String> {
        let skip = r.rank.is_some_and(|rank| rank > 0).then(|| {
            skip_of(
                r.asked
                    .iter()
                    .rev()
                    .find(|(dest, _)| *dest == r.rank0)
                    .map(|(_, a)| *a),
            )
        });
        if let Some(skip) = skip {
            self.incr(skip_counters(skip).0);
        }
        let next = LastOrigin {
            node: r.node,
            rank0: r.rank0,
            ok: r.ok,
            done: r.done,
            skip,
        };
        let last = self.last.insert(r.page, next)?;
        let mut unexplained = None;
        let class = if last.rank0 != r.rank0 {
            "origin_repeats_after_move"
        } else if last.done > r.now {
            "origin_repeats_concurrent"
        } else if !last.ok {
            "origin_repeats_after_failure"
        } else if !held(last.node) {
            "origin_repeats_copy_lost"
        } else {
            let asked = |kind: fn(Asked) -> bool| {
                r.asked
                    .iter()
                    .any(|&(dest, a)| dest == last.node && kind(a))
            };
            let cause = if last.done > r.started {
                "origin_repeats_held_raced"
            } else if asked(|a| matches!(a, Asked::Unreachable(_) | Asked::Failed | Asked::Relayed))
            {
                "origin_repeats_held_unreachable"
            } else if asked(|a| a == Asked::Busy) {
                "origin_repeats_held_busy"
            } else if asked(|a| a == Asked::Unfunded) {
                "origin_repeats_held_unfunded"
            } else if asked(|a| matches!(a, Asked::Miss | Asked::Copy)) {
                "origin_repeats_held_missed"
            } else {
                "origin_repeats_held_unasked"
            };
            self.incr(cause);
            if let Some(orphan) = last.skip {
                self.incr(skip_counters(orphan).1);
            }
            let explained =
                skip.is_some() || last.skip.is_some() || cause == "origin_repeats_held_raced";
            if !explained {
                self.incr("origin_repeats_held_unexplained");
                unexplained = Some(format!(
                    "page {:?} fetched by {} while {} still holds it ({cause})",
                    r.page, r.node, last.node
                ));
            }
            "origin_repeats_copy_held"
        };
        if class == "origin_repeats_after_failure" {
            *self.failure_retries.entry(r.page).or_default() += 1;
        }
        self.incr(class);
        unexplained
    }

    /// racer-sim's `origin_per_page` repeat fields, from `per_page` origin requests by page.
    pub fn json(&self, per_page: &BTreeMap<Page, u64>) -> Map<String, Value> {
        let c = |k: &str| self.get(k);
        let retries = |page: &Page| self.failure_retries.get(page).copied().unwrap_or(0);
        let without = per_page
            .iter()
            .map(|(p, n)| n - retries(p))
            .max()
            .unwrap_or(0);
        let most_retries = self.failure_retries.values().copied().max().unwrap_or(0);
        let mut o = Map::new();
        o.insert("requests".into(), json!(per_page.values().sum::<u64>()));
        o.insert(
            "max_requests_per_page_excluding_failure_retries".into(),
            json!(without),
        );
        o.insert("max_failure_retries_per_page".into(), json!(most_retries));
        o.insert(
            "repeats".into(),
            json!({
                "after_owner_move": c("origin_repeats_after_move"),
                "copy_lost": c("origin_repeats_copy_lost"),
                "after_failure": c("origin_repeats_after_failure"),
                "concurrent": c("origin_repeats_concurrent"),
                "copy_held": c("origin_repeats_copy_held"),
                "held": {
                    "raced": c("origin_repeats_held_raced"),
                    "unreachable": c("origin_repeats_held_unreachable"),
                    "busy": c("origin_repeats_held_busy"),
                    "unfunded": c("origin_repeats_held_unfunded"),
                    "missed": c("origin_repeats_held_missed"),
                    "unasked": c("origin_repeats_held_unasked"),
                },
                "unexplained": c("origin_repeats_held_unexplained"),
                "orphan": SKIPS[..11]
                    .iter()
                    .map(|(k, _, o)| (k.to_string(), json!(c(o))))
                    .collect::<Map<_, _>>(),
            }),
        );
        o.insert(
            "skips".into(),
            SKIPS[..11]
                .iter()
                .map(|(k, s, _)| (k.to_string(), json!(c(s))))
                .collect::<Map<_, _>>()
                .into(),
        );
        o
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PAGE: Page = Page {
        object: 0,
        number: 7,
    };

    fn request(now: Nanos, node: u32, rank: usize, asked: &[(u32, Asked)]) -> Request<'_> {
        Request {
            now,
            node,
            page: PAGE,
            rank0: 0,
            rank: Some(rank),
            started: now,
            asked,
            done: now + 10,
            ok: true,
        }
    }

    #[test]
    fn a_skip_orphans_the_rank_zero_refetch() {
        let mut r = Repeats::default();
        let cut = [(0, Asked::Unreachable(Some(Cut::Partition)))];
        assert_eq!(r.origin(&request(0, 1, 1, &cut), |_| true), None);
        assert_eq!(r.get("origin_skips_partition"), 1);
        // Rank 0 refetches while rank 1 holds the page: the skip's orphan.
        assert_eq!(r.origin(&request(100, 0, 0, &[]), |_| true), None);
        assert_eq!(r.get("origin_repeats_copy_held"), 1);
        assert_eq!(r.get("origin_repeats_orphan_partition"), 1);
        assert_eq!(r.get("origin_repeats_held_unasked"), 1);
    }

    #[test]
    fn repeats_are_classified_against_the_last_request() {
        let mut r = Repeats::default();
        r.origin(&request(0, 0, 0, &[]), |_| true);
        // Still in flight.
        r.origin(&request(5, 0, 0, &[]), |_| true);
        assert_eq!(r.get("origin_repeats_concurrent"), 1);
        // Failed; itself a repeat of a held copy nobody asked for.
        let mut failed = request(100, 0, 0, &[]);
        failed.ok = false;
        assert!(r.origin(&failed, |_| true).is_some());
        assert_eq!(r.get("origin_repeats_held_unasked"), 1);
        r.origin(&request(200, 0, 0, &[]), |_| true);
        assert_eq!(r.get("origin_repeats_after_failure"), 1);
        // The copy is gone.
        r.origin(&request(300, 0, 0, &[]), |_| false);
        assert_eq!(r.get("origin_repeats_copy_lost"), 1);
        // Held, asked it, it missed, and nobody skipped: unexplained.
        let missed = [(0, Asked::Miss)];
        let why = r.origin(&request(400, 0, 0, &missed), |_| true);
        assert!(why.is_some());
        assert_eq!(r.get("origin_repeats_held_missed"), 1);
        assert_eq!(r.get("origin_repeats_held_unexplained"), 2);
        // Rank 0 moved.
        let mut moved = request(500, 3, 0, &[]);
        moved.rank0 = 3;
        r.origin(&moved, |_| true);
        assert_eq!(r.get("origin_repeats_after_move"), 1);
        let per_page: BTreeMap<Page, u64> = [(PAGE, 7)].into();
        let o = r.json(&per_page);
        assert_eq!(o["requests"], 7);
        assert_eq!(o["max_failure_retries_per_page"], 1);
        assert_eq!(o["max_requests_per_page_excluding_failure_retries"], 6);
        assert_eq!(o["repeats"]["unexplained"], 2);
    }
}
