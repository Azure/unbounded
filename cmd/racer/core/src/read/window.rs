//! `ReadWindow`: one range read's window of pages as a sans-IO machine.
//!
//! Slot `i` is page `next + i`. Slots hold [`EffectId`]s, not futures: the interpreter keeps the
//! probe, fill and selection operations and their page buffers keyed by those ids, and answers
//! each with an [`Input`]. [`ReadWindow::pump`] plans new work (client credits, direct fills of
//! the head plus `direct_ahead`, up to [`SELECTIONS`] provider selections, spill holds past the
//! window) and [`ReadWindow::next`] delivers pages strictly in order; a failure waits in its slot
//! until it reaches the head.
//!
//! The node-wide scheduler is outside: which pages other windows of the version are busy with
//! and how many granted flights are spare arrive as [`Facts`]; pages other selections offer
//! arrive as [`Input::Offered`].

use super::effect::{EffectId, EffectIds, Stale};
use super::{
    AcquisitionBudget, DIRECT_AHEAD, Error, PageBudget, SlotKind, direct_fills_into, first_failure,
    may_select, selection_demand, share,
};
use crate::subscriptions::MAX_DEMAND_INTERVALS;
use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::ops::Range;
use std::time::{Duration, Instant};

/// Restarts of one page's direct fill after `Unavailable`. Each restart gets a fresh page budget
/// ([`PageBudget::next_page`]), so this bounds those attempts per page. Local pressure
/// (`Overloaded`, `FlightFull`) resolves as other reads release memory and flight slots, so it is
/// retried without this cap, after a [`retry_delay`] backoff, until the read's scope ends.
pub const PAGE_RETRIES: u8 = 16;

/// First retry backoff of a page fill; each later retry doubles it up to [`RETRY_DELAY_MAX`].
pub const RETRY_DELAY_MIN: Duration = Duration::from_millis(2);

/// Largest retry backoff of a page fill.
pub const RETRY_DELAY_MAX: Duration = Duration::from_millis(250);

/// Backoff before the `retry`th restart of a page fill; zero for the first attempt.
pub fn retry_delay(retry: u8) -> Duration {
    match retry {
        0 => Duration::ZERO,
        retry => RETRY_DELAY_MIN
            .saturating_mul(1 << u32::from(retry - 1).min(16))
            .min(RETRY_DELAY_MAX),
    }
}

/// Spill work (hold plus spill fill) a read keeps in flight.
pub const SPILL_FILLS: usize = 2;

/// Lifetime of a read's spill hold. A page whose hold expires before delivery is fetched again.
pub const SPILL_HOLD: Duration = Duration::from_secs(60);

/// A spill hold epoch: each held page owes exactly one release under it.
pub type Epoch = u64;

/// Fixed shape of one read.
#[derive(Clone, Copy, Debug)]
pub struct WindowConfig {
    /// First and one-past-last page of the range.
    pub start: u64,
    pub end: u64,
    /// Pages admitted into memory at once.
    pub window_pages: usize,
    /// Fills ciphertext for a GPU sink: fills its whole window directly, never selects.
    pub sealed: bool,
    /// Never selects; fills its whole window directly.
    pub passthrough: bool,
    /// Pages past the window that may be held on disk (0: no spill).
    pub spill: usize,
    pub budget: PageBudget,
}

/// Facts from the node-wide scheduler for one [`ReadWindow::pump`].
#[derive(Clone, Debug, Default)]
pub struct Facts {
    /// Pages another window of this version is selecting or filling.
    pub elsewhere: BTreeSet<u64>,
    /// Unused granted flights of the version's contracts.
    pub spare: usize,
    /// The read has a scheduler lease (it may select).
    pub leased: bool,
}

