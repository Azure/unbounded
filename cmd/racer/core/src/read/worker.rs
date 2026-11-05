//! `WorkerCore`: the per-worker aggregate of the read-path machines (skeleton).
//!
//! It owns the worker's [`PageAcquisition`]s and [`ReadWindow`]s by id, the serve-side
//! [`ServeDecision`], the prefetch [`OwnerBudget`] and the one [`EffectIds`] allocator, routes
//! inputs to the machine that waits for them, and emits only external effects as the closed
//! [`Effect`] enum. Interpreters (production, and the byte-free Model) dispatch on
//! [`Effect::class`] with an exhaustive match and make no decisions.
//!
//! Not yet owned here: the page and metadata `FlightTable`s and the `MetadataCohort`; their
//! machines exist and move in when the production interpreter is wired.

use super::AcquisitionBudget;
use super::acquisition::{self, PageAcquisition};
use super::effect::{EffectId, EffectIds, Stale};
use super::serve::ServeDecision;
use super::walk::FillConfig;
use super::window::{self, Facts, Next, ReadWindow, WindowConfig};
use crate::prefetch::OwnerBudget;
use std::collections::BTreeMap;
use std::hash::Hash;
use std::time::Instant;

/// One page acquisition of this worker.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ReadId(pub u64);

/// One range read window of this worker.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct WindowId(pub u64);

/// The external resource an effect uses; the Model interpreter charges its cost model by class.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Class {
    /// A request to another node (probe, hedge, granted batch, provider selection).
    PeerSend,
    OriginFetch,
    /// Local memory or disk read (window probes).
    DiskRead,
    /// Local persistence (publish, spill hold and fill).
    DiskWrite,
    /// Payload verification and decryption.
    Crypto,
    Timer,
    /// A page or result handed to the client.
    ClientDeliver,
    /// A message to another worker of this node (direct fills, spill releases, buffer drops,
    /// rankings and membership state held by the topology owner).
    WorkerMsg,
}

/// One external effect of the worker.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Effect<R, M, A> {
    Acquisition {
        read: ReadId,
        effect: acquisition::Effect<R, M, A>,
    },
    Window {
        window: WindowId,
        effect: window::Effect,
    },
    /// The window's next delivery.
    Deliver { window: WindowId, next: Next },
}

impl<R, M, A> Effect<R, M, A> {
    /// The resource class of this effect.
    pub fn class(&self) -> Class {
        use acquisition::Effect as Acq;
        use window::Effect as Win;
        match self {
            Effect::Acquisition { effect, .. } => match effect {
                Acq::Send { .. } | Acq::Hedge { .. } | Acq::Batch { .. } => Class::PeerSend,
                Acq::Origin { .. } => Class::OriginFetch,
                Acq::Validate { .. } => Class::Crypto,
                Acq::Sleep { .. } => Class::Timer,
                Acq::Publish { .. } => Class::DiskWrite,
                Acq::Rank { .. } | Acq::Hints { .. } | Acq::Refresh { .. } | Acq::Report(_) => {
                    Class::WorkerMsg
                }
                Acq::Done(_) => Class::ClientDeliver,
            },
            Effect::Window { effect, .. } => match effect {
                Win::Probe { .. } => Class::DiskRead,
                Win::Select { .. } => Class::PeerSend,
                Win::Hold { .. } | Win::SpillFill { .. } => Class::DiskWrite,
                Win::Fetch { .. } | Win::Release(_) | Win::Discard(_) => Class::WorkerMsg,
            },
            Effect::Deliver { .. } => Class::ClientDeliver,
        }
    }

    /// The id the interpreter answers, when the effect expects an answer.
    pub fn id(&self) -> Option<EffectId> {
        use acquisition::Effect as Acq;
        use window::Effect as Win;
        match self {
            Effect::Acquisition { effect, .. } => match effect {
                Acq::Rank { id, .. }
                | Acq::Hints { id, .. }
                | Acq::Hedge { id, .. }
                | Acq::Refresh { id, .. }
                | Acq::Send { id, .. }
                | Acq::Validate { id, .. }
                | Acq::Batch { id, .. }
                | Acq::Sleep { id, .. }
                | Acq::Origin { id, .. }
                | Acq::Publish { id, .. } => Some(*id),
                Acq::Report(_) | Acq::Done(_) => None,
            },
            Effect::Window { effect, .. } => match effect {
                Win::Probe { id, .. }
                | Win::Fetch { id, .. }
                | Win::Select { id, .. }
                | Win::Hold { id, .. }
                | Win::SpillFill { id, .. } => Some(*id),
                Win::Release(_) | Win::Discard(_) => None,
            },
            Effect::Deliver { .. } => None,
        }
    }
}

