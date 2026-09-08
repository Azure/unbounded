// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Deterministic simulation tests.
//!
//! Each test runs a whole cluster — allocator, consensus, anti-entropy, fabric — in one
//! thread against simulated devices, so a seed is the whole of a failure's repro.

#![cfg(feature = "sim")]

use std::time::Duration;

use racer::sim::{Faults, Options, Sim};

/// Virtual milliseconds to wait on a request. A down member is never detected, only
/// timed out against, so every attempt through it pays the fabric's two seconds.
const PATIENCE: usize = 60_000;

/// Long enough for anti-entropy to pass over every group twice: it sweeps one group per
/// core per second, and the simulator declares one group per node.
fn convergence(sim: &Sim) -> Duration {
    Duration::from_secs(2 * sim.nodes() as u64 + 2)
}

fn warm(sim: &mut Sim) {
    let d = convergence(sim);
    sim.run(d);
}

/// A cluster that has finished talking to itself: booting stores nothing, so the first
/// anti-entropy round is only the members agreeing they are all empty.
fn cluster(opts: Options) -> Sim {
    let mut sim = Sim::new(opts).expect("boot");
    warm(&mut sim);
    sim
}

fn settled(sim: &mut Sim, id: u64) -> Result<(), i32> {
    for _ in 0..PATIENCE {
        sim.run(Duration::from_millis(1));
        if let Some(r) = sim.result(id) {
            return r;
        }
    }
    panic!("request {id} never completed");
}

fn settle_all(sim: &mut Sim, ids: &[u64]) {
    for _ in 0..PATIENCE {
        if ids.iter().all(|id| sim.result(*id).is_some()) {
            return;
        }
        sim.run(Duration::from_millis(1));
    }
    panic!("a request never completed");
}

/// One write, one answer; an error is the caller's to handle.
fn put(sim: &mut Sim, node: usize, lba: u64, fill: u8) -> Result<(), i32> {
    let id = sim.write(node, lba, fill);
    settled(sim, id)
}

fn get(sim: &mut Sim, node: usize, lba: u64) -> Result<Vec<u8>, i32> {
    let id = sim.read(node, lba);
    settled(sim, id)?;
    Ok(sim.payload(id).unwrap().to_vec())
}

/// A real client retries. Anti-entropy can mark a member as replaying, which makes it
/// refuse accepts, so a single-shot mutation is not what a filesystem above would do.
fn eventually(sim: &mut Sim, what: &str, mut op: impl FnMut(&mut Sim) -> u64) {
    for _ in 0..8 {
        let id = op(sim);
        if settled(sim, id).is_ok() {
            return;
        }
        sim.run(Duration::from_millis(200));
    }
    panic!("{what} never succeeded");
}

/// Reads retry too: one can still meet a group anti-entropy has not finished with.
fn get_eventually(sim: &mut Sim, node: usize, lba: u64) -> u8 {
    for _ in 0..16 {
        let id = sim.read(node, lba);
        if settled(sim, id).is_ok() {
            return sim.payload(id).unwrap()[0];
        }
        sim.run(Duration::from_millis(500));
    }
    panic!("read of page {lba} on node {node} never succeeded");
}

fn put_eventually(sim: &mut Sim, node: usize, lba: u64, fill: u8) {
    eventually(sim, &format!("write to page {lba}"), |s| {
        s.write(node, lba, fill)
    });
}

fn assert_page(sim: &mut Sim, node: usize, lba: u64, want: u8) {
    let got = get(sim, node, lba).unwrap_or_else(|e| panic!("read page {lba} on {node}: {e}"));
    assert!(
        got.iter().all(|&b| b == want),
        "page {lba} on node {node}: wanted {want:#x}, got {:#x}",
        got[0]
    );
}

fn put_huge(sim: &mut Sim, node: usize, page: u64, fill: u8) {
    eventually(sim, &format!("huge write to page {page}"), |s| {
        s.write_huge(node, page, fill)
    });
}

