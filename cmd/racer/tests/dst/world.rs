//! The campaign's state: one cluster, what has been asked of it, and what it
//! is still allowed to say.

use std::collections::{BTreeMap, BTreeSet};
use std::time::Duration;

use racer::sim::{Faults, Hit, Sim};

use crate::coverage::{Coverage, Reach};
use crate::model::{self, Class, Kind, Page, Value};
use crate::plan::{Action, Profile, Rng};

/// Virtual milliseconds to wait on the cluster before giving up on it.
pub const PATIENCE: usize = 60_000;

/// How many operations one page may have outstanding. Concurrency on a single
/// page is where a register goes wrong, so this is deliberately more than one.
const DEPTH: usize = 3;

/// How much history one page may accumulate before the generator leaves it
/// alone to drain. The checker works on the stretch between two idle moments,
/// and a page that never goes idle never gets checked.
const HISTORY: usize = 20;

/// How far apart the small pages the workload touches are, so that a working
/// set of a couple of dozen still lands across many groups.
const STRIDE: u64 = 17;

/// Errors a client may legitimately be told. Anything else is either an
/// internal wire status that escaped or a mapping the workload cannot provoke.
const ALLOWED: [i32; 2] = [libc::EIO, libc::EAGAIN];

/// Where huge pages' fill values start, far enough above the small pages'
/// running token that the two can never be confused in a failure report.
const FILL: u32 = 1 << 20;

/// How many huge pages every profile keeps back from the workload. Nothing
/// names them until the faults stop, when filling them is what proves a
/// reservation left behind by an abandoned assembly gave its room back.
pub const SPARE: u64 = 2;

/// One request the cluster has not answered yet.
struct Flight {
    /// What the simulator calls it.
    id: u64,
    /// Which page, and of which kind.
    page: u64,
    huge: bool,
    /// Where in that page's history it sits.
    at: usize,
    /// What was asked, so the outcome can be classified.
    kind: Kind,
    /// Who was asked.
    node: usize,
}

/// A cluster, a workload, and everything known about what it may answer.
pub struct World {
    /// The cluster.
    pub sim: Sim,
    /// The shape it was built to.
    pub profile: Profile,
    /// The choice stream. Seeded once, so a seed is the whole repro.
    pub rng: Rng,
    /// Where this seed has been.
    pub cov: Coverage,
    /// The weather currently in force.
    pub faults: Faults,
    /// Links currently cut, as node indices.
    pub cuts: BTreeSet<(usize, usize)>,
    /// Whether the campaign is currently willing to break more than one thing.
    pub hostile: bool,
    /// Per page histories.
    small: Vec<Page>,
    huge: Vec<Page>,
    /// Requests outstanding.
    flights: Vec<Flight>,
    /// What the cluster last said, by request, for a caller that waited on one
    /// in particular.
    answers: BTreeMap<u64, (bool, Option<Value>)>,
    /// A counter that only moves forward, which is the real time the checker
    /// orders operations by.
    clock: u64,
    /// The last value handed out. Every mutation in the run gets its own, so a
    /// page holding another page's bytes is as visible as a torn one.
    token: u32,
    /// Replicas damaged on purpose, and left alone by the workload afterwards.
    damaged: BTreeSet<(u64, usize)>,
    /// Small pages whose trim the cluster has acknowledged. A read of one is not the
    /// same as a read of a page nobody wrote: the bytes it replaced are still on
    /// whichever replica missed the trim.
    trimmed: BTreeSet<u64>,
    /// A page sized buffer to stamp into.
    scratch: Vec<u8>,
    /// What has been done, for the failure message.
    log: Vec<String>,
}

