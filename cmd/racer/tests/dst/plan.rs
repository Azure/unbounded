//! What to try next.
//!
//! Two kinds of choice live here. Configuration is stratified: the campaign
//! walks a table of cluster shapes so that split transfers, second zones,
//! warming, rate limits and non member ingress are reached by construction
//! rather than by luck. Everything after that is random, nudged towards the
//! obligations this seed has not met yet.

use std::time::Duration;

use racer::sim::{Faults, Options};

use crate::coverage::Reach;
use crate::model;
use crate::world::{self, World};

/// A deterministic stream of choices. Small on purpose: the seed is the whole
/// reproduction, so the generator must not depend on anything else.
pub struct Rng(u64);

impl Rng {
    /// A stream nobody else is using.
    pub fn new(seed: u64) -> Self {
        Self(seed ^ 0x5eed_face_cafe_d00d)
    }

    /// The next raw draw.
    pub fn next(&mut self) -> u64 {
        let mut x = self.0;

        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.0 = x;

        x
    }

    /// A draw below `n`.
    pub fn below(&mut self, n: u64) -> u64 {
        self.next() % n.max(1)
    }

    /// A draw in `lo..=hi`.
    pub fn between(&mut self, lo: u64, hi: u64) -> u64 {
        lo + self.below(hi - lo + 1)
    }

    /// True `n` times in a thousand.
    pub fn chance(&mut self, n: u64) -> bool {
        self.below(1000) < n
    }

    /// One of `xs`.
    pub fn pick<T: Copy>(&mut self, xs: &[T]) -> T {
        xs[self.below(xs.len() as u64) as usize]
    }
}

/// A cluster shape, and how hard to work it.
#[derive(Clone)]
pub struct Profile {
    /// Which row of the table this is.
    pub stratum: usize,
    /// What the row is for, so a failure says which shape broke.
    pub name: &'static str,
    /// The cluster to build.
    pub opts: Options,
    /// How many small pages the workload touches.
    pub small: u64,
    /// How many immutable pages the workload touches.
    pub huge: u64,
    /// How many actions to take.
    pub steps: usize,
}

/// How many cluster shapes the campaign sweeps.
pub const STRATA: usize = 8;

/// The shape seed `n` runs, and the workload to run on it.
///
/// Rare dimensions are placed rather than drawn. A coin flip that comes up
/// tails for every seed in a run is how coverage disappears quietly.
pub fn profile(seed: u64) -> Profile {
    let stratum = (seed as usize).wrapping_sub(1) % STRATA;
    let base = Options {
        seed,
        cores: 1,
        pages: 512,
        clique: true,
        ..Options::default()
    };

    let (name, opts, small, huge, steps) = match stratum {
        0 => (
            "three nodes, small pages only",
            Options { nodes: 3, ..base },
            24,
            0,
            420,
        ),
        1 => (
            "five nodes, so some writes arrive at a non member",
            Options {
                nodes: 5,
                huge_pages: 8,
                cache_admit: 1,
                ..base
            },
            20,
            4,
            300,
        ),
        2 => (
            "transfers split below the fabric's limit",
            Options {
                nodes: 3,
                cores: 2,
                huge_pages: 8,
                mdts: 128 << 10,
                ..base
            },
            20,
            4,
            260,
        ),
        3 => (
            "a store held to a rate budget",
            Options {
                nodes: 3,
                huge_pages: 4,
                device_iops: 128,
                ..base
            },
            16,
            2,
            240,
        ),
        4 => (
            "two zones, with the far one warmed",
            Options {
                nodes: 6,
                zones: 2,
                huge_pages: 8,
                cache_admit: 1,
                warm: true,
                ..base
            },
            20,
            4,
            300,
        ),
        5 => (
            "two zones, reading cold across the fabric",
            Options {
                nodes: 6,
                zones: 2,
                huge_pages: 4,
                ..base
            },
            20,
            3,
            300,
        ),
        6 => (
            "six nodes that only talk to the peers they need",
            Options {
                nodes: 6,
                clique: false,
                huge_pages: 4,
                cache_admit: 2,
                ..base
            },
            24,
            3,
            300,
        ),
        _ => (
            "two zones, warmed, with transfers split as well",
            Options {
                nodes: 6,
                cores: 2,
                zones: 2,
                huge_pages: 8,
                cache_admit: 1,
                warm: true,
                mdts: 64 << 10,
                ..base
            },
            16,
            4,
            240,
        ),
    };

    // The extent is sized from the workload rather than by hand, so that the
    // pages held back for the reclamation invariant are always there.
    let opts = Options {
        huge_pages: if huge > 0 { huge + world::SPARE } else { 0 },
        ..opts
    };

    Profile {
        stratum,
        name,
        opts,
        small,
        huge,
        steps,
    }
}