/// One page of the window.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Slot {
    /// Looking the page up locally.
    Probing(EffectId),
    /// Not in memory and not requested; `select` is false once a selection covering it failed.
    Missing { select: bool },
    /// A direct fill.
    Fetching(EffectId),
    /// Covered by an outstanding provider selection.
    Selecting(EffectId),
    /// Fetched; the interpreter holds the page under this handle.
    Ready { handle: EffectId, sealed: bool },
    /// Failed; delivered as an error when it reaches the head.
    Failed(Error),
}

impl Slot {
    pub fn kind(&self) -> SlotKind {
        match self {
            Slot::Probing(_) => SlotKind::Probing,
            Slot::Missing { select } => SlotKind::Missing { select: *select },
            Slot::Fetching(_) => SlotKind::Fetching,
            Slot::Selecting(_) => SlotKind::Selecting,
            Slot::Ready { .. } => SlotKind::Ready,
            Slot::Failed(_) => SlotKind::Failed,
        }
    }
}

/// What a probe of a page should read.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProbeFrom {
    /// Memory (plaintext; a sealed read probes live plaintext only).
    Memory,
    /// The page's held disk copy, after the spill fill `after` finishes when set.
    Held { after: Option<EffectId> },
}

/// One external effect.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Effect {
    /// Probe `page`; answer [`Input::Probed`].
    Probe {
        id: EffectId,
        page: u64,
        from: ProbeFrom,
    },
    /// Fill `page` through its owning worker; answer [`Input::Fetched`].
    Fetch {
        id: EffectId,
        page: u64,
        budget: AcquisitionBudgetSpec,
        sealed: bool,
        /// Restarts of this page's fill so far; the interpreter waits
        /// [`retry_delay`] of it before starting the fill.
        retry: u8,
    },
    /// Select `pages` (as `intervals`) from a provider; answer [`Input::Selected`].
    Select {
        id: EffectId,
        intervals: Vec<Range<u64>>,
        pages: BTreeSet<u64>,
        budget: AcquisitionBudgetSpec,
    },
    /// Hold `page` on disk for `hold`; answer [`Input::Held`].
    Hold {
        id: EffectId,
        page: u64,
        hold: Duration,
    },
    /// Fill held `page`, persisting it under the hold; answer [`Input::SpillFilled`].
    SpillFill { id: EffectId, page: u64 },
    /// Release spill holds. Each `(page, epoch)` appears exactly once over the read.
    Release(Vec<(u64, Epoch)>),
    /// Drop the interpreter's buffer for `handle` (a page that will never be delivered).
    Discard(EffectId),
}

/// A page budget, as data: build it with [`AcquisitionBudget::new`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AcquisitionBudgetSpec {
    pub deadline: Instant,
    pub attempts: u32,
    pub links: u8,
    /// A route failure was observed (an inherited budget past the normal route links), so each
    /// attempt may reserve the failure route ceiling.
    pub route_failed: bool,
}

impl AcquisitionBudgetSpec {
    pub fn build(self) -> AcquisitionBudget {
        let mut budget = AcquisitionBudget::new(self.deadline, self.attempts, self.links);
        if self.route_failed {
            budget.note_route_failure();
        }
        budget
    }
}

impl From<&AcquisitionBudget> for AcquisitionBudgetSpec {
    /// The spec of an unspent budget, such as a serving read's
    /// [`super::budget::inherited_budget`].
    fn from(budget: &AcquisitionBudget) -> Self {
        Self {
            deadline: budget.deadline(),
            attempts: budget.remaining_attempts(),
            links: budget.remaining_links(),
            route_failed: budget.route_failed(),
        }
    }
}

/// How a direct fill ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fetched {
    /// Plaintext (or a sealed read's plaintext from memory).
    Page,
    /// Ciphertext for the GPU, already validated by the interpreter.
    Sealed,
}