impl World {
    /// Builds the cluster this seed calls for.
    pub fn new(profile: Profile) -> Result<Self, String> {
        let sim = Sim::new(profile.opts.clone()).map_err(|e| format!("boot: {e}"))?;
        let mut cov = Coverage::default();

        cov.stratum(profile.stratum);

        let mut w = Self {
            small: (0..profile.small).map(|_| Page::new(Class::Lww)).collect(),
            huge: (0..profile.huge + SPARE)
                .map(|_| Page::new(Class::Immutable))
                .collect(),
            sim,
            rng: Rng::new(profile.opts.seed),
            cov,
            faults: Faults::default(),
            cuts: BTreeSet::new(),
            hostile: false,
            profile,
            flights: Vec::new(),
            answers: BTreeMap::new(),
            clock: 0,
            token: 0,
            damaged: BTreeSet::new(),
            trimmed: BTreeSet::new(),
            scratch: vec![0; model::HUGE],
            log: Vec::new(),
        };

        // A cluster that has finished talking to itself. Booting stores
        // nothing, so this is only the members agreeing they are all empty.
        w.sim.run(w.convergence());

        Ok(w)
    }

    /// Long enough for anti-entropy to pass over every group twice.
    pub fn convergence(&self) -> Duration {
        Duration::from_secs(2 * self.sim.nodes() as u64 + 2)
    }

    /// How many requests are outstanding.
    pub fn inflight(&self) -> usize {
        self.flights.len()
    }

    /// How much of the cluster is currently broken.
    pub fn harm(&self) -> usize {
        (0..self.sim.nodes()).filter(|&i| !self.sim.up(i)).count() + self.cuts.len()
    }

    /// Whether a small page can take another operation without pushing its
    /// history past what the checker will look at in one go.
    pub fn small_ready(&self, page: u64) -> bool {
        let p = &self.small[page as usize];

        p.live() < DEPTH && p.pending() < HISTORY && !self.is_damaged(page)
    }

    /// As [`World::small_ready`], for an immutable page.
    pub fn huge_ready(&self, page: u64) -> bool {
        let p = &self.huge[page as usize];

        p.live() < DEPTH && p.pending() < HISTORY
    }

    /// An immutable page that is certainly filled, so filling it again has to
    /// be refused rather than obeyed.
    pub fn filled_huge(&self) -> Option<u64> {
        (0..self.profile.huge).find(|&p| {
            self.huge_ready(p)
                && self.huge[p as usize]
                    .possible()
                    .iter()
                    .all(|v| *v != Value::Hole)
        })
    }

    /// A page and a node that does not hold it, which is a different path
    /// through the server than a member taking its own write.
    pub fn non_member(&self) -> Option<(usize, u64)> {
        for p in 0..self.profile.small {
            if !self.small_ready(p) {
                continue;
            }

            let held = self.sim.small_members(self.lba(p));
            let out = (0..self.sim.nodes()).find(|i| self.sim.up(*i) && !held.contains(i));

            if let Some(node) = out {
                return Some((node, p));
            }
        }

        None
    }

    /// A replica whose bytes can be damaged without confusing the model: the
    /// page has to be idle, certain, and actually present on that replica.
    pub fn damageable(&self) -> Option<(u64, usize)> {
        for p in 0..self.profile.small {
            let page = &self.small[p as usize];

            if page.live() > 0 || self.is_damaged(p) || page.possible().len() != 1 {
                continue;
            }

            if page.possible().contains(&Value::Hole) {
                continue;
            }

            for r in 0..3 {
                if self.sim.small_replica_live(self.lba(p), r) {
                    return Some((p, r));
                }
            }
        }

        None
    }

    fn is_damaged(&self, page: u64) -> bool {
        self.damaged.iter().any(|(p, _)| *p == page)
    }

    /// Where a small page in the working set lives.
    fn lba(&self, page: u64) -> u64 {
        page * STRIDE + 3
    }

    /// Whether a node sits outside the zone that owns every extent. The simulator homes
    /// them all in zone 1 and hands out node indices in zone order, so the nodes past the
    /// first zone's share are the ones whose reads cross the fabric.
    fn remote(&self, node: usize) -> bool {
        self.sim.nodes() / self.profile.opts.zones.max(1) as usize <= node
    }

    fn tick(&mut self) -> u64 {
        self.clock += 1;

        self.clock
    }

    fn token(&mut self) -> Value {
        self.token += 1;

        Value::Token(self.token)
    }