/// Everything the campaign can do next.
#[derive(Clone)]
pub enum Action {
    /// Put a fresh value in a small page.
    Write { node: usize, page: u64 },
    /// Put the hole back.
    Trim { node: usize, page: u64 },
    /// Ask a small page what it holds.
    Read { node: usize, page: u64 },
    /// Fill an immutable page.
    Fill { node: usize, page: u64 },
    /// Read an immutable page whole.
    ReadHuge { node: usize, page: u64 },
    /// Read `len` bytes at byte offset `at` of an immutable page. The class is written
    /// whole but may be read in pieces, and a node outside the page's group has to fetch
    /// the piece rather than answer from a slab that was never going to hold it.
    ReadHugePiece {
        node: usize,
        page: u64,
        at: usize,
        len: usize,
    },
    /// Let the cluster run.
    Advance(Duration),
    /// Take a node away without warning.
    Crash(usize),
    /// Bring one back.
    Restart(usize),
    /// Stop one node hearing another.
    Cut(usize, usize),
    /// Let it hear again.
    Heal(usize, usize),
    /// Make one direction crawl.
    Slow(usize, usize),
    /// Change the weather: drops, disk errors, corruption, jitter.
    Weather(Faults),
    /// Damage a replica's persisted bytes behind the cluster's back.
    Damage(u64, usize),
}

/// How much breakage the campaign is willing to hold at once. Under a calm sky
/// a group keeps its quorum, so the workload has to make progress; under a
/// hostile one it does not, and only safety is owed.
pub fn budget(w: &World) -> usize {
    if w.hostile { 3 } else { 1 }
}

/// What to do next.
pub fn choose(w: &mut World) -> Action {
    // Nothing can complete without time passing, and a pile of requests that
    // never resolves teaches nothing.
    if w.inflight() >= 24 || w.rng.chance(340) {
        return Action::Advance(Duration::from_micros(step(w)));
    }

    // Steer towards whatever this seed has not managed to reach yet.
    if w.rng.chance(220)
        && let Some(a) = nudge(w)
    {
        return a;
    }

    if w.rng.chance(120) {
        return weather(w);
    }

    if w.rng.chance(90)
        && let Some(a) = harm(w)
    {
        return a;
    }

    work(w)
}

/// How long to let the cluster run. Spread over three orders of magnitude so
/// the campaign visits the inside of a round trip, the edge of a timeout, and
/// the anti entropy period.
fn step(w: &mut World) -> u64 {
    match w.rng.below(100) {
        0..50 => w.rng.between(20, 400),
        50..85 => w.rng.between(400, 20_000),
        85..98 => w.rng.between(20_000, 400_000),
        _ => w.rng.between(400_000, 2_500_000),
    }
}

/// A client request, on some node, for some page.
fn work(w: &mut World) -> Action {
    let node = client(w);

    if w.profile.huge > 0 && w.rng.chance(220) {
        let page = w.rng.below(w.profile.huge);

        // A page still busy would push the segment the checker works on past
        // what it will take on.
        if w.huge_ready(page) {
            return if w.rng.chance(450) {
                Action::Fill { node, page }
            } else if w.rng.chance(400) {
                piece(w, node, page)
            } else {
                Action::ReadHuge { node, page }
            };
        }
    }

    let page = w.rng.below(w.profile.small);

    if !w.small_ready(page) {
        return Action::Advance(Duration::from_micros(step(w)));
    }

    match w.rng.below(100) {
        0..45 => Action::Write { node, page },
        45..90 => Action::Read { node, page },
        _ => Action::Trim { node, page },
    }
}

/// Part of an immutable page: a block-aligned range somewhere inside it, weighted
/// towards the short reads a filesystem actually issues. A piece is the shape a node
/// outside the page's group cannot answer from its own slab, so it has to be asked for
/// as often as the whole page is.
fn piece(w: &mut World, node: usize, page: u64) -> Action {
    /// How many 4 KiB blocks an immutable page is.
    const BLOCKS: u64 = (model::HUGE / model::BLOCK) as u64;

    let blocks = match w.rng.below(100) {
        0..60 => 1,
        60..85 => w.rng.between(2, 8),
        _ => w.rng.between(9, 128),
    };
    let at = w.rng.below(BLOCKS - blocks + 1);

    Action::ReadHugePiece {
        node,
        page,
        at: at as usize * model::BLOCK,
        len: blocks as usize * model::BLOCK,
    }
}

