//! What the campaign has to have reached.
//!
//! An invariant nothing ever reaches passes for the wrong reason. Deleting a
//! named scenario is only safe if something notices when the fuzzer stops
//! going where that scenario went, so every path the old suite scripted is
//! listed here as an obligation and the campaign fails if the seeds between
//! them never got there.
//!
//! Obligations are aggregate. No single seed has to reach everything, which is
//! what lets the generator stay random instead of turning back into a list of
//! scenarios.

use racer::sim::Hit;

/// A place the campaign has to have been.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Reach {
    /// A small write was acknowledged.
    Wrote,
    /// A small write came back as a failure.
    WriteFailed,
    /// A small read was answered.
    Read,
    /// A small read came back as a failure.
    ReadFailed,
    /// A trim was acknowledged.
    Trimmed,
    /// A read found a page nobody had written.
    ReadHole,
    /// A read found a page whose trim had been acknowledged. Distinct from a hole,
    /// because the bytes it replaced are still on whichever replica missed the trim,
    /// and a repair that preferred them would hand a client back what it discarded.
    ReadTrimmed,
    /// A read from a zone that does not own the extent found a hole. The far side
    /// answers for the group there, so it owes the reader the zeroes a reader at home
    /// would have seen, and not the miss it would report to a repair.
    ReadHoleRemote,
    /// An immutable page was filled.
    Filled,
    /// A fill came back as a failure.
    FillFailed,
    /// An immutable page was read whole.
    ReadHuge,
    /// A read of an immutable page came back as a failure.
    HugeReadFailed,
    /// An immutable page was read before anyone filled it.
    ReadHugeHole,
    /// An immutable page was read from a zone that does not own it before anyone
    /// filled it: the same debt as [`Reach::ReadHoleRemote`], down the 4 MiB path,
    /// which takes no round and so has nothing to fall back on.
    ReadHugeHoleRemote,
    /// Part of a filled immutable page was read, rather than the whole of it.
    ReadHugePiece,
    /// A second fill of an immutable page was refused.
    Refilled,
    /// A request was submitted to a node outside the page's group.
    NonMember,
    /// A request was submitted to a node inside the page's group.
    Member,
    /// A request was submitted from a zone that does not own the extent.
    Remote,
    /// A replica's persisted bytes were damaged behind the cluster's back.
    Damaged,
    /// A damaged replica was made whole again.
    Repaired,
    /// A repair was still there after the node that made it restarted.
    RepairDurable,
    /// Both directions between two nodes were cut at once.
    Partitioned,
    /// A node was crashed while it had work in flight.
    CrashedBusy,
    /// A page was part way through arriving.
    Assembling,
    /// A write that never finished left its reservation behind.
    Abandoned,
    /// A page filled after the faults stopped, so the room came back.
    Reclaimed,
    /// A store was pushed to the rate its configuration allows.
    Throttled,
    /// The cluster was left holding more than one broken thing at a time.
    Hostile,
    /// Everything healed, everything drained, and every node agreed.
    Converged,
}

impl Reach {
    /// Every obligation, in the order they are reported.
    pub const ALL: [Reach; 30] = [
        Reach::Wrote,
        Reach::WriteFailed,
        Reach::Read,
        Reach::ReadFailed,
        Reach::Trimmed,
        Reach::ReadHole,
        Reach::ReadTrimmed,
        Reach::ReadHoleRemote,
        Reach::Filled,
        Reach::FillFailed,
        Reach::ReadHuge,
        Reach::HugeReadFailed,
        Reach::ReadHugeHole,
        Reach::ReadHugeHoleRemote,
        Reach::ReadHugePiece,
        Reach::Refilled,
        Reach::NonMember,
        Reach::Member,
        Reach::Remote,
        Reach::Damaged,
        Reach::Repaired,
        Reach::RepairDurable,
        Reach::Partitioned,
        Reach::CrashedBusy,
        Reach::Assembling,
        Reach::Abandoned,
        Reach::Reclaimed,
        Reach::Throttled,
        Reach::Hostile,
        Reach::Converged,
    ];