/// Why the worker refused an event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refused {
    /// No such acquisition or window (finished or never started).
    Unknown,
    Stale(EffectId),
}

impl From<Stale> for Refused {
    fn from(stale: Stale) -> Self {
        Refused::Stale(stale.0)
    }
}

struct Read<R, M, A> {
    machine: PageAcquisition<R, M, A>,
    budget: AcquisitionBudget,
}

/// The worker aggregate. `P` identifies a page (serve counters, prefetch owner leases).
pub struct WorkerCore<R, M, A, P> {
    ids: EffectIds,
    next: u64,
    reads: BTreeMap<ReadId, Read<R, M, A>>,
    windows: BTreeMap<WindowId, ReadWindow>,
    pub serve: ServeDecision<P>,
    pub prefetch: OwnerBudget<P>,
}

impl<R: Clone, M, A, P: Clone + Eq + Hash> WorkerCore<R, M, A, P> {
    pub fn new(serve: ServeDecision<P>, prefetch: OwnerBudget<P>) -> Self {
        Self {
            ids: EffectIds::new(),
            next: 0,
            reads: BTreeMap::new(),
            windows: BTreeMap::new(),
            serve,
            prefetch,
        }
    }

    pub fn reads(&self) -> usize {
        self.reads.len()
    }

    pub fn windows(&self) -> usize {
        self.windows.len()
    }

    pub fn read(&self, read: ReadId) -> Option<&PageAcquisition<R, M, A>> {
        self.reads.get(&read).map(|r| &r.machine)
    }

    pub fn window(&self, window: WindowId) -> Option<&ReadWindow> {
        self.windows.get(&window)
    }

    fn allocate(&mut self) -> u64 {
        self.next += 1;
        self.next
    }

    /// Start a page acquisition from the caller's ranking; the first effect follows
    /// [`acquisition::Input::Start`].
    pub fn acquire(
        &mut self,
        config: FillConfig,
        initial: R,
        budget: AcquisitionBudget,
        now: Instant,
    ) -> ReadId {
        let read = ReadId(self.allocate());
        self.reads.insert(
            read,
            Read {
                machine: PageAcquisition::new(config, initial, now),
                budget,
            },
        );
        read
    }

    /// Feed an acquisition input. A finished acquisition is dropped after its `Done`.
    pub fn on_acquisition(
        &mut self,
        read: ReadId,
        input: acquisition::Input<R, M, A>,
        now: Instant,
        out: &mut Vec<Effect<R, M, A>>,
    ) -> Result<(), Refused> {
        let entry = self.reads.get_mut(&read).ok_or(Refused::Unknown)?;
        let mut effects = Vec::new();
        entry
            .machine
            .on(input, &mut entry.budget, now, &mut self.ids, &mut effects)?;
        let done = entry.machine.done().is_some();
        out.extend(
            effects
                .into_iter()
                .map(|effect| Effect::Acquisition { read, effect }),
        );
        if done {
            self.reads.remove(&read);
        }
        Ok(())
    }

    /// The budget the acquisition's outstanding send spends.
    pub fn request_budget(&mut self, read: ReadId) -> Option<&mut AcquisitionBudget> {
        let entry = self.reads.get_mut(&read)?;
        Some(entry.machine.request_budget(&mut entry.budget))
    }

    pub fn open(&mut self, config: WindowConfig) -> WindowId {
        let window = WindowId(self.allocate());
        self.windows.insert(window, ReadWindow::new(config));
        window
    }

    /// Plan window work, then deliver what is ready.
    pub fn pump(
        &mut self,
        window: WindowId,
        now: Instant,
        facts: &Facts,
        reserve: impl FnMut(u64) -> bool,
        out: &mut Vec<Effect<R, M, A>>,
    ) -> Result<(), Refused> {
        let machine = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
        let mut effects = Vec::new();
        machine.pump(now, facts, reserve, &mut self.ids, &mut effects);
        out.extend(
            effects
                .into_iter()
                .map(|effect| Effect::Window { window, effect }),
        );
        self.deliver(window, out)
    }