/// Which node to ask. Every up node is fair game, member or not, home zone or
/// not: which one it is decides the path the request takes.
fn client(w: &mut World) -> usize {
    let up: Vec<usize> = (0..w.sim.nodes()).filter(|&i| w.sim.up(i)).collect();

    if up.is_empty() {
        return 0;
    }

    w.rng.pick(&up)
}

/// Weather over the whole fabric.
fn weather(w: &mut World) -> Action {
    let (drop, io, corrupt) = match w.rng.below(100) {
        0..35 => (0, 0, 0),
        35..70 => (w.rng.below(60), w.rng.below(30), w.rng.below(20)),
        70..90 => (w.rng.between(60, 250), w.rng.below(80), w.rng.below(60)),
        _ => (w.rng.between(250, 600), w.rng.below(200), w.rng.below(150)),
    };

    // Only the 4 KiB class carries a page checksum, so a flipped byte in a 4 MiB page is
    // something the design lets through rather than something a client is protected
    // from. Rotting bytes under a cluster that holds huge pages would therefore be
    // asserting the opposite of what racer promises, so the strata that hold them get
    // every other kind of weather instead.
    let corrupt = if w.profile.huge > 0 { 0 } else { corrupt };

    Action::Weather(Faults {
        drop: drop as u32,
        io_error: io as u32,
        corrupt: corrupt as u32,
        jitter_us: w.rng.pick(&[0, 50, 400, 4_000]),
        ..w.faults.clone()
    })
}

/// Something structural: a node away, a link cut, a link crawling, or any of
/// those put back.
fn harm(w: &mut World) -> Option<Action> {
    let n = w.sim.nodes();

    // Put something back, and always be willing to once the budget is spent.
    if w.harm() >= budget(w) || w.rng.chance(400) {
        if let Some(&(a, b)) = w.cuts.iter().next()
            && w.rng.chance(500)
        {
            return Some(Action::Heal(a, b));
        }

        let down = (0..n).find(|&i| !w.sim.up(i))?;

        return Some(Action::Restart(down));
    }

    let a = w.rng.below(n as u64) as usize;
    let b = w.rng.below(n as u64) as usize;

    if a == b {
        return Some(Action::Crash(a));
    }

    match w.rng.below(100) {
        0..40 if w.sim.up(a) => Some(Action::Crash(a)),
        40..85 => Some(Action::Cut(a, b)),
        _ => Some(Action::Slow(a, b)),
    }
}

/// Head for an obligation this seed has not met. Coverage that depends on a
/// rare draw is coverage that quietly stops happening.
fn nudge(w: &mut World) -> Option<Action> {
    let node = client(w);

    if !w.cov.has(Reach::Refilled)
        && w.profile.huge > 0
        && let Some(page) = w.filled_huge()
    {
        return Some(Action::Fill { node, page });
    }

    if !w.cov.has(Reach::Damaged)
        && w.rng.chance(500)
        && let Some((page, replica)) = w.damageable()
    {
        return Some(Action::Damage(page, replica));
    }

    if !w.cov.has(Reach::NonMember)
        && let Some((node, page)) = w.non_member()
    {
        return Some(Action::Write { node, page });
    }

    if !w.cov.has(Reach::ReadHole) {
        let page = w.rng.below(w.profile.small);

        if w.small_ready(page) {
            return Some(Action::Read { node, page });
        }
    }

    if !w.cov.has(Reach::ReadHugeHole) && w.profile.huge > 0 {
        let page = w.rng.below(w.profile.huge);

        if w.huge_ready(page) {
            return Some(Action::ReadHuge { node, page });
        }
    }

    if !w.cov.has(Reach::Partitioned) && w.harm() < budget(w) {
        let n = w.sim.nodes() as u64;
        let a = w.rng.below(n) as usize;
        let b = ((a as u64 + w.rng.between(1, n - 1)) % n) as usize;

        return Some(if w.cuts.contains(&(a, b)) {
            Action::Cut(b, a)
        } else {
            Action::Cut(a, b)
        });
    }

    None
}