    /// What reaching it means, for the report.
    pub fn name(self) -> &'static str {
        match self {
            Reach::Wrote => "a small write was acknowledged",
            Reach::WriteFailed => "a small write failed",
            Reach::Read => "a small read was answered",
            Reach::ReadFailed => "a small read failed",
            Reach::Trimmed => "a trim was acknowledged",
            Reach::ReadHole => "a read found a hole",
            Reach::ReadTrimmed => "a read found a page that had been trimmed",
            Reach::ReadHoleRemote => "a read from another zone found a hole",
            Reach::Filled => "an immutable page was filled",
            Reach::FillFailed => "a fill failed",
            Reach::ReadHuge => "an immutable page was read whole",
            Reach::HugeReadFailed => "a read of an immutable page failed",
            Reach::ReadHugeHole => "an unfilled immutable page was read",
            Reach::ReadHugeHoleRemote => "an unfilled immutable page was read from another zone",
            Reach::ReadHugePiece => "part of an immutable page was read",
            Reach::Refilled => "a second fill was refused",
            Reach::NonMember => "a request arrived at a non member",
            Reach::Member => "a request arrived at a member",
            Reach::Remote => "a request arrived from another zone",
            Reach::Damaged => "a replica was damaged on disk",
            Reach::Repaired => "a damaged replica was repaired",
            Reach::RepairDurable => "a repair survived a restart",
            Reach::Partitioned => "two nodes were cut apart in both directions",
            Reach::CrashedBusy => "a node crashed with work in flight",
            Reach::Assembling => "a page was part way through arriving",
            Reach::Abandoned => "an unfinished write left a reservation behind",
            Reach::Reclaimed => "a page filled after everything healed",
            Reach::Throttled => "a store was pushed to its configured rate",
            Reach::Hostile => "more than one fault was held at once",
            Reach::Converged => "the cluster healed, drained and agreed",
        }
    }
}

/// The paths in the simulator itself that the campaign has to have taken. The
/// rest of [`Hit`] is reported but not demanded: target saturation, for one,
/// depends on slot counts the campaign does not control.
const REQUIRED: [Hit; 11] = [
    Hit::Drop,
    Hit::IoError,
    Hit::Corrupt,
    Hit::CutSubmit,
    Hit::CutDeliver,
    Hit::CutReply,
    Hit::Slow,
    Hit::Split,
    Hit::Warm,
    Hit::Crossing,
    Hit::Crash,
];

/// Where a campaign has been so far.
#[derive(Clone)]
pub struct Coverage {
    /// Obligations reached.
    reach: [u64; Reach::ALL.len()],
    /// Simulator paths taken.
    hits: [u64; Hit::ALL.len()],
    /// Configurations exercised, one bit per stratum.
    strata: u64,
}

impl Default for Coverage {
    fn default() -> Self {
        Self {
            reach: [0; Reach::ALL.len()],
            hits: [0; Hit::ALL.len()],
            strata: 0,
        }
    }
}

impl Coverage {
    /// Records having reached somewhere.
    pub fn reach(&mut self, r: Reach) {
        self.reach[r as usize] += 1;
    }

    /// Whether a seed has been there yet, which is what steers the generator.
    pub fn has(&self, r: Reach) -> bool {
        self.reach[r as usize] > 0
    }

    /// Records the configuration this seed ran.
    pub fn stratum(&mut self, i: usize) {
        self.strata |= 1 << i;
    }

    /// Folds in the simulator's own counters at the end of a seed.
    pub fn hits(&mut self, h: [u64; Hit::ALL.len()]) {
        for (a, b) in self.hits.iter_mut().zip(h) {
            *a += b;
        }
    }

    /// Folds one seed's coverage into the campaign's.
    pub fn merge(&mut self, o: &Coverage) {
        for (a, b) in self.reach.iter_mut().zip(o.reach) {
            *a += b;
        }

        self.hits(o.hits);
        self.strata |= o.strata;
    }

    /// What the campaign owes, given how many configurations it ran.
    pub fn missing(&self, strata: usize) -> Vec<String> {
        let mut out = Vec::new();

        for r in Reach::ALL {
            if self.reach[r as usize] == 0 {
                out.push(r.name().to_string());
            }
        }

        for h in REQUIRED {
            if self.hits[h as usize] == 0 {
                out.push(h.name().to_string());
            }
        }

        for i in 0..strata {
            if self.strata & 1 << i == 0 {
                out.push(format!("configuration {i} was never run"));
            }
        }

        out
    }

    /// Everywhere the campaign went, and how often.
    pub fn report(&self) -> String {
        let mut out = String::new();

        for r in Reach::ALL {
            out += &format!("  {:>9}  {}\n", self.reach[r as usize], r.name());
        }

        for h in Hit::ALL {
            out += &format!("  {:>9}  {}\n", self.hits[h as usize], h.name());
        }

        out
    }
}
