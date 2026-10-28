//! Probe sequences for owners, non-owners, stale membership, version misses,
//! and budget exhaustion. Most walks are rows of [`walks`]: a configuration,
//! a budget, scripted outcomes, and the expected [`trace`].

// The attribute below also marks this file as test-only for tests/purity.rs.
#[cfg(test)]
use super::*;
use crate::read::budget::{
    HELPER_ATTEMPTS, HELPER_FLOOR_LINKS, HELPER_LINKS, HELPER_MIN_ATTEMPTS, inherited_budget,
};
use crate::read::{Answering, ReplicaSelection, demote_unanswered, visit_order};
use std::fmt::Write;

fn config(rank: Option<usize>, owners: usize, order: VisitOrder) -> WalkConfig {
    WalkConfig {
        rank,
        owners,
        order,
        page_operation: true,
        continuation: Continuation::default(),
        handoff: 0,
        previous: 0,
        helper: false,
        lineage: [0; MAX_HANDOFF],
        granted: 0,
        parallel_granted: false,
        rank0: Rank0State::Unknown,
    }
}

/// An owner at `rank` of three, in canonical order.
fn owner(rank: usize) -> WalkConfig {
    config(Some(rank), 3, VisitOrder::canonical(3))
}

/// A reader of `owners` owners, in canonical order.
fn reader(owners: usize) -> WalkConfig {
    config(None, owners, VisitOrder::canonical(owners))
}

/// A reader that visits rank `first` first.
fn first(first: usize) -> WalkConfig {
    config(
        None,
        3,
        visit_order(ReplicaSelection::Spread, 3, |r| r == first, |_| 0),
    )
}

/// A reader that visits ranks 1, 2, 0.
fn order_120() -> VisitOrder {
    visit_order(ReplicaSelection::Spread, 3, |r| r != 0, |r| 3 - r as u64)
}

fn budget(attempts: u32, links: u8) -> AcquisitionBudget {
    AcquisitionBudget::new(Instant::now() + Duration::from_secs(60), attempts, links)
}

/// Drive a walk with scripted outcomes; return probes, with the request
/// budget's attempts, links and route links and the pending handoff and
/// granted flags, and the end.
fn drive(
    walk: &mut CandidateWalk,
    budget: &mut AcquisitionBudget,
    mut outcome: impl FnMut(&Probe, &mut AcquisitionBudget) -> Outcome,
) -> (Vec<(Probe, u32, u8, String)>, End) {
    let mut probes = Vec::new();
    loop {
        match walk.next(budget).unwrap() {
            Step::Probe(probe) => {
                let p = walk.pending.as_ref().unwrap();
                let flags = format!(
                    "{}{}",
                    if p.handoff { "h" } else { "" },
                    if p.granted { "g" } else { "" }
                );
                let request = walk.request_budget(budget);
                let (a, l, r) = (
                    request.remaining_attempts(),
                    request.remaining_links(),
                    request.route_links(),
                );
                probes.push((probe, a, l, format!("{flags}:{a}/{l}/{r}")));
                let result = outcome(&probe, request);
                if let Some(end) = walk.complete(budget, result).unwrap() {
                    return (probes, end);
                }
            }
            Step::Granted(batch) => {
                let mut copy = false;
                for probe in batch {
                    let mut grant = granted_budget(budget.deadline());
                    let (a, l) = (grant.remaining_attempts(), grant.remaining_links());
                    probes.push((probe, a, l, format!("b:{a}/{l}/{}", grant.route_links())));
                    copy |= outcome(&probe, &mut grant) == Outcome::Copy;
                }
                if let Some(end) = walk.complete_granted(copy) {
                    return (probes, end);
                }
            }
            Step::End(end) => return (probes, end),
        }
    }
}

/// Plan `probe`'s exchange on `request` with a one second attempt timeout.
fn plan(request: &mut AcquisitionBudget, deadline: Instant, probe: &Probe) -> Result<AttemptPlan> {
    plan_attempt(
        request,
        AttemptInputs {
            now: Instant::now(),
            caller_deadline: deadline,
            remaining_opportunities: probe.remaining_opportunities,
            route: probe.route,
            mode: probe.mode,
            attempt_timeout: Duration::from_secs(1),
            failure_links: 8,
        },
    )
}

fn charge(walk: &mut CandidateWalk, b: &mut AcquisitionBudget, probe: &Probe) -> AttemptPlan {
    let deadline = b.deadline();
    plan(walk.request_budget(b), deadline, probe).unwrap()
}

/// What a scripted probe spends of its request budget before it answers.
#[derive(Clone, Copy)]
enum Spend {
    Nothing,
    /// Every attempt, as a forwarded funded route does.
    All,
    One,
    /// Every credit, as an exchange that forwards its whole budget.
    Transfer,
}

const COPY: Outcome = Outcome::Copy;
const MISS: Outcome = Outcome::Probe(ProbeOutcome::CopyMiss);
const VU: Outcome = Outcome::Probe(ProbeOutcome::VersionUnavailable);
const OVL: Outcome = Outcome::Probe(ProbeOutcome::Overloaded);
const UNR: Outcome = Outcome::Probe(ProbeOutcome::Unreachable);
const T_UNR: Outcome = Outcome::Transport(ProbeOutcome::Unreachable);
const T_OVL: Outcome = Outcome::Transport(ProbeOutcome::Overloaded);
const SHED: Outcome = Outcome::Shed;
const TIMEOUT: Outcome = Outcome::TimedOut;
const FAILED: Outcome = Outcome::Failed;
const STALE: Outcome = Outcome::StaleMembership;
const BEHIND: Outcome = Outcome::StaleDestination;

/// Walk `cfg` on `b`; each probe answers `script[canonical]` (the last entry
/// for later ranks; no entry: no probe expected). The trace lists each probe
/// as `{canonical}{A|C}{opportunities}{F|S|D}{p}` with its request budget,
/// then the end, the walk's budget after, and `r0` when rank 0 left an owner
/// unanswered.
fn trace(cfg: WalkConfig, mut b: AcquisitionBudget, spend: Spend, script: &[Outcome]) -> String {
    let mut walk = CandidateWalk::new(cfg);
    let (probes, end) = drive(&mut walk, &mut b, |p, request| {
        match spend {
            Spend::Nothing => {}
            Spend::All => drop(request.partition(request.remaining_attempts(), 0).unwrap()),
            Spend::One => drop(request.partition(1, 0).unwrap()),
            Spend::Transfer => drop(request.transfer()),
        }
        *script
            .get(p.canonical)
            .or(script.last())
            .expect("no probe expected")
    });
    let mut s = String::new();
    for (p, _, _, budget) in &probes {
        let mode = if p.mode == Mode::Acquire { 'A' } else { 'C' };
        let route = match p.route {
            RouteMode::FundedRoute => 'F',
            RouteMode::SharedRoute => 'S',
            RouteMode::DirectHedge => 'D',
        };
        let previous = if p.previous { "p" } else { "" };
        let (c, o) = (p.canonical, p.remaining_opportunities);
        write!(s, "{c}{mode}{o}{route}{previous}{budget} ").unwrap();
    }
    let r0 = if walk.rank0_unanswered { " r0" } else { "" };
    let (a, l, r) = (b.remaining_attempts(), b.remaining_links(), b.route_links());
    write!(s, "=> {end:?} {a}/{l}/{r}{r0}").unwrap();
    s
}

