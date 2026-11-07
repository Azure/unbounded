//! The run entry point, effect coverage names and core construction.

use super::*;

/// The coverage name of an effect: the variant path, exhaustively, so a new core variant fails
/// to compile here until it has a name (see [`coverage`]).
pub fn effect_name(effect: &Effect<ModelTypes>) -> &'static str {
    use acquisition::Io as Acq;
    use racer_core::read::window::{Effect as Win, Next};
    match effect {
        Effect::Acquisition { effect, .. } => match effect {
            Acq::Rank { .. } => "Acquisition::Rank",
            Acq::Hints { .. } => "Acquisition::Hints",
            Acq::Hedge { .. } => "Acquisition::Hedge",
            Acq::Refresh { .. } => "Acquisition::Refresh",
            Acq::Send { .. } => "Acquisition::Send",
            Acq::Validate { .. } => "Acquisition::Validate",
            Acq::Batch { .. } => "Acquisition::Batch",
            Acq::Sleep { .. } => "Acquisition::Sleep",
            Acq::Origin { .. } => "Acquisition::Origin",
            Acq::Publish { .. } => "Acquisition::Publish",
        },
        Effect::Window { effect, .. } => match effect {
            Win::Probe { .. } => "Window::Probe",
            Win::Fetch { .. } => "Window::Fetch",
            Win::Select { .. } => "Window::Select",
            Win::Hold { .. } => "Window::Hold",
            Win::SpillFill { .. } => "Window::SpillFill",
            Win::Release(_) => "Window::Release",
            Win::Discard(_) => "Window::Discard",
        },
        Effect::Deliver { next, .. } => match next {
            Next::End => "Deliver::End",
            Next::Wait => "Deliver::Wait",
            Next::Page { .. } => "Deliver::Page",
            Next::Fail(_) => "Deliver::Fail",
        },
        Effect::Worker { msg, .. } => match msg {
            WorkerMsg::Fetch { .. } => "Worker::Fetch",
            WorkerMsg::Fetched { .. } => "Worker::Fetched",
        },
        Effect::Completed { result: Ok(()), .. } => "Completed::Ok",
        Effect::Completed { result: Err(_), .. } => "Completed::Err",
        Effect::Metadata { .. } => "Metadata",
        Effect::Silence { .. } => "Silence",
        Effect::MetadataReady { result: Ok(_), .. } => "MetadataReady::Ok",
        Effect::MetadataReady { result: Err(_), .. } => "MetadataReady::Err",
    }
}

/// The coverage name of a serve-side outcome.
pub(super) fn served_name(served: &Served) -> &'static str {
    match served {
        Served::Defer(_) => "Served::Defer",
        Served::Decided(Decision::Reject(_)) => "Served::Reject",
        Served::Decided(Decision::Miss) => "Served::Miss",
        Served::Decided(Decision::Shed) => "Served::Shed",
        Served::Decided(Decision::CopyOnly { .. }) => "Served::CopyOnly",
        Served::Decided(Decision::Acquire) => "Served::Acquire",
    }
}

/// One real [`WorkerCore`] for `(node, worker)`.
pub(super) fn core(
    node: u32,
    worker: u32,
    workers: u32,
    versions: &Rc<RefCell<Versions>>,
    placement: &Rc<Placement<Phantom>>,
    base: Instant,
    spec: &ModelSpec,
) -> WorkerCore<ModelTypes> {
    // Every closure reads the version this node adopted when the core calls it.
    let (v, p) = (versions.clone(), placement.clone());
    let plan = Box::new(move |page: &Page, budget: &AcquisitionBudgetSpec| {
        let config = FillConfig {
            operation: OperationKind::Page,
            hedge: false,
            walk_only: false,
            scope: budget.deadline,
            features: Features::PRODUCTION,
        };
        let versions = v.borrow();
        let version = versions.adopted(node);
        (
            config,
            Box::new((version.number, owners(&p, version, *page))),
        )
    });
    let (v, p) = (versions.clone(), placement.clone());
    let mesh = Box::new(move |page: &Page, version: u64| {
        let versions = v.borrow();
        Some(Mesh {
            owners: owners(&p, versions.get(version)?, *page),
            start: helper_start(&node.to_be_bytes()),
            reader: node,
        })
    });
    let v = versions.clone();
    let helper_of = Box::new(move |owner: u32| {
        let versions = v.borrow();
        let version = versions.adopted(node);
        let pool = version.members.pools().first()?;
        let helper = pool.helper(version.position(node)?, version.position(owner)?)?;
        Some(version.node(helper))
    });
    WorkerCore::new(
        Routing {
            me: worker,
            owner_of: Box::new(move |page| owner_worker(*page, workers)),
            plan,
            mesh,
            helper_of,
        },
        FlightLimits {
            waiters_per_flight: spec.waiters_per_flight,
            ..flight_limits()
        },
        64,
        ServeDecision::new(1 << 20),
        SuccessorHints::new(HINT_CAPACITY, HINT_TTL),
        OwnerBudget::new(4, 0),
        base,
    )
}

/// Production `Relay` transfers per worker (`racer_core::admission::Limits::default`
/// `relay_transfers`).
pub(super) fn relay_transfers() -> usize {
    racer_core::admission::Limits::default()
        .relay_transfers
        .get()
}

/// Production crypto submissions per worker: `queue_entries` port slots plus as many admission
/// waiters (`src/security.rs` `CryptoClient::new`, `runtime/src/offload.rs`
/// `AdmissionQueue::enter`).
pub(super) fn crypto_queue() -> usize {
    racer_core::admission::Limits::default().crypto_submissions()
}

