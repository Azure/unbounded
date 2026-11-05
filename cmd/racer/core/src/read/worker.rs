//! `WorkerCore`: the per-worker aggregate of the read-path machines.
//!
//! It owns the worker's page [`FlightTable`] and the [`PageAcquisition`] each flight leader runs,
//! the [`ReadWindow`]s by id, the metadata [`MetadataCohort`], the serve-side [`ServeDecision`]
//! with the successor hints it defers to, the prefetch [`OwnerBudget`] and the one [`EffectIds`]
//! allocator. It routes events between them and emits only external effects as the closed
//! [`Effect`] enum. Interpreters (production, and the byte-free Model) dispatch on
//! [`Effect::class`] with an exhaustive match and make no decisions.
//!
//! Routing inside the core:
//!
//! - A page read joins the page's flight. The elected leader runs a [`PageAcquisition`]; the
//!   other waiters get a completion when the flight settles. A canceled leader hands its running
//!   acquisition over to another live waiter; a flight with no waiter left cancels it.
//! - A window `Fetch` of a page this worker owns joins the local flight and answers the window
//!   directly. A page owned by another worker leaves as [`Effect::Worker`] with a typed
//!   [`WorkerMsg`]; the owner answers with [`WorkerMsg::Fetched`].
//! - An acquisition probe for the overflow helper (`Send { dest: None }`, the requester's
//!   reaction to a shed `Overloaded`) is resolved here with [`mesh_helper`] over the routing's
//!   `helper_of`; with no eligible helper the acquisition is answered `NoHelper` at once.
//! - [`acquisition::Report`]s are consumed into [`Observed`] counters; they never leave as effects.

use super::AcquisitionBudget;
use super::acquisition::{self, PageAcquisition};
use super::budget::mesh_helper;
use super::cohort::{self, CohortKey, MetadataCohort};
use super::effect::{EffectId, EffectIds, Stale};
use super::flight::{FlightLimits, FlightTable, Joined, Operation, Settled, Wait};
use super::hints::{MAX_DEFERRALS, SuccessorHints, defer_allowed, successor_deferral};
use super::serve::{Decision, ServeDecision, ServeRequest, Step};
use super::walk::{Evidence, FillConfig, SilenceNote};
use super::window::{self, AcquisitionBudgetSpec, Facts, Fetched, Next, ReadWindow, WindowConfig};
use super::{Error, RefreshPurpose, acquisition_failure};
use crate::flow::coalesce::flight::state::Published;
use crate::prefetch::OwnerBudget;
use std::collections::hash_map::DefaultHasher;
use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Debug;
use std::hash::{BuildHasherDefault, Hash};
use std::time::Instant;

/// The payload types of one worker: rankings, memberships, probe answers, page keys, objects and
/// metadata observations.
pub trait Types: 'static {
    type Rank: Clone + Debug + Eq;
    type Membership: Clone + Debug + Eq;
    type Answer: Clone + Debug + Eq;
    type Page: Clone + Debug + Eq + Hash + Ord;
    type Object: Clone + Debug + Eq + Hash;
    type Metadata: Clone + Debug + Eq;
}

/// One page acquisition of this worker.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ReadId(pub u64);

/// One range read window of this worker.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct WindowId(pub u64);

/// One page or metadata request joined to a flight of this worker.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct RequestId(pub u64);

/// A worker of this node, by index.
pub type Worker = u32;

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
    /// A message to another worker of this node (direct fills owned elsewhere, spill releases,
    /// buffer drops, rankings and membership state held by the topology owner).
    WorkerMsg,
}

/// A typed message between workers of one node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WorkerMsg<P> {
    /// Fill `page` for the sender's window fetch `id`; answer [`WorkerMsg::Fetched`].
    Fetch {
        id: EffectId,
        from: Worker,
        page: P,
        budget: AcquisitionBudgetSpec,
        sealed: bool,
    },
    /// The answer to the receiver's window fetch `id`.
    Fetched {
        id: EffectId,
        result: Result<Fetched, Error>,
    },
}

/// One external effect of the worker.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Effect<T: Types> {
    /// An effect of a running acquisition. Its `Report`s and `Done` are consumed by the core and
    /// cannot be expressed here.
    Acquisition {
        read: ReadId,
        page: T::Page,
        effect: acquisition::Io<T::Rank, T::Membership, T::Answer>,
    },
    /// A window effect (never a `Fetch`: the core routes it).
    Window {
        window: WindowId,
        effect: window::Effect,
    },
    /// The window's next delivery.
    Deliver { window: WindowId, next: Next },
    /// A message to another worker of this node.
    Worker { to: Worker, msg: WorkerMsg<T::Page> },
    /// A client page read ended; on `Ok` the page is in the local tiers.
    Completed {
        request: RequestId,
        page: T::Page,
        result: Result<(), Error>,
    },
    /// Refresh `key`'s metadata from origin; answer [`WorkerCore::on_metadata`].
    Metadata {
        id: EffectId,
        key: CohortKey<T::Object>,
    },
    /// A metadata request ended.
    MetadataReady {
        request: RequestId,
        result: Result<T::Metadata, Error>,
    },
    /// What a probe of `page` to member position `dest`, sent at `sent`, showed about `dest`'s
    /// liveness (production `CandidatePolicy::apply` into the node's `LinkHealth`): the
    /// interpreter records it in its node-wide [`crate::liveness::Silence`] and orders later
    /// rankings with [`crate::read::demote_unanswered`]. For an overflow helper probe, `dest` is the
    /// helper.
    Silence {
        page: T::Page,
        dest: u32,
        sent: Instant,
        note: SilenceNote,
    },
}