/// A whole 4 MiB page, checked byte for byte: the immutable class carries no checksum,
/// so a page that arrived from the wrong buffer is only visible by looking.
fn assert_huge_page(sim: &mut Sim, node: usize, page: u64, want: u8) {
    for _ in 0..16 {
        let id = sim.read_huge(node, page);
        if settled(sim, id).is_ok() {
            let got = sim.payload(id).unwrap();
            let at = got.iter().position(|&b| b != want);
            assert!(
                at.is_none(),
                "huge page {page} on node {node}: wanted {want:#x}, got {:#x} at byte {}",
                got[at.unwrap()],
                at.unwrap()
            );
            return;
        }
        sim.run(Duration::from_millis(500));
    }
    panic!("read of huge page {page} on node {node} never succeeded");
}

/// The single byte a whole huge page is made of. A page put together from pieces is
/// either all of one write or all of another, never a seam between two.
fn uniform_huge(sim: &mut Sim, node: usize, page: u64) -> u8 {
    for _ in 0..16 {
        let id = sim.read_huge(node, page);
        if settled(sim, id).is_ok() {
            let got = sim.payload(id).unwrap();
            let want = got[0];
            let at = got.iter().position(|&b| b != want);
            assert!(
                at.is_none(),
                "huge page {page} on node {node} is a mixture: {want:#x} at byte 0 and {:#x} at byte {}",
                got[at.unwrap()],
                at.unwrap()
            );
            return want;
        }
        sim.run(Duration::from_millis(500));
    }
    panic!("read of huge page {page} on node {node} never succeeded");
}

// --------------------------------------------------------------------- the basics

#[test]
fn a_write_is_read_back() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    put_eventually(&mut sim, 0, 7, 0xab);
    assert_page(&mut sim, 0, 7, 0xab);
}

#[test]
fn every_node_serves_every_page() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    for lba in 0..24u64 {
        put_eventually(&mut sim, lba as usize % 3, lba, 0x40 + lba as u8);
    }
    for lba in 0..24u64 {
        for node in 0..3 {
            assert_page(&mut sim, node, lba, 0x40 + lba as u8);
        }
    }
}

#[test]
fn a_trimmed_page_reads_as_a_hole() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    put_eventually(&mut sim, 0, 11, 0x77);
    assert_page(&mut sim, 0, 11, 0x77);
    // Settle anti-entropy first, so the single trim attempt cannot meet a member that
    // is mid-replay and refusing accepts.
    warm(&mut sim);
    let id = sim.trim(0, 11);
    settled(&mut sim, id).expect("trim");
    for node in 0..3 {
        assert_page(&mut sim, node, 11, 0);
    }
}

#[test]
fn a_huge_page_round_trips() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 256,
        huge_pages: 4,
        ..Options::default()
    });
    eventually(&mut sim, "huge write", |s| s.write_huge(0, 2, 0xcd));
    for node in 0..3 {
        let r = sim.read_huge(node, 2);
        settled(&mut sim, r).expect("huge read");
        let p = sim.payload(r).unwrap();
        assert!(
            p.iter().all(|&b| b == 0xcd),
            "huge page differs on node {node}"
        );
    }
    // An unwritten immutable page is a hole, not an error.
    let r = sim.read_huge(1, 3);
    settled(&mut sim, r).expect("huge hole");
    assert!(sim.payload(r).unwrap().iter().all(|&b| b == 0));
}

#[test]
fn a_huge_page_replicates_when_the_fabric_splits_it() {
    // Real transports cap a command at their MDTS, so one 4 MiB frame reaches the target
    // as consecutive pieces that it has to put back together by offset.
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 256,
        huge_pages: 8,
        mdts: 128 * 1024,
        ..Options::default()
    });
    for page in 0..3u64 {
        put_huge(&mut sim, page as usize % 3, page, 0x20 + page as u8);
    }
    warm(&mut sim);
    for page in 0..3u64 {
        for node in 0..3 {
            assert_huge_page(&mut sim, node, page, 0x20 + page as u8);
        }
    }
    // A hole still reads as one, and reads split the same way.
    let r = sim.read_huge(1, 5);
    settled(&mut sim, r).expect("huge hole");
    assert!(sim.payload(r).unwrap().iter().all(|&b| b == 0));
}