    /// The one value a huge page will ever be filled with.
    ///
    /// A 4 MiB page is CORFU fill-once and unchecksummed, and its accepts derive
    /// their guard and ballot at the acceptor rather than carrying a proposer's
    /// ballot (`src/server.rs`, `accept`). Two clients racing *different* bytes
    /// into one position therefore leave replicas holding different pages under
    /// the same register, and nothing can tell them apart afterwards. That is
    /// the class doing what it promises rather than a bug: a filler owns the
    /// position it fills, and a retry re-sends the same bytes. So the workload
    /// retries, never rewrites, and the page still has to be a hole or exactly
    /// this value, never a mixture of the two.
    fn fill_of(&self, page: u64) -> Value {
        Value::Token(FILL + page as u32)
    }

    fn note(&mut self, what: String) {
        self.log
            .push(format!("{:>9}us {what}", self.sim.now().as_micros()));

        if self.log.len() > 4096 {
            self.log.drain(..1024);
        }
    }

    /// Puts the whole fault profile in force. Cuts, slow links and weather all live in
    /// the same profile, so they are always installed together: setting one by itself
    /// would quietly undo the others.
    fn weather(&mut self) -> Result<(), String> {
        let f = self.faults.clone();

        self.sim.faults(f);

        Ok(())
    }

    /// What has been done lately, for a failure to point at.
    pub fn history(&self) -> String {
        self.log.join("\n")
    }

    /// Does the next thing, whatever it is.
    pub fn apply(&mut self, a: Action) -> Result<(), String> {
        match a {
            Action::Write { node, page } => {
                let v = self.token();

                self.submit(node, page, false, Kind::Write(v)).map(drop)
            }
            Action::Trim { node, page } => self
                .submit(node, page, false, Kind::Write(Value::Hole))
                .map(drop),
            Action::Read { node, page } => self.submit(node, page, false, Kind::Read).map(drop),
            Action::Fill { node, page } => {
                let v = self.fill_of(page);

                self.submit(node, page, true, Kind::Write(v)).map(drop)
            }
            Action::ReadHuge { node, page } => self.submit(node, page, true, Kind::Read).map(drop),
            Action::Advance(d) => {
                self.sim.run(d);

                Ok(())
            }
            Action::Crash(i) => {
                if !self.sim.up(i) {
                    return Ok(());
                }

                if self.flights.iter().any(|f| f.node == i) {
                    self.cov.reach(Reach::CrashedBusy);
                }

                if self
                    .flights
                    .iter()
                    .any(|f| f.huge && matches!(f.kind, Kind::Write(_)))
                {
                    self.cov.reach(Reach::Assembling);
                }

                self.note(format!("crash node {i}"));
                self.sim.crash(i);

                Ok(())
            }
            Action::Restart(i) => {
                if self.sim.up(i) {
                    return Ok(());
                }

                self.note(format!("restart node {i}"));
                self.sim.restart(i).map_err(|e| format!("restart {i}: {e}"))
            }
            Action::Cut(a, b) => {
                if a == b {
                    return Ok(());
                }

                self.note(format!("cut {a} to {b}"));
                self.cuts.insert((a, b));

                if self
                    .flights
                    .iter()
                    .any(|f| f.huge && matches!(f.kind, Kind::Write(_)))
                {
                    self.cov.reach(Reach::Assembling);
                }

                if self.cuts.contains(&(b, a)) {
                    self.cov.reach(Reach::Partitioned);
                }

                self.faults.cut.insert((a as u32 + 1, b as u32 + 1));
                self.weather()
            }
            Action::Heal(a, b) => {
                self.note(format!("heal {a} to {b}"));
                self.cuts.remove(&(a, b));
                self.faults.cut.remove(&(a as u32 + 1, b as u32 + 1));
                self.weather()
            }
            Action::Slow(a, b) => {
                if a == b {
                    return Ok(());
                }

                self.note(format!("slow {a} to {b}"));
                self.faults.slow.insert((a as u32 + 1, b as u32 + 1));
                self.weather()
            }
            Action::Weather(f) => {
                self.note(format!(
                    "weather: {} drops, {} disk errors, {} corruption, {}us jitter",
                    f.drop, f.io_error, f.corrupt, f.jitter_us
                ));
                self.faults = f;
                self.weather()
            }
            Action::Damage(page, replica) => {
                let lba = self.lba(page);

                if !self.sim.small_replica_live(lba, replica) {
                    return Ok(());
                }

                let node = self.sim.corrupt_small_replica(lba, replica);

                self.note(format!(
                    "damage page {lba} replica {replica} on node {node}"
                ));

                if self.sim.small_replica_valid(lba, replica) {
                    return Err(format!(
                        "page {lba} replica {replica} still looks valid after its \
                         bytes were damaged, so the campaign is not testing repair"
                    ));
                }

                self.damaged.insert((page, replica));
                self.cov.reach(Reach::Damaged);

                Ok(())
            }
        }
    }