#[test]
fn walks() {
    let failure = |attempts, links| {
        let mut b = budget(attempts, links);
        b.note_route_failure();
        b
    };
    let helper = |order| WalkConfig {
        helper: true,
        ..config(None, 3, order)
    };
    let canonical = VisitOrder::canonical(3);
    let with = |cfg: WalkConfig, f: &dyn Fn(&mut WalkConfig)| {
        let mut cfg = cfg;
        f(&mut cfg);
        cfg
    };
    let rank0 = |rank, state| WalkConfig {
        rank0: state,
        ..owner(rank)
    };
    let nothing = Spend::Nothing;
    #[rustfmt::skip]
    #[allow(clippy::type_complexity)]
    let rows: Vec<(&str, WalkConfig, AcquisitionBudget, Spend, &[Outcome], &str)> = vec![
        // An owner asks rank 0 to acquire, the rest copy-only, then gets origin. The Acquire
        // carries exactly the exchange and one origin credit.
        ("owner", owner(2), budget(32, 96), nothing, &[MISS], "0A3F:2/4/4 1C2S:32/96/4 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 32/96/4"),
        ("rank 0 owner: origin at once", owner(0), budget(1, 1), nothing, &[], "=> Origin { rank: 0, evidence: [] } 1/1/1"),
        // An owner delegates only with credit left for its own origin: rank 1 needs three, rank 2
        // four (the Acquire, rank 1's probe, and its origin).
        ("rank 1, 2 credits", owner(1), budget(2, 16), nothing, &[MISS], "0C2S:2/16/4 => Origin { rank: 1, evidence: [CopyMiss] } 2/16/4"),
        ("rank 1, 3 credits", owner(1), budget(3, 16), nothing, &[MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [CopyMiss] } 3/16/4"),
        ("rank 2, 3 credits", owner(2), budget(3, 16), nothing, &[MISS], "0C3S:3/16/4 1C2S:3/16/4 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 3/16/4"),
        ("rank 2, 4 credits", owner(2), budget(4, 16), nothing, &[MISS], "0A3F:2/4/4 1C2S:4/16/4 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 4/16/4"),
        // A non-owner visiting 1, 2, 0 funds rank 1 fully while rank 2's minimum still fits.
        ("reader slack", config(None, 3, order_120()), budget(8, 16), Spend::All, &[MISS], "1A3F:4/8/8 2A2F:4/8/8 0A1S:0/16/8 => Exhausted { funding: false } 0/16/8"),
        // A delegated version miss ends without origin; a copy-only one is only evidence.
        ("delegated version miss", owner(2), budget(8, 16), nothing, &[VU], "0A3F:2/4/4 => VersionUnavailable 8/16/4"),
        ("copy-only version miss", owner(2), budget(8, 16), nothing, &[MISS, VU], "0A3F:2/4/4 1C2S:8/16/4 => Origin { rank: 2, evidence: [CopyMiss, VersionUnavailable] } 8/16/4"),
        ("metadata never delegates", with(owner(1), &|c| c.page_operation = false), budget(8, 16), nothing, &[MISS], "0C2S:8/16/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/16/4"),
        // Handoff targets: copy-only after predecessors, never predecessor evidence; a stale one
        // is only a miss; a copy wins; non-page walks ignore them.
        ("handoff", with(owner(1), &|c| c.handoff = 2), budget(8, 32), nothing, &[OVL, MISS], "0A4F:5/4/4 3C3Sh:8/32/8 4C2Sh:8/32/8 => Origin { rank: 1, evidence: [Overloaded] } 8/32/8"),
        ("stale handoff", with(owner(0), &|c| c.handoff = 1), budget(8, 32), nothing, &[STALE], "3C2Sh:8/32/4 => Origin { rank: 0, evidence: [] } 8/32/4"),
        ("handoff copy", with(owner(0), &|c| c.handoff = 3), budget(8, 32), nothing, &[MISS, MISS, MISS, MISS, COPY], "3C4Sh:8/32/4 4C3Sh:8/32/4 => Copy 8/32/4"),
        ("metadata handoff", with(owner(0), &|c| { c.handoff = 3; c.page_operation = false; }), budget(8, 32), nothing, &[], "=> Origin { rank: 0, evidence: [] } 8/32/4"),
        // Handoff probes never spend the origin credit, nor go out without links.
        ("handoff, 3 credits", with(owner(0), &|c| c.handoff = 3), budget(3, 32), Spend::One, &[MISS], "3C4Sh:3/32/4 4C3Sh:2/32/4 => Origin { rank: 0, evidence: [] } 1/32/4"),
        ("handoff, 1 credit", with(owner(0), &|c| c.handoff = 3), budget(1, 32), nothing, &[], "=> Origin { rank: 0, evidence: [] } 1/32/4"),
        ("handoff, no links", with(owner(0), &|c| c.handoff = 1), budget(8, 0), nothing, &[], "=> Origin { rank: 0, evidence: [] } 8/0/0"),
        // A stale handoff target is a miss, never walk evidence.
        ("behind handoff", with(owner(1), &|c| c.handoff = 1), budget(8, 32), nothing, &[MISS, MISS, MISS, BEHIND], "0A3F:5/4/4 3C2Sh:8/32/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        // Tight links: the first probe of rank 0 (Acquire or copy-only) gets a full failure route
        // and leaves one link for rank 1; the last predecessor may use the whole pool. Without a
        // failure route the first route is capped at four links.
        ("tight links, delegated", owner(2), failure(4, 7), nothing, &[MISS], "0A3F:2/6/6 1C2S:4/7/7 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 4/7/7"),
        ("tight links, copy-only", owner(2), failure(3, 7), nothing, &[MISS], "0C3S:1/6/6 1C2S:3/7/7 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 3/7/7"),
        ("tight links, no failure", owner(2), budget(3, 4), nothing, &[MISS], "0C3S:1/3/3 1C2S:3/4/4 => Origin { rank: 2, evidence: [CopyMiss, CopyMiss] } 3/4/4"),
        // A non-owner funds an out-of-order owner (8 links), at least its minimum.
        ("out of order", first(1), budget(32, 96), nothing, &[VU], "1A3F:4/8/8 0A2S:32/96/8 2A1S:32/96/8 => VersionUnavailable 32/96/8"),
        ("out of order, minimum", first(1), budget(3, 96), nothing, &[VU], "1A3F:3/8/8 0A2S:3/96/8 2A1S:3/96/8 => VersionUnavailable 3/96/8"),
        // Transient evidence exhausts a reader and notes a route failure.
        ("transient", reader(2), budget(32, 96), nothing, &[T_UNR, VU], "0A2S:32/96/4 1A1S:32/96/8 => Exhausted { funding: false } 32/96/8"),
        ("last attempt is copy-only", reader(1), budget(1, 8), nothing, &[COPY], "0C1S:1/8/4 => Copy 1/8/4"),
        // A bounded route that cannot be funded stops with the funding site.
        ("unfundable bounded", with(reader(3), &|c| c.continuation.bounded_routes = true), budget(2, 16), nothing, &[MISS], "0A3F:2/8/8 => Exhausted { funding: true } 2/16/8"),
        // `StaleMembership` ends the epoch and returns the child's credits.
        ("stale membership", first(2), budget(32, 96), nothing, &[STALE], "2A3F:5/8/8 => StaleMembership 32/96/8"),
        // A destination that is behind is skipped, without a route failure.
        ("behind, next owner", reader(3), budget(32, 96), nothing, &[BEHIND, COPY], "0A3S:32/96/4 1A2S:32/96/4 => Copy 32/96/4"),
        ("behind everywhere", reader(2), budget(32, 96), nothing, &[BEHIND], "0A2S:32/96/4 1A1S:32/96/4 => Exhausted { funding: false } 32/96/4"),
        // An owner keeps its authority, with transient evidence.
        ("owner behind", config(Some(1), 2, VisitOrder::canonical(2)), budget(32, 96), nothing, &[BEHIND], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 32/96/4"),
        // A hedge continuation skips the primary and carries its outcome.
        ("hedge continuation", with(reader(2), &|c| { c.continuation = Continuation { primary_consumed: true, primary_outcome: Some(ProbeOutcome::VersionUnavailable), bounded_routes: true } }), budget(32, 96), nothing, &[VU], "1A1F:4/8/8 => VersionUnavailable 32/96/8"),
        ("hedge continuation, no outcome", with(reader(2), &|c| c.continuation.primary_consumed = true), budget(32, 96), nothing, &[VU], "1A1S:32/96/4 => Exhausted { funding: false } 32/96/4"),
        // Predecessors that held the same rank under the previous membership are asked under it;
        // handoff targets never are; a copy from one wins; readers and metadata ignore the bits.
        ("previous", with(owner(2), &|c| { c.previous = 0b01; c.handoff = 1; }), budget(8, 32), nothing, &[OVL, MISS], "0A4Fp:5/4/4 1C3S:8/32/8 3C2Sh:8/32/8 => Origin { rank: 2, evidence: [Overloaded, CopyMiss] } 8/32/8"),
        ("previous copy", with(owner(1), &|c| c.previous = 0b01), budget(8, 32), nothing, &[COPY], "0A2Fp:2/4/4 => Copy 8/32/4"),
        ("previous, reader", with(reader(3), &|c| c.previous = 0b111), budget(8, 32), nothing, &[MISS], "0A3S:8/32/4 1A2S:8/32/4 2A1S:8/32/4 => Exhausted { funding: false } 8/32/4"),
        ("previous, metadata", with(owner(1), &|c| { c.previous = 0b01; c.page_operation = false; }), budget(8, 32), nothing, &[MISS], "0C2S:8/32/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        // A stale answer under the previous membership is transient evidence, never a refresh.
        ("previous stale", with(owner(2), &|c| c.previous = 0b11), budget(8, 32), nothing, &[STALE, MISS], "0A3Fp:2/4/4 1C2Sp:8/32/4 => Origin { rank: 2, evidence: [Unreachable, CopyMiss] } 8/32/4"),
        ("previous behind", with(owner(2), &|c| c.previous = 0b11), budget(8, 32), nothing, &[BEHIND, MISS], "0A3Fp:2/4/4 1C2Sp:8/32/4 => Origin { rank: 2, evidence: [Unreachable, CopyMiss] } 8/32/4"),
        // An owner that sheds the reader sends it to its helper once, funded from what is left,
        // with the helper's full funded envelope; a helper miss falls back to the other owners.
        ("helper copy", helper(canonical), budget(8, 16), nothing, &[SHED, SHED, SHED, COPY], "0A3S:8/16/4 3A3Fh:4/8/8 => Copy 8/16/8"),
        ("helper, floor links", helper(canonical), budget(8, HELPER_FLOOR_LINKS), nothing, &[SHED, SHED, SHED, COPY], "0A3S:8/4/4 3A3Fh:4/4/4 => Copy 8/4/4"),
        ("helper, helper links", helper(canonical), budget(8, HELPER_LINKS), nothing, &[SHED, SHED, SHED, COPY], "0A3S:8/8/4 3A3Fh:4/8/8 => Copy 8/8/8"),
        ("helper miss", helper(canonical), budget(32, 64), nothing, &[SHED], "0A3S:32/64/4 3A3Fh:4/8/8 1A2S:32/64/8 2A1S:32/64/8 => Exhausted { funding: false } 32/64/8"),
        // Only a destination's own shed asks the helper.
        ("relay overload, no helper", helper(canonical), budget(32, 64), nothing, &[OVL], "0A3S:32/64/4 1A2S:32/64/8 2A1S:32/64/8 => Exhausted { funding: false } 32/64/8"),
        ("transport overload, no helper", helper(canonical), budget(32, 64), nothing, &[T_OVL], "0A3S:32/64/4 1A2S:32/64/8 2A1S:32/64/8 => Exhausted { funding: false } 32/64/8"),
        ("unreachable, no helper", helper(canonical), budget(32, 64), nothing, &[T_UNR], "0A3S:32/64/4 1A2S:32/64/8 2A1S:32/64/8 => Exhausted { funding: false } 32/64/8"),
        ("miss, no helper", helper(canonical), budget(32, 64), nothing, &[MISS], "0A3S:32/64/4 1A2S:32/64/4 2A1S:32/64/4 => Exhausted { funding: false } 32/64/4"),
        // Without overflow the helper changes nothing (same trace with and without).
        ("first 0, helper", helper(first(0).order), budget(8, 16), nothing, &[MISS], "0A3S:8/16/4 1A2S:8/16/4 2A1S:8/16/4 => Exhausted { funding: false } 8/16/4"),
        ("first 0", first(0), budget(8, 16), nothing, &[MISS], "0A3S:8/16/4 1A2S:8/16/4 2A1S:8/16/4 => Exhausted { funding: false } 8/16/4"),
        ("first 1, helper", helper(first(1).order), budget(8, 16), nothing, &[MISS], "1A3F:4/8/8 0A2S:8/16/8 2A1S:8/16/8 => Exhausted { funding: false } 8/16/8"),
        ("first 1", first(1), budget(8, 16), nothing, &[MISS], "1A3F:4/8/8 0A2S:8/16/8 2A1S:8/16/8 => Exhausted { funding: false } 8/16/8"),
        ("first 2, helper", helper(first(2).order), budget(8, 16), nothing, &[MISS], "2A3F:5/8/8 0A2S:8/16/8 1A1S:8/16/8 => Exhausted { funding: false } 8/16/8"),
        ("first 2", first(2), budget(8, 16), nothing, &[MISS], "2A3F:5/8/8 0A2S:8/16/8 1A1S:8/16/8 => Exhausted { funding: false } 8/16/8"),
        // A helper never chains; owners never use helpers; too little left skips it.
        ("helper budget", helper(canonical), budget(HELPER_ATTEMPTS, 8), nothing, &[SHED], "0A3S:4/8/4 1A2S:4/8/4 2A1S:4/8/4 => Exhausted { funding: false } 4/8/4"),
        ("owner ignores helper", with(owner(2), &|c| c.helper = true), budget(8, 16), nothing, &[SHED], "0A3F:2/4/4 1C2S:8/16/4 => Origin { rank: 2, evidence: [Overloaded, Overloaded] } 8/16/4"),
        ("helper unaffordable", helper(first(2).order), budget(HELPER_MIN_ATTEMPTS + 1, 16), Spend::Transfer, &[SHED], "2A3F:5/8/8 0C2S:1/8/8 1A1S:0/0/0 => Exhausted { funding: false } 0/0/0"),
        // A small budget visits rank 0 first instead of failing to fund rank 2.
        ("small budget", first(2), budget(HELPER_ATTEMPTS - 1, 7), nothing, &[COPY, MISS], "0A3S:3/7/4 => Copy 3/7/4"),
        // A hedge reports only `Overloaded`, never a destination shed.
        ("hedged overload", with(helper(canonical), &|c| { c.continuation.primary_consumed = true; c.continuation.primary_outcome = Some(ProbeOutcome::Overloaded); }), budget(8, 16), nothing, &[COPY], "1A2S:8/16/4 => Copy 8/16/4"),
        // An owner whose probe of rank 0 (delegated or copy-only) is unanswered leaves the fetch to
        // rank 0; a later predecessor's copy still wins.
        ("rank 0 silent", owner(1), budget(8, 32), nothing, &[TIMEOUT], "0A2F:2/4/4 => Exhausted { funding: false } 8/32/8 r0"),
        ("rank 0 silent, copy-only", owner(1), budget(2, 16), nothing, &[TIMEOUT], "0C2S:2/16/4 => Exhausted { funding: false } 2/16/8 r0"),
        ("rank 0 silent, copy", owner(2), budget(8, 32), nothing, &[TIMEOUT, COPY], "0A3F:2/4/4 1C2S:8/32/8 => Copy 8/32/8 r0"),
        ("rank 0 silent, miss", owner(2), budget(8, 32), nothing, &[TIMEOUT, MISS], "0A3F:2/4/4 1C2S:8/32/8 => Exhausted { funding: false } 8/32/8 r0"),
        // Only an owner's probe of rank 0 timing out withholds origin.
        ("rank 0 unreachable", owner(1), budget(8, 32), nothing, &[T_UNR], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("rank 0 overloaded", owner(1), budget(8, 32), nothing, &[OVL], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/8"),
        ("rank 0 miss", owner(1), budget(8, 32), nothing, &[MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        ("rank 0 shed", owner(1), budget(8, 32), nothing, &[SHED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/4"),
        ("rank 1 silent", owner(2), budget(8, 32), nothing, &[MISS, TIMEOUT], "0A3F:2/4/4 1C2S:8/32/4 => Origin { rank: 2, evidence: [CopyMiss, Unreachable] } 8/32/8"),
        ("metadata silent", with(owner(1), &|c| c.page_operation = false), budget(8, 32), nothing, &[TIMEOUT], "0C2S:8/32/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("handoff silent", with(owner(0), &|c| c.handoff = 1), budget(8, 32), nothing, &[TIMEOUT], "3C2Sh:8/32/4 => Origin { rank: 0, evidence: [] } 8/32/8"),
        // For a reader a timeout is `Transport(Unreachable)` (same trace).
        ("reader silent", reader(2), budget(32, 96), nothing, &[TIMEOUT], "0A2S:32/96/4 1A1S:32/96/8 => Exhausted { funding: false } 32/96/8"),
        ("reader unreachable", reader(2), budget(32, 96), nothing, &[T_UNR], "0A2S:32/96/4 1A1S:32/96/8 => Exhausted { funding: false } 32/96/8"),
        // The rank 0 watch: `Fetching` keeps the timeout rule and nothing else changes; `Overdue`
        // gives authority after a timeout. Probes are planned the same in every state, and
        // readers and rank 0 ignore it.
        ("fetching, silent", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[TIMEOUT], "0A2F:2/4/4 => Exhausted { funding: false } 8/32/8 r0"),
        ("fetching, unreachable", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[T_UNR], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("fetching, transport overload", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[T_OVL], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/8"),
        ("fetching, overloaded", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[OVL], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/8"),
        ("fetching, shed", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[SHED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/4"),
        ("fetching, miss", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        ("fetching, relay unreachable", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[UNR], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("overdue, silent", rank0(1, Rank0State::Overdue), budget(8, 32), nothing, &[TIMEOUT], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("overdue, rank 2", rank0(2, Rank0State::Overdue), budget(8, 32), nothing, &[TIMEOUT, MISS], "0A3F:2/4/4 1C2S:8/32/8 => Origin { rank: 2, evidence: [Unreachable, CopyMiss] } 8/32/8"),
        ("reader, fetching", with(reader(3), &|c| c.rank0 = Rank0State::Fetching), budget(8, 32), nothing, &[TIMEOUT], "0A3S:8/32/4 1A2S:8/32/8 2A1S:8/32/8 => Exhausted { funding: false } 8/32/8"),
        ("reader, overdue", with(reader(3), &|c| c.rank0 = Rank0State::Overdue), budget(8, 32), nothing, &[TIMEOUT], "0A3S:8/32/4 1A2S:8/32/8 2A1S:8/32/8 => Exhausted { funding: false } 8/32/8"),
        ("reader, unknown", reader(3), budget(8, 32), nothing, &[TIMEOUT], "0A3S:8/32/4 1A2S:8/32/8 2A1S:8/32/8 => Exhausted { funding: false } 8/32/8"),
        ("rank 0, fetching", rank0(0, Rank0State::Fetching), budget(8, 32), nothing, &[TIMEOUT], "=> Origin { rank: 0, evidence: [] } 8/32/4"),
        ("rank 0, overdue", rank0(0, Rank0State::Overdue), budget(8, 32), nothing, &[TIMEOUT], "=> Origin { rank: 0, evidence: [] } 8/32/4"),
        ("rank 0, unknown", owner(0), budget(8, 32), nothing, &[TIMEOUT], "=> Origin { rank: 0, evidence: [] } 8/32/4"),
        // Successors: on their own grant, before a delegated rank 0's origin; an unreachable one
        // is a miss without a route failure; a copy ends the walk; the walk's budget never grows.
        ("successor", with(owner(0), &|c| { c.handoff = 2; c.granted = 1; }), budget(1, 1), nothing, &[MISS], "3C7Fhg:1/8/4 => Origin { rank: 0, evidence: [] } 1/1/1"),
        ("successor copy", with(owner(0), &|c| { c.handoff = 1; c.granted = 1; }), budget(1, 1), nothing, &[COPY], "3C6Fhg:1/8/4 => Copy 1/1/1"),
        ("successor unreachable", with(owner(0), &|c| { c.handoff = 2; c.granted = 2; }), budget(1, 10), nothing, &[STALE, STALE, STALE, T_UNR, STALE], "3C7Fhg:1/8/4 4C6Fhg:1/8/4 => Origin { rank: 0, evidence: [] } 1/10/4"),
        ("successor after delegation", with(owner(1), &|c| { c.handoff = 1; c.granted = 1; }), budget(3, 8), nothing, &[UNR, MISS], "0A3F:2/4/4 3C6Fhg:1/8/4 => Origin { rank: 1, evidence: [Unreachable] } 3/8/8"),
        ("reader, no successors", with(reader(3), &|c| c.granted = 1), budget(32, 96), nothing, &[MISS], "0A3S:32/96/4 1A2S:32/96/4 2A1S:32/96/4 => Exhausted { funding: false } 32/96/4"),
        // Granted lineage targets of a delegated rank 0 keep the origin credit.
        ("granted lineage", with(owner(0), &|c| { c.handoff = 3; c.granted = 2; }), budget(1, 1), nothing, &[MISS], "3C8Fhg:1/8/4 4C7Fhg:1/8/4 => Origin { rank: 0, evidence: [] } 1/1/1"),
        ("granted lineage copy", with(owner(0), &|c| { c.handoff = 3; c.granted = 2; }), budget(1, 1), nothing, &[MISS, MISS, MISS, MISS, COPY], "3C8Fhg:1/8/4 4C7Fhg:1/8/4 => Copy 1/1/1"),
        // Granted targets timing out never withhold origin; rank 0's timeout still does.
        ("granted silent", with(owner(1), &|c| { c.handoff = 2; c.granted = 2; }), budget(8, 32), nothing, &[MISS, TIMEOUT], "0A4F:5/4/4 3C7Fhg:1/8/4 4C6Fhg:1/8/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        ("granted silent, fetching", with(rank0(1, Rank0State::Fetching), &|c| { c.handoff = 2; c.granted = 2; }), budget(8, 32), nothing, &[MISS, TIMEOUT], "0A4F:5/4/4 3C7Fhg:1/8/4 4C6Fhg:1/8/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        ("granted, rank 0 silent", with(owner(1), &|c| { c.handoff = 2; c.granted = 2; }), budget(8, 32), nothing, &[TIMEOUT, MISS], "0A4F:5/4/4 3C7Fhg:1/8/4 4C6Fhg:1/8/4 => Exhausted { funding: false } 8/32/8 r0"),
        ("granted, rank 0 silent, fetching", with(rank0(1, Rank0State::Fetching), &|c| { c.handoff = 2; c.granted = 2; }), budget(8, 32), nothing, &[TIMEOUT, MISS], "0A4F:5/4/4 3C7Fhg:1/8/4 4C6Fhg:1/8/4 => Exhausted { funding: false } 8/32/8 r0"),
        // A relay's `Overloaded` on an owner's route to rank 0 gives authority after one probe,
        // without a retry; never a reader's.
        ("relay overload, rank 1", owner(1), budget(owner_funding_min(1), 8), nothing, &[OVL, MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 3/8/8"),
        ("relay overload, rank 2", owner(2), budget(owner_funding_min(2), 8), nothing, &[OVL, MISS], "0A3F:2/4/4 1C2S:4/8/8 => Origin { rank: 2, evidence: [Overloaded, CopyMiss] } 4/8/8"),
        ("relay overload, rank 1, fetching", rank0(1, Rank0State::Fetching), budget(owner_funding_min(1), 8), nothing, &[OVL, MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 3/8/8"),
        ("relay overload, rank 2, fetching", rank0(2, Rank0State::Fetching), budget(owner_funding_min(2), 8), nothing, &[OVL, MISS], "0A3F:2/4/4 1C2S:4/8/8 => Origin { rank: 2, evidence: [Overloaded, CopyMiss] } 4/8/8"),
        ("relay overload, reader", reader(3), budget(8, 16), nothing, &[OVL], "0A3S:8/16/4 1A2S:8/16/8 2A1S:8/16/8 => Exhausted { funding: false } 8/16/8"),
        // Rank 0 answering that its own acquisition failed withholds origin only while this
        // owner's own origin request failed recently; probes match `Probe(Unreachable)`.
        ("rank 0 failed", rank0(1, Rank0State::Failed), budget(8, 32), nothing, &[FAILED], "0A2F:2/4/4 => Exhausted { funding: false } 8/32/8 r0"),
        ("rank 0 failed, copy", rank0(2, Rank0State::Failed), budget(8, 32), nothing, &[FAILED, COPY], "0A3F:2/4/4 1C2S:8/32/8 => Copy 8/32/8 r0"),
        ("rank 0 failed, copy-only", rank0(1, Rank0State::Failed), budget(2, 16), nothing, &[FAILED], "0C2S:2/16/4 => Exhausted { funding: false } 2/16/8 r0"),
        ("rank 0 failed, unknown", rank0(1, Rank0State::Unknown), budget(8, 32), nothing, &[FAILED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("rank 0 failed, fetching", rank0(1, Rank0State::Fetching), budget(8, 32), nothing, &[FAILED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("rank 0 failed, overdue", rank0(1, Rank0State::Overdue), budget(8, 32), nothing, &[FAILED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("failed state, unreachable", rank0(1, Rank0State::Failed), budget(8, 32), nothing, &[T_UNR], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("failed state, relay unreachable", rank0(1, Rank0State::Failed), budget(8, 32), nothing, &[UNR], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Unreachable] } 8/32/8"),
        ("failed state, miss", rank0(1, Rank0State::Failed), budget(8, 32), nothing, &[MISS], "0A2F:2/4/4 => Origin { rank: 1, evidence: [CopyMiss] } 8/32/4"),
        ("failed state, shed", rank0(1, Rank0State::Failed), budget(8, 32), nothing, &[SHED], "0A2F:2/4/4 => Origin { rank: 1, evidence: [Overloaded] } 8/32/4"),
        ("rank 2 failed", rank0(2, Rank0State::Failed), budget(8, 32), nothing, &[FAILED, MISS], "0A3F:2/4/4 1C2S:8/32/8 => Exhausted { funding: false } 8/32/8 r0"),
        ("rank 2 relay unreachable", rank0(2, Rank0State::Failed), budget(8, 32), nothing, &[UNR, MISS], "0A3F:2/4/4 1C2S:8/32/8 => Origin { rank: 2, evidence: [Unreachable, CopyMiss] } 8/32/8"),
        ("reader failed", with(reader(3), &|c| c.rank0 = Rank0State::Failed), budget(16, 64), nothing, &[FAILED], "0A3S:16/64/4 1A2S:16/64/8 2A1S:16/64/8 => Exhausted { funding: false } 16/64/8"),
        // Rank 2 with inherited credits: rank 0 behind, rank 1 misses; the origin credit is left.
        ("inherited owner", owner(2), { let deadline = Instant::now() + Duration::from_secs(60); inherited_budget(deadline, deadline, 3, 7, 1).unwrap() }, nothing, &[BEHIND, MISS], "0C3S:1/5/5 1C2S:3/6/6 => Origin { rank: 2, evidence: [Unreachable, CopyMiss] } 3/6/6"),
    ];
    let mut wrong = String::new();
    for (name, cfg, b, spend, script, expected) in rows {
        let actual = trace(cfg, b, spend, script);
        if actual != expected {
            writeln!(wrong, "        // {name}\n        \"{actual}\",").unwrap();
        }
    }
    assert!(wrong.is_empty(), "\n{wrong}");
}

/// The first probe of a walk: `{canonical}{mode}{route}`, what the walk
/// kept, and the probe's request budget.
fn first_probe(cfg: WalkConfig, mut b: AcquisitionBudget) -> String {
    let mut walk = CandidateWalk::new(cfg);
    let Step::Probe(p) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    let kept = (b.remaining_attempts(), b.remaining_links());
    let request = walk.request_budget(&mut b);
    let (a, l) = (request.remaining_attempts(), request.remaining_links());
    format!(
        "{}{:?}{:?} kept {}/{} child {a}/{l}",
        p.canonical, p.mode, p.route, kept.0, kept.1
    )
}

#[test]
fn first_probes() {
    let with = |cfg: WalkConfig, handoff: usize, lineage: [u8; 3]| WalkConfig {
        handoff,
        lineage,
        ..cfg
    };
    let failure = {
        let mut b = budget(6, 5);
        b.note_route_failure();
        b
    };
    let deadline = Instant::now() + Duration::from_secs(60);
    let inherited = inherited_budget(deadline, deadline, 4, 8, 1).unwrap();
    assert_eq!(
        (inherited.remaining_attempts(), inherited.remaining_links()),
        (4, 7)
    );
    // Readers fund rank 1 visited first by `owner_funding(1) + 1` per lineage target with slack:
    // 11 = 4 + 1 + 4 + 2 keeps the later visits' minimums.
    assert_eq!(
        owner_funding(1) + 1 + owner_funding_min(2) + owner_funding_min(0),
        11
    );
    let slack = |lineage| with(config(None, 3, order_120()), 0, lineage);
    #[rustfmt::skip]
    let rows = [
        // Handoff targets never block the delegation: the owner keeps one credit per later
        // predecessor and one for its origin.
        (with(owner(1), 0, [0; 3]), budget(3, 16), "0AcquireFundedRoute kept 1/12 child 2/4"),
        (with(owner(1), 1, [0; 3]), budget(3, 16), "0AcquireFundedRoute kept 1/12 child 2/4"),
        (with(owner(1), 2, [0; 3]), budget(3, 16), "0AcquireFundedRoute kept 1/12 child 2/4"),
        (with(owner(1), 3, [0; 3]), budget(3, 16), "0AcquireFundedRoute kept 1/12 child 2/4"),
        (with(owner(2), 0, [0; 3]), budget(4, 16), "0AcquireFundedRoute kept 2/12 child 2/4"),
        (with(owner(2), 1, [0; 3]), budget(4, 16), "0AcquireFundedRoute kept 2/12 child 2/4"),
        (with(owner(2), 2, [0; 3]), budget(4, 16), "0AcquireFundedRoute kept 2/12 child 2/4"),
        (with(owner(2), 3, [0; 3]), budget(4, 16), "0AcquireFundedRoute kept 2/12 child 2/4"),
        // A delegation forwards spare credits (up to `MAX_HANDOFF`) for rank 0's handoff or
        // lineage targets, never this owner's origin credit.
        (with(owner(1), 0, [0; 3]), budget(8, 16), "0AcquireFundedRoute kept 6/12 child 2/4"),
        (with(owner(1), 1, [0; 3]), budget(4, 16), "0AcquireFundedRoute kept 1/12 child 3/4"),
        (with(owner(1), 2, [0; 3]), budget(6, 16), "0AcquireFundedRoute kept 1/12 child 5/4"),
        (with(owner(1), 3, [0; 3]), budget(16, 16), "0AcquireFundedRoute kept 11/12 child 5/4"),
        (with(owner(1), 0, [1, 0, 0]), budget(3, 16), "0AcquireFundedRoute kept 1/12 child 2/4"),
        (with(owner(1), 0, [1, 0, 0]), budget(4, 16), "0AcquireFundedRoute kept 1/12 child 3/4"),
        (with(owner(1), 0, [2, 0, 0]), budget(8, 16), "0AcquireFundedRoute kept 4/12 child 4/4"),
        (with(owner(1), 0, [2, 0, 0]), budget(4, 16), "0AcquireFundedRoute kept 1/12 child 3/4"),
        // A reader's out-of-order Acquire carries one more credit per lineage target of the
        // destination and of rank 0 (not of another later rank), only from slack.
        (slack([0, 1, 0]), budget(32, 16), "1AcquireFundedRoute kept 27/8 child 5/8"),
        (slack([2, 0, 1]), budget(32, 16), "1AcquireFundedRoute kept 26/8 child 6/8"),
        (slack([1, 1, 0]), budget(32, 16), "1AcquireFundedRoute kept 26/8 child 6/8"),
        (slack([0, 0, 1]), budget(32, 16), "1AcquireFundedRoute kept 28/8 child 4/8"),
        (slack([0, 1, 0]), budget(11, 16), "1AcquireFundedRoute kept 6/8 child 5/8"),
        (slack([0, 1, 0]), budget(10, 16), "1AcquireFundedRoute kept 6/8 child 4/8"),
        (slack([0, 1, 0]), budget(8, 16), "1AcquireFundedRoute kept 4/8 child 4/8"),
        (slack([0, 0, 0]), budget(8, 16), "1AcquireFundedRoute kept 4/8 child 4/8"),
        // The delegation route comes first: four of five failure-route links, one for rank 1.
        (owner(2), failure, "0AcquireFundedRoute kept 4/1 child 2/4"),
        // Handoff targets take the remaining links in order.
        (with(owner(0), 3, [0; 3]), budget(8, 3), "3CopyOnlySharedRoute kept 8/3 child 8/3"),
        // A reader reached by a funded route funds an out-of-order rank 2 with the attempts it
        // needs and a shared route's links; budgets never grow.
        (first(2), inherited, "2AcquireFundedRoute kept 0/0 child 4/7"),
    ];
    let mut wrong = String::new();
    for (cfg, b, expected) in rows {
        let actual = first_probe(cfg, b);
        if actual != expected {
            writeln!(wrong, "        {actual:?},").unwrap();
        }
    }
    assert!(wrong.is_empty(), "\n{wrong}");
    let mut b = inherited_budget(deadline, deadline, 4, 8, 1).unwrap();
    let mut walk = CandidateWalk::new(first(2));
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!()
    };
    let plan = plan(walk.request_budget(&mut b), deadline, &probe).unwrap();
    assert_eq!((plan.links, plan.remote_attempts), (7, 3));
}

#[test]
fn pure_rules() {
    // Owner funding: rank 0 the exchange and its origin; rank r > 0 the exchange, an Acquire to
    // rank 0 (two), r - 1 copy-only probes and its origin; without delegation one copy-only probe
    // per predecessor.
    assert_eq!(
        [owner_funding(0), owner_funding(1), owner_funding(2)],
        [2, 4, 5]
    );
    assert_eq!(
        [
            owner_funding_min(0),
            owner_funding_min(1),
            owner_funding_min(2)
        ],
        [2, 3, 4]
    );
    assert_eq!(order_120().ranks().collect::<Vec<_>>(), [1, 2, 0]);
    assert_eq!(first(1).order.get(0), Some(1));
    assert_eq!(first(2).order.get(0), Some(2));
    assert_eq!(budget(32, 96).route_links(), 4);
    const { assert!(HELPER_ATTEMPTS < HELPER_MIN_ATTEMPTS) };
    // Handoff targets skip this node, its predecessors and unchanged sets.
    assert!(handoff_targets(&[1, 2, 3], &[2, 1, 3], 0).is_empty());
    assert_eq!(handoff_targets(&[1, 2, 3], &[9, 1, 2], 0), [1, 2, 3]);
    assert_eq!(handoff_targets(&[1, 2, 3], &[9, 1, 2], 1), [2, 3]);
    assert_eq!(handoff_targets(&[1, 2, 3], &[2, 3, 4], 2), [1]);
    assert_eq!(handoff_targets(&[1], &[1, 2], 1), Vec::<i32>::new());
    assert_eq!(handoff_targets::<i32>(&[], &[1], 0), Vec::<i32>::new());
    // Predecessors that held the same rank.
    for (previous, current, rank, bits) in [
        (&[1, 2, 3][..], &[1, 2, 3][..], 2, 0b11),
        (&[1, 2, 3], &[1, 2, 3], 0, 0),
        (&[1, 2, 3], &[9, 1, 2], 2, 0),
        (&[1, 2, 3], &[1, 3, 4], 2, 0b01),
        (&[1], &[1, 2, 3], 2, 0b01),
        (&[], &[1, 2], 1, 0),
    ] {
        assert_eq!(same_rank_previous(previous, current, rank), bits);
    }
    // Each epoch refreshes at most once, and only to a strictly newer membership.
    for (retried, previous, latest, action) in [
        (true, 1, Some(2), Ok(StaleAction::Skip)),
        (false, 2, Some(2), Ok(StaleAction::Skip)),
        (false, 3, Some(2), Ok(StaleAction::Skip)),
        (false, 2, None, Err(Error::Unavailable)),
        (false, 2, Some(3), Ok(StaleAction::Refresh)),
    ] {
        assert_eq!(stale_action(retried, previous, latest), action);
    }
    // A reader falls back once, with a recent previous membership that lists it.
    for (owner, retried, recent, fallback) in [
        (false, false, true, true),
        (false, true, true, false),
        (false, false, false, false),
        (true, false, true, false),
    ] {
        assert_eq!(previous_fallback(owner, retried, recent), fallback);
    }
    // A reader that cannot fall back waits a third of what is left, within bounds, twice; a
    // second wait must be affordable. Two waits cover the 5 s propagation window of a 10 s read.
    let (secs, ms) = (Duration::from_secs, Duration::from_millis);
    for (waits, affordable, remaining, wait) in [
        (0, true, secs(9), Some(secs(3))),
        (0, false, secs(9), Some(secs(3))),
        (2, true, secs(5), None),
        (0, true, secs(60), Some(MAX_STALE_WAIT)),
        (0, true, ms(900), Some(ms(300))),
        (MAX_STALE_WAITS, true, secs(9), None),
        (1, false, secs(9), None),
        (0, true, ms(749), None),
        (0, false, Duration::ZERO, None),
        (0, true, ms(750), Some(MIN_STALE_WAIT)),
    ] {
        assert_eq!(
            stale_wait(waits, affordable, remaining),
            wait,
            "{waits} {remaining:?}"
        );
    }
    let first_wait = stale_wait(0, true, secs(10)).unwrap();
    let second = stale_wait(1, true, secs(10) - first_wait).unwrap();
    assert!(first_wait + second >= ms(5500));
    // Hedge eligibility: (page, owner, owners, first, second, attempts, links).
    for (page, owner, owners, first, second, attempts, links, eligible) in [
        (true, false, &[1, 2, 3][..], 0, 1, 6, 10, true),
        (true, false, &[1, 2, 3], 0, 1, 5, 10, false),
        (true, false, &[1, 2, 3], 0, 1, 6, 9, false),
        (true, false, &[1, 2, 3], 1, 1, 6, 10, false),
        (true, true, &[1, 2, 3], 0, 1, 6, 10, false),
        (true, false, &[1], 0, 0, 6, 10, false),
        (true, false, &[1, 1, 3], 0, 1, 6, 10, false),
        (false, false, &[1, 2, 3], 0, 1, 6, 10, false),
    ] {
        let b = budget(attempts, links);
        assert_eq!(
            hedge_eligible(page, owner, owners, first, second, &b),
            eligible
        );
    }
    let mut b = budget(6, 10);
    let (secondary, primary) = hedge_budgets(&mut b).unwrap();
    for (b, expected) in [(secondary, (1, 1)), (primary, (2, 1)), (b, (3, 8))] {
        assert_eq!((b.remaining_attempts(), b.remaining_links()), expected);
    }
    assert!(hedge_fallback(Error::CorruptRecord));
    assert!(!hedge_fallback(Error::Unauthorized));
    assert!(!hedge_fallback(Error::Cancelled));
    // A local hop budget shortfall is evidence, not terminal.
    for (error, outcome) in [
        (Error::Os(1), Some(ProbeOutcome::Unreachable)),
        (Error::Overloaded, Some(ProbeOutcome::Overloaded)),
        (Error::Unauthorized, None),
        (Error::DeadlineExceeded, None),
        (Error::HopBudgetExhausted, Some(ProbeOutcome::Unreachable)),
    ] {
        assert_eq!(transport_outcome(error), outcome);
    }
    // Funded links: a full route, else a shared route's four, never the owner's last link;
    // failure routing (inherited from a funded route) takes all.
    for (links, funded) in [
        (16, Some(MAX_ROUTE_LINKS)),
        (8, Some(MAX_ROUTE_LINKS)),
        (7, Some(4)),
        (3, Some(3)),
        (2, Some(2)),
        (1, None),
        (0, None),
    ] {
        assert_eq!(funded_links(&budget(4, links)), funded, "{links}");
    }
    let deadline = Instant::now() + Duration::from_secs(60);
    let b = inherited_budget(deadline, deadline, 4, 8, 1).unwrap();
    assert_eq!(funded_links(&b), Some(7));
    // A shared share is short only below an owner's minimum.
    for (attempts, canonical, visits, short) in [
        (2, 0, 3, false),
        (4, 1, 2, false),
        (3, 1, 2, true),
        (4, 2, 1, false),
        (3, 2, 1, true),
    ] {
        assert_eq!(
            shared_share_short(&budget(attempts, 8), canonical, visits),
            short
        );
    }
}

#[test]
fn target_rules() {
    let owners = ["a", "b", "c"];
    // Later owners that arrived earlier are lineage targets; equal or unknown arrivals never are.
    // `a` joined in generation 5 above `b` (0) and `c` (3).
    let since = [Some(5), Some(0), Some(3)];
    let together = [Some(0); 3];
    for (owners, since, rank, targets) in [
        (&owners[..], &since[..], 0, &["b", "c"][..]),
        (&owners, &since, 1, &[]),
        (&owners, &since, 2, &[]),
        (&owners, &since, 3, &[]),
        (&owners, &together, 0, &[]),
        (&owners, &[None, Some(0), Some(0)], 0, &[]),
        (&owners, &[Some(2), None, Some(1)], 0, &["c"]),
        (&owners[..2], &since[..2], 0, &["b"]),
    ] {
        assert_eq!(*lineage_targets(owners, since, rank), *targets);
    }
    for (since, counts) in [
        (&since[..], [2, 0, 0]),
        (&together, [0; 3]),
        (&[Some(2), None, Some(1)], [1, 0, 0]),
        (&since[..1], [0; 3]),
        (&[], [0; 3]),
    ] {
        assert_eq!(lineage_counts(since), counts);
    }
    // Lineage targets first, without duplicates, version - 1 targets only below `MAX_HANDOFF`.
    for (previous, lineage, merged) in [
        (&["x"][..], &["b", "x", "c"][..], &["b", "x", "c"][..]),
        (&["x", "y", "z"], &["b"], &["b", "x", "y"]),
        (&[], &["b"], &["b"]),
        (&["x", "y"], &[], &["x", "y"]),
        (&["x", "y"], &["b", "c"], &["b", "c", "x"]),
        (&[], &[], &[]),
        (
            &["x", "y"],
            &["l1", "d3", "l2", "l3"],
            &["l1", "d3", "l2", "l3"],
        ),
    ] {
        assert_eq!(*merge_handoff(previous, lineage), *merged);
    }
    let handoff = merge_handoff(&["p1", "p2"], &["l1", "d3"]);
    assert_eq!(handoff, ["l1", "d3", "p1"]);
    for (handoff, granted, prefix) in [
        (&["s", "l", "p"][..], &["s", "l"][..], 2),
        (&["p", "l"], &["l"], 0),
        (&[], &["l"], 0),
        (&handoff, &["l1", "d3"], 2),
    ] {
        assert_eq!(granted_prefix(handoff, granted), prefix);
    }
    // Demoted members that were owners when they arrived. Owners arrived in 5, 6, 7.
    let d = ["d3", "d4"];
    let new = [Some(5), Some(6), Some(7)];
    for (since, demoted_since, rank, targets) in [
        (&new[..], &[Some(0), Some(1)][..], 0, &["d3", "d4"][..]),
        // Rank 2 is the newest owner: same targets.
        (&new, &[Some(0), Some(1)], 2, &["d3", "d4"]),
        // An owner that arrived before a demoted member never displaced it.
        (&new, &[Some(5), Some(1)], 0, &["d4"]),
        // Three members above d4 were present when it arrived.
        (
            &[Some(9), Some(2), Some(2)],
            &[Some(0), Some(3)],
            0,
            &["d3"],
        ),
        // Unknown arrivals never add a target and count as present above.
        (&[None, Some(1), Some(1)], &[Some(0), Some(0)], 0, &[]),
        (&[Some(9), None, None], &[Some(0), None], 0, &["d3"]),
        (&[Some(9), None, None], &[Some(0), Some(0)], 0, &["d3"]),
        // Members that arrived together pay nothing; nor does an unknown own arrival.
        (&[Some(0); 3], &[Some(0); 2], 0, &[]),
        (&[None, Some(3), Some(3)], &[Some(0); 2], 0, &[]),
    ] {
        assert_eq!(
            *demoted_targets(since, &d, demoted_since, rank, None),
            *targets
        );
    }
    assert!(demoted_targets::<&str>(&[Some(3); 3], &[], &[], 0, None).is_empty());
    // Past the third target, up to `MAX_LINEAGE`: owners arrived in 10, 11, 12; d3 at 0, d4 at 1,
    // d5 at 2 qualify, d6 at 3 had three members above. In reverse every one qualifies.
    let since = [Some(10), Some(11), Some(12)];
    let demoted: Vec<u32> = (3..12).collect();
    let forward: Vec<_> = (0..7).map(Some).collect();
    let reverse: Vec<_> = (0..7).rev().map(Some).collect();
    assert_eq!(
        demoted_targets(&since, &demoted[..7], &forward, 0, None),
        [3, 4, 5]
    );
    let targets = demoted_targets(&since, &demoted[..7], &reverse, 0, None);
    assert_eq!(*targets, demoted[..MAX_LINEAGE]);
    assert_eq!(*merge_handoff(&[90, 91], &targets), *targets);
    assert_eq!(merge_handoff(&[90, 91], &[3]), [3, 90, 91]);
    assert_eq!(merge_handoff(&[90, 91], &[3, 4, 5, 6]), [3, 4, 5, 6]);
    // Unknown ages (the chain's start, 5) qualify in rank order when this owner arrived later,
    // within `MAX_LINEAGE`; a known arrival keeps the arrival rule; the chain start must match;
    // an owner as old as the chain displaced no one.
    let since = [Some(8), Some(5), Some(5)];
    let start = [Some(5); 9];
    let mixed = [Some(6), Some(6), Some(6), Some(5), Some(5)];
    assert_eq!(demoted_targets(&since, &demoted, &start, 0, None), [3]);
    let targets = demoted_targets(&since, &demoted, &start, 0, Some(5));
    assert_eq!(*targets, demoted[..MAX_LINEAGE]);
    assert_eq!(
        demoted_targets(&since, &demoted[..5], &mixed, 0, Some(5)),
        [3, 6, 7]
    );
    assert_eq!(demoted_targets(&since, &demoted, &start, 0, Some(4)), [3]);
    assert!(demoted_targets(&[Some(5); 3], &demoted, &start, 0, Some(5)).is_empty());
    assert!(demoted_targets(&since, &demoted, &start, 1, Some(5)).is_empty());
}

#[test]
fn remaining_walk() {
    // (rank, owners, outcomes, probes `(canonical, opportunities)`, end, route links after)
    for (rank, owners, outcomes, probes, end, route) in [
        (
            0,
            3,
            &[T_OVL, MISS][..],
            &[(1, 2), (2, 1)][..],
            RemainingEnd::Unavailable,
            8,
        ),
        (1, 3, &[VU], &[(2, 1)], RemainingEnd::None, 4),
        (0, 2, &[COPY], &[(1, 1)], RemainingEnd::Copy, 4),
        (2, 3, &[], &[], RemainingEnd::None, 4),
        (0, 2, &[BEHIND], &[(1, 1)], RemainingEnd::Unavailable, 4),
        (0, 2, &[TIMEOUT], &[(1, 1)], RemainingEnd::Unavailable, 8),
        (0, 2, &[FAILED], &[(1, 1)], RemainingEnd::Unavailable, 4),
    ] {
        let mut walk = RemainingWalk::new(rank, owners);
        let mut b = budget(8, 16);
        let mut seen = Vec::new();
        let mut last = None;
        for outcome in outcomes {
            let p = walk.next_probe().unwrap();
            assert_eq!(p.mode, Mode::CopyOnly);
            seen.push((p.canonical, p.remaining_opportunities));
            last = walk.complete(&mut b, *outcome);
        }
        assert_eq!(seen, probes);
        assert_eq!(last.unwrap_or_else(|| walk.next_probe().unwrap_err()), end);
        assert_eq!(b.route_links(), route);
    }
}

#[test]
fn attempt_plan_shares_deadline_and_reserves_credits() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(12);
    let mut b = AcquisitionBudget::new(deadline, 9, 16);
    let inputs = AttemptInputs {
        now,
        caller_deadline: deadline,
        remaining_opportunities: 3,
        route: RouteMode::SharedRoute,
        mode: Mode::Acquire,
        attempt_timeout: Duration::from_secs(60),
        failure_links: 8,
    };
    let plan = plan_attempt(&mut b, inputs).unwrap();
    assert_eq!(plan.links, 4);
    assert_eq!(plan.overall, deadline);
    assert_eq!(plan.share, now + Duration::from_secs(4));
    assert_eq!(plan.attempt_end, deadline);
    // 8 attempts after the sender's, ceil(8 / 3) = 3 forwarded.
    assert_eq!(plan.remote_attempts, 3);
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (5, 12));
    // Reserve min(share, (overall - share) / 2) = 4s for fallback.
    assert_eq!(plan.complete_by, deadline - Duration::from_secs(4));
    assert_eq!(plan.observation, Duration::from_secs(4));
    let hedge = AttemptInputs {
        route: RouteMode::DirectHedge,
        mode: Mode::CopyOnly,
        remaining_opportunities: 1,
        ..inputs
    };
    let hedge = plan_attempt(&mut b, hedge).unwrap();
    assert_eq!(hedge.attempt_end, now + Duration::from_secs(4));
    assert_eq!(hedge.remote_attempts, 0);
    assert_eq!(hedge.complete_by, hedge.attempt_end);
    let funded = AttemptInputs {
        route: RouteMode::FundedRoute,
        ..inputs
    };
    assert_eq!(plan_attempt(&mut b, funded).unwrap().remote_attempts, 3);
    assert_eq!(b.remaining_attempts(), 0);
    let mut empty = AcquisitionBudget::new(deadline, 1, 0);
    assert_eq!(
        plan_attempt(&mut empty, inputs),
        Err(Error::HopBudgetExhausted)
    );
    let mut late = AcquisitionBudget::new(now, 1, 8);
    assert_eq!(
        plan_attempt(&mut late, inputs),
        Err(Error::DeadlineExceeded)
    );
}

/// Once the links are spent, each later probe fails locally in `plan_attempt` and is recorded as
/// unreachable; the walk ends `Exhausted` (recoverable) instead of abandoning.
#[test]
fn spent_links_end_the_walk_exhausted() {
    let mut walk = CandidateWalk::new(reader(3));
    let mut b = budget(32, 16);
    b.note_route_failure();
    let deadline = Instant::now() + Duration::from_secs(10);
    let mut ends = Vec::new();
    let (_, end) = drive(&mut walk, &mut b, |probe, request| {
        Outcome::Transport(match plan(request, deadline, probe) {
            // The route was spent on an overloaded relay.
            Ok(_) => ProbeOutcome::Overloaded,
            Err(error) => {
                ends.push(error);
                transport_outcome(error).expect("recoverable")
            }
        })
    });
    assert!(ends.contains(&Error::HopBudgetExhausted), "{ends:?}");
    assert_eq!(end, End::Exhausted { funding: false });
    assert_eq!(b.remaining_links(), 0);
}

/// Abandoning returns a pending child's credits, and drops a pending successor's grant.
#[test]
fn abandon_returns_credits() {
    let mut walk = CandidateWalk::new(first(2));
    let mut b = budget(32, 96);
    assert!(matches!(walk.next(&mut b).unwrap(), Step::Probe(_)));
    assert_eq!(b.remaining_attempts(), 32 - owner_funding(2));
    walk.abandon(&mut b).unwrap();
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (32, 96));
    let mut walk = CandidateWalk::new(WalkConfig {
        handoff: 1,
        granted: 1,
        ..owner(0)
    });
    let mut b = budget(2, 3);
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!("expected a successor probe");
    };
    assert!(
        walk.pending
            .as_ref()
            .is_some_and(|p| p.granted && p.handoff)
    );
    assert_eq!(probe.canonical, 3);
    walk.abandon(&mut b).unwrap();
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (2, 3));
}

/// A destination's own shed is evidence but not a route failure; a transport overload is both.
#[test]
fn destination_shed_is_evidence_but_not_a_route_failure() {
    for (outcome, route) in [(SHED, 4), (T_OVL, 8)] {
        let mut walk = CandidateWalk::new(reader(3));
        let mut b = budget(32, 64);
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("probe")
        };
        assert_eq!(walk.complete(&mut b, outcome).unwrap(), None);
        assert_eq!(&walk.evidence[..], &[ProbeOutcome::Overloaded]);
        assert_eq!(b.route_links(), route, "{outcome:?}");
    }
}

/// A reader waiting for a destination that is behind asks the same destination again, with the
/// credits its verified `StaleMembership` returned, and then walks on.
#[test]
fn retry_pending_plans_the_same_destination_again() {
    let mut walk = CandidateWalk::new(config(None, 3, order_120()));
    let mut b = budget(8, 16);
    let Step::Probe(first) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(first.canonical, 1);
    let deadline = Instant::now() + Duration::from_secs(30);
    let sent = plan(walk.request_budget(&mut b), deadline, &first)
        .unwrap()
        .remote_attempts;
    assert_eq!(sent, owner_funding(1) - 1);
    // The destination was behind: its credits come back, the exchange not.
    walk.request_budget(&mut b).refund_remote(sent).unwrap();
    walk.retry_pending(&mut b).unwrap();
    assert_eq!(b.remaining_attempts(), 7);
    let Step::Probe(again) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(
        (again.index, again.canonical),
        (first.index, first.canonical)
    );
    assert_eq!(
        walk.request_budget(&mut b).remaining_attempts(),
        owner_funding_min(1)
    );
    walk.complete(&mut b, BEHIND).unwrap();
    let Step::Probe(next) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(next.canonical, 2);
}

/// A further ask of a destination that is behind must leave attempts for every later visit's
/// minimum and a full route share of links for each; the last visit needs nothing after it.
#[test]
fn retry_affordable_keeps_later_visits_funded() {
    for (attempts, links, affordable) in [
        (8, 16, false),
        (16, 32, true),
        (16, 8, false),
        (6, 32, false),
    ] {
        let mut walk = CandidateWalk::new(config(None, 3, order_120()));
        let mut b = budget(attempts, links);
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        assert_eq!(walk.retry_affordable(&b), affordable, "{attempts} {links}");
    }
    let mut walk = CandidateWalk::new(config(None, 3, order_120()));
    let mut b = budget(8, 16);
    for _ in 0..2 {
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        walk.complete(&mut b, BEHIND).unwrap();
    }
    let Step::Probe(last) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(last.index, 2);
    assert!(walk.retry_affordable(&b));
}

/// The bound on a read's second stale wait. Every exchange reserves its route's links and
/// transport never returns them. With the `stale_unused_links` refund, a default read whose owners
/// are all behind asks each of them and waits once at its first visit; the second wait (a fifth
/// exchange) is affordable only at its last visit, and only when that visit is rank 0 or 1.
#[test]
fn a_default_read_reaches_every_owner_and_waits_again_at_its_last() {
    use crate::read::{PAGE_ATTEMPTS, PAGE_LINKS};
    // Per visit after the first wait, whether a second wait was affordable there.
    let run_with = |owners: usize, attempts: u32, links: u8, hops: Option<usize>, rev: bool| {
        let score = |r: usize| if rev { (owners - r) as u64 } else { r as u64 };
        let order = visit_order(ReplicaSelection::Spread, owners, |_| true, score);
        let mut walk = CandidateWalk::new(config(None, owners, order));
        let mut b = budget(attempts, links);
        let mut waited = false;
        let mut affordable = Vec::new();
        loop {
            let Ok(Step::Probe(probe)) = walk.next(&mut b) else {
                return affordable;
            };
            let deadline = b.deadline();
            let request = walk.request_budget(&mut b);
            let Ok(plan) = plan(request, deadline, &probe) else {
                return affordable;
            };
            request.refund_remote(plan.remote_attempts).unwrap();
            if let Some(hops) = hops {
                let unused = crate::read::budget::stale_unused_links(plan.links, hops);
                request.refund_links(unused).unwrap();
            }
            if !waited {
                waited = true;
                walk.retry_pending(&mut b).unwrap();
                continue;
            }
            affordable.push(walk.retry_affordable(&b));
            if walk.complete(&mut b, BEHIND).unwrap().is_some() {
                return affordable;
            }
        }
    };
    let run = |owners, attempts, links, hops| run_with(owners, attempts, links, hops, false);
    assert_eq!(run(3, PAGE_ATTEMPTS, PAGE_LINKS, None), [false]);
    for hops in 1..=3 {
        let h = Some(hops);
        assert_eq!(run(3, PAGE_ATTEMPTS, PAGE_LINKS, h), [false, false, true]);
        assert_eq!(run(2, PAGE_ATTEMPTS, PAGE_LINKS, h), [true, true]);
        assert_eq!(run(1, PAGE_ATTEMPTS, PAGE_LINKS, h), [true]);
        assert_eq!(
            run_with(3, PAGE_ATTEMPTS, PAGE_LINKS, h, true),
            [false, false, false]
        );
        assert_eq!(
            run(3, 2 * PAGE_ATTEMPTS, 2 * PAGE_LINKS, h),
            [true, true, true]
        );
    }
}

/// Rank 0 asks a successor (own grant), then a lineage target. A `StaleMembership` refund of the
/// successor's remote credits lands in the dropped grant, so the walk's budget never grows; and
/// neither timing out withholds origin.
#[test]
fn successor_and_lineage_probes_neither_grow_the_budget_nor_withhold_origin() {
    let mut walk = CandidateWalk::new(WalkConfig {
        handoff: 2,
        granted: 1,
        lineage: [1, 0, 0],
        ..owner(0)
    });
    let mut b = budget(4, 32);
    let start = (b.remaining_attempts(), b.remaining_links());
    let mut probes = Vec::new();
    let end = loop {
        match walk.next(&mut b).unwrap() {
            Step::Granted(_) => unreachable!("grants are sequential here"),
            Step::Probe(probe) => {
                probes.push(probe.canonical);
                let successor = walk.pending.as_ref().is_some_and(|p| p.granted);
                let request = walk.request_budget(&mut b);
                let outcome = if successor {
                    let sent = request.send_remote(1).unwrap();
                    request.refund_remote(sent).unwrap();
                    BEHIND
                } else {
                    TIMEOUT
                };
                if let Some(end) = walk.complete(&mut b, outcome).unwrap() {
                    break end;
                }
                let now = (b.remaining_attempts(), b.remaining_links());
                assert!(now.0 <= start.0 && now.1 <= start.1, "{now:?} > {start:?}");
            }
            Step::End(end) => break end,
        }
    };
    assert_eq!(probes, [3, 4]);
    assert!(matches!(end, End::Origin { rank: 0, .. }), "{end:?}");
    assert!(!walk.rank0_unanswered);
}

/// Granted probes go out at once, each on its own grant and one shared time share; misses leave
/// the walk exactly as the sequential walk leaves it, and any copy ends it.
#[test]
fn parallel_granted_probes_go_out_at_once_and_match_the_sequential_walk() {
    for rank in [0, 1, 2] {
        for granted in 1..=MAX_TARGETS - 1 {
            let cfg = WalkConfig {
                handoff: granted + 1,
                granted,
                ..owner(rank)
            };
            let script =
                |p: &Probe, _: &mut AcquisitionBudget| if p.canonical < 3 { T_UNR } else { MISS };
            let run = |parallel_granted| {
                let mut walk = CandidateWalk::new(WalkConfig {
                    parallel_granted,
                    ..cfg
                });
                let mut b = budget(8, 16);
                let (probes, end) = drive(&mut walk, &mut b, script);
                let canonical: Vec<_> = probes.iter().map(|(p, ..)| p.canonical).collect();
                (
                    end,
                    canonical,
                    b.remaining_attempts(),
                    b.remaining_links(),
                    probes,
                )
            };
            let (seq, par) = (run(false), run(true));
            assert_eq!(
                (&seq.0, &seq.1, seq.2, seq.3),
                (&par.0, &par.1, par.2, par.3),
                "rank {rank}, {granted} granted"
            );
            // The batch shares the first granted probe's time share; each has the grant's credits.
            let batch: Vec<_> = par
                .4
                .iter()
                .filter(|(p, ..)| p.canonical >= 3)
                .take(granted)
                .collect();
            assert!(batch.iter().all(|(p, a, l, _)| {
                p.remaining_opportunities == batch[0].0.remaining_opportunities
                    && p.mode == Mode::CopyOnly
                    && (*a, *l) == (SUCCESSOR_GRANT_ATTEMPTS, SUCCESSOR_GRANT_LINKS)
            }));
        }
    }
    let mut walk = CandidateWalk::new(WalkConfig {
        handoff: 4,
        granted: 3,
        parallel_granted: true,
        ..owner(0)
    });
    let mut b = budget(1, 1);
    let Step::Granted(batch) = walk.next(&mut b).unwrap() else {
        panic!("expected granted probes");
    };
    assert_eq!(
        batch.iter().map(|p| p.canonical).collect::<Vec<_>>(),
        [3, 4, 5]
    );
    assert!(batch.iter().all(|p| p.mode == Mode::CopyOnly
        && p.remaining_opportunities == batch[0].remaining_opportunities));
    assert_eq!(walk.complete_granted(true), Some(End::Copy));
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (1, 1));
}

#[test]
#[should_panic(expected = "complete the granted probes first")]
fn parallel_granted_probes_must_be_reported_before_the_next_step() {
    let mut walk = CandidateWalk::new(WalkConfig {
        handoff: 2,
        granted: 2,
        parallel_granted: true,
        ..owner(0)
    });
    let mut b = budget(4, 8);
    let Step::Granted(_) = walk.next(&mut b).unwrap() else {
        panic!("expected granted probes");
    };
    let _ = walk.next(&mut b);
}

/// A reader at rank 1 in visit order after rank 0 failed gets the credits rank 1 needs when its
/// share is short; a busy rank 0 (shed, silent, overloaded) keeps rank 1 on its share.
#[test]
fn in_order_owner_funding_after_rank_zero() {
    for (outcome, route, remote) in [
        (UNR, RouteMode::FundedRoute, owner_funding_min(1) - 1),
        (FAILED, RouteMode::FundedRoute, owner_funding_min(1) - 1),
        (T_UNR, RouteMode::FundedRoute, owner_funding_min(1) - 1),
        (SHED, RouteMode::SharedRoute, 1),
        (TIMEOUT, RouteMode::SharedRoute, 1),
        (OVL, RouteMode::SharedRoute, 1),
        (T_OVL, RouteMode::SharedRoute, 1),
    ] {
        let mut walk = CandidateWalk::new(reader(3));
        let mut b = budget(6, 16);
        let Step::Probe(first) = walk.next(&mut b).unwrap() else {
            panic!()
        };
        assert_eq!((first.canonical, first.route), (0, RouteMode::SharedRoute));
        // Rank 0's share is not short.
        assert_eq!(charge(&mut walk, &mut b, &first).remote_attempts, 2);
        walk.complete(&mut b, outcome).unwrap();
        assert_eq!(b.remaining_attempts(), 3);
        assert!(shared_share_short(&b, 1, 2));
        let Step::Probe(second) = walk.next(&mut b).unwrap() else {
            panic!()
        };
        assert_eq!((second.canonical, second.route), (1, route), "{outcome:?}");
        let plan = charge(&mut walk, &mut b, &second);
        assert_eq!(plan.remote_attempts, remote, "{outcome:?}");
        if outcome == UNR {
            assert_eq!(second.mode, Mode::Acquire);
            // Failure routing after the route failure above.
            assert_eq!(plan.links, 8);
            walk.complete(&mut b, UNR).unwrap();
            // Budgets never grow: nothing is left for rank 2.
            assert_eq!(b.remaining_attempts(), 0);
            assert!(matches!(
                walk.next(&mut b).unwrap(),
                Step::Probe(_) | Step::End(_)
            ));
        }
    }
}

/// An owner that shed this node is visited after the others; no hop ever carries more than its
/// sender had, and with enough attempts to fund rank 1 the demoted rank 0 is not asked first.
#[test]
fn a_shed_demoted_rank0_never_grows_budgets_per_hop() {
    let answering = |r: usize| {
        if r == 0 {
            Answering::Unanswered
        } else {
            Answering::Answers
        }
    };
    for links in [7, 16] {
        for attempts in 1..=8 {
            for outcome in [SHED, FAILED, TIMEOUT] {
                let order = demote_unanswered(VisitOrder::canonical(3), answering);
                let mut walk = CandidateWalk::new(config(None, 3, order));
                let mut b = budget(attempts, links);
                let mut visited = Vec::new();
                loop {
                    let before = (b.remaining_attempts(), b.remaining_links());
                    let Ok(Step::Probe(probe)) = walk.next(&mut b) else {
                        break;
                    };
                    let deadline = b.deadline();
                    let Ok(plan) = plan(walk.request_budget(&mut b), deadline, &probe) else {
                        break;
                    };
                    let what = format!("{attempts} {outcome:?}");
                    assert!(plan.remote_attempts < before.0, "{what}");
                    assert!(plan.links <= before.1, "{what}");
                    assert!(b.remaining_attempts() < before.0);
                    visited.push(probe.canonical);
                    if walk.complete(&mut b, outcome).is_err() {
                        break;
                    }
                }
                if attempts >= owner_funding(1) {
                    assert_ne!(visited.first(), Some(&0), "{attempts} {links} {outcome:?}");
                }
            }
        }
    }
}

/// An owner funded at its minimum asks each predecessor copy-only, never delegates, and ends with
/// at most one origin attempt, only with authority: not after a copy, rank 0 left it unanswered,
/// or rank 0 failed while this owner's own origin request failed recently.
#[test]
fn a_minimum_funded_owner_fetches_at_most_once_and_only_with_authority() {
    for rank in [1usize, 2] {
        for (state, rank0, origin) in [
            (Rank0State::Unknown, COPY, false),
            (Rank0State::Unknown, MISS, true),
            (Rank0State::Unknown, T_UNR, true),
            (Rank0State::Unknown, UNR, true),
            (Rank0State::Unknown, FAILED, true),
            (Rank0State::Failed, FAILED, false),
            (Rank0State::Unknown, TIMEOUT, false),
            (Rank0State::Fetching, TIMEOUT, false),
        ] {
            let mut walk = CandidateWalk::new(WalkConfig {
                rank0: state,
                ..owner(rank)
            });
            let mut b = budget(owner_funding_min(rank) - 1, 7);
            let deadline = b.deadline();
            let (probes, end) = drive(&mut walk, &mut b, |p, request| {
                // Spend each probe's exchange as the caller would.
                plan(request, deadline, p).unwrap();
                if p.canonical == 0 { rank0 } else { MISS }
            });
            let what = format!("rank {rank} {state:?} {rank0:?}: {end:?}");
            assert!(
                probes.iter().all(|(p, ..)| p.mode == Mode::CopyOnly),
                "{what}"
            );
            assert!(probes.len() <= rank, "{what}");
            if origin {
                assert!(
                    matches!(end, End::Origin { rank: r, .. } if r == rank),
                    "{what}"
                );
                // Exactly the owner's own origin attempt is left.
                assert_eq!(b.remaining_attempts(), 1, "{what}");
                assert_eq!(probes.len(), rank, "{what}");
            } else {
                assert!(!matches!(end, End::Origin { .. }), "{what}");
            }
        }
    }
}

// ---- Fill: the acquisition as one state machine ----

fn fill(hedge: bool, features: Features) -> Fill {
    Fill::new(FillConfig {
        operation: OperationKind::Page,
        hedge,
        walk_only: false,
        scope: Instant::now() + Duration::from_secs(60),
        features,
    })
}

/// Owners 10, 11, 12 under membership `version`, ranked at `rank`.
fn owners(version: u64, rank: Option<usize>) -> Ranked {
    Ranked {
        members: 3,
        ..Ranked::new(version, &[10, 11, 12], rank)
    }
}

fn answer(kind: ResponseKind) -> Event<'static> {
    Event::Reply(Reply::Answer { kind, signed: true })
}

const START: Event<'static> = Event::Start { local_hit: false };
const NO_HINTS: Event<'static> = Event::Hints {
    hints: &[],
    rank0: Rank0State::Unknown,
};
const UNAVAILABLE: Event<'static> = Event::Reply(Reply::Failed {
    error: Error::Unavailable,
    sent: true,
    scope_live: true,
});

/// Feed `events`; one line per event: the action, then the evidence that is
/// set, then the fill's walked version, waits and handoff targets.
fn script(f: &mut Fill, b: &mut AcquisitionBudget, events: &[Event<'_>]) -> String {
    let mut s = String::new();
    for event in events {
        let (action, e) = f.on(*event, b, Instant::now());
        write!(s, "{action:?}").unwrap();
        if let Some(outcome) = e.outcome {
            write!(s, " outcome={outcome:?}").unwrap();
        }
        if let Some(silence) = e.silence {
            write!(s, " silence={silence:?}").unwrap();
        }
        if let Some(rank0) = e.rank0 {
            write!(s, " rank0={rank0:?}").unwrap();
        }
        for (set, flag) in [
            (e.helper, "helper"),
            (e.granted, "granted"),
            (e.rank0_probe, "rank0_probe"),
        ] {
            if set {
                write!(s, " {flag}").unwrap();
            }
        }
        if e.notes != 0 {
            write!(s, " notes={:#x}", e.notes).unwrap();
        }
        s.push('\n');
    }
    write!(
        s,
        "v{} waits={} handoff={:?}",
        f.version, f.waits, &*f.handoff
    )
    .unwrap();
    s
}

#[test]
fn fills() {
    let owner_walk = |rank| {
        vec![
            START,
            Event::Ranked(Ok(owners(5, Some(rank)))),
            Event::Ranked(Err(Error::Unavailable)),
            NO_HINTS,
        ]
    };
    let then = |mut prefix: Vec<Event<'static>>, rest: &[Event<'static>]| {
        prefix.extend_from_slice(rest);
        prefix
    };
    let reader =
        |rest: &[Event<'static>]| then(vec![START, Event::Ranked(Ok(owners(5, None)))], rest);
    let stale = |latest, previous_recent_member, rest: &[Event<'static>]| {
        reader(&then(
            vec![
                answer(ResponseKind::StaleMembership),
                Event::Membership {
                    latest: Some(latest),
                    previous_recent_member,
                },
            ],
            rest,
        ))
    };
    let origin = |rest: &[Event<'static>]| {
        then(
            owner_walk(0),
            &then(vec![Event::Ranked(Ok(owners(5, Some(0))))], rest),
        )
    };
    // Rank 1 delegates to rank 0, which ends with `rank0`; after an origin version miss the
    // remaining copy-only probe of rank 2 ends with `last`.
    let remaining = |rank0, last: &[Event<'static>]| {
        then(
            owner_walk(1),
            &then(
                vec![
                    rank0,
                    Event::Ranked(Ok(owners(5, Some(1)))),
                    Event::Origin(Err(Error::VersionUnavailable)),
                ],
                last,
            ),
        )
    };
    let production = Features::PRODUCTION;
    let no_wait = Features::PRODUCTION.with(Features::STALE_WAIT, false);
    #[rustfmt::skip]
    let rows: Vec<(&str, bool, Features, Vec<Event<'static>>, &str)> = vec![
        ("local hit", true, production, vec![Event::Start { local_hit: true }], "Done(Ok(Local))\nv0 waits=0 handoff=[]"),
        // A plaintext fill offers a hedge before the walk; a winner is published.
        ("hedge won", true, production, reader(&[Event::Hedge(HedgeEnd::Won), Event::Published(Ok(()))]), "Rank { ranking: Current, version: None }\nHedge { eligible: true, first: 0, second: 1 }\nPublish(Hedge)\nDone(Ok(Hedge))\nv5 waits=0 handoff=[]"),
        ("ciphertext walks at once", false, production, reader(&[]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nv5 waits=0 handoff=[]"),
        // Rank 0 has no predecessor: origin authority after ranking again, then publish or fail.
        ("origin", false, production, origin(&[Event::Origin(Ok(())), Event::Published(Ok(()))]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nRank { ranking: Authority, version: Some(5) }\nOrigin { rank: 0 }\nPublish(Origin)\nDone(Ok(Origin))\nv5 waits=0 handoff=[]"),
        ("publish fails", false, production, origin(&[Event::Origin(Ok(())), Event::Published(Err(Error::Overloaded))]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nRank { ranking: Authority, version: Some(5) }\nOrigin { rank: 0 }\nPublish(Origin)\nDone(Err(Overloaded))\nv5 waits=0 handoff=[]"),
        ("origin fails", false, production, origin(&[Event::Origin(Err(Error::OriginForbidden))]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nRank { ranking: Authority, version: Some(5) }\nOrigin { rank: 0 }\nDone(Err(OriginForbidden))\nv5 waits=0 handoff=[]"),
        // A ranking that changed before origin gives no authority.
        ("ranking changed", false, production, then(owner_walk(0), &[Event::Ranked(Ok(Ranked::new(5, &[11, 10, 12], Some(1))))]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nRank { ranking: Authority, version: Some(5) }\nDone(Err(IncompatibleMembership))\nv5 waits=0 handoff=[]"),
        ("remaining copy", false, production, remaining(UNAVAILABLE, &[answer(ResponseKind::Page), Event::Validated(Ok(None)), Event::Published(Ok(()))]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Transport(Unreachable)) rank0_probe\nOrigin { rank: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: CopyOnly, remaining_opportunities: 1, route: SharedRoute, previous: false }) notes=0x1000\nValidate silence=Answered\nPublish(Peer) outcome=Ok(Copy)\nDone(Ok(Peer))\nv5 waits=0 handoff=[]"),
        ("remaining miss", false, production, remaining(UNAVAILABLE, &[answer(ResponseKind::Miss)]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Transport(Unreachable)) rank0_probe\nOrigin { rank: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: CopyOnly, remaining_opportunities: 1, route: SharedRoute, previous: false }) notes=0x1000\nDone(Err(Unavailable)) outcome=Ok(Probe(CopyMiss)) silence=Answered\nv5 waits=0 handoff=[]"),
        ("remaining unreachable", false, production, remaining(UNAVAILABLE, &[UNAVAILABLE]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Transport(Unreachable)) rank0_probe\nOrigin { rank: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: CopyOnly, remaining_opportunities: 1, route: SharedRoute, previous: false }) notes=0x1000\nDone(Err(Unavailable)) outcome=Ok(Transport(Unreachable))\nv5 waits=0 handoff=[]"),
        ("remaining aborted", false, production, remaining(UNAVAILABLE, &[Event::Abort(Error::Cancelled)]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Transport(Unreachable)) rank0_probe\nOrigin { rank: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: CopyOnly, remaining_opportunities: 1, route: SharedRoute, previous: false }) notes=0x1000\nDone(Err(Cancelled))\nv5 waits=0 handoff=[]"),
        ("version unavailable", false, production, remaining(answer(ResponseKind::Miss), &[answer(ResponseKind::Miss)]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Probe(CopyMiss)) silence=Answered rank0=Answered rank0_probe\nOrigin { rank: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: CopyOnly, remaining_opportunities: 1, route: SharedRoute, previous: false }) notes=0x1000\nDone(Err(VersionUnavailable)) outcome=Ok(Probe(CopyMiss)) silence=Answered\nv5 waits=0 handoff=[]"),
        // A stale answer to the remaining-copy walk is terminal.
        ("remaining stale", false, production, origin(&[Event::Origin(Err(Error::VersionUnavailable)), answer(ResponseKind::StaleMembership)]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nRank { ranking: Authority, version: Some(5) }\nOrigin { rank: 0 }\nProbe(Probe { index: 1, canonical: 1, mode: CopyOnly, remaining_opportunities: 2, route: SharedRoute, previous: false }) notes=0x1000\nDone(Err(IncompatibleMembership)) outcome=Err(IncompatibleMembership) silence=Answered\nv5 waits=0 handoff=[]"),
        // Every owner of a reader unreachable: exhausted. An empty ranking; a terminal error.
        ("exhausted", false, production, reader(&[Event::Reply(Reply::Failed { error: Error::Io, sent: true, scope_live: true }), Event::Reply(Reply::Failed { error: Error::Io, sent: true, scope_live: true }), Event::Reply(Reply::Failed { error: Error::Io, sent: true, scope_live: true })]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nProbe(Probe { index: 1, canonical: 1, mode: Acquire, remaining_opportunities: 2, route: SharedRoute, previous: false }) outcome=Ok(Transport(Unreachable))\nProbe(Probe { index: 2, canonical: 2, mode: Acquire, remaining_opportunities: 1, route: SharedRoute, previous: false }) outcome=Ok(Transport(Unreachable))\nDone(Err(Unavailable)) outcome=Ok(Transport(Unreachable)) notes=0x200\nv5 waits=0 handoff=[]"),
        ("empty ranking", false, production, vec![START, Event::Ranked(Ok(Ranked::new(5, &[], None)))], "Rank { ranking: Current, version: None }\nDone(Err(Unavailable))\nv0 waits=0 handoff=[]"),
        ("terminal", false, production, reader(&[Event::Reply(Reply::Failed { error: Error::Unauthorized, sent: true, scope_live: true })]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nDone(Err(Unauthorized)) outcome=Err(Unauthorized)\nv5 waits=0 handoff=[]"),
        // A stale reader refreshes to a newer membership; behind and a recent member of version
        // - 1 it walks there; owning the page there it waits instead; not a member it waits, and
        // without waits it skips.
        ("refresh", false, production, stale(6, false, &[]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nRefresh silence=Answered\nRank { ranking: Current, version: Some(6) } outcome=Ok(StaleMembership) notes=0x4\nv5 waits=0 handoff=[]"),
        ("fallback", false, production, stale(5, true, &[Event::Ranked(Ok(owners(4, None)))]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nRefresh silence=Answered\nRank { ranking: Fallback, version: Some(4) }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false }) outcome=Ok(StaleDestination) notes=0x9\nv4 waits=0 handoff=[]"),
        ("fallback denied", false, production, stale(5, true, &[Event::Ranked(Ok(owners(4, Some(2)))), Event::Timer]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nRefresh silence=Answered\nRank { ranking: Fallback, version: Some(4) }\nWait(4s) notes=0x30\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nv5 waits=1 handoff=[]"),
        ("wait", false, production, stale(5, false, &[]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nRefresh silence=Answered\nWait(4s)\nv5 waits=1 handoff=[]"),
        ("skip", false, no_wait, stale(5, false, &[]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nRefresh silence=Answered\nProbe(Probe { index: 1, canonical: 1, mode: Acquire, remaining_opportunities: 2, route: SharedRoute, previous: false }) outcome=Ok(StaleDestination) notes=0x41\nv5 waits=0 handoff=[]"),
        // A reader's probe of rank 0 timing out asks a ping; the next owner's own shed is busy,
        // not hung; a relay's `Overloaded` is no silence evidence.
        ("silence", false, production, reader(&[Event::Reply(Reply::TimedOut), answer(ResponseKind::Overloaded), Event::Reply(Reply::Answer { kind: ResponseKind::Overloaded, signed: false })]), "Rank { ranking: Current, version: None }\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 3, route: SharedRoute, previous: false })\nProbe(Probe { index: 1, canonical: 1, mode: Acquire, remaining_opportunities: 2, route: SharedRoute, previous: false }) outcome=Ok(TimedOut) silence=TimedOut { needed: 1 }\nProbe(Probe { index: 2, canonical: 2, mode: Acquire, remaining_opportunities: 1, route: SharedRoute, previous: false }) outcome=Ok(Shed) silence=Shed\nDone(Err(Unavailable)) outcome=Ok(Probe(Overloaded)) notes=0x200\nv5 waits=0 handoff=[]"),
        // Rank 0 answers an owner's delegation that its own acquisition failed.
        ("rank 0 failed", false, production, then(owner_walk(1), &[answer(ResponseKind::Unavailable)]), "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nProbe(Probe { index: 0, canonical: 0, mode: Acquire, remaining_opportunities: 2, route: FundedRoute, previous: false })\nRank { ranking: Authority, version: Some(5) } outcome=Ok(Failed) silence=Answered rank0=Failed rank0_probe\nv5 waits=0 handoff=[]"),
        // Successor hints become a granted batch.
        ("successors", false, production, vec![START, Event::Ranked(Ok(owners(5, Some(0)))), Event::Ranked(Err(Error::Unavailable)), Event::Hints { hints: &[12, 99], rank0: Rank0State::Unknown }, Event::BatchDone { copy: true }], "Rank { ranking: Current, version: None }\nRank { ranking: Previous, version: Some(4) }\nHints\nBatch { first: 0, count: 1, remaining_opportunities: 6 }\nPublish(Peer) notes=0x100\nv5 waits=0 handoff=[12]"),
    ];
    let mut wrong = String::new();
    for (name, hedge, features, events, expected) in rows {
        let actual = script(&mut fill(hedge, features), &mut budget(8, 16), &events);
        if actual != expected {
            writeln!(wrong, "            // {name}\n            {actual:?},").unwrap();
        }
    }
    assert!(wrong.is_empty(), "\n{wrong}");
    // The batch is the successor target, remaining opportunities past rank 0's share.
    let mut f = fill(false, production);
    script(
        &mut f,
        &mut budget(8, 16),
        &[
            START,
            Event::Ranked(Ok(owners(5, Some(0)))),
            Event::Ranked(Err(Error::Unavailable)),
            Event::Hints {
                hints: &[12, 99],
                rank0: Rank0State::Unknown,
            },
        ],
    );
    assert_eq!(f.target_kind(0), TargetKind::Successor);
}

#[test]
#[should_panic(expected = "out of order")]
fn fill_origin_without_a_request_panics() {
    fill(false, Features::PRODUCTION).on(Event::Origin(Ok(())), &mut budget(8, 16), Instant::now());
}