#[test]
fn a_split_huge_page_written_to_a_bystander_still_replicates() {
    // Five nodes means every node is outside some groups, so a write can land on a node
    // that is not a member. It then has to put the pieces together before it can put the
    // page to the members, which is the longest way a 4 MiB write can travel.
    let mut sim = cluster(Options {
        nodes: 5,
        pages: 256,
        huge_pages: 8,
        mdts: 128 * 1024,
        ..Options::default()
    });
    for page in 0..5u64 {
        put_huge(&mut sim, 0, page, 0x30 + page as u8);
    }
    warm(&mut sim);
    for page in 0..5u64 {
        for node in 0..5 {
            assert_huge_page(&mut sim, node, page, 0x30 + page as u8);
        }
    }
}

#[test]
fn a_page_that_arrives_in_pieces_is_never_served_half_written() {
    // Pieces get lost, and a member can restart with some of them already on its disk.
    // A 4 MiB page carries no checksum, so the only defense is that a page becomes a
    // version once it is whole and not a moment before.
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 256,
        huge_pages: 32,
        mdts: 128 * 1024,
        ..Options::default()
    });
    put_huge(&mut sim, 0, 0, 0xaa);
    warm(&mut sim);

    // Half of every command's pieces go missing, and a member restarts underneath one.
    sim.faults(Faults {
        drop: 500,
        ..Faults::default()
    });
    for round in 0..4u8 {
        let w = sim.write_huge(0, 0, 0xb0 + round);
        if round == 1 {
            sim.run(Duration::from_micros(200));
            sim.crash(2);
        }
        let _ = settled(&mut sim, w);
        if round == 1 {
            sim.restart(2).expect("restart");
        }
    }
    sim.faults(Faults::default());
    warm(&mut sim);

    // Whichever write won, every node serves one whole page of it.
    let fill = uniform_huge(&mut sim, 0, 0);
    assert!(
        fill == 0xaa || (0xb0..0xb4).contains(&fill),
        "page 0 holds {fill:#x}, which nobody wrote"
    );
    for node in 0..3 {
        assert_huge_page(&mut sim, node, 0, fill);
    }

    // More half-arrived pages than a core will hold assemblies for, so the reservations
    // the abandoned ones sit on have to come back or the class runs out of slots.
    sim.faults(Faults {
        drop: 500,
        ..Faults::default()
    });
    for page in 1..24u64 {
        let w = sim.write_huge(0, page, 0xc0 + page as u8);
        let _ = settled(&mut sim, w);
    }
    sim.faults(Faults::default());
    warm(&mut sim);
    for page in 1..24u64 {
        put_huge(&mut sim, 0, page, 0xc0 + page as u8);
        assert_huge_page(&mut sim, 2, page, 0xc0 + page as u8);
    }
}

#[test]
fn a_straggling_replica_gets_the_page_it_was_sent() {
    // One replica sits behind a slow link, so its leg of every 4 MiB write is still in
    // flight when the faster replica has already made the quorum. A 4 MiB write hands
    // the guest's own buffer to both legs, so a leg that outlives the request puts
    // whatever replaced it on the wire — and the immutable class carries no checksum to
    // catch that afterwards.
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 256,
        huge_pages: 8,
        faults: Faults {
            slow: [(1, 3)].into_iter().collect(),
            ..Faults::default()
        },
        ..Options::default()
    });
    for page in 0..4u64 {
        put_huge(&mut sim, 0, page, 0x10 + page as u8);
    }
    sim.faults(Faults::default());
    warm(&mut sim);
    for page in 0..4u64 {
        for node in 0..3 {
            assert_huge_page(&mut sim, node, page, 0x10 + page as u8);
        }
    }
}

// ----------------------------------------------------------------------- failures