impl<T: Types> Effect<T> {
    /// The resource class of this effect.
    pub fn class(&self) -> Class {
        use acquisition::Io as Acq;
        use window::Effect as Win;
        match self {
            Effect::Acquisition { effect, .. } => match effect {
                Acq::Send { .. } | Acq::Hedge { .. } | Acq::Batch { .. } => Class::PeerSend,
                Acq::Origin { .. } => Class::OriginFetch,
                Acq::Validate { .. } => Class::Crypto,
                Acq::Sleep { .. } => Class::Timer,
                Acq::Publish { .. } => Class::DiskWrite,
                Acq::Rank { .. } | Acq::Hints { .. } | Acq::Refresh { .. } => Class::WorkerMsg,
            },
            Effect::Window { effect, .. } => match effect {
                Win::Probe { .. } => Class::DiskRead,
                Win::Select { .. } => Class::PeerSend,
                Win::Hold { .. } | Win::SpillFill { .. } => Class::DiskWrite,
                Win::Fetch { .. } | Win::Release(_) | Win::Discard(_) => Class::WorkerMsg,
            },
            Effect::Worker { .. } => Class::WorkerMsg,
            Effect::Metadata { .. } => Class::OriginFetch,
            Effect::Deliver { .. } | Effect::Completed { .. } | Effect::MetadataReady { .. } => {
                Class::ClientDeliver
            }
            Effect::Silence { .. } => Class::WorkerMsg,
        }
    }

    /// The id the interpreter answers, when the effect expects an answer. A
    /// [`WorkerMsg::Fetch`] is answered by the receiving worker.
    pub fn id(&self) -> Option<EffectId> {
        use acquisition::Io as Acq;
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
            },
            Effect::Window { effect, .. } => match effect {
                Win::Probe { id, .. }
                | Win::Fetch { id, .. }
                | Win::Select { id, .. }
                | Win::Hold { id, .. }
                | Win::SpillFill { id, .. } => Some(*id),
                Win::Release(_) | Win::Discard(_) => None,
            },
            Effect::Worker { msg, .. } => match msg {
                WorkerMsg::Fetch { id, .. } => Some(*id),
                WorkerMsg::Fetched { .. } => None,
            },
            Effect::Metadata { id, .. } => Some(*id),
            Effect::Deliver { .. }
            | Effect::Completed { .. }
            | Effect::MetadataReady { .. }
            | Effect::Silence { .. } => None,
        }
    }
}

/// Why the worker refused an event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refused {
    /// No such acquisition, window or request (finished or never started).
    Unknown,
    Stale(EffectId),
}

impl From<Stale> for Refused {
    fn from(stale: Stale) -> Self {
        Refused::Stale(stale.0)
    }
}

/// What a shed requester knows about a page's owners when it picks its overflow helper.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Mesh {
    /// The page's ranked owners (member positions).
    pub owners: Vec<u32>,
    /// [`super::budget::helper_start`] of this node.
    pub start: usize,
    /// This node's member position.
    pub reader: u32,
}

/// The worker owning a page's flight.
pub type OwnerOf<P> = Box<dyn Fn(&P) -> Worker>;
/// The fill configuration and initial ranking of a leader acquiring a page on a budget.
pub type Plan<P, R> = Box<dyn Fn(&P, &AcquisitionBudgetSpec) -> (FillConfig, R)>;
/// A page's owners under a version, for an overflow helper choice.
pub type MeshOf<P> = Box<dyn Fn(&P, u64) -> Option<Mesh>>;

/// The worker's view of the node and its topology. Pure callbacks supplied by the interpreter.
pub struct Routing<T: Types> {
    /// This worker.
    pub me: Worker,
    /// The worker that owns `page`'s flight.
    pub owner_of: OwnerOf<T::Page>,
    /// The fill configuration and initial ranking of a leader acquiring `page` on `budget`.
    pub plan: Plan<T::Page, T::Rank>,
    /// The page's owners under `version`, for an overflow helper choice.
    pub mesh: MeshOf<T::Page>,
    /// The middle node of this node's primary mesh path to an owner.
    pub helper_of: Box<dyn Fn(u32) -> Option<u32>>,
}