    /// Feed a window input, then deliver what is ready.
    pub fn on_window(
        &mut self,
        window: WindowId,
        input: window::Input,
        scope_live: bool,
        out: &mut Vec<Effect<R, M, A>>,
    ) -> Result<(), Refused> {
        let machine = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
        let mut effects = Vec::new();
        machine.on(input, scope_live, &mut self.ids, &mut effects)?;
        out.extend(
            effects
                .into_iter()
                .map(|effect| Effect::Window { window, effect }),
        );
        self.deliver(window, out)
    }

    /// Hand ready head pages to the client, in order, until the window waits or ends.
    fn deliver(&mut self, window: WindowId, out: &mut Vec<Effect<R, M, A>>) -> Result<(), Refused> {
        let machine = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
        loop {
            let mut effects = Vec::new();
            let next = machine.next(&mut effects);
            out.extend(
                effects
                    .into_iter()
                    .map(|effect| Effect::Window { window, effect }),
            );
            match next {
                Next::Wait => return Ok(()),
                Next::Page { .. } => out.push(Effect::Deliver { window, next }),
                Next::End | Next::Fail(_) => {
                    out.push(Effect::Deliver { window, next });
                    return Ok(());
                }
            }
        }
    }

    /// Close a window: its holds are released and its buffers dropped. Pending holds stay
    /// routable until they complete.
    pub fn close(&mut self, window: WindowId, out: &mut Vec<Effect<R, M, A>>) {
        if let Some(machine) = self.windows.get_mut(&window) {
            let mut effects = Vec::new();
            machine.terminate(&mut effects);
            out.extend(
                effects
                    .into_iter()
                    .map(|effect| Effect::Window { window, effect }),
            );
        }
    }