#[test]
fn a_crash_does_not_lose_an_acknowledged_write() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    for lba in 0..8u64 {
        put_eventually(&mut sim, 0, lba, 0xa0 + lba as u8);
    }
    warm(&mut sim);

    sim.crash(1);
    sim.run(Duration::from_millis(100));
    assert!(!sim.up(1));

    // A quorum survives, so the cluster keeps taking writes.
    for lba in 8..16u64 {
        put_eventually(&mut sim, 0, lba, 0xa0 + lba as u8);
    }

    sim.restart(1).expect("restart");
    warm(&mut sim);

    // After anti-entropy, node 1 serves everything acked before and during its outage.
    for lba in 0..16u64 {
        assert_page(&mut sim, 1, lba, 0xa0 + lba as u8);
    }
}

#[test]
fn a_partition_keeps_a_quorum_serving() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });

    // Nodes 1 and 2 cannot hear each other, but node 0 reaches both, so either of them
    // still has a quorum and writes keep landing from either side.
    sim.cut(1, 2, true);
    sim.cut(2, 1, true);
    for lba in 0..8u64 {
        put_eventually(&mut sim, (lba % 2) as usize, lba, 0xb0 + lba as u8);
    }

    sim.cut(1, 2, false);
    sim.cut(2, 1, false);
    warm(&mut sim);
    for lba in 0..8u64 {
        for node in 0..3 {
            assert_page(&mut sim, node, lba, 0xb0 + lba as u8);
        }
    }
}

#[test]
fn an_asymmetric_partition_converges() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    // One direction only: node 1 hears node 2, node 2 never hears node 1.
    sim.cut(1, 2, true);
    for lba in 0..8u64 {
        put_eventually(&mut sim, 0, lba, 0xc0 + lba as u8);
    }
    sim.cut(1, 2, false);
    warm(&mut sim);
    for lba in 0..8u64 {
        for node in 0..3 {
            assert_page(&mut sim, node, lba, 0xc0 + lba as u8);
        }
    }
}

#[test]
fn lost_frames_do_not_lose_a_write() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        faults: Faults {
            drop: 250,
            ..Faults::default()
        },
        ..Options::default()
    });
    for lba in 0..12u64 {
        put_eventually(&mut sim, 0, lba, 0xd0 + lba as u8);
    }
    sim.faults(Faults::default());
    warm(&mut sim);
    for lba in 0..12u64 {
        for node in 0..3 {
            assert_page(&mut sim, node, lba, 0xd0 + lba as u8);
        }
    }
}

#[test]
fn disk_errors_are_reported_not_swallowed() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    sim.faults(Faults {
        io_error: 300,
        ..Faults::default()
    });
    let mut failed = 0;
    for lba in 0..24u64 {
        if put(&mut sim, 0, lba, 0xe1).is_err() {
            failed += 1;
        }
    }
    assert!(
        failed > 0,
        "a device that fails 30% of its writes reported no errors"
    );

    // Recovering the medium is enough: every page is writable and readable again.
    sim.faults(Faults::default());
    for lba in 0..24u64 {
        put_eventually(&mut sim, 0, lba, 0xe2);
    }
    warm(&mut sim);
    for lba in 0..24u64 {
        for node in 0..3 {
            assert_page(&mut sim, node, lba, 0xe2);
        }
    }
}

// --------------------------------------------------------------------- rate budget

/// A cap tight enough that the cluster spends most of its time waiting on it, so the
/// measurement below is of the budget and not of anything else.
const CAP: u64 = 64;

/// The budget is a promise to the device, so the test is that it is kept — and that
/// keeping it does not deadlock a group commit or starve the op slab, which is the
/// failure a pacer this deep in the write path would actually have.
#[test]
fn a_rate_budget_is_kept_and_the_cluster_still_commits() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        device_iops: CAP,
        ..Options::default()
    });
    let start = sim.now();
    let before: Vec<u64> = (0..3).map(|n| sim.device_ops(n)).collect();

    for lba in 0..32u64 {
        put_eventually(&mut sim, lba as usize % 3, lba, 0x50 + lba as u8);
    }
    for lba in 0..32u64 {
        assert_page(&mut sim, 0, lba, 0x50 + lba as u8);
    }

    let elapsed = (sim.now() - start).as_secs_f64();
    for (n, before) in before.iter().enumerate() {
        let ops = sim.device_ops(n) - before;
        assert!(
            ops > CAP,
            "node {n} issued only {ops} transfers; the cap was never reached"
        );
        let rate = ops as f64 / elapsed;
        assert!(
            rate <= CAP as f64 * 1.2,
            "node {n} ran at {rate:.0}/s against a cap of {CAP}"
        );
    }
}