/// One completion or external event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Input {
    /// `found`: a valid page is now held under `id`.
    Probed {
        id: EffectId,
        found: bool,
    },
    Fetched {
        id: EffectId,
        result: Result<Fetched, Error>,
    },
    /// `started`: false when the selection could not start (its pages are filled directly).
    Selected {
        id: EffectId,
        ok: bool,
        started: bool,
    },
    /// Another selection of the version offered `page`, held under `handle`.
    Offered {
        page: u64,
        handle: EffectId,
    },
    Held {
        id: EffectId,
        result: Result<Epoch, Error>,
    },
    SpillFilled {
        id: EffectId,
        ok: bool,
    },
    /// The read's scope ended, or the client went away.
    Cancel(Error),
}

/// What [`ReadWindow::next`] hands the client.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Next {
    /// The range is complete.
    End,
    /// Nothing deliverable yet.
    Wait,
    /// Page `page` held under `handle`.
    Page {
        page: u64,
        handle: EffectId,
        sealed: bool,
    },
    /// The read failed.
    Fail(Error),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Spilling {
    Holding(EffectId),
    Filling(EffectId),
    Spilled,
}

#[derive(Debug, Default)]
struct Spill {
    limit: usize,
    refused: bool,
    pages: BTreeMap<u64, Spilling>,
    held: BTreeMap<u64, Epoch>,
}

/// The window machine. See the module docs.
pub struct ReadWindow {
    config: WindowConfig,
    next: u64,
    slots: VecDeque<Slot>,
    selections: BTreeMap<EffectId, BTreeSet<u64>>,
    /// Per page: `Unavailable` restarts (capped by [`PAGE_RETRIES`]) and all restarts.
    retries: BTreeMap<u64, (u8, u8)>,
    spill: Option<Spill>,
    /// Spill fills moved into a probe; their completion is ignored.
    moved: BTreeSet<EffectId>,
    /// Holds still pending when the read ended, by page: released on completion.
    orphans: BTreeMap<EffectId, u64>,
    error: Option<Error>,
    terminated: bool,
    /// Scratch slot kinds for planning, reused across pumps.
    kinds: Vec<SlotKind>,
    /// Scratch direct-fill plan, reused across pumps.
    plan: Vec<usize>,
    /// Page of each probe or fetch issued, so completions find their slot without a scan. An entry
    /// is only trusted while its slot still holds the id; it is dropped on completion.
    issued: BTreeMap<EffectId, u64>,
}

impl ReadWindow {
    pub fn new(config: WindowConfig) -> Self {
        Self {
            config,
            next: config.start,
            slots: VecDeque::new(),
            selections: BTreeMap::new(),
            retries: BTreeMap::new(),
            spill: (config.spill > 0).then(|| Spill {
                limit: config.spill,
                ..Spill::default()
            }),
            moved: BTreeSet::new(),
            orphans: BTreeMap::new(),
            error: None,
            terminated: false,
            kinds: Vec::new(),
            plan: Vec::new(),
            issued: BTreeMap::new(),
        }
    }

    pub fn config(&self) -> &WindowConfig {
        &self.config
    }

    /// Replace the page budget of pages admitted from now on; admitted pages keep theirs.
    pub fn set_budget(&mut self, budget: PageBudget) {
        self.config.budget = budget;
    }

    /// Whether the read ended (delivered its range, failed or was canceled).
    pub fn terminated(&self) -> bool {
        self.terminated
    }

    /// Place `slot` at window index `index`, appending when `index` is the window length. For
    /// interpreter tests that script a window; the caller owns any client credit of the page.
    #[doc(hidden)]
    pub fn place(&mut self, index: usize, slot: Slot) {
        let number = self.page_at(index);
        match slot {
            Slot::Selecting(id) => {
                self.selections.entry(id).or_default().insert(number);
            }
            Slot::Probing(id) | Slot::Fetching(id) => {
                self.issued.insert(id, number);
            }
            _ => {}
        }
        if index == self.slots.len() {
            self.slots.push_back(slot);
        } else {
            self.slots[index] = slot;
        }
    }

    /// The next page to deliver.
    pub fn cursor(&self) -> u64 {
        self.next
    }