/// Production per-worker flight bounds (`racer_core::admission::Limits::default`): 64 flights
/// of 64 waiters each. A join past the entry cap is answered
/// `Overloaded`; past a flight's waiter cap, `FlightFull`.
pub(super) fn flight_limits() -> FlightLimits {
    let limits = racer_core::admission::Limits::default();
    FlightLimits {
        entries: limits.flights.get(),
        waiters_per_flight: limits.waiters_per_flight.get(),
        ..FlightLimits::default()
    }
}

/// Run a Model scenario over `choices`: the outcome and the JSON report.
pub fn run(scenario: &Scenario, choices: Choices) -> (Outcome, Value) {
    if let Some(spec) = scenario.model.as_deref()
        && let Some(ctl) = spec.control.as_ref().filter(|c| !c.combined)
    {
        return control::run_scenario(scenario, spec, ctl, &choices);
    }
    let Some(spec) = scenario.model.as_deref() else {
        let failure = Failure {
            class: Class::Safety,
            name: "fidelity".into(),
            at: 0,
            step: 0,
            message: "Model fidelity needs a ModelSpec".into(),
        };
        return (
            Outcome {
                trace: 0,
                steps: 0,
                end: 0,
                draws: choices.drawn(),
                quiescent: false,
                failure: Some(failure),
            },
            Value::Null,
        );
    };
    let mut model = Model::new(scenario, spec, choices.clone());
    // Draw a fault schedule only when the scenario asks for one, so fault-free scenarios keep
    // their choice streams.
    let faults = if scenario.fault_mix.max_faults == 0 {
        scenario.faults.clone()
    } else {
        scenario.fault_schedule(&choices)
    };
    model.schedule_faults(faults);
    model.schedule_storms();
    let mut ctl = spec.control.as_ref().map(|c| {
        let mut ctl = control::combined(c, scenario.nodes, choices.seed(), scenario.end);
        ctl.record = true;
        ctl
    });
    match ctl.as_mut() {
        Some(ctl) => combined::run(&mut model, ctl, scenario.end, scenario.max_steps),
        None => model.run(scenario.end, scenario.max_steps),
    }
    // A probe answered before its bound leaves its timeout queued; it would be a no-op.
    let quiescent =
        model.engine.pending() as u64 == model.timeouts_queued && model.probes.is_empty();
    model.finish();
    let mut report = model
        .report
        .json(scenario, spec, choices.seed(), model.now());
    if let Some(ctl) = ctl.as_mut() {
        combined::report(&mut report, &model, ctl, &scenario.name);
        let control = report["control_trace_hash"].as_str().unwrap_or("0");
        let control = u64::from_str_radix(control, 16).unwrap_or(0);
        model.trace.record(model.now(), 5, control, 0);
    }
    let steps = model.engine.processed();
    let mut failure = None;
    let hard = report["invariants"]["hard_failures"].as_u64().unwrap_or(0);
    if hard != 0 {
        failure = Some(Failure {
            class: Class::Safety,
            name: "hard-failures".into(),
            at: model.now(),
            step: steps,
            message: report["invariants"].to_string(),
        });
    }
    for (name, check) in &spec.checks {
        if failure.is_some() {
            break;
        }
        if let Err(message) = check(&report) {
            failure = Some(Failure {
                class: Class::Safety,
                name: name.clone(),
                at: model.now(),
                step: steps,
                message,
            });
        }
    }
    let mut trace = model.trace;
    trace.record(
        model.now(),
        3,
        steps,
        report["metrics"]["counters"].to_string().len() as u64,
    );
    trace.record_bytes(model.now(), 4, report["metrics"].to_string().as_bytes());
    (
        Outcome {
            trace: trace.value(),
            steps,
            end: model.now(),
            draws: choices.drawn(),
            quiescent,
            failure,
        },
        report,
    )
}

/// A trace code for `fault`.
pub(super) fn fault_code(fault: crate::world::Fault) -> u64 {
    use crate::world::Fault as F;
    match fault {
        F::Crash { node } => u64::from(node),
        F::Restart { node } => 1 << 32 | u64::from(node),
        F::Partition { a, b } => 2 << 32 | u64::from(a) << 16 | u64::from(b),
        F::Heal { a, b } => 3 << 32 | u64::from(a) << 16 | u64::from(b),
        F::SlowPeer { node, factor } => 4 << 32 | u64::from(factor) << 16 | u64::from(node),
        _ => 5 << 32,
    }
}

/// Polls of one ranking over `members` (production `topology/src/placement.rs` `rank_async`):
/// a first ranking scores `WORK_QUANTUM` (256) members per poll; a cached one takes one poll.
pub(super) fn ranking_polls(first: bool, members: u64) -> u64 {
    const WORK_QUANTUM: u64 = 256;
    if first {
        members.div_ceil(WORK_QUANTUM).max(1)
    } else {
        1
    }
}

/// The response kind a relay signs for `r`.
pub(super) fn response_kind(r: Response) -> ResponseKind {
    match r {
        Response::Overloaded => ResponseKind::Overloaded,
        Response::Unavailable => ResponseKind::Unavailable,
        Response::NotFound => ResponseKind::NotFound,
        Response::VersionUnavailable => ResponseKind::VersionUnavailable,
        Response::OriginRejected => ResponseKind::OriginRejected,
        Response::OriginForbidden => ResponseKind::OriginForbidden,
        Response::StaleMembership => ResponseKind::StaleMembership,
    }
}