#[test]
fn corruption_never_reaches_the_client() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });
    for lba in 0..24u64 {
        put_eventually(&mut sim, 0, lba, 0x30 + lba as u8);
    }
    warm(&mut sim);

    // Bit rot on every device at once: a read may fail, it may never lie.
    sim.faults(Faults {
        corrupt: 200,
        ..Faults::default()
    });
    let mut answered = 0;
    for round in 0..4 {
        for lba in 0..24u64 {
            match get(&mut sim, round % 3, lba) {
                Ok(p) => {
                    assert!(
                        p.iter().all(|&b| b == 0x30 + lba as u8),
                        "page {lba} came back corrupt"
                    );
                    answered += 1;
                }
                Err(e) => assert_eq!(e, libc::EIO, "unexpected error on page {lba}"),
            }
        }
    }
    assert!(answered > 0, "every read failed; the test proved nothing");
}

#[test]
fn persistent_corruption_is_repaired_from_a_peer() {
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        ..Options::default()
    });

    for (lba, replica, fill) in [(31, 0, 0x91), (47, 2, 0xa7)] {
        put_eventually(&mut sim, 0, lba, fill);
        warm(&mut sim);
        for i in 0..3 {
            assert!(
                sim.small_replica_valid(lba, i),
                "page {lba} replica {i} was not valid"
            );
        }

        let node = sim.corrupt_small_replica(lba, replica);
        assert!(
            !sim.small_replica_valid(lba, replica),
            "page {lba} was not corrupted"
        );
        assert_page(&mut sim, node, lba, fill);
        assert!(
            sim.small_replica_valid(lba, replica),
            "page {lba} was not repaired"
        );

        sim.crash(node);
        sim.restart(node).expect("restart repaired node");
        warm(&mut sim);
        assert!(
            sim.small_replica_valid(lba, replica),
            "page {lba} repair was not durable"
        );
        assert_page(&mut sim, node, lba, fill);
    }
}

// ---------------------------------------------------------------- linearizability

/// A deterministic stream of choices, so a failing run is a seed.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.0 = x;
        x
    }

    fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
}

/// What a page could legally hold.
///
/// Writes to one page are issued one at a time, so the only ambiguity is a write that
/// returned an error: it may or may not have landed. `settled` indexes the newest write
/// known to have happened, and everything from there on is a candidate.
#[derive(Default)]
struct Page {
    writes: Vec<u8>,
    settled: Option<usize>,
}

impl Page {
    fn allows(&self, got: u8) -> bool {
        let from = self.settled.map_or(0, |i| i);
        if self.settled.is_none() && got == 0 {
            return true; // nothing is known to have landed, so a hole is legal
        }
        self.writes[from..].contains(&got)
    }

    /// A read is evidence: whatever it returned is now known to have happened.
    fn observe(&mut self, got: u8) {
        let from = self.settled.map_or(0, |i| i);
        if let Some(i) = self.writes[from..].iter().rposition(|&v| v == got) {
            self.settled = Some(from + i);
        }
    }
}

/// One disturbance the cluster must survive. Only ever one at a time, so a quorum always
/// exists and the run tests consensus rather than availability.
enum Hurt {
    Down(usize),
    Cut(usize, usize),
}

/// Break something, or heal what was broken last time. Called with requests in flight,
/// so a crash also answers for the work that node was in the middle of.
fn churn(sim: &mut Sim, rng: &mut Rng, hurt: &mut Option<Hurt>) {
    // Hold an existing break for a round or two; one that heals at once is never noticed.
    if hurt.is_some() && rng.below(3) > 0 {
        return;
    }
    match hurt.take() {
        Some(Hurt::Down(i)) => sim.restart(i).expect("restart"),
        Some(Hurt::Cut(a, b)) => sim.cut(a, b, false),
        None => {
            let a = rng.below(3) as usize;
            match rng.below(6) {
                0 => {
                    sim.crash(a);
                    *hurt = Some(Hurt::Down(a));
                }
                1 => {
                    // One direction only: b never hears a, but a still hears b.
                    let b = (a + 1 + rng.below(2) as usize) % 3;
                    sim.cut(a, b, true);
                    *hurt = Some(Hurt::Cut(a, b));
                }
                _ => {}
            }
        }
    }
}