    pub fn slots(&self) -> impl ExactSizeIterator<Item = &Slot> {
        self.slots.iter()
    }

    /// Outstanding provider selections.
    pub fn selections(&self) -> usize {
        self.selections.len()
    }

    /// Pages this window wants and pages it is busy with, for the scheduler's fan-out.
    pub fn interest(&self) -> (BTreeSet<u64>, BTreeSet<u64>) {
        let mut wants = BTreeSet::new();
        let mut busy = BTreeSet::new();
        self.interest_into(&mut wants, &mut busy);
        (wants, busy)
    }

    /// [`Self::interest`] into existing sets, which are cleared first.
    pub fn interest_into(&self, wants: &mut BTreeSet<u64>, busy: &mut BTreeSet<u64>) {
        wants.clear();
        busy.clear();
        for (index, slot) in self.slots.iter().enumerate() {
            let number = self.page_at(index);
            match slot {
                Slot::Selecting(_) => {
                    wants.insert(number);
                    busy.insert(number);
                }
                Slot::Fetching(_) => {
                    busy.insert(number);
                }
                Slot::Probing(_) | Slot::Missing { .. } => {
                    wants.insert(number);
                }
                Slot::Ready { .. } | Slot::Failed(_) => {}
            }
        }
    }

    /// Spill pages held and not yet released.
    pub fn held(&self) -> impl Iterator<Item = (&u64, &Epoch)> {
        self.spill.iter().flat_map(|spill| spill.held.iter())
    }

    fn page_at(&self, index: usize) -> u64 {
        self.next + index as u64
    }

    /// Direct fills kept in flight beyond the head page.
    pub fn direct_ahead(&self) -> usize {
        if self.config.sealed || self.config.passthrough {
            self.config.window_pages.saturating_sub(1).max(DIRECT_AHEAD)
        } else {
            DIRECT_AHEAD
        }
    }

    fn slot_mut(&mut self, number: u64) -> Option<&mut Slot> {
        let index = usize::try_from(number.checked_sub(self.next)?).ok()?;
        self.slots.get_mut(index)
    }

    /// Window index of the probe or fetch `id`, if its slot still holds it.
    pub fn index_of(&self, id: EffectId) -> Option<usize> {
        let number = *self.issued.get(&id)?;
        let index = usize::try_from(number.checked_sub(self.next)?).ok()?;
        match self.slots.get(index)? {
            Slot::Probing(i) | Slot::Fetching(i) if *i == id => Some(index),
            _ => None,
        }
    }

    /// Like [`Self::index_of`], forgetting `id`: for its completion.
    fn complete(&mut self, id: EffectId) -> Option<usize> {
        let index = self.index_of(id);
        self.issued.remove(&id);
        index
    }

    /// Plan new work. `reserve(page)` takes the client credit of a page entering the window and
    /// returns false when it does not fit.
    pub fn pump(
        &mut self,
        now: Instant,
        facts: &Facts,
        mut reserve: impl FnMut(u64) -> bool,
        ids: &mut EffectIds,
        out: &mut Vec<Effect>,
    ) {
        if self.terminated || self.error.is_some() {
            return;
        }
        self.refill(&mut reserve, ids, out);
        if let Err(error) = self.admit(now, facts, ids, out) {
            self.error = Some(error);
            return;
        }
        self.admit_spill(now, ids, out);
    }