/// Counters of the acquisitions' [`acquisition::Report`]s.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Observed {
    pub evidence: u64,
    pub origin_fetched: u64,
    pub origin_failed: u64,
    /// Overflow helper probes sent, and shed requesters with no helper to ask.
    pub helpers: u64,
    pub no_helper: u64,
    /// Waiters elected to lead a flight after an earlier leader of it ended without
    /// settling it (production `Flights::fail`; racer-sim `flight_reelected`).
    pub reelected: u64,
    /// Acquisitions that ended with a result, and those whose result was an error
    /// (racer-sim `fills_*`).
    pub fills: u64,
    pub fills_failed: u64,
}

/// A serve-side decision with successor deferral.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Served {
    Decided(Decision),
    /// Answer `Miss` and point the requester at this hinted successor owner.
    Defer(u32),
}

/// The page view a [`WorkerCore::serve`] decision needs.
pub struct ServeView<'a> {
    pub owners: &'a [u32],
    pub local: u32,
    pub requester: u32,
    pub column_mate: bool,
}

/// Where a page request's completion goes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ReplyTo {
    Client,
    Window {
        window: WindowId,
        id: EffectId,
        sealed: bool,
    },
    Worker {
        to: Worker,
        id: EffectId,
        sealed: bool,
    },
}

struct Request<P> {
    page: P,
    waiter: u64,
    budget: AcquisitionBudgetSpec,
    reply: ReplyTo,
}

struct Read<T: Types> {
    machine: PageAcquisition<T::Rank, T::Membership, T::Answer>,
    budget: AcquisitionBudget,
    page: T::Page,
    /// The overflow helper the read's latest probe went to, if it went to one: the acquisition
    /// asks for a helper without a destination ([`Self::helper`] resolves it), so its evidence
    /// about that probe names none.
    helper: Option<u32>,
}

#[derive(Default)]
struct Flight {
    /// The running acquisition and the operation it retains, so a canceled leader can hand it
    /// over instead of revoking it.
    read: Option<(ReadId, Operation)>,
    requests: BTreeSet<RequestId>,
    /// A leader was elected before.
    led: bool,
}

struct MetaRequest<Obj> {
    key: CohortKey<Obj>,
    waiter: cohort::Member,
    attempts: u32,
    /// The refresh this request leads, when it leads one.
    leading: Option<(EffectId, cohort::Started)>,
}

struct Window<P> {
    machine: ReadWindow,
    key_of: Box<dyn Fn(u64) -> P>,
    /// The client was handed `End` or `Fail`; nothing more is delivered.
    ended: bool,
}

type Hasher = BuildHasherDefault<DefaultHasher>;
type PageFlights<P> = FlightTable<P, (), (), (), (), Hasher>;

fn complete(value: ()) -> Published<(), ()> {
    Published::Complete(value)
}

/// The worker aggregate.
pub struct WorkerCore<T: Types> {
    ids: EffectIds,
    next: u64,
    routing: Routing<T>,
    flights: PageFlights<T::Page>,
    flight: BTreeMap<T::Page, Flight>,
    requests: BTreeMap<RequestId, Request<T::Page>>,
    reads: BTreeMap<ReadId, Read<T>>,
    windows: BTreeMap<WindowId, Window<T::Page>>,
    /// Window fetches sent to another worker, by id.
    remote: BTreeMap<EffectId, WindowId>,
    metadata: MetadataCohort<T::Object, T::Metadata, (), Hasher>,
    meta_requests: BTreeMap<RequestId, MetaRequest<T::Object>>,
    observed: Observed,
    pub serve: ServeDecision<T::Page>,
    pub successors: SuccessorHints<T::Page, u32>,
    pub prefetch: OwnerBudget<T::Page>,
}

impl<T: Types> WorkerCore<T> {
    #[allow(clippy::too_many_arguments)] // Each machine has its own configuration.
    pub fn new(
        routing: Routing<T>,
        limits: FlightLimits,
        metadata_entries: usize,
        serve: ServeDecision<T::Page>,
        successors: SuccessorHints<T::Page, u32>,
        prefetch: OwnerBudget<T::Page>,
        now: Instant,
    ) -> Self {
        Self {
            ids: EffectIds::new(),
            next: 0,
            routing,
            flights: FlightTable::new(limits, complete, now, Hasher::default()),
            flight: BTreeMap::new(),
            requests: BTreeMap::new(),
            reads: BTreeMap::new(),
            windows: BTreeMap::new(),
            remote: BTreeMap::new(),
            metadata: MetadataCohort::with_limits(metadata_entries, now, Hasher::default()),
            meta_requests: BTreeMap::new(),
            observed: Observed::default(),
            serve,
            successors,
            prefetch,
        }
    }

    pub fn me(&self) -> Worker {
        self.routing.me
    }

    /// Running acquisitions.
    pub fn reads(&self) -> usize {
        self.reads.len()
    }

    pub fn windows(&self) -> usize {
        self.windows.len()
    }

    /// Page flights in the table.
    pub fn flights(&self) -> usize {
        self.flights.len()
    }

    /// Page requests waiting on a flight.
    pub fn requests(&self) -> usize {
        self.requests.len()
    }