    /// Forget a closed window once its outstanding effects completed.
    pub fn drop_window(&mut self, window: WindowId) {
        self.windows.remove(&window);
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use crate::read::acquisition::{Input as AcqInput, RankAnswer};
    use crate::read::walk::{Features, Ranked, Reply};
    use crate::read::{Error, OperationKind, PageBudget, ResponseKind, Source};
    use std::time::Duration;

    type Core = WorkerCore<&'static str, u64, &'static str, u64>;

    fn core() -> Core {
        WorkerCore::new(ServeDecision::new(1 << 20), OwnerBudget::new(4, 0))
    }

    fn fill_config(now: Instant) -> FillConfig {
        FillConfig {
            operation: OperationKind::Page,
            hedge: false,
            walk_only: false,
            scope: now + Duration::from_secs(60),
            features: Features::PRODUCTION,
        }
    }

    #[test]
    fn an_acquisition_runs_to_done_and_is_dropped() {
        let now = Instant::now();
        let mut c = core();
        let read = c.acquire(
            fill_config(now),
            "r",
            AcquisitionBudget::new(now + Duration::from_secs(30), 8, 16),
            now,
        );
        let mut out = Vec::new();
        c.on_acquisition(read, AcqInput::Start { local_hit: false }, now, &mut out)
            .unwrap();
        let id = out[0].id().unwrap();
        assert_eq!(out[0].class(), Class::WorkerMsg, "ranking");
        out.clear();
        c.on_acquisition(
            read,
            AcqInput::Ranked {
                id,
                result: Ok(RankAnswer {
                    ranked: Ranked {
                        members: 3,
                        ..Ranked::new(5, &[10, 11, 12], None)
                    },
                    ranking: Some("r"),
                    recent: None,
                }),
            },
            now,
            &mut out,
        )
        .unwrap();
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].class(), Class::PeerSend);
        assert!(c.request_budget(read).is_some());
        let id = out[0].id().unwrap();
        out.clear();
        c.on_acquisition(
            read,
            AcqInput::Replied {
                id,
                reply: Reply::Answer {
                    kind: ResponseKind::Page,
                    signed: true,
                },
                answer: Some("a"),
            },
            now,
            &mut out,
        )
        .unwrap();
        assert_eq!(out.last().map(Effect::class), Some(Class::Crypto));
        let id = out.last().unwrap().id().unwrap();
        out.clear();
        c.on_acquisition(
            read,
            AcqInput::Validated {
                id,
                result: Ok(None),
            },
            now,
            &mut out,
        )
        .unwrap();
        assert_eq!(out.last().map(Effect::class), Some(Class::DiskWrite));
        let id = out.last().unwrap().id().unwrap();
        out.clear();
        c.on_acquisition(
            read,
            AcqInput::Published { id, result: Ok(()) },
            now,
            &mut out,
        )
        .unwrap();
        assert!(matches!(
            out.last(),
            Some(Effect::Acquisition {
                effect: acquisition::Effect::Done(acquisition::Done {
                    result: Ok(Source::Peer),
                    ..
                }),
                ..
            })
        ));
        assert_eq!(out.last().map(Effect::class), Some(Class::ClientDeliver));
        assert_eq!(c.reads(), 0);
        assert_eq!(
            c.on_acquisition(read, AcqInput::Cancel(Error::Cancelled), now, &mut out),
            Err(Refused::Unknown)
        );
    }

    #[test]
    fn stale_acquisition_inputs_are_refused() {
        let now = Instant::now();
        let mut c = core();
        let read = c.acquire(
            fill_config(now),
            "r",
            AcquisitionBudget::new(now + Duration::from_secs(30), 8, 16),
            now,
        );
        let mut out = Vec::new();
        c.on_acquisition(read, AcqInput::Start { local_hit: false }, now, &mut out)
            .unwrap();
        let bogus = EffectId(77);
        assert_eq!(
            c.on_acquisition(read, AcqInput::NoHelper { id: bogus }, now, &mut out),
            Err(Refused::Stale(bogus))
        );
    }

    #[test]
    fn a_window_delivers_through_the_core_and_ends() {
        let now = Instant::now();
        let mut c = core();
        let window = c.open(WindowConfig {
            start: 0,
            end: 2,
            window_pages: 2,
            sealed: false,
            passthrough: false,
            spill: 0,
            budget: PageBudget::Client {
                deadline: now + Duration::from_secs(60),
            },
        });
        let mut out = Vec::new();
        c.pump(window, now, &Facts::default(), |_| true, &mut out)
            .unwrap();
        let probes: Vec<_> = out.iter().filter_map(Effect::id).collect();
        assert_eq!(probes.len(), 2);
        assert!(out.iter().all(|e| e.class() == Class::DiskRead));
        out.clear();
        c.on_window(
            window,
            window::Input::Probed {
                id: probes[1],
                found: true,
            },
            true,
            &mut out,
        )
        .unwrap();
        assert!(out.is_empty(), "page 1 waits for page 0");
        c.on_window(
            window,
            window::Input::Probed {
                id: probes[0],
                found: true,
            },
            true,
            &mut out,
        )
        .unwrap();
        let delivered: Vec<_> = out
            .iter()
            .filter_map(|e| match e {
                Effect::Deliver { next, .. } => Some(*next),
                _ => None,
            })
            .collect();
        assert!(matches!(
            delivered[..],
            [
                Next::Page { page: 0, .. },
                Next::Page { page: 1, .. },
                Next::End
            ]
        ));
        c.close(window, &mut out);
        c.drop_window(window);
        assert_eq!(c.windows(), 0);
        assert_eq!(
            c.pump(window, now, &Facts::default(), |_| true, &mut out),
            Err(Refused::Unknown)
        );
    }

    #[test]
    fn ids_are_unique_across_machines() {
        let now = Instant::now();
        let mut c = core();
        let window = c.open(WindowConfig {
            start: 0,
            end: 3,
            window_pages: 3,
            sealed: false,
            passthrough: false,
            spill: 0,
            budget: PageBudget::Client {
                deadline: now + Duration::from_secs(60),
            },
        });
        let read = c.acquire(
            fill_config(now),
            "r",
            AcquisitionBudget::new(now + Duration::from_secs(30), 8, 16),
            now,
        );
        let mut out = Vec::new();
        c.pump(window, now, &Facts::default(), |_| true, &mut out)
            .unwrap();
        c.on_acquisition(read, AcqInput::Start { local_hit: false }, now, &mut out)
            .unwrap();
        let ids: Vec<_> = out.iter().filter_map(Effect::id).collect();
        let unique: std::collections::BTreeSet<_> = ids.iter().collect();
        assert_eq!(ids.len(), 4);
        assert_eq!(unique.len(), ids.len());
    }
}