    fn refill(
        &mut self,
        reserve: &mut impl FnMut(u64) -> bool,
        ids: &mut EffectIds,
        out: &mut Vec<Effect>,
    ) {
        while self.slots.len() < self.config.window_pages
            && self.page_at(self.slots.len()) < self.config.end
        {
            let number = self.page_at(self.slots.len());
            if !reserve(number) {
                break;
            }
            let from = match self
                .spill
                .as_mut()
                .map(|spill| spill.pages.get(&number).copied())
            {
                Some(Some(Spilling::Spilled)) => {
                    self.spill.as_mut().unwrap().pages.remove(&number);
                    // A sealed read probes memory only; the GPU decrypts the disk copy.
                    if self.config.sealed {
                        ProbeFrom::Memory
                    } else {
                        ProbeFrom::Held { after: None }
                    }
                }
                Some(Some(Spilling::Filling(fill))) => {
                    self.spill.as_mut().unwrap().pages.remove(&number);
                    self.moved.insert(fill);
                    if self.config.sealed {
                        ProbeFrom::Memory
                    } else {
                        ProbeFrom::Held { after: Some(fill) }
                    }
                }
                // A pending hold stays with the spill state so it is released exactly once.
                _ => ProbeFrom::Memory,
            };
            let id = ids.allocate();
            self.issued.insert(id, number);
            self.slots.push_back(Slot::Probing(id));
            out.push(Effect::Probe {
                id,
                page: number,
                from,
            });
        }
    }

    fn refresh_kinds(&mut self) {
        self.kinds.clear();
        self.kinds.extend(self.slots.iter().map(Slot::kind));
    }

    fn admit(
        &mut self,
        now: Instant,
        facts: &Facts,
        ids: &mut EffectIds,
        out: &mut Vec<Effect>,
    ) -> Result<(), Error> {
        self.refresh_kinds();
        let failed = first_failure(&self.kinds);
        let next = self.next;
        let mut plan = std::mem::take(&mut self.plan);
        direct_fills_into(
            &self.kinds,
            self.direct_ahead(),
            |index| facts.elsewhere.contains(&(next + index as u64)),
            &mut plan,
        );
        let mut started = Ok(());
        for &index in &plan {
            let budget = match self.page_budget(now) {
                Ok(budget) => budget,
                Err(error) => {
                    started = Err(error);
                    break;
                }
            };
            let id = ids.allocate();
            let page = self.page_at(index);
            self.slots[index] = Slot::Fetching(id);
            self.issued.insert(id, page);
            out.push(Effect::Fetch {
                id,
                page,
                budget,
                sealed: self.config.sealed,
                retry: self.retries.get(&page).map_or(0, |&(_, all)| all),
            });
        }
        self.plan = plan;
        started?;
        if failed.is_some() || self.config.passthrough || self.config.sealed || !facts.leased {
            return Ok(());
        }
        while may_select(self.selections.len(), facts.spare) {
            let holes = self
                .slots
                .iter()
                .filter(|slot| **slot == Slot::Missing { select: true })
                .count();
            let limit = share(holes, self.selections.len(), facts.spare);
            self.refresh_kinds();
            let Some((intervals, pages)) = selection_demand(
                &self.kinds,
                self.next,
                |number| facts.elsewhere.contains(&number),
                limit,
                MAX_DEMAND_INTERVALS,
            ) else {
                break;
            };
            let budget = self.page_budget(now)?;
            let id = ids.allocate();
            for number in &pages {
                if let Some(slot) = self.slot_mut(*number) {
                    *slot = Slot::Selecting(id);
                }
            }
            self.selections.insert(id, pages.clone());
            out.push(Effect::Select {
                id,
                intervals,
                pages,
                budget,
            });
        }
        Ok(())
    }

    fn page_budget(&self, now: Instant) -> Result<AcquisitionBudgetSpec, Error> {
        let budget = self.config.budget.next_page(now)?;
        Ok(AcquisitionBudgetSpec::from(&budget))
    }

    fn admit_spill(&mut self, now: Instant, ids: &mut EffectIds, out: &mut Vec<Effect>) {
        let start = self.page_at(self.slots.len());
        let end_page = self.config.end;
        let hold = match self.config.budget {
            PageBudget::Client { deadline } => {
                SPILL_HOLD.min(deadline.saturating_duration_since(now))
            }
            PageBudget::Progressing { .. } => SPILL_HOLD,
        };
        let Some(spill) = self.spill.as_mut() else {
            return;
        };
        if spill.refused || hold.is_zero() {
            return;
        }
        let mut active = spill
            .pages
            .values()
            .filter(|state| !matches!(state, Spilling::Spilled))
            .count();
        let end = start.saturating_add(spill.limit as u64).min(end_page);
        for number in start..end {
            if active >= SPILL_FILLS {
                break;
            }
            if spill.pages.contains_key(&number) || spill.held.contains_key(&number) {
                continue;
            }
            let id = ids.allocate();
            spill.pages.insert(number, Spilling::Holding(id));
            out.push(Effect::Hold {
                id,
                page: number,
                hold,
            });
            active += 1;
        }
    }