/// The gate on consensus. Every page is read and written from all three nodes while
/// frames drop, disks fail, bytes rot, a node comes and goes and a link is cut, and every
/// read must be explainable by some serialization of the writes still in flight.
#[test]
fn a_page_is_linearizable_under_faults() {
    // One seed drives both the fault and the operation stream, so `DST_SEED` re-runs a
    // failing one alone.
    if let Some(s) = std::env::var_os("DST_SEED") {
        linearizable(s.to_string_lossy().parse().unwrap());
        return;
    }
    for seed in 1..=32 {
        linearizable(seed);
    }
}

fn linearizable(seed: u64) {
    const PAGES: u64 = 12;
    let mut sim = cluster(Options {
        nodes: 3,
        pages: 1024,
        seed,
        faults: Faults {
            drop: 60,
            io_error: 40,
            corrupt: 40,
            jitter_us: 400,
            ..Faults::default()
        },
        ..Options::default()
    });
    let mut rng = Rng(0x5eed ^ seed);
    let mut pages: Vec<Page> = (0..PAGES).map(|_| Page::default()).collect();
    let mut fill = 1u8;
    let mut hurt = None;

    for round in 0..40 {
        // One op per page, but every page at once: the interesting interleavings are
        // between pages, and within a page a client is sequential.
        let mut writes = Vec::new();
        let mut reads = Vec::new();
        for lba in 0..PAGES {
            let node = rng.below(3) as usize;
            if rng.below(3) == 0 {
                reads.push((lba, sim.read(node, lba)));
            } else {
                fill = fill.wrapping_add(1).max(1);
                pages[lba as usize].writes.push(fill);
                writes.push((lba, fill, sim.write(node, lba, fill)));
            }
        }
        let ids: Vec<u64> = writes
            .iter()
            .map(|w| w.2)
            .chain(reads.iter().map(|r| r.1))
            .collect();
        churn(&mut sim, &mut rng, &mut hurt);
        settle_all(&mut sim, &ids);

        for (lba, fill, id) in writes {
            if sim.result(id).unwrap().is_ok() {
                let p = &mut pages[lba as usize];
                let i = p.writes.iter().rposition(|&v| v == fill).unwrap();
                p.settled = Some(i);
            }
        }
        for (lba, id) in reads {
            if sim.result(id).unwrap().is_ok() {
                let got = sim.payload(id).unwrap()[0];
                let p = &mut pages[lba as usize];
                assert!(
                    p.allows(got),
                    "seed {seed} round {round} page {lba} returned {got:#x}, which no write could explain"
                );
                p.observe(got);
            }
        }
    }

    // With the faults healed the group must agree, on something a client could have
    // written. Not necessarily the newest write we saw acked: a later one that reported
    // an error may still have landed.
    match hurt {
        Some(Hurt::Down(i)) => sim.restart(i).expect("restart"),
        Some(Hurt::Cut(a, b)) => sim.cut(a, b, false),
        None => {}
    }
    sim.faults(Faults::default());
    warm(&mut sim);
    warm(&mut sim);
    for lba in 0..PAGES {
        let p = &pages[lba as usize];
        if p.settled.is_none() {
            continue;
        }
        let mut agreed: Option<u8> = None;
        for node in 0..3 {
            let got = get_eventually(&mut sim, node, lba);
            assert!(
                p.allows(got),
                "seed {seed} page {lba} holds {got:#x}, which no write could explain"
            );
            match agreed {
                None => agreed = Some(got),
                Some(v) => assert_eq!(got, v, "seed {seed} page {lba} disagrees on node {node}"),
            }
        }
    }
}