    /// Metadata requests waiting on a cohort, and live cohorts.
    pub fn metadata(&self) -> (usize, usize) {
        (self.meta_requests.len(), self.metadata.table().len())
    }

    /// Window fetches waiting on another worker.
    pub fn remote_fetches(&self) -> usize {
        self.remote.len()
    }

    pub fn observed(&self) -> Observed {
        self.observed
    }

    /// Whether `page` has an acquisition running on this worker.
    pub fn acquiring(&self, page: &T::Page) -> bool {
        self.flights.acquiring(page)
    }

    pub fn read(
        &self,
        read: ReadId,
    ) -> Option<&PageAcquisition<T::Rank, T::Membership, T::Answer>> {
        self.reads.get(&read).map(|r| &r.machine)
    }

    /// The budget the outstanding `Send` of acquisition `read` spends
    /// ([`PageAcquisition::request_budget`]): plan the attempt on it before sending.
    pub fn request_budget(&mut self, read: ReadId) -> Option<&mut AcquisitionBudget> {
        let r = self.reads.get_mut(&read)?;
        Some(r.machine.request_budget(&mut r.budget))
    }

    pub fn window(&self, window: WindowId) -> Option<&ReadWindow> {
        self.windows.get(&window).map(|w| &w.machine)
    }

    fn allocate(&mut self) -> u64 {
        self.next += 1;
        self.next
    }

    // Page flights.