    /// Feed one completion. `scope_live` is false once the read's scope ended: transient failures
    /// are then not retried.
    pub fn on(
        &mut self,
        input: Input,
        scope_live: bool,
        ids: &mut EffectIds,
        out: &mut Vec<Effect>,
    ) -> Result<(), Stale> {
        match input {
            Input::Cancel(error) => {
                self.error.get_or_insert(error);
                self.terminate(out);
                Ok(())
            }
            Input::Held { id, result } => self.on_held(id, result, ids, out),
            Input::SpillFilled { id, ok } => self.on_spill_filled(id, ok, out),
            _ if self.terminated => {
                if let Input::Probed { id, found: true }
                | Input::Fetched { id, result: Ok(_) }
                | Input::Offered { handle: id, .. } = input
                {
                    out.push(Effect::Discard(id));
                }
                Ok(())
            }
            Input::Probed { id, found } => {
                let index = self.complete(id).ok_or(Stale(id))?;
                self.slots[index] = if found {
                    Slot::Ready {
                        handle: id,
                        sealed: false,
                    }
                } else {
                    Slot::Missing { select: true }
                };
                Ok(())
            }
            Input::Fetched { id, result } => {
                let index = self.complete(id).ok_or(Stale(id))?;
                let number = self.page_at(index);
                self.slots[index] = match result {
                    Ok(kind) => Slot::Ready {
                        handle: id,
                        sealed: kind == Fetched::Sealed,
                    },
                    // Sealed fills share the plaintext retry policy.
                    Err(error @ (Error::Overloaded | Error::FlightFull | Error::Unavailable))
                        if scope_live && self.retry_page(number, error == Error::Unavailable) =>
                    {
                        Slot::Missing { select: false }
                    }
                    Err(error) => Slot::Failed(error),
                };
                Ok(())
            }
            Input::Selected { id, ok, started } => {
                let pages = self.selections.remove(&id).ok_or(Stale(id))?;
                // Pages the provider did not send may be selected again; after a failure they
                // are only filled directly.
                let select = ok && started;
                for number in pages {
                    if let Some(slot) = self.slot_mut(number)
                        && *slot == Slot::Selecting(id)
                    {
                        *slot = Slot::Missing { select };
                    }
                }
                Ok(())
            }
            Input::Offered { page, handle } => {
                match self.slot_mut(page) {
                    Some(slot @ (Slot::Probing(_) | Slot::Missing { .. } | Slot::Selecting(_))) => {
                        *slot = Slot::Ready {
                            handle,
                            sealed: false,
                        };
                    }
                    _ => out.push(Effect::Discard(handle)),
                }
                Ok(())
            }
        }
    }

    /// Count one restart of `number`'s fill; false once its `capped` retries are spent.
    /// Uncapped (local pressure) restarts only grow the backoff.
    fn retry_page(&mut self, number: u64, capped: bool) -> bool {
        let next = self.next;
        self.retries.retain(|page, _| *page >= next);
        let (count, all) = self.retries.entry(number).or_insert((0, 0));
        if capped {
            if *count >= PAGE_RETRIES {
                return false;
            }
            *count += 1;
        }
        *all = all.saturating_add(1);
        true
    }