    /// Submits one client request and records it in the page's history.
    fn submit(&mut self, node: usize, page: u64, huge: bool, kind: Kind) -> Result<u64, String> {
        let start = self.tick();
        let at = if huge {
            self.huge[page as usize].begin(kind, start, node)
        } else {
            self.small[page as usize].begin(kind, start, node)
        };

        let held = if huge {
            self.sim.huge_members(page)
        } else {
            self.sim.small_members(self.lba(page))
        };

        self.cov.reach(if held.contains(&node) {
            Reach::Member
        } else {
            Reach::NonMember
        });

        if self.remote(node) {
            self.cov.reach(Reach::Remote);
        }

        let id = match (huge, kind) {
            (false, Kind::Write(v)) => {
                model::stamp(v, &mut self.scratch[..model::BLOCK]);

                if v == Value::Hole {
                    self.sim.trim(node, self.lba(page))
                } else {
                    let lba = self.lba(page);

                    self.sim
                        .write_with(node, lba, &self.scratch[..model::BLOCK])
                }
            }
            (false, Kind::Read) => self.sim.read(node, self.lba(page)),
            (true, Kind::Write(v)) => {
                model::stamp(v, &mut self.scratch);

                self.sim.write_huge_with(node, page, &self.scratch)
            }
            (true, Kind::Read) => self.sim.read_huge(node, page),
        };

        self.note(format!(
            "node {node} {}",
            match (huge, kind) {
                (false, Kind::Write(Value::Hole)) => format!("trims page {page}"),
                (false, Kind::Write(v)) => format!("writes {v} to page {page}"),
                (false, Kind::Read) => format!("reads page {page}"),
                (true, Kind::Write(v)) => format!("fills huge page {page} with {v}"),
                (true, Kind::Read) => format!("reads huge page {page}"),
            }
        ));

        self.flights.push(Flight {
            id,
            page,
            huge,
            at,
            kind,
            node,
        });

        Ok(id)
    }