    /// A client reads `page` on `budget`: join its flight. Completes with [`Effect::Completed`].
    pub fn read_page(
        &mut self,
        page: T::Page,
        budget: AcquisitionBudgetSpec,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> RequestId {
        self.join(page, budget, ReplyTo::Client, now, out)
    }

    /// A copy-only peer request may wait on `page`'s running acquisition (production
    /// `Fill::copy_only_joining` through `Flights::join_copy`): it never leads, adopts or starts
    /// an acquisition. `None` when no flight is running (the request is a miss); otherwise the
    /// request completes with [`Effect::Completed`] like [`Self::read_page`], for example
    /// `FlightFull` past the flight's waiter cap.
    pub fn copy_page(
        &mut self,
        page: T::Page,
        deadline: Instant,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Option<RequestId> {
        let mut wakes = Vec::new();
        let joined = self
            .flights
            .join_copy(&page, now, deadline, |_| Ok(()), &mut wakes);
        let reply = ReplyTo::Client;
        let request = match joined {
            Ok(Joined::Miss) => None,
            Ok(Joined::Waiter(waiter)) => {
                let request = RequestId(self.allocate());
                self.requests.insert(
                    request,
                    Request {
                        page: page.clone(),
                        waiter,
                        // A copy waiter has no election rights, so it never spends attempts.
                        budget: AcquisitionBudgetSpec {
                            deadline,
                            attempts: 0,
                            links: 0,
                            route_failed: false,
                        },
                        reply,
                    },
                );
                self.flight
                    .entry(page)
                    .or_default()
                    .requests
                    .insert(request);
                self.poll(request, now, out);
                Some(request)
            }
            Ok(Joined::Settled(_)) => {
                let request = RequestId(self.allocate());
                self.reply(request, &page, reply, Ok(()), now, out);
                Some(request)
            }
            Err(error) => {
                let request = RequestId(self.allocate());
                self.reply(request, &page, reply, Err(error), now, out);
                Some(request)
            }
        };
        self.wake(wakes, now, out);
        request
    }

    /// The scope of client request `request` ended. A leader hands its acquisition over.
    pub fn cancel(&mut self, request: RequestId, now: Instant, out: &mut Vec<Effect<T>>) {
        let Some(r) = self.requests.get(&request) else {
            return;
        };
        let (page, waiter) = (r.page.clone(), r.waiter);
        let mut wakes = Vec::new();
        self.flights.cancel(&page, waiter, &mut wakes);
        self.respond(request, Err(Error::Cancelled), now, out);
        self.wake(wakes, now, out);
    }

    fn join(
        &mut self,
        page: T::Page,
        budget: AcquisitionBudgetSpec,
        reply: ReplyTo,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> RequestId {
        let request = RequestId(self.allocate());
        let mut wakes = Vec::new();
        let joined = self.flights.join(
            &page,
            now,
            budget.deadline,
            budget.attempts,
            true,
            |_| Ok(()),
            &mut wakes,
        );
        match joined {
            Ok(Joined::Waiter(waiter)) => {
                self.requests.insert(
                    request,
                    Request {
                        page: page.clone(),
                        waiter,
                        budget,
                        reply,
                    },
                );
                self.flight
                    .entry(page)
                    .or_default()
                    .requests
                    .insert(request);
                self.poll(request, now, out);
            }
            Ok(Joined::Settled(_)) => self.reply(request, &page, reply, Ok(()), now, out),
            Ok(Joined::Miss) => self.reply(request, &page, reply, Err(Error::Internal), now, out),
            Err(error) => self.reply(request, &page, reply, Err(error), now, out),
        }
        self.wake(wakes, now, out);
        request
    }

    /// Poll a joined request and act on what its flight says.
    fn poll(&mut self, request: RequestId, now: Instant, out: &mut Vec<Effect<T>>) {
        let Some(r) = self.requests.get(&request) else {
            return;
        };
        let (page, waiter, budget) = (r.page.clone(), r.waiter, r.budget);
        let mut wakes = Vec::new();
        let polled = self
            .flights
            .poll(&page, waiter, now, budget.attempts, request.0, &mut wakes);
        match polled {
            Ok(Wait::Pending) => {}
            Ok(Wait::Lead) => {
                // A canceled or expired leader passes its running acquisition on.
                let _ = self.flights.allow_handover(&page, waiter, &mut wakes);
                match self.flights.retain_operation(&page, waiter, &mut wakes) {
                    Ok(operation) => self.lead(page, budget, operation, now, out),
                    Err(error) => {
                        let _ = self.flights.finish(
                            &page,
                            waiter,
                            Err(acquisition_failure(error)),
                            &mut wakes,
                        );
                        self.respond(request, Err(error), now, out);
                    }
                }
            }
            Ok(Wait::Ready(Settled::Complete(()) | Settled::Partial(()))) => {
                self.respond(request, Ok(()), now, out)
            }
            Ok(Wait::Failed(error)) | Err(error) => self.respond(request, Err(error), now, out),
        }
        self.wake(wakes, now, out);
    }

    fn wake(&mut self, wakes: Vec<u64>, now: Instant, out: &mut Vec<Effect<T>>) {
        for token in wakes {
            self.poll(RequestId(token), now, out);
        }
    }

    /// Poll every request of `page` (after its flight changed).
    fn poll_flight(&mut self, page: &T::Page, now: Instant, out: &mut Vec<Effect<T>>) {
        let requests: Vec<_> = self
            .flight
            .get(page)
            .map(|f| f.requests.iter().copied().collect())
            .unwrap_or_default();
        for request in requests {
            self.poll(request, now, out);
        }
    }

    /// Start the leader's acquisition of `page`.
    fn lead(
        &mut self,
        page: T::Page,
        budget: AcquisitionBudgetSpec,
        operation: Operation,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) {
        let (config, initial) = (self.routing.plan)(&page, &budget);
        let read = ReadId(self.allocate());
        let flight = self.flight.entry(page.clone()).or_default();
        // A new generation is elected only after the previous operation completed.
        debug_assert!(flight.read.is_none());
        self.observed.reelected += u64::from(flight.led);
        flight.led = true;
        flight.read = Some((read, operation));
        self.reads.insert(
            read,
            Read {
                machine: PageAcquisition::new(config, initial, now),
                helper: None,
                budget: budget.build(),
                page,
            },
        );
        let _ = self.feed(
            read,
            acquisition::Input::Start { local_hit: false },
            now,
            out,
        );
    }

    /// Feed an acquisition input. A finished acquisition settles its flight.
    pub fn on_acquisition(
        &mut self,
        read: ReadId,
        input: acquisition::Input<T::Rank, T::Membership, T::Answer>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        self.feed(read, input, now, out)
    }

    fn feed(
        &mut self,
        read: ReadId,
        input: acquisition::Input<T::Rank, T::Membership, T::Answer>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        let mut pending = vec![input];
        while let Some(input) = pending.pop() {
            let entry = self.reads.get_mut(&read).ok_or(Refused::Unknown)?;
            let mut effects = Vec::new();
            entry
                .machine
                .on(input, &mut entry.budget, now, &mut self.ids, &mut effects)?;
            let page = entry.page.clone();
            let mut helper_dest = entry.helper;
            for effect in effects {
                use acquisition::Io as Acq;
                let effect = match effect.split() {
                    Ok(io) => io,
                    Err(acquisition::Terminal::Report(report)) => {
                        if let acquisition::Report::Evidence {
                            evidence:
                                Evidence {
                                    silence: Some(note),
                                    ..
                                },
                            dest,
                            sent,
                            ..
                        } = report
                            // Production names the helper (`CandidatePolicy::apply`,
                            // `ProbeDest::Helper`), so a helper probe's timeout counts against
                            // the helper too.
                            && let Some(dest) = dest.or(helper_dest)
                        {
                            out.push(Effect::Silence {
                                page: page.clone(),
                                dest,
                                sent,
                                note,
                            });
                        }
                        self.observe(report);
                        continue;
                    }
                    Err(acquisition::Terminal::Done(done)) => {
                        self.finish(read, done.result.map(|_| ()), now, out);
                        continue;
                    }
                };
                if let Acq::Send { .. } | Acq::Hedge { .. } | Acq::Batch { .. } = effect {
                    helper_dest = None;
                }
                match effect {
                    Acq::Send {
                        id,
                        probe,
                        dest: None,
                        version,
                    } => match self.helper(&page, version) {
                        Some(helper) => {
                            helper_dest = Some(helper);
                            self.observed.helpers += 1;
                            out.push(Effect::Acquisition {
                                read,
                                page: page.clone(),
                                effect: Acq::Send {
                                    id,
                                    probe,
                                    dest: Some(helper),
                                    version,
                                },
                            });
                        }
                        None => {
                            self.observed.no_helper += 1;
                            pending.push(acquisition::Input::NoHelper { id });
                        }
                    },
                    effect => out.push(Effect::Acquisition {
                        read,
                        page: page.clone(),
                        effect,
                    }),
                }
            }
            if let Some(entry) = self.reads.get_mut(&read) {
                entry.helper = helper_dest;
            }
        }
        Ok(())
    }

    /// The overflow helper a shed requester asks for `page` under `version`.
    fn helper(&self, page: &T::Page, version: u64) -> Option<u32> {
        let mesh = (self.routing.mesh)(page, version)?;
        mesh_helper(
            &mesh.owners,
            mesh.start,
            |o| (self.routing.helper_of)(*o),
            |n| *n == mesh.reader || mesh.owners.contains(n),
        )
    }

    fn observe(&mut self, report: acquisition::Report) {
        match report {
            acquisition::Report::Evidence { .. } => self.observed.evidence += 1,
            acquisition::Report::OriginFetched { .. } => self.observed.origin_fetched += 1,
            acquisition::Report::OriginFailed { .. } => self.observed.origin_failed += 1,
        }
    }

    /// Acquisition `read` ended: settle its flight for the leader it now serves.
    fn finish(
        &mut self,
        read: ReadId,
        result: Result<(), Error>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) {
        let Some(entry) = self.reads.remove(&read) else {
            return;
        };
        self.observed.fills += 1;
        self.observed.fills_failed += u64::from(result.is_err());
        let page = entry.page;
        let Some(flight) = self.flight.get_mut(&page) else {
            return;
        };
        let Some((_, operation)) = flight.read.filter(|(r, _)| *r == read) else {
            return;
        };
        flight.read = None;
        let mut wakes = Vec::new();
        let _ = self
            .flights
            .complete_operation(&page, operation, &mut wakes);
        let Some(flight) = self.flight.get(&page) else {
            self.wake(wakes, now, out);
            return;
        };
        let leader = flight
            .requests
            .iter()
            .filter_map(|r| self.requests.get(r))
            .map(|r| r.waiter)
            .find(|w| self.flights.is_leader(&page, *w));
        if let Some(leader) = leader {
            let result = result.map_err(acquisition_failure);
            let _ = self.flights.finish(&page, leader, result, &mut wakes);
        }
        self.wake(wakes, now, out);
        self.poll_flight(&page, now, out);
    }

    /// Request `request` ended: detach it, drop an abandoned flight, and send its reply.
    fn respond(
        &mut self,
        request: RequestId,
        result: Result<(), Error>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) {
        let Some(r) = self.requests.remove(&request) else {
            return;
        };
        let mut wakes = Vec::new();
        self.flights.detach(&r.page, r.waiter, &mut wakes);
        if let Some(flight) = self.flight.get_mut(&r.page) {
            flight.requests.remove(&request);
            if flight.requests.is_empty() {
                let flight = self.flight.remove(&r.page).expect("present");
                if let Some((read, operation)) = flight.read {
                    // Nobody waits: stop the acquisition. Its outstanding effects go stale.
                    self.reads.remove(&read);
                    let _ = self
                        .flights
                        .complete_operation(&r.page, operation, &mut wakes);
                }
            }
        }
        self.reply(request, &r.page, r.reply, result, now, out);
        self.wake(wakes, now, out);
    }

    fn reply(
        &mut self,
        request: RequestId,
        page: &T::Page,
        reply: ReplyTo,
        result: Result<(), Error>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) {
        let fetched = |sealed: bool| {
            result.map(|()| {
                if sealed {
                    Fetched::Sealed
                } else {
                    Fetched::Page
                }
            })
        };
        match reply {
            ReplyTo::Client => out.push(Effect::Completed {
                request,
                page: page.clone(),
                result,
            }),
            ReplyTo::Window { window, id, sealed } => {
                let input = window::Input::Fetched {
                    id,
                    result: fetched(sealed),
                };
                let _ = self.on_window(window, input, true, now, out);
            }
            ReplyTo::Worker { to, id, sealed } => out.push(Effect::Worker {
                to,
                msg: WorkerMsg::Fetched {
                    id,
                    result: fetched(sealed),
                },
            }),
        }
    }

    /// Expire waiters whose deadline passed and settle what that changes.
    pub fn tick(&mut self, now: Instant, out: &mut Vec<Effect<T>>) {
        let pages: Vec<_> = self.flight.keys().cloned().collect();
        for page in pages {
            let mut wakes = Vec::new();
            self.flights.refresh(&page, now, &mut wakes);
            self.wake(wakes, now, out);
            self.poll_flight(&page, now, out);
        }
    }

    // Worker messages.

    /// A message from another worker of this node.
    pub fn on_worker_msg(
        &mut self,
        msg: WorkerMsg<T::Page>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        match msg {
            WorkerMsg::Fetch {
                id,
                from,
                page,
                budget,
                sealed,
            } => {
                self.join(
                    page,
                    budget,
                    ReplyTo::Worker {
                        to: from,
                        id,
                        sealed,
                    },
                    now,
                    out,
                );
                Ok(())
            }
            WorkerMsg::Fetched { id, result } => {
                let window = self.remote.remove(&id).ok_or(Refused::Unknown)?;
                self.on_window(
                    window,
                    window::Input::Fetched { id, result },
                    true,
                    now,
                    out,
                )
            }
        }
    }

    // Windows.

    /// Open a window over pages whose flight keys are `key_of(page number)`.
    pub fn open(&mut self, config: WindowConfig, key_of: Box<dyn Fn(u64) -> T::Page>) -> WindowId {
        let window = WindowId(self.allocate());
        self.windows.insert(
            window,
            Window {
                machine: ReadWindow::new(config),
                key_of,
                ended: false,
            },
        );
        window
    }

    /// Plan window work, then deliver what is ready.
    pub fn pump(
        &mut self,
        window: WindowId,
        now: Instant,
        facts: &Facts,
        reserve: impl FnMut(u64) -> bool,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        let entry = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
        let mut effects = Vec::new();
        entry
            .machine
            .pump(now, facts, reserve, &mut self.ids, &mut effects);
        self.route_window(window, effects, now, out);
        self.deliver(window, now, out)
    }

    /// Feed a window input, then deliver what is ready.
    pub fn on_window(
        &mut self,
        window: WindowId,
        input: window::Input,
        scope_live: bool,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        let entry = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
        let mut effects = Vec::new();
        entry
            .machine
            .on(input, scope_live, &mut self.ids, &mut effects)?;
        self.route_window(window, effects, now, out);
        self.deliver(window, now, out)
    }

    /// Route window fetches to the owning worker's flight; pass the rest out.
    fn route_window(
        &mut self,
        window: WindowId,
        effects: Vec<window::Effect>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) {
        for effect in effects {
            let window::Effect::Fetch {
                id,
                page,
                budget,
                sealed,
            } = effect
            else {
                out.push(Effect::Window { window, effect });
                continue;
            };
            let Some(entry) = self.windows.get(&window) else {
                continue;
            };
            let key = (entry.key_of)(page);
            let owner = (self.routing.owner_of)(&key);
            if owner == self.routing.me {
                self.join(
                    key,
                    budget,
                    ReplyTo::Window { window, id, sealed },
                    now,
                    out,
                );
            } else {
                self.remote.insert(id, window);
                out.push(Effect::Worker {
                    to: owner,
                    msg: WorkerMsg::Fetch {
                        id,
                        from: self.routing.me,
                        page: key,
                        budget,
                        sealed,
                    },
                });
            }
        }
    }

    /// Hand ready head pages to the client, in order, until the window waits or ends.
    fn deliver(
        &mut self,
        window: WindowId,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        loop {
            let entry = self.windows.get_mut(&window).ok_or(Refused::Unknown)?;
            if entry.ended {
                return Ok(());
            }
            let mut effects = Vec::new();
            let next = entry.machine.next(&mut effects);
            if matches!(next, Next::End | Next::Fail(_)) {
                entry.ended = true;
            }
            self.route_window(window, effects, now, out);
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

    /// Close a window: its holds are released, its buffers dropped and its local fetches
    /// canceled. Pending holds stay routable until they complete.
    pub fn close(&mut self, window: WindowId, now: Instant, out: &mut Vec<Effect<T>>) {
        let Some(entry) = self.windows.get_mut(&window) else {
            return;
        };
        let mut effects = Vec::new();
        entry.machine.terminate(&mut effects);
        out.extend(
            effects
                .into_iter()
                .map(|effect| Effect::Window { window, effect }),
        );
        let local: Vec<_> = self
            .requests
            .iter()
            .filter(|(_, r)| matches!(r.reply, ReplyTo::Window { window: w, .. } if w == window))
            .map(|(id, _)| *id)
            .collect();
        for request in local {
            if let Some(r) = self.requests.get_mut(&request) {
                // The window is gone: complete the waiter without answering it.
                r.reply = ReplyTo::Client;
            }
            self.cancel_silently(request, now, out);
        }
    }

    fn cancel_silently(&mut self, request: RequestId, now: Instant, out: &mut Vec<Effect<T>>) {
        let mut scratch = Vec::new();
        self.cancel(request, now, &mut scratch);
        out.extend(
            scratch
                .into_iter()
                .filter(|e| !matches!(e, Effect::Completed { request: r, .. } if *r == request)),
        );
    }

    /// Forget a closed window once its outstanding effects completed. Answers of fetches sent to
    /// other workers are refused from then on.
    pub fn drop_window(&mut self, window: WindowId) {
        self.windows.remove(&window);
        self.remote.retain(|_, w| *w != window);
    }

    // Metadata cohorts.

    /// Refresh `object`'s metadata for `purpose`, joining its cohort. Completes with
    /// [`Effect::MetadataReady`].
    pub fn refresh_metadata(
        &mut self,
        object: T::Object,
        purpose: RefreshPurpose,
        deadline: Instant,
        attempts: u32,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> RequestId {
        let request = RequestId(self.allocate());
        let key = CohortKey { object, purpose };
        let mut wakes = Vec::new();
        match self
            .metadata
            .join(&key, now, deadline, attempts, |_| Ok(()), &mut wakes)
        {
            Ok(waiter) => {
                self.meta_requests.insert(
                    request,
                    MetaRequest {
                        key,
                        waiter,
                        attempts,
                        leading: None,
                    },
                );
                self.meta_poll(request, now, out);
            }
            Err(error) => out.push(Effect::MetadataReady {
                request,
                result: Err(error),
            }),
        }
        self.meta_wake(wakes, now, out);
        request
    }

    fn meta_poll(&mut self, request: RequestId, now: Instant, out: &mut Vec<Effect<T>>) {
        let Some(r) = self.meta_requests.get(&request) else {
            return;
        };
        if r.leading.is_some() {
            return;
        }
        let (key, waiter, attempts) = (r.key.clone(), r.waiter, r.attempts);
        let mut wakes = Vec::new();
        match self
            .metadata
            .poll(&key, waiter, now, attempts, request.0, &mut wakes)
        {
            Ok(cohort::Step::Pending) => {}
            Ok(cohort::Step::Lead(started)) => {
                let id = self.ids.allocate();
                if let Some(r) = self.meta_requests.get_mut(&request) {
                    r.leading = Some((id, started));
                }
                out.push(Effect::Metadata { id, key });
            }
            Ok(cohort::Step::Ready(m)) => self.meta_done(request, Ok(m), out),
            Ok(cohort::Step::Failed(error)) | Err(error) => {
                self.meta_done(request, Err(error), out)
            }
        }
        self.meta_wake(wakes, now, out);
    }

    fn meta_wake(&mut self, wakes: Vec<u64>, now: Instant, out: &mut Vec<Effect<T>>) {
        for token in wakes {
            self.meta_poll(RequestId(token), now, out);
        }
    }

    fn meta_done(
        &mut self,
        request: RequestId,
        result: Result<T::Metadata, Error>,
        out: &mut Vec<Effect<T>>,
    ) {
        if let Some(r) = self.meta_requests.remove(&request) {
            let mut wakes = Vec::new();
            self.metadata.detach(&r.key, r.waiter, &mut wakes);
            out.push(Effect::MetadataReady { request, result });
        }
    }

    /// The answer to [`Effect::Metadata`] `id`.
    pub fn on_metadata(
        &mut self,
        id: EffectId,
        result: Result<T::Metadata, Error>,
        now: Instant,
        out: &mut Vec<Effect<T>>,
    ) -> Result<(), Refused> {
        let (request, started) = self
            .meta_requests
            .iter()
            .find_map(|(r, m)| match m.leading {
                Some((lead, started)) if lead == id => Some((*r, started)),
                _ => None,
            })
            .ok_or(Refused::Stale(id))?;
        let r = self.meta_requests.get_mut(&request).expect("present");
        r.leading = None;
        r.attempts = r.attempts.saturating_sub(1);
        let (key, remaining) = (r.key.clone(), r.attempts);
        let mut wakes = Vec::new();
        let _ = self
            .metadata
            .settle(&key, started, result, remaining, &mut wakes);
        let waiting: Vec<_> = self
            .meta_requests
            .iter()
            .filter(|(_, m)| m.key == key)
            .map(|(r, _)| *r)
            .collect();
        for request in waiting {
            self.meta_poll(request, now, out);
        }
        self.meta_wake(wakes, now, out);
        Ok(())
    }

    // Serve side.

    /// Decide how to serve a peer page request, deferring an `Acquire` to a hinted successor owner
    /// when no local acquisition of the page runs.
    pub fn serve(
        &mut self,
        page: &T::Page,
        request: ServeRequest,
        view: &ServeView<'_>,
        now: Instant,
    ) -> Served {
        let decision = match self.serve.decide(page, request) {
            Step::Decided(decision) => decision,
            Step::Rank => self.serve.ranked(
                page,
                request,
                view.owners,
                &view.local,
                &view.requester,
                || view.column_mate,
            ),
        };
        if decision != Decision::Acquire || !defer_allowed(self.acquiring(page)) {
            return Served::Decided(decision);
        }
        let hints = self.successors.peek_for(page, &view.requester, now);
        match successor_deferral(hints, &view.local, &view.requester, view.owners) {
            Some(to) => {
                self.successors
                    .charge(page, view.requester, &to, MAX_DEFERRALS, now);
                Served::Defer(to)
            }
            None => Served::Decided(decision),
        }
    }
}

#[cfg(test)]
mod tests;