    fn on_held(
        &mut self,
        id: EffectId,
        result: Result<Epoch, Error>,
        ids: &mut EffectIds,
        out: &mut Vec<Effect>,
    ) -> Result<(), Stale> {
        if let Some(page) = self.orphans.remove(&id) {
            if let Ok(epoch) = result {
                out.push(Effect::Release(vec![(page, epoch)]));
            }
            return Ok(());
        }
        let horizon = self.page_at(self.slots.len());
        let Some(spill) = self.spill.as_mut() else {
            return Err(Stale(id));
        };
        let Some((&number, _)) = spill
            .pages
            .iter()
            .find(|(_, state)| **state == Spilling::Holding(id))
        else {
            return Err(Stale(id));
        };
        match result {
            Err(_) => {
                // Hold limits, a missing key or no disk: keep the memory window.
                spill.refused = true;
                spill.pages.remove(&number);
            }
            // The window reached the page while its hold was pending.
            Ok(epoch) if number < horizon => {
                spill.pages.remove(&number);
                out.push(Effect::Release(vec![(number, epoch)]));
            }
            Ok(epoch) => {
                spill.held.insert(number, epoch);
                let fill = ids.allocate();
                spill.pages.insert(number, Spilling::Filling(fill));
                out.push(Effect::SpillFill {
                    id: fill,
                    page: number,
                });
            }
        }
        Ok(())
    }

    fn on_spill_filled(
        &mut self,
        id: EffectId,
        ok: bool,
        out: &mut Vec<Effect>,
    ) -> Result<(), Stale> {
        if self.moved.remove(&id) {
            return Ok(());
        }
        let Some(spill) = self.spill.as_mut() else {
            return Err(Stale(id));
        };
        let Some((&number, _)) = spill
            .pages
            .iter()
            .find(|(_, state)| **state == Spilling::Filling(id))
        else {
            return Err(Stale(id));
        };
        if ok {
            spill.pages.insert(number, Spilling::Spilled);
            return Ok(());
        }
        spill.pages.remove(&number);
        if let Some(epoch) = spill.held.remove(&number) {
            out.push(Effect::Release(vec![(number, epoch)]));
        }
        // A failed spill fill would likely fail again; stop spilling.
        spill.refused = true;
        Ok(())
    }

    /// Deliver the head page if it is ready. A delivered page's spill hold is released.
    pub fn next(&mut self, out: &mut Vec<Effect>) -> Next {
        if let Some(error) = self.error {
            return Next::Fail(error);
        }
        if self.next >= self.config.end {
            self.terminated = true;
            return Next::End;
        }
        match self.slots.front().copied() {
            Some(Slot::Ready { handle, sealed }) => {
                self.slots.pop_front();
                let page = self.next;
                self.next += 1;
                if let Some(spill) = self.spill.as_mut()
                    && let Some(epoch) = spill.held.remove(&page)
                {
                    out.push(Effect::Release(vec![(page, epoch)]));
                }
                Next::Page {
                    page,
                    handle,
                    sealed,
                }
            }
            Some(Slot::Failed(error)) => {
                self.error = Some(error);
                self.terminate(out);
                Next::Fail(error)
            }
            _ => Next::Wait,
        }
    }

    /// End the read: release every hold it owns (pending holds on completion) and drop buffers.
    pub fn terminate(&mut self, out: &mut Vec<Effect>) {
        if self.terminated {
            return;
        }
        self.terminated = true;
        for slot in self.slots.drain(..) {
            if let Slot::Ready { handle, .. } = slot {
                out.push(Effect::Discard(handle));
            }
        }
        self.selections.clear();
        self.issued.clear();
        if let Some(spill) = self.spill.take() {
            for (page, state) in &spill.pages {
                match state {
                    Spilling::Holding(id) => {
                        self.orphans.insert(*id, *page);
                    }
                    Spilling::Filling(id) => {
                        self.moved.insert(*id);
                    }
                    Spilling::Spilled => {}
                }
            }
            if !spill.held.is_empty() {
                out.push(Effect::Release(spill.held.into_iter().collect()));
            }
        }
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