    /// Collects everything the cluster has answered, and checks each answer on
    /// its own terms: the bytes have to decode, and the error has to be one a
    /// client is allowed to see.
    pub fn reap(&mut self) -> Result<(), String> {
        let mut i = 0;

        while i < self.flights.len() {
            let Some(res) = self.sim.result(self.flights[i].id) else {
                i += 1;

                continue;
            };

            let f = self.flights.swap_remove(i);
            let end = self.tick();
            let ok = res.is_ok();
            let mut saw = None;

            if let Err(e) = res {
                if !ALLOWED.contains(&e) {
                    return Err(format!(
                        "a client was told {e} for a {} of page {}, which is not an \
                         error this workload can produce",
                        if f.huge { "huge page" } else { "small page" },
                        f.page
                    ));
                }

                self.cov.reach(match (f.huge, f.kind) {
                    (false, Kind::Read) => Reach::ReadFailed,
                    (false, _) => Reach::WriteFailed,
                    (true, Kind::Read) => Reach::HugeReadFailed,
                    (true, _) => Reach::FillFailed,
                });

                if f.huge
                    && matches!(f.kind, Kind::Write(_))
                    && e == libc::EAGAIN
                    && self.huge[f.page as usize]
                        .possible()
                        .iter()
                        .all(|v| *v != Value::Hole)
                {
                    self.cov.reach(Reach::Refilled);
                }
            } else if f.kind == Kind::Read {
                let bytes = self
                    .sim
                    .payload(f.id)
                    .ok_or_else(|| format!("request {} answered without a payload", f.id))?;
                let want = if f.huge { model::HUGE } else { model::BLOCK };

                if bytes.len() != want {
                    return Err(format!(
                        "a read of page {} came back {} bytes long, not {want}",
                        f.page,
                        bytes.len()
                    ));
                }

                let Some(v) = model::parse(bytes) else {
                    return Err(format!(
                        "a read of {} page {} on node {} returned bytes no client ever \
                         wrote: they are torn, mixed or corrupt",
                        if f.huge { "huge" } else { "small" },
                        f.page,
                        f.node
                    ));
                };

                saw = Some(v);

                let far = self.remote(f.node);

                self.cov.reach(match (f.huge, v) {
                    (false, Value::Hole) if self.trimmed.contains(&f.page) => Reach::ReadTrimmed,
                    (false, Value::Hole) => Reach::ReadHole,
                    (false, _) => Reach::Read,
                    (true, Value::Hole) => Reach::ReadHugeHole,
                    (true, _) => Reach::ReadHuge,
                });

                if far && v == Value::Hole {
                    self.cov.reach(if f.huge {
                        Reach::ReadHugeHoleRemote
                    } else {
                        Reach::ReadHoleRemote
                    });
                }

                if !f.huge && v != Value::Hole {
                    self.cov.reach(Reach::Read);
                }
            } else {
                self.cov.reach(match (f.huge, f.kind) {
                    (true, _) => Reach::Filled,
                    (false, Kind::Write(Value::Hole)) => Reach::Trimmed,
                    (false, _) => Reach::Wrote,
                });

                if !f.huge && f.kind == Kind::Write(Value::Hole) {
                    self.trimmed.insert(f.page);
                }
            }

            let page = if f.huge {
                &mut self.huge[f.page as usize]
            } else {
                &mut self.small[f.page as usize]
            };

            page.finish(f.at, end, ok, saw);
            self.answers.insert(f.id, (ok, saw));
            // What was asked is only half a trace: an ordering that cannot be explained is
            // explained by the answers, so the log has to carry them too.
            self.note(format!(
                "node {} {} {} page {} {}",
                f.node,
                match f.kind {
                    Kind::Read => "read of",
                    Kind::Write(Value::Hole) => "trim of",
                    Kind::Write(_) => "write to",
                },
                if f.huge { "huge" } else { "small" },
                f.page,
                match (&res, saw) {
                    (Err(e), _) => format!("failed with {e}"),
                    (Ok(_), Some(v)) => format!("returned {v}"),
                    (Ok(_), None) => "succeeded".to_string(),
                }
            ));
            // The value is what the campaign keeps; the bytes are not. A run of a few
            // hundred fills that held on to every 4 MiB page it had read would cost
            // more memory than the cluster it is testing.
            self.sim.forget(f.id);
        }

        Ok(())
    }

    /// Folds every settled page's history into what it could now be holding,
    /// which is where a value that could not have come from anywhere is caught.
    pub fn settle(&mut self) -> Result<(), String> {
        for (i, p) in self.small.iter_mut().enumerate() {
            p.settle().map_err(|e| format!("small page {i}: {e}"))?;
        }

        for (i, p) in self.huge.iter_mut().enumerate() {
            p.settle().map_err(|e| format!("huge page {i}: {e}"))?;
        }

        Ok(())
    }

    /// Heals everything, brings everyone back, and waits for the cluster to go
    /// quiet. Convergence is only owed once the faults stop.
    pub fn quiesce(&mut self) -> Result<(), String> {
        self.note("heal everything".to_string());
        self.faults = Faults::default();
        self.sim.faults(Faults::default());
        self.cuts.clear();

        for i in 0..self.sim.nodes() {
            if !self.sim.up(i) {
                self.sim
                    .restart(i)
                    .map_err(|e| format!("restart {i}: {e}"))?;
            }
        }

        self.drain()?;
        self.sim.run(self.convergence());
        self.drain()?;

        Ok(())
    }

    /// Runs until nothing is outstanding.
    pub fn drain(&mut self) -> Result<(), String> {
        for _ in 0..PATIENCE {
            self.reap()?;
            self.settle()?;

            if self.flights.is_empty() && self.sim.status().idle() {
                return Ok(());
            }

            self.sim.run(Duration::from_millis(1));
        }

        Err(format!(
            "the cluster never went quiet: {} client requests and {:?} outstanding",
            self.flights.len(),
            self.sim.status()
        ))
    }

    /// Reads a page from a node and waits for the answer, retrying while the
    /// cluster is still settling. Used once everything has healed, where a
    /// refusal is a liveness failure rather than a legal outcome.
    pub fn read_now(&mut self, node: usize, page: u64, huge: bool) -> Result<Value, String> {
        let mut why = 0;

        for _ in 0..16 {
            let id = self.submit(node, page, huge, Kind::Read)?;

            self.drain()?;

            if let Some((true, Some(v))) = self.answers.get(&id) {
                return Ok(*v);
            }

            if let Some(Err(e)) = self.sim.result(id) {
                why = e;
            }

            self.sim.run(Duration::from_millis(500));
        }

        let says = self.probe(page, huge)?;

        Err(format!(
            "a healed cluster could not answer a read of {} page {page} on node {node}: \
             it kept answering {why}. asked once more, every node said: {says}",
            if huge { "huge" } else { "small" }
        ))
    }

    /// One read of a page from every node, for a failure to point at. Says
    /// whether a refusal is the page's problem or the node's.
    fn probe(&mut self, page: u64, huge: bool) -> Result<String, String> {
        let mut says = Vec::new();

        for node in 0..self.sim.nodes() {
            if !self.sim.up(node) {
                says.push(format!("node {node} is down"));
                continue;
            }

            let id = self.submit(node, page, huge, Kind::Read)?;

            self.drain()?;

            says.push(match self.answers.get(&id) {
                Some((true, Some(v))) => format!("node {node} says {v}"),
                _ => match self.sim.result(id) {
                    Some(Err(e)) => format!("node {node} says error {e}"),
                    _ => format!("node {node} said nothing"),
                },
            });
        }

        Ok(says.join(", "))
    }

    /// Fills a page and waits for it to land, retrying while the cluster is
    /// still settling. Used once everything has healed, where a refusal is a
    /// liveness failure rather than a legal outcome.
    pub fn fill_now(&mut self, page: u64) -> Result<(), String> {
        let mut why = 0;
        let node = 0;

        for _ in 0..16 {
            let id = self.submit(node, page, true, Kind::Write(self.fill_of(page)))?;

            self.drain()?;

            if let Some((true, _)) = self.answers.get(&id) {
                return Ok(());
            }

            if let Some(Err(e)) = self.sim.result(id) {
                why = e;
            }

            self.sim.run(Duration::from_millis(500));
        }

        Err(format!(
            "a healed cluster would not take a fill of huge page {page}: \
             it kept answering {why}"
        ))
    }

    /// Every replica damaged on purpose, for the repair invariant to check.
    pub fn damaged(&self) -> Vec<(u64, u64, usize)> {
        self.damaged
            .iter()
            .map(|&(p, r)| (p, self.lba(p), r))
            .collect()
    }

    /// The pages the workload touched, which is what has to agree at the end.
    pub fn pages(&self) -> (u64, u64) {
        (self.profile.small, self.profile.huge)
    }

    /// Folds the simulator's own path counters into this seed's coverage.
    pub fn collect(&mut self) {
        let mut hits = [0u64; Hit::ALL.len()];

        for (i, h) in Hit::ALL.iter().enumerate() {
            hits[i] = self.sim.hits(*h);
        }

        self.cov.hits(hits);
    }
}
