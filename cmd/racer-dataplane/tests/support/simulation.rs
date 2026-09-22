// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Deterministic machines: explicit managed clock; legacy fixtures retain theirs.
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    io,
    net::SocketAddr,
    rc::Rc,
    sync::{Arc, Mutex},
    time::{Duration, Instant, SystemTime},
};

#[path = "simulation/history.rs"]
pub(crate) mod history;
#[path = "simulation/replay.rs"]
pub(crate) mod journal;

thread_local! { static ACTIVE: RefCell<Option<World>> = const { RefCell::new(None) }; }
pub(crate) fn current() -> Option<World> {
    ACTIVE.with(|a| a.borrow().clone())
}
#[derive(Clone)]
pub(crate) struct World(Rc<RefCell<State>>);
struct TaskDrain(World);
impl Drop for TaskDrain {
    fn drop(&mut self) {
        self.0.0.borrow_mut().running_tasks = false;
    }
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord)]
pub(crate) struct Process {
    pub node: Option<usize>,
    pub incarnation: u64,
}
struct Task {
    process: Process,
    callback: Box<dyn FnOnce()>,
}
struct State {
    mutant: Option<history::Mutant>,
    replay: BTreeMap<Process, crate::http_auth::ReplayLedger>,
    seed: u64,
    entropy_seed: u64,
    entropy: BTreeMap<Process, u64>,
    timing: u64,
    tick: u64,
    base: Instant,
    managed: bool,
    process: Process,
    incarnations: BTreeMap<Option<usize>, u64>,
    next: i32,
    objects: BTreeMap<i32, Object>,
    listeners: BTreeMap<SocketAddr, i32>,
    trace: blake3::Hasher,
    operations: [usize; 64],
    short: usize,
    link_delay: Option<u64>,
    fail: Option<(u8, i32)>,
    tasks: BTreeMap<(u64, u64), Task>,
    task_sequence: u64,
    running_tasks: bool,
    sequence: u64,
    events: VecDeque<Event>,
    event_count: u64,
    trace_limit: usize,
    prefix: Vec<usize>,
    replay_prefix: Vec<Choice>,
    journal: Option<journal::Journal>,
    choices: VecDeque<Choice>,
    choice_count: u64,
    deterministic: blake3::Hasher,
    choice_limit: u64,
    steps: u64,
    step_limit: u64,
    gates: Vec<Gate>,
    sockets: BTreeMap<i32, SocketTag>,
    producers: BTreeMap<(usize, [u8; 32]), u64>,
    requests: BTreeMap<[u8; 32], String>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub(crate) struct Choice {
    pub index: u64,
    pub enabled: usize,
    pub selected: usize,
    pub fingerprint: [u8; 32],
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Phase {
    ConnectAdmission,
    Registration,
    Connect,
    Request,
    Headers,
    PartialBody,
    RdmaRequest,
}
#[derive(Clone, Debug)]
pub(crate) struct Event {
    pub tick: u64,
    pub node: Option<usize>,
    pub kind: &'static str,
    pub target: String,
    pub detail: String,
    pub flight: Option<u64>,
    pub depends_on: Option<u64>,
    pub key: Option<[u8; 32]>,
}
#[derive(Clone)]
struct SocketTag {
    node: Option<usize>,
    endpoint: SocketAddr,
    target: String,
    phase: Phase,
}
pub(crate) struct Gate {
    pub node: usize,
    pub endpoint: SocketAddr,
    pub target: String,
    pub phase: Phase,
    /// None stalls until released; Some emits one errno unless persistent.
    pub errno: Option<i32>,
    repeat: bool,
    hits: usize,
    released: bool,
}
impl Gate {
    pub fn new(
        node: usize,
        endpoint: SocketAddr,
        target: &str,
        phase: Phase,
        errno: Option<i32>,
    ) -> Self {
        Self {
            node,
            endpoint,
            target: target.into(),
            phase,
            errno,
            repeat: false,
            hits: 0,
            released: false,
        }
    }
    pub fn persistent(mut self) -> Self {
        self.repeat = true;
        self
    }
}
// Mutable mapped generations expose overwrites of queued file pages.
type Sector = Arc<Mutex<[u8; 512]>>;
#[derive(Clone)]
enum SegmentBytes {
    Owned(Arc<[u8]>),
    Page(Sector),
}
struct Segment {
    bytes: SegmentBytes,
    range: std::ops::Range<usize>,
}
#[derive(Default)]
struct ByteQueue {
    segments: VecDeque<Segment>,
    len: usize,
}
impl ByteQueue {
    fn len(&self) -> usize {
        self.len
    }
    fn is_empty(&self) -> bool {
        self.len == 0
    }
    fn append(&mut self, mut other: Self) {
        self.len += other.len;
        self.segments.append(&mut other.segments);
    }
    fn extend(&mut self, bytes: &[u8]) {
        if !bytes.is_empty() {
            self.segments.push_back(Segment {
                bytes: SegmentBytes::Owned(Arc::from(bytes)),
                range: 0..bytes.len(),
            });
            self.len += bytes.len();
        }
    }
    fn take(&mut self, len: usize) -> Self {
        let mut out = Self::default();
        while out.len < len && !self.is_empty() {
            let front = self.segments.front_mut().unwrap();
            let n = (len - out.len).min(front.range.len());
            out.segments.push_back(Segment {
                bytes: front.bytes.clone(),
                range: front.range.start..front.range.start + n,
            });
            front.range.start += n;
            self.len -= n;
            out.len += n;
            if front.range.is_empty() {
                self.segments.pop_front();
            }
        }
        out
    }
    fn read(&mut self, out: &mut [u8]) {
        let mut offset = 0;
        while offset < out.len() && !self.is_empty() {
            let front = self.segments.front_mut().unwrap();
            let n = (out.len() - offset).min(front.range.len());
            let range = front.range.start..front.range.start + n;
            match &front.bytes {
                SegmentBytes::Owned(bytes) => {
                    out[offset..offset + n].copy_from_slice(&bytes[range])
                }
                SegmentBytes::Page(page) => {
                    out[offset..offset + n].copy_from_slice(&page.lock().unwrap()[range])
                }
            }
            front.range.start += n;
            self.len -= n;
            offset += n;
            if front.range.is_empty() {
                self.segments.pop_front();
            }
        }
    }
}
enum Object {
    Socket {
        peer: Option<i32>,
        bytes: ByteQueue,
        closed: bool,
    },
    Listener {
        address: SocketAddr,
        queue: VecDeque<Handle>,
    },
    Disk(Disk),
    Pipe(Rc<RefCell<ByteQueue>>),
}
pub(crate) struct Scope {
    previous: Option<World>,
}
impl Drop for Scope {
    fn drop(&mut self) {
        ACTIVE.with(|a| *a.borrow_mut() = self.previous.take());
    }
}
pub(crate) struct ProcessScope {
    world: World,
    previous: Process,
}
impl Drop for ProcessScope {
    fn drop(&mut self) {
        self.world.0.borrow_mut().process = self.previous;
    }
}
fn draw(seed: &mut u64) -> u64 {
    *seed = seed.wrapping_add(0x9e3779b97f4a7c15);
    let mut z = *seed;
    z = (z ^ (z >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94d049bb133111eb);
    z ^ (z >> 31)
}
impl World {
    pub fn new(seed: u64) -> Self {
        Self(Rc::new(RefCell::new(State {
            mutant: None,
            replay: BTreeMap::new(),
            seed,
            entropy_seed: seed,
            entropy: BTreeMap::new(),
            timing: seed ^ 0x54494d494e47,
            tick: 0,
            base: Instant::now(),
            managed: false,
            process: Process::default(),
            incarnations: BTreeMap::new(),
            next: 1,
            objects: BTreeMap::new(),
            listeners: BTreeMap::new(),
            trace: blake3::Hasher::new(),
            operations: [0; 64],
            short: 16384,
            link_delay: None,
            fail: None,
            tasks: BTreeMap::new(),
            task_sequence: 0,
            running_tasks: false,
            sequence: 0,
            events: VecDeque::new(),
            event_count: 0,
            trace_limit: usize::MAX,
            prefix: vec![],
            replay_prefix: vec![],
            journal: None,
            choices: VecDeque::new(),
            choice_count: 0,
            deterministic: blake3::Hasher::new(),
            choice_limit: u64::MAX,
            steps: 0,
            step_limit: u64::MAX,
            gates: vec![],
            sockets: BTreeMap::new(),
            producers: BTreeMap::new(),
            requests: BTreeMap::new(),
        })))
    }
    pub fn enter(&self) -> Scope {
        Scope {
            previous: ACTIVE.with(|a| a.borrow_mut().replace(self.clone())),
        }
    }
    pub fn process(&self) -> Process {
        self.0.borrow().process
    }
    pub fn scoped_process(&self, process: Process) -> ProcessScope {
        let previous = std::mem::replace(&mut self.0.borrow_mut().process, process);
        ProcessScope {
            world: self.clone(),
            previous,
        }
    }
    pub fn scoped_node(&self, node: Option<usize>) -> ProcessScope {
        let incarnation = self
            .0
            .borrow()
            .incarnations
            .get(&node)
            .copied()
            .unwrap_or(0);
        self.scoped_process(Process { node, incarnation })
    }
    pub fn node(&self, node: Option<usize>) {
        let mut s = self.0.borrow_mut();
        s.process = Process {
            node,
            incarnation: s.incarnations.get(&node).copied().unwrap_or(0),
        };
    }
    /// Quiesce/crash, restart, then construct the replacement under scoped_node.
    pub fn restart_node(&self, node: Option<usize>) -> Process {
        let mut s = self.0.borrow_mut();
        let incarnation = s.incarnations.entry(node).or_default();
        *incarnation = incarnation.checked_add(1).expect("incarnation overflow");
        let process = Process {
            node,
            incarnation: *incarnation,
        };
        s.replay.retain(|p, _| p.node != node);
        s.entropy.retain(|p, _| p.node != node);
        s.producers.retain(|(n, _), _| Some(*n) != node);
        let keys: Vec<_> = s
            .tasks
            .iter()
            .filter(|(_, t)| t.process.node == node)
            .map(|(k, _)| *k)
            .collect();
        let retired: Vec<_> = keys
            .into_iter()
            .filter_map(|k| s.tasks.remove(&k))
            .collect();
        if s.process.node == node {
            s.process = process;
        }
        drop(s);
        drop(retired); // callback destruction may release World handles
        process
    }
    pub fn is_current(&self, process: Process) -> bool {
        self.0
            .borrow()
            .incarnations
            .get(&process.node)
            .copied()
            .unwrap_or(0)
            == process.incarnation
    }
    pub fn accept_nonce(&self, nonce: [u8; 32]) -> io::Result<()> {
        if !self.is_current(self.process()) {
            return Err(io::Error::other("retired simulated process"));
        }
        let now = self.now();
        let mut s = self.0.borrow_mut();
        let process = s.process;
        let key = Self::ledger_key(s.entropy_seed, process);
        s.replay
            .entry(process)
            .or_insert_with(|| {
                crate::http_auth::ReplayLedger::simulated(Default::default(), key).unwrap()
            })
            .accept(nonce, now)
    }
    pub fn configure_replay(&self, config: crate::http_auth::replay::Config) {
        let mut s = self.0.borrow_mut();
        let process = s.process;
        assert!(!s.replay.contains_key(&process));
        let key = Self::ledger_key(s.entropy_seed, process);
        s.replay.insert(
            process,
            crate::http_auth::ReplayLedger::simulated(config, key).unwrap(),
        );
    }
    fn ledger_key(seed: u64, process: Process) -> [u8; 32] {
        let mut hash = blake3::Hasher::new();
        hash.update(b"racer/simulation/replay-ledger/v1");
        hash.update(&seed.to_le_bytes());
        hash.update(&[u8::from(process.node.is_some())]);
        hash.update(&(process.node.unwrap_or(0) as u64).to_le_bytes());
        hash.update(&process.incarnation.to_le_bytes());
        *hash.finalize().as_bytes()
    }
    pub fn seeds(&self, seeds: journal::Seeds) {
        let mut s = self.0.borrow_mut();
        assert_eq!(s.choice_count, 0);
        assert!(s.entropy.is_empty());
        s.seed = seeds.scheduler;
        s.timing = seeds.timing;
        s.entropy_seed = seeds.entropy;
    }
    pub fn journal(&self, journal: journal::Journal) {
        let mut s = self.0.borrow_mut();
        assert!(s.managed && s.choice_count == 0 && s.journal.is_none());
        assert!(s.prefix.is_empty() && s.replay_prefix.is_empty());
        s.journal = Some(journal);
    }
    pub fn finish_journal(&self, outcome: serde_json::Value) {
        let mut s = self.0.borrow_mut();
        let terminal = serde_json::json!({
            "outcome": outcome, "choices": s.choice_count,
            "digest": s.trace.finalize().to_hex().to_string(),
            "singletons": s.deterministic.finalize().to_hex().to_string(),
        });
        s.journal
            .take()
            .expect("journal not installed")
            .finish(terminal);
    }
    pub fn enable_scheduler(&self) {
        let mut s = self.0.borrow_mut();
        s.managed = true;
        if s.trace_limit == usize::MAX {
            s.trace_limit = 65536;
        }
        if s.step_limit == u64::MAX {
            s.step_limit = 10_000_000;
        }
        if s.choice_limit == u64::MAX {
            s.choice_limit = 100_000_000;
        }
        while s.events.len() > s.trace_limit {
            s.events.pop_front();
        }
    }
    pub fn managed(&self) -> bool {
        self.0.borrow().managed
    }
    /// Call once per global turn, even when workers remain runnable.
    pub fn service_tick(&self) {
        self.watchdog();
        self.advance(Duration::from_millis(1));
        self.run_tasks();
    }
    pub fn limits(&self, steps: u64, choices: u64, trace: usize) {
        let mut s = self.0.borrow_mut();
        s.step_limit = steps;
        s.choice_limit = choices;
        s.trace_limit = trace;
        while s.events.len() > trace {
            s.events.pop_front();
        }
        while s.choices.len() > trace {
            s.choices.pop_front();
        }
    }
    pub fn watchdog(&self) {
        let mut s = self.0.borrow_mut();
        assert!(
            s.steps < s.step_limit,
            "simulation watchdog: tick={} steps={} choices={}",
            s.tick,
            s.steps,
            s.choice_count
        );
        s.steps += 1;
    }
    pub fn script(&self, prefix: impl Into<Vec<usize>>) {
        let mut s = self.0.borrow_mut();
        assert_eq!(s.choice_count, 0, "install replay before choices");
        s.prefix = prefix.into();
        assert!(
            !s.managed || s.prefix.len() <= s.trace_limit,
            "script exceeds retained trace limit"
        );
        s.replay_prefix.clear();
    }
    pub fn assert_replay_consumed(&self) {
        let s = self.0.borrow();
        assert!(
            s.choice_count >= s.replay_prefix.len().max(s.prefix.len()) as u64,
            "execution ended before replay prefix was consumed"
        );
    }
    /// Strict bounded prefix: checks ordered enabled identities as well as count.
    pub fn replay(&self, prefix: Vec<Choice>) {
        let mut s = self.0.borrow_mut();
        assert!(
            s.managed && s.choice_count == 0,
            "install replay before choices"
        );
        assert!(
            prefix.len() <= s.trace_limit,
            "replay exceeds retained trace limit"
        );
        for (index, choice) in prefix.iter().enumerate() {
            assert_eq!(
                choice.index, index as u64,
                "replay must begin at choice zero"
            );
        }
        s.prefix.clear();
        s.replay_prefix = prefix;
    }
    /// Keys must be unique and in stable order. Domain distinguishes event types.
    pub fn choose_enabled(&self, domain: &str, keys: &[u64]) -> usize {
        assert_eq!(
            keys.iter().collect::<BTreeSet<_>>().len(),
            keys.len(),
            "duplicate event identity"
        );
        let mut hash = blake3::Hasher::new();
        hash.update(&(domain.len() as u64).to_le_bytes());
        hash.update(domain.as_bytes());
        let process = self.process();
        hash.update(&[u8::from(process.node.is_some())]);
        hash.update(&(process.node.unwrap_or(0) as u64).to_le_bytes());
        hash.update(&process.incarnation.to_le_bytes());
        for key in keys {
            hash.update(&key.to_le_bytes());
        }
        self.choose_fingerprint(keys.len(), *hash.finalize().as_bytes())
    }
    pub fn choices(&self) -> Vec<Choice> {
        self.0.borrow().choices.iter().copied().collect()
    }
    pub fn choice_count(&self) -> u64 {
        self.0.borrow().choice_count
    }
    pub fn event(&self, kind: &'static str, target: &str, detail: impl Into<String>) {
        self.record_event(Event {
            tick: self.tick(),
            node: self.process().node,
            kind,
            target: target.into(),
            detail: detail.into(),
            flight: None,
            depends_on: None,
            key: None,
        });
    }
    pub fn observation(&self, transition: history::Transition) {
        let mut s = self.0.borrow_mut();
        let value = serde_json::json!({"tick": s.tick, "node": s.process.node,
            "incarnation": s.process.incarnation, "transition": transition});
        s.trace
            .update(serde_json::to_string(&value).unwrap().as_bytes());
        if let Some(journal) = &mut s.journal {
            journal.observe("history", value);
        }
    }
    pub fn mutant(&self, mutant: Option<history::Mutant>) {
        self.0.borrow_mut().mutant = mutant;
    }
    pub fn activate_mutant(&self, mutant: history::Mutant) -> bool {
        if self.0.borrow().mutant != Some(mutant) {
            return false;
        }
        self.observation(history::Transition::MutantActivated { mutant });
        true
    }
    fn record_event(&self, event: Event) {
        let mut s = self.0.borrow_mut();
        s.trace.update(format!("{event:?}").as_bytes());
        s.event_count += 1;
        if s.trace_limit != 0 {
            s.events.push_back(event);
        }
        while s.events.len() > s.trace_limit {
            s.events.pop_front();
        }
    }
    pub fn events(&self) -> Vec<Event> {
        self.0.borrow().events.iter().cloned().collect()
    }
    pub fn events_since(&self, cursor: &mut u64) -> io::Result<Vec<Event>> {
        let s = self.0.borrow();
        let base = s.event_count - s.events.len() as u64;
        if *cursor < base || *cursor > s.event_count {
            return Err(io::Error::other("trace cursor outside retained history"));
        }
        let events = s
            .events
            .iter()
            .skip((*cursor - base) as usize)
            .cloned()
            .collect();
        *cursor = s.event_count;
        Ok(events)
    }
    pub fn gate(&self, gate: Gate) -> usize {
        let mut s = self.0.borrow_mut();
        s.gates.push(gate);
        s.gates.len() - 1
    }
    pub fn hits(&self, gate: usize) -> usize {
        self.0.borrow().gates[gate].hits
    }
    pub fn release(&self, gate: usize) {
        assert!(self.hits(gate) > 0, "delivery gate never intercepted IO");
        self.0.borrow_mut().gates[gate].released = true;
        self.event("gate-release", "", format!("gate={gate}"));
    }
    // Outer None: pass; Some(None): held; Some(Some(errno)): fail once.
    pub fn intercept(
        &self,
        node: Option<usize>,
        endpoint: SocketAddr,
        target: &str,
        phase: Phase,
    ) -> Option<Option<i32>> {
        let mut s = self.0.borrow_mut();
        let (id, gate) = s.gates.iter_mut().enumerate().find(|(_, g)| {
            !g.released
                && Some(g.node) == node
                && g.endpoint == endpoint
                && g.target == target
                && g.phase == phase
        })?;
        gate.hits += 1;
        let first = gate.hits == 1;
        let errno = gate.errno;
        if errno.is_some() && !gate.repeat {
            gate.released = true;
        }
        drop(s);
        if first {
            self.event(
                "gate-hit",
                target,
                format!("gate={id} from={node:?} to={endpoint} phase={phase:?} errno={errno:?}"),
            );
        }
        Some(errno)
    }
    pub fn tag_socket(&self, fd: i32, endpoint: SocketAddr, target: String) {
        let mut s = self.0.borrow_mut();
        let node = s.process.node;
        s.sockets.insert(
            fd,
            SocketTag {
                node,
                endpoint,
                target,
                phase: Phase::Connect,
            },
        );
    }
    pub fn socket_phase(&self, fd: i32, phase: Phase) {
        if let Some(t) = self.0.borrow_mut().sockets.get_mut(&fd) {
            t.phase = phase;
        }
    }
    pub fn copy_socket_tag(&self, from: i32, to: i32) {
        let tag = self.0.borrow().sockets.get(&from).cloned().unwrap();
        self.0.borrow_mut().sockets.insert(to, tag);
    }
    pub fn admission(&self, fd: i32, phase: Phase) -> bool {
        let s = self.0.borrow();
        let Some(t) = s.sockets.get(&fd) else {
            return false;
        };
        let (node, endpoint, target) = (t.node, t.endpoint, t.target.clone());
        drop(s);
        self.intercept(node, endpoint, &target, phase).is_some()
    }
    pub fn socket_timeout(&self, fd: i32) {
        let s = self.0.borrow();
        let t = s.sockets.get(&fd).expect("tagged HTTP exchange");
        let event = Event {
            tick: s.tick,
            node: t.node,
            kind: "http-timeout",
            target: t.target.clone(),
            detail: format!(
                "endpoint={} phase={:?} cause=Io(TimedOut)",
                t.endpoint, t.phase
            ),
            flight: None,
            depends_on: None,
            key: None,
        };
        drop(s);
        self.record_event(event);
    }
    pub fn request(&self, key: [u8; 32], target: &str) {
        self.0.borrow_mut().requests.insert(key, target.into());
    }
    pub fn request_target(&self, key: &[u8; 32]) -> Option<String> {
        self.0.borrow().requests.get(key).cloned()
    }
    pub fn flight(&self, id: u64, key: [u8; 32], target: &str, producer: bool) {
        let mut s = self.0.borrow_mut();
        let Some(node) = s.process.node else {
            return;
        };
        let dependency = if producer {
            s.producers.insert((node, key), id);
            None
        } else {
            s.producers.get(&(node, key)).copied()
        };
        drop(s);
        self.record_event(Event {
            tick: self.tick(),
            node: Some(node),
            kind: if producer {
                "flight-fill"
            } else {
                "flight-wait"
            },
            target: target.into(),
            detail: format!(
                "flight={id} key={} depends_on={dependency:?}",
                blake3::Hash::from(key).to_hex()
            ),
            flight: Some(id),
            depends_on: dependency,
            key: Some(key),
        });
    }
    pub fn now(&self) -> Instant {
        let s = self.0.borrow();
        s.base + Duration::from_millis(s.tick)
    }
    pub fn wall(&self) -> SystemTime {
        SystemTime::UNIX_EPOCH
            + Duration::from_secs(1_800_000_000)
            + Duration::from_millis(self.tick())
    }
    pub fn tick(&self) -> u64 {
        self.0.borrow().tick
    }
    pub fn advance(&self, d: Duration) {
        let millis = u64::try_from(d.as_millis()).expect("simulation duration overflow");
        let mut s = self.0.borrow_mut();
        s.tick = s
            .tick
            .checked_add(millis)
            .expect("simulation clock overflow");
    }
    pub fn schedule(&self, task: impl FnOnce() + 'static) {
        let due = self.delay();
        let mut s = self.0.borrow_mut();
        let id = s.task_sequence;
        s.task_sequence += 1;
        let process = s.process;
        s.tasks.insert(
            (due, id),
            Task {
                process,
                callback: Box::new(task),
            },
        );
    }
    pub fn next_task_tick(&self) -> Option<u64> {
        self.0.borrow().tasks.first_key_value().map(|(k, _)| k.0)
    }
    pub fn run_tasks(&self) {
        self.drain_tasks(None);
    }
    /// After closing sources, release their jobs without polling other drivers.
    pub fn finish_process_tasks(&self, process: Process) {
        self.drain_tasks(Some(process));
    }
    fn drain_tasks(&self, process: Option<Process>) {
        let mut s = self.0.borrow_mut();
        if s.running_tasks {
            return;
        }
        s.running_tasks = true;
        drop(s);
        let _guard = TaskDrain(self.clone());
        for _ in 0..65536 {
            let task = {
                let mut s = self.0.borrow_mut();
                let key = match process {
                    Some(process) => s
                        .tasks
                        .iter()
                        .find(|(_, t)| t.process == process)
                        .map(|(k, _)| *k),
                    None => s
                        .tasks
                        .first_key_value()
                        .filter(|(k, _)| k.0 <= s.tick)
                        .map(|(k, _)| *k),
                };
                let Some(key) = key else {
                    return;
                };
                s.tasks.remove(&key).unwrap()
            };
            if self.is_current(task.process) {
                let _world = self.enter();
                let _process = self.scoped_process(task.process);
                (task.callback)();
            }
        }
        panic!(
            "compute callback drain exceeded zero-time budget at tick {}",
            self.tick()
        );
    }
    /// Choose in stable enabled order; out-of-range prefix entries fail.
    pub fn choose(&self, n: usize) -> usize {
        self.choose_fingerprint(n, [0; 32])
    }
    fn choose_fingerprint(&self, n: usize, fingerprint: [u8; 32]) -> usize {
        assert!(n > 0, "empty enabled set");
        let mut s = self.0.borrow_mut();
        if s.managed && n == 1 {
            // A deterministic dispatch is an observation, not a scheduling branch.
            s.trace.update(b"singleton");
            s.trace.update(&fingerprint);
            s.deterministic.update(&fingerprint);
            return 0;
        }
        let fingerprint = if s.managed {
            let mut history = s.deterministic.clone();
            history.update(&fingerprint);
            *history.finalize().as_bytes()
        } else {
            fingerprint
        };
        let random = draw(&mut s.seed) as usize % n;
        if !s.managed {
            return random;
        }
        assert!(
            s.choice_count < s.choice_limit,
            "simulation choice limit at tick {}",
            s.tick
        );
        let index = s.choice_count;
        let selected = if let Some(journal) = s.journal.as_mut() {
            journal
                .choice(Choice {
                    index,
                    enabled: n,
                    selected: random,
                    fingerprint,
                })
                .selected
        } else if let Some(expected) = s.replay_prefix.get(index as usize) {
            assert_eq!(
                expected.enabled, n,
                "replay enabled count at choice {index}"
            );
            assert_eq!(
                expected.fingerprint, fingerprint,
                "replay enabled identities at choice {index}"
            );
            expected.selected
        } else {
            s.prefix.get(index as usize).copied().unwrap_or(random)
        };
        assert!(
            selected < n,
            "replay choice {index}: {selected} outside {n} enabled alternatives"
        );
        s.choice_count += 1;
        s.trace.update(&index.to_le_bytes());
        s.trace.update(&(n as u64).to_le_bytes());
        s.trace.update(&(selected as u64).to_le_bytes());
        s.trace.update(&fingerprint);
        if s.trace_limit != 0 {
            s.choices.push_back(Choice {
                index,
                enabled: n,
                selected,
                fingerprint,
            });
        }
        while s.choices.len() > s.trace_limit {
            s.choices.pop_front();
        }
        selected
    }
    pub fn random(&self, bytes: &mut [u8]) {
        if !self.managed() {
            for b in bytes {
                *b = self.choose(256) as u8;
            }
            return;
        }
        assert!(
            self.is_current(self.process()),
            "entropy requested by retired process"
        );
        let mut s = self.0.borrow_mut();
        let process = s.process;
        let seed = s.entropy_seed;
        let stream = s.entropy.entry(process).or_insert_with(|| {
            let mut h = blake3::Hasher::new();
            h.update(b"racer/simulation/process-entropy/v1");
            h.update(&seed.to_le_bytes());
            h.update(&[u8::from(process.node.is_some())]);
            h.update(&(process.node.unwrap_or(0) as u64).to_le_bytes());
            h.update(&process.incarnation.to_le_bytes());
            u64::from_le_bytes(h.finalize().as_bytes()[..8].try_into().unwrap())
        });
        for chunk in bytes.chunks_mut(8) {
            let value = draw(stream).to_le_bytes();
            chunk.copy_from_slice(&value[..chunk.len()]);
        }
        if let Some(journal) = s.journal.as_mut() {
            journal.observe(
                "entropy",
                serde_json::json!({"node": process.node,
                "incarnation": process.incarnation, "length": bytes.len(),
                "digest": blake3::hash(bytes).to_hex().to_string()}),
            );
        }
    }
    pub fn delay(&self) -> u64 {
        if !self.managed() {
            return self.tick() + 1 + self.choose(3) as u64;
        }
        let mut s = self.0.borrow_mut();
        let due = s.tick + 1 + draw(&mut s.timing) % 3;
        if let Some(journal) = s.journal.as_mut() {
            journal.observe("delay", serde_json::json!(due));
        }
        due
    }
    pub fn record(&self, op: u8, fd: i32, res: i32) {
        let mut s = self.0.borrow_mut();
        s.operations[op as usize] += 1;
        let tick = s.tick;
        s.trace.update(&tick.to_le_bytes());
        s.trace.update(&[op]);
        s.trace.update(&fd.to_le_bytes());
        s.trace.update(&res.to_le_bytes());
    }
    pub fn digest(&self) -> [u8; 32] {
        *self.0.borrow().trace.clone().finalize().as_bytes()
    }
    pub fn counts(&self) -> [usize; 64] {
        self.0.borrow().operations
    }
    pub fn short_transfers(&self, max: usize) {
        assert!(max > 0);
        self.0.borrow_mut().short = max;
    }
    pub fn link_profile(&self, bytes: usize, delay_ms: Option<u64>) {
        assert!(bytes > 0 && delay_ms.is_none_or(|d| d > 0));
        let mut state = self.0.borrow_mut();
        state.short = bytes;
        state.link_delay = delay_ms;
    }
    fn io_delay(&self, opcode: u8) -> u64 {
        if self.managed() && matches!(opcode, 26 | 27 | 30 | 47) {
            if let Some(delay) = self.0.borrow().link_delay {
                return self.tick() + delay;
            }
        }
        self.delay()
    }
    pub fn fail_next(&self, op: u8) {
        self.fail_next_errno(op, libc::EIO);
    }
    pub fn fail_next_errno(&self, op: u8, errno: i32) {
        assert!(errno > 0);
        self.0.borrow_mut().fail = Some((op, errno));
    }
    pub fn fault_fired(&self) -> bool {
        self.0.borrow().fail.is_none()
    }
    pub fn sequence(&self) -> u64 {
        let mut s = self.0.borrow_mut();
        s.sequence += 1;
        s.sequence
    }
    pub fn trace_bytes(&self, bytes: &[u8]) {
        self.0.borrow_mut().trace.update(bytes);
    }
    pub fn assert_clean(&self) {
        let s = self.0.borrow();
        assert!(
            s.objects.is_empty(),
            "{} leaked descriptors",
            s.objects.len()
        );
        assert!(s.tasks.is_empty(), "pending compute jobs");
    }
    fn allocate(&self, object: Object) -> Handle {
        let mut s = self.0.borrow_mut();
        let id = s.next;
        s.next = s.next.checked_add(1).expect("descriptor overflow");
        s.objects.insert(id, object);
        Handle {
            id,
            world: self.clone(),
        }
    }
    pub fn socket(&self) -> Handle {
        self.allocate(Object::Socket {
            peer: None,
            bytes: ByteQueue::default(),
            closed: false,
        })
    }
    pub fn listen(&self, address: SocketAddr) -> io::Result<Handle> {
        if self.0.borrow().listeners.contains_key(&address) {
            return Err(io::ErrorKind::AddrInUse.into());
        }
        let h = self.allocate(Object::Listener {
            address,
            queue: VecDeque::new(),
        });
        self.0.borrow_mut().listeners.insert(address, h.id);
        Ok(h)
    }
    pub fn disk(&self, disk: Disk) -> Handle {
        self.allocate(Object::Disk(disk))
    }
    pub fn pipe(&self) -> (Handle, Handle) {
        let bytes = Rc::new(RefCell::new(ByteQueue::default()));
        (
            self.allocate(Object::Pipe(bytes.clone())),
            self.allocate(Object::Pipe(bytes)),
        )
    }
    fn close(&self, id: i32) {
        self.0.borrow_mut().sockets.remove(&id);
        let removed = self.0.borrow_mut().objects.remove(&id);
        if let Some(Object::Listener { address, queue }) = removed {
            self.0.borrow_mut().listeners.remove(&address);
            drop(queue);
        }
    }
    fn operation_ready(&self, op: u8, fd: i32) -> bool {
        let s = self.0.borrow();
        if let Some(t) = s.sockets.get(&fd) {
            let relevant = matches!(
                (op, t.phase),
                (16, Phase::Connect)
                    | (26 | 47, Phase::Request)
                    | (27, Phase::Headers | Phase::PartialBody)
            );
            if relevant
                && let Some(gate) = s.gates.iter().find(|g| {
                    !g.released
                        && Some(g.node) == t.node
                        && g.endpoint == t.endpoint
                        && g.target == t.target
                        && g.phase == t.phase
                })
            {
                return gate.errno.is_some() || gate.hits == 0;
            }
        }
        if s.fail.is_some_and(|(opcode, _)| opcode == op) {
            return true;
        }
        match (op, s.objects.get(&fd)) {
            (13, Some(Object::Listener { queue, .. })) => !queue.is_empty(),
            (
                27,
                Some(Object::Socket {
                    peer,
                    bytes,
                    closed,
                }),
            ) => {
                !bytes.is_empty()
                    || *closed
                    || !peer.is_some_and(|p| {
                        matches!(
                            s.objects.get(&p),
                            Some(Object::Socket { closed: false, .. })
                        )
                    })
            }
            _ => true,
        }
    }
    // Caller owns all SQE memory; accesses are synchronous and never retained.
    pub unsafe fn operation(
        &self,
        op: u8,
        fd: i32,
        addr: u64,
        len: u32,
        off: u64,
        _flags: u32,
        input: i32,
    ) -> Option<(i32, Option<Handle>)> {
        let tag = self
            .0
            .borrow()
            .sockets
            .get(&fd)
            .map(|t| (t.node, t.endpoint, t.target.clone(), t.phase));
        if let Some((node, endpoint, target, phase)) = tag {
            if matches!(
                (op, phase),
                (16, Phase::Connect)
                    | (26 | 47, Phase::Request)
                    | (27, Phase::Headers | Phase::PartialBody)
            ) && let Some(result) = self.intercept(node, endpoint, &target, phase)
            {
                return result.map(|errno| (-errno, None));
            }
        }
        let fault = self.0.borrow().fail.filter(|(opcode, _)| *opcode == op);
        if let Some((_, errno)) = fault {
            self.0.borrow_mut().fail = None;
            return Some((-errno, None));
        }
        if op == 16 {
            let address = unsafe {
                let a = &*(addr as *const libc::sockaddr_in);
                assert_eq!(a.sin_family as i32, libc::AF_INET);
                SocketAddr::from((a.sin_addr.s_addr.to_ne_bytes(), u16::from_be(a.sin_port)))
            };
            let listener = self.0.borrow().listeners.get(&address).copied();
            let Some(listener) = listener else {
                return Some((-libc::ECONNREFUSED, None));
            };
            let remote = self.socket();
            let remote_id = remote.id;
            let mut s = self.0.borrow_mut();
            if let Some(Object::Socket { peer, .. }) = s.objects.get_mut(&fd) {
                *peer = Some(remote_id);
            }
            if let Some(Object::Socket { peer, .. }) = s.objects.get_mut(&remote_id) {
                *peer = Some(fd);
            }
            if let Some(Object::Listener { queue, .. }) = s.objects.get_mut(&listener) {
                queue.push_back(remote);
            }
            return Some((0, None));
        }
        let mut s = self.0.borrow_mut();
        let short = s.short;
        match op {
            13 => match s.objects.get_mut(&fd) {
                Some(Object::Listener { queue, .. }) => queue.pop_front().map(|h| (h.id, Some(h))),
                _ => Some((-libc::EBADF, None)),
            },
            26 | 47 => {
                let peer = match s.objects.get(&fd) {
                    Some(Object::Socket {
                        peer,
                        closed: false,
                        ..
                    }) => *peer,
                    _ => None,
                };
                let Some(Object::Socket {
                    bytes,
                    closed: false,
                    ..
                }) = peer.and_then(|p| s.objects.get_mut(&p))
                else {
                    return Some((-libc::EPIPE, None));
                };
                let n = (len as usize).min(short);
                let input = unsafe { std::slice::from_raw_parts(addr as *const u8, n) };
                bytes.extend(input);
                s.trace.update(input);
                Some((n as i32, None))
            }
            27 => {
                let peer_closed = match s.objects.get(&fd) {
                    Some(Object::Socket { peer: Some(p), .. }) => {
                        !matches!(s.objects.get(p), Some(Object::Socket { closed: false, .. }))
                    }
                    _ => true,
                };
                let Some(Object::Socket { bytes, closed, .. }) = s.objects.get_mut(&fd) else {
                    return Some((-libc::EBADF, None));
                };
                if bytes.is_empty() {
                    return if peer_closed || *closed {
                        Some((0, None))
                    } else {
                        None
                    };
                }
                let n = (len as usize).min(short).min(bytes.len());
                let out = unsafe { std::slice::from_raw_parts_mut(addr as *mut u8, n) };
                bytes.read(out);
                Some((n as i32, None))
            }
            30 => {
                let mut n = (len as usize).min(short).min(65536);
                // Validate output before consuming source; page references survive
                // the pipe and socket stages until received, even after punching.
                let destination = match s.objects.get(&fd) {
                    Some(Object::Pipe(pipe)) => {
                        n = n.min(65536 - pipe.borrow().len());
                        if n == 0 {
                            return Some((-libc::EAGAIN, None));
                        }
                        fd
                    }
                    Some(Object::Socket {
                        peer: Some(peer),
                        closed: false,
                        ..
                    }) => {
                        if !matches!(
                            s.objects.get(peer),
                            Some(Object::Socket { closed: false, .. })
                        ) {
                            return Some((-libc::EPIPE, None));
                        }
                        *peer
                    }
                    _ => return Some((-libc::EBADF, None)),
                };
                let bytes = match s.objects.get(&input) {
                    Some(Object::Disk(d)) => match d.pages(addr, n) {
                        Ok(bytes) => bytes,
                        Err(_) => return Some((-libc::EIO, None)),
                    },
                    Some(Object::Pipe(pipe)) => {
                        if pipe.borrow().is_empty() {
                            return Some((-libc::EAGAIN, None));
                        }
                        pipe.borrow_mut().take(n)
                    }
                    _ => return Some((-libc::EBADF, None)),
                };
                let n = bytes.len();
                match s.objects.get_mut(&destination) {
                    Some(Object::Pipe(pipe)) => pipe.borrow_mut().append(bytes),
                    Some(Object::Socket { bytes: queue, .. }) => queue.append(bytes),
                    _ => unreachable!(),
                }
                Some((n as i32, None))
            }
            3 | 4 | 5 | 17 | 22 | 23 => {
                let Some(Object::Disk(d)) = s.objects.get(&fd) else {
                    return Some((-libc::EBADF, None));
                };
                let result = match op {
                    3 => d.sync_data().map(|_| 0),
                    17 => {
                        d.punch(off, addr);
                        Ok(0)
                    }
                    4 | 22 => d
                        .read_exact_at(
                            unsafe {
                                std::slice::from_raw_parts_mut(addr as *mut u8, len as usize)
                            },
                            off,
                        )
                        .map(|_| len as i32),
                    _ => d
                        .write_all_at(
                            unsafe { std::slice::from_raw_parts(addr as *const u8, len as usize) },
                            off,
                        )
                        .map(|_| len as i32),
                };
                Some((
                    d.completion_fault(op)
                        .map_or_else(|| result.unwrap_or(-libc::EIO), |errno| -errno),
                    None,
                ))
            }
            0 => Some((0, None)),
            _ => panic!("unimplemented simulated opcode {op}"),
        }
    }
}
pub(crate) struct Handle {
    pub id: i32,
    world: World,
}
impl Handle {
    pub fn belongs_to(&self, world: &World) -> bool {
        Rc::ptr_eq(&self.world.0, &world.0)
    }
    pub fn shutdown(&self) {
        if let Some(Object::Socket { closed, .. }) =
            self.world.0.borrow_mut().objects.get_mut(&self.id)
        {
            *closed = true;
        }
    }
}
impl Drop for Handle {
    fn drop(&mut self) {
        self.world.close(self.id);
    }
}

/// Sparse disk: sync commits writes; crash persists ordered dirty sectors/holes.
#[derive(Clone)]
pub(crate) struct Disk(Arc<Mutex<Image>>);
struct Image {
    completion_fault: Option<u8>,
    available: u64,
    size: u64,
    live: BTreeMap<u64, Sector>,
    durable: BTreeMap<u64, [u8; 512]>,
    dirty: BTreeSet<u64>,
}
impl Image {
    // Skip unchanged bytes in crash prefixes; allocated zeros differ from holes.
    fn persist(&mut self, sectors: usize) {
        let mut committed = 0;
        while committed < sectors {
            let Some(k) = self.dirty.pop_first() else {
                break;
            };
            let value = self.live.get(&k).map(|v| *v.lock().unwrap());
            if self.durable.get(&k).copied() == value {
                continue;
            }
            if let Some(value) = value {
                self.durable.insert(k, value);
            } else {
                self.durable.remove(&k);
            }
            committed += 1;
        }
    }
}
impl Disk {
    pub fn new(size: u64) -> Self {
        Self(Arc::new(Mutex::new(Image {
            completion_fault: None,
            available: u64::MAX,
            size,
            live: BTreeMap::new(),
            durable: BTreeMap::new(),
            dirty: BTreeSet::new(),
        })))
    }
    pub fn read_exact_at(&self, out: &mut [u8], offset: u64) -> io::Result<()> {
        let d = self.0.lock().unwrap();
        if offset
            .checked_add(out.len() as u64)
            .is_none_or(|end| end > d.size)
        {
            return Err(io::ErrorKind::UnexpectedEof.into());
        }
        let mut done = 0;
        while done < out.len() {
            let p = offset + done as u64;
            let start = p as usize % 512;
            let n = (512 - start).min(out.len() - done);
            if let Some(sector) = d.live.get(&(p / 512)) {
                out[done..done + n].copy_from_slice(&sector.lock().unwrap()[start..start + n]);
            } else {
                out[done..done + n].fill(0);
            }
            done += n;
        }
        Ok(())
    }
    pub fn available_bytes(&self) -> u64 {
        self.0.lock().unwrap().available
    }
    // Deliberately ambiguous: bytes/barrier take effect before ENOSPC is reported.
    pub fn fail_after_effect(&self, op: u8) {
        self.0.lock().unwrap().completion_fault = Some(op);
    }
    fn completion_fault(&self, op: u8) -> Option<i32> {
        let mut d = self.0.lock().unwrap();
        if d.completion_fault == Some(op) {
            d.completion_fault = None;
            Some(libc::ENOSPC)
        } else {
            None
        }
    }
    pub fn completion_fault_fired(&self) -> bool {
        self.0.lock().unwrap().completion_fault.is_none()
    }
    pub fn set_available_bytes(&self, bytes: u64) {
        self.0.lock().unwrap().available = bytes;
    }
    pub fn write_all_at(&self, input: &[u8], offset: u64) -> io::Result<()> {
        let mut d = self.0.lock().unwrap();
        if offset
            .checked_add(input.len() as u64)
            .is_none_or(|end| end > d.size)
        {
            return Err(io::ErrorKind::WriteZero.into());
        }
        let mut done = 0;
        while done < input.len() {
            let p = offset + done as u64;
            let start = p as usize % 512;
            let n = (512 - start).min(input.len() - done);
            d.dirty.insert(p / 512);
            d.live
                .entry(p / 512)
                .or_insert_with(|| Arc::new(Mutex::new([0; 512])))
                .lock()
                .unwrap()[start..start + n]
                .copy_from_slice(&input[done..done + n]);
            done += n;
        }
        Ok(())
    }
    pub fn sync_data(&self) -> io::Result<()> {
        let mut d = self.0.lock().unwrap();
        d.persist(usize::MAX);
        Ok(())
    }
    pub fn crash(&self, sectors: usize) {
        let mut d = self.0.lock().unwrap();
        d.persist(sectors);
        d.dirty.clear();
        d.live = d
            .durable
            .iter()
            .map(|(k, v)| (*k, Arc::new(Mutex::new(*v))))
            .collect();
    }
    fn punch(&self, offset: u64, len: u64) {
        assert_eq!(offset % 4096, 0);
        assert_eq!(len % 4096, 0);
        let end = offset.checked_add(len).expect("punch overflow");
        let mut d = self.0.lock().unwrap();
        let keys: Vec<_> = d
            .live
            .range(offset / 512..end / 512)
            .map(|(k, _)| *k)
            .collect();
        for k in keys {
            d.live.remove(&k);
            d.dirty.insert(k);
        }
    }
    fn pages(&self, offset: u64, len: usize) -> io::Result<ByteQueue> {
        let mut d = self.0.lock().unwrap();
        if offset > d.size {
            return Err(io::ErrorKind::UnexpectedEof.into());
        }
        let len = len.min((d.size - offset) as usize);
        let mut out = ByteQueue::default();
        while out.len < len {
            let p = offset + out.len as u64;
            let start = p as usize % 512;
            let n = (512 - start).min(len - out.len);
            // pages() materializes holes; preserve their historical digest entry.
            if !d.live.contains_key(&(p / 512)) {
                d.dirty.insert(p / 512);
            }
            let sector = d
                .live
                .entry(p / 512)
                .or_insert_with(|| Arc::new(Mutex::new([0; 512])))
                .clone();
            out.segments.push_back(Segment {
                bytes: SegmentBytes::Page(sector),
                range: start..start + n,
            });
            out.len += n;
        }
        Ok(out)
    }
    pub fn digest(&self) -> [u8; 32] {
        let d = self.0.lock().unwrap();
        let mut h = blake3::Hasher::new();
        for (k, v) in &d.durable {
            h.update(&k.to_le_bytes());
            h.update(v);
        }
        *h.finalize().as_bytes()
    }
}

#[test]
fn queued_file_pages_require_detachment_before_reuse() {
    let disk = Disk::new(8192);
    disk.write_all_at(&[11; 8192], 0).unwrap();
    let mut unsafe_queue = disk.pages(0, 4096).unwrap();
    disk.write_all_at(&[22; 4096], 0).unwrap();
    let mut bytes = [0; 4096];
    unsafe_queue.read(&mut bytes);
    assert_eq!(
        bytes, [22; 4096],
        "model must detect overwrite of queued pages"
    );
    let mut pipe = disk.pages(4096, 4096).unwrap();
    let mut socket = ByteQueue::default();
    socket.append(pipe.take(123));
    socket.append(pipe.take(4096));
    disk.punch(4096, 4096);
    disk.write_all_at(&[33; 4096], 4096).unwrap();
    socket.read(&mut bytes);
    assert_eq!(bytes, [11; 4096]);
    disk.read_exact_at(&mut bytes, 4096).unwrap();
    assert_eq!(bytes, [33; 4096]);
    disk.sync_data().unwrap();
    disk.punch(4096, 4096);
    disk.crash(8);
    disk.read_exact_at(&mut bytes, 4096).unwrap();
    assert_eq!(bytes, [0; 4096], "persisted hole survives recovery");
}
#[test]
fn copied_chunks_and_mutable_pages_match_byte_queue_boundaries() {
    let disk = Disk::new(1024);
    disk.write_all_at(&[3; 1024], 0).unwrap();
    let mut queue = disk.pages(511, 514).unwrap();
    let mut input = vec![7; 65536];
    queue.extend(&input);
    input.fill(9);
    disk.write_all_at(&[5; 1024], 0).unwrap();
    let expected = [&[5; 513][..], &[7; 65536][..]].concat();
    let mut split = queue.take(1024);
    split.append(queue);
    let mut actual = Vec::new();
    for n in [0, 1, 510, 2, 65535, 17] {
        let mut out = vec![255; n];
        let consumed = n.min(split.len());
        split.read(&mut out);
        actual.extend_from_slice(&out[..consumed]);
        assert!(out[consumed..].iter().all(|b| *b == 255));
    }
    assert_eq!(actual, expected);
    assert!(split.is_empty());
}
#[test]
fn incremental_disk_matches_full_snapshot_crash_prefix() {
    for prefix in [0, 1, 7, 8, 9, usize::MAX] {
        let disk = Disk::new(12288);
        disk.write_all_at(&[1; 8192], 0).unwrap();
        disk.sync_data().unwrap();
        let mut expected = disk.0.lock().unwrap().durable.clone();
        disk.write_all_at(&[1; 513], 511).unwrap(); // unchanged dirty sectors
        disk.write_all_at(&[2; 514], 4095).unwrap();
        disk.punch(0, 4096);
        disk.write_all_at(&[1; 512], 0).unwrap(); // restore one punched sector
        drop(disk.pages(8191, 514).unwrap()); // allocated zeros differ from holes
        let live: BTreeMap<_, _> = disk
            .0
            .lock()
            .unwrap()
            .live
            .iter()
            .map(|(k, v)| (*k, *v.lock().unwrap()))
            .collect();
        let keys: BTreeSet<_> = live.keys().chain(expected.keys()).copied().collect();
        for k in keys
            .into_iter()
            .filter(|k| live.get(k) != expected.get(k))
            .take(prefix)
            .collect::<Vec<_>>()
        {
            if let Some(v) = live.get(&k) {
                expected.insert(k, *v);
            } else {
                expected.remove(&k);
            }
        }
        if prefix == usize::MAX {
            disk.sync_data().unwrap();
        } else {
            disk.crash(prefix);
        }
        assert_eq!(disk.0.lock().unwrap().durable, expected);
        let mut hash = blake3::Hasher::new();
        for (k, v) in &expected {
            hash.update(&k.to_le_bytes());
            hash.update(v);
        }
        assert_eq!(disk.digest(), *hash.finalize().as_bytes());
        disk.sync_data().unwrap();
        assert!(disk.0.lock().unwrap().dirty.is_empty());
        assert_eq!(disk.digest(), *hash.finalize().as_bytes());
    }
}

// Raw backend below the real Ring ownership table. No public raw submission API.
use crate::uring::{
    File,
    abi::{Cqe, Sqe},
};
fn completion(user_data: u64, res: i32, flags: u32) -> Cqe {
    Cqe {
        user_data,
        res,
        flags,
    }
}
pub(crate) struct SimRing {
    pub world: World,
    pub process: Process,
    pub entries: u32,
    pub draining: bool,
    pub staged: Vec<(Sqe, u64)>,
    pub pending: Vec<(Sqe, u64)>,
    pub completions: Vec<(Cqe, u64)>,
    pub cq: VecDeque<Cqe>,
    pub fixed: RefCell<Vec<i32>>,
    pub accepted: BTreeMap<u64, File>,
}
impl SimRing {
    pub fn new(world: World, entries: u32) -> Self {
        Self {
            process: world.process(),
            world,
            entries,
            draining: false,
            staged: Vec::new(),
            pending: Vec::new(),
            completions: Vec::new(),
            cq: VecDeque::new(),
            fixed: RefCell::new(Vec::new()),
            accepted: BTreeMap::new(),
        }
    }
    /// Caller supplies the matching live registration ABI argument.
    pub unsafe fn register(&self, op: u32, arg: *const libc::c_void, count: u32) -> io::Result<()> {
        match op {
            0 | 1 => {}
            2 => *self.fixed.borrow_mut() = vec![-1; count as usize],
            6 => {
                // SAFETY: Ring's private registration caller retains both structs.
                let update = unsafe { &*arg.cast::<crate::uring::abi::FilesUpdate>() };
                self.fixed.borrow_mut()[update.offset as usize] =
                    unsafe { *(update.fds as *const i32) };
            }
            _ => panic!("unsupported simulated registration {op}"),
        }
        Ok(())
    }
    pub fn push(&mut self, sqe: Sqe) {
        let due = self.world.io_delay(sqe.opcode);
        if self.world.managed() {
            self.staged.push((sqe, due));
        } else {
            self.pending.push((sqe, due));
        }
    }
    pub fn submit(&mut self, wait: bool) -> io::Result<()> {
        if !self.world.managed() {
            if wait {
                self.world.advance(Duration::from_millis(1));
            }
            self.world.run_tasks();
        }
        self.enter();
        Ok(())
    }
    pub fn reap(&mut self, output: &mut Vec<Cqe>, budget: usize) -> io::Result<()> {
        output.extend(self.cq.drain(..budget.min(self.cq.len())));
        Ok(())
    }
    pub fn submissions(&self) -> usize {
        if self.world.managed() {
            self.staged.len()
        } else {
            self.pending.len()
        }
    }
    pub fn discard_unsubmitted(&mut self, id: u64) -> bool {
        if let Some((s, _)) = self.staged.iter_mut().find(|(s, _)| s.user_data == id) {
            *s = Sqe {
                user_data: id,
                ..Default::default()
            };
            true
        } else {
            false
        }
    }
    fn fd(&self, s: &Sqe) -> i32 {
        if s.flags & 1 != 0 {
            self.fixed.borrow()[s.fd as usize]
        } else {
            s.fd
        }
    }
    pub fn next_tick(&self) -> Option<u64> {
        self.staged
            .iter()
            .chain(&self.pending)
            .filter(|(s, _)| self.world.operation_ready(s.opcode, self.fd(s)))
            .map(|(_, t)| *t)
            .chain(self.completions.iter().map(|(_, t)| *t))
            .min()
    }
    pub fn enter(&mut self) {
        if self.draining {
            self.quiesce();
            return;
        }
        if !self.world.is_current(self.process) {
            return;
        }
        if !self.world.managed() {
            self.enter_legacy();
            return;
        }
        let _scope = self.world.scoped_process(self.process);
        self.pending.append(&mut self.staged);
        let tick = self.world.tick();
        let mut enabled: Vec<_> = self
            .pending
            .iter()
            .filter(|(s, due)| *due <= tick && self.world.operation_ready(s.opcode, self.fd(s)))
            .map(|(s, _)| s.user_data)
            .collect();
        enabled.sort_unstable(); // stable ticket identities, never vector rotation
        while !enabled.is_empty() {
            // Earlier effects may consume the last bytes/accept or close a peer.
            enabled.retain(|id| {
                self.pending.iter().any(|(s, _)| {
                    s.user_data == *id && self.world.operation_ready(s.opcode, self.fd(s))
                })
            });
            if enabled.is_empty() {
                break;
            }
            let keys: Vec<_> = enabled
                .iter()
                .map(|id| {
                    let s = &self
                        .pending
                        .iter()
                        .find(|(s, _)| s.user_data == *id)
                        .unwrap()
                        .0;
                    let mut hash = blake3::Hasher::new();
                    for value in [
                        *id,
                        s.opcode as u64,
                        self.fd(s) as u64,
                        s.len as u64,
                        s.op_flags as u64,
                        s.file_index as u64,
                    ] {
                        hash.update(&value.to_le_bytes());
                    }
                    // off is a file position/socket length, never a pointer. addr is
                    // only semantic for cancel IDs, punch lengths and splice offsets.
                    hash.update(&s.off.to_le_bytes());
                    if matches!(s.opcode, 14 | 17 | 30) {
                        hash.update(&s.addr.to_le_bytes());
                    }
                    u64::from_le_bytes(hash.finalize().as_bytes()[..8].try_into().unwrap())
                })
                .collect();
            let i = self.world.choose_enabled("uring-effect", &keys);
            let id = enabled.remove(i);
            let Some(i) = self.pending.iter().position(|(s, _)| s.user_data == id) else {
                continue;
            };
            let (s, due) = self.pending.remove(i);
            if s.opcode == 14 {
                let all = s.op_flags != 0;
                let targets: Vec<_> = self
                    .pending
                    .iter()
                    .filter(|(p, _)| p.opcode != 14 && (all || p.user_data == s.addr))
                    .map(|(p, _)| p.user_data)
                    .collect();
                let already = self
                    .completions
                    .iter()
                    .any(|(c, _)| all || c.user_data == s.addr)
                    || self.cq.iter().any(|c| all || c.user_data == s.addr);
                self.pending
                    .retain(|(p, _)| !targets.contains(&p.user_data));
                enabled.retain(|id| !targets.contains(id));
                for id in &targets {
                    self.completions
                        .push((completion(*id, -libc::ECANCELED, 0), self.world.delay()));
                }
                let res = if !targets.is_empty() {
                    if all { targets.len() as i32 } else { 0 }
                } else if already {
                    -libc::EALREADY
                } else {
                    -libc::ENOENT
                };
                self.completions
                    .push((completion(id, res, 0), self.world.delay()));
                self.world.record(14, -1, res);
                continue;
            }
            let fd = self.fd(&s);
            // SAFETY: Ring retains pointers/storage and both SPLICE fds until
            // terminal CQE/NOTIF. Delivery never reexecutes pointer effects.
            let result = unsafe {
                self.world.operation(
                    s.opcode,
                    fd,
                    s.addr,
                    s.len,
                    s.off,
                    s.op_flags,
                    s.file_index as i32,
                )
            };
            let Some((res, accepted)) = result else {
                self.pending.push((s, due));
                continue;
            };
            if let Some(file) = accepted {
                self.accepted.insert(id, File::simulated(file));
            }
            let more = s.opcode == 47 && self.world.choose_enabled("send-zc-result", &[0, 1]) != 0;
            let done = self.world.io_delay(s.opcode);
            self.completions
                .push((completion(id, res, if more { 2 } else { 0 }), done));
            if more {
                self.completions.push((
                    completion(id, 0, 8),
                    done + 1
                        + self
                            .world
                            .choose_enabled("send-zc-notification-delay", &[1, 2, 3])
                            as u64,
                ));
            }
            self.world.record(s.opcode, fd, res);
        }
        let mut ready: Vec<_> = self
            .completions
            .iter()
            .enumerate()
            .filter(|(_, (_, t))| *t <= tick)
            .map(|(i, (c, _))| (c.user_data, c.flags == 8, i))
            .collect();
        ready.sort_unstable();
        while !ready.is_empty() {
            // A clock jump must not deliver NOTIF ahead of the initial result.
            let eligible: Vec<_> = ready
                .iter()
                .enumerate()
                .filter(|(_, (id, notif, _))| {
                    !*notif
                        || !ready
                            .iter()
                            .any(|(other, initial_notif, _)| other == id && !initial_notif)
                })
                .map(|(i, _)| i)
                .collect();
            let keys: Vec<_> = eligible
                .iter()
                .map(|i| {
                    let c = self.completions[ready[*i].2].0;
                    let mut hash = blake3::Hasher::new();
                    hash.update(&c.user_data.to_le_bytes());
                    hash.update(&c.res.to_le_bytes());
                    hash.update(&c.flags.to_le_bytes());
                    u64::from_le_bytes(hash.finalize().as_bytes()[..8].try_into().unwrap())
                })
                .collect();
            let i = eligible[self.world.choose_enabled("uring-completion", &keys)];
            let (_, _, index) = ready.remove(i);
            self.cq.push_back(self.completions[index].0);
        }
        self.completions.retain(|(_, due)| *due > tick);
    }
    // Legacy fixtures retain immediate CQEs and seeded rotation.
    fn enter_legacy(&mut self) {
        self.pending.append(&mut self.staged);
        if !self.pending.is_empty() {
            let n = self.world.choose(self.pending.len());
            self.pending.rotate_left(n);
        }
        let mut next = Vec::new();
        let mut cancelled = Vec::new();
        for (s, due) in &self.pending {
            if s.opcode == 14 && *due <= self.world.tick() {
                if s.op_flags != 0 {
                    cancelled.extend(
                        self.pending
                            .iter()
                            .filter(|(p, _)| p.opcode != 14)
                            .map(|(p, _)| p.user_data),
                    );
                } else {
                    cancelled.push(s.addr);
                }
            }
        }
        for (mut s, due) in std::mem::take(&mut self.pending) {
            if cancelled.contains(&s.user_data) && s.opcode != 255 {
                self.cq
                    .push_back(completion(s.user_data, -libc::ECANCELED, 0));
                continue;
            }
            if due > self.world.tick() {
                next.push((s, due));
                continue;
            }
            if s.opcode == 255 || s.opcode == 14 {
                self.cq.push_back(completion(
                    s.user_data,
                    0,
                    if s.opcode == 255 { 8 } else { 0 },
                ));
                continue;
            }
            let fd = self.fd(&s);
            // SAFETY: same retained request-table ownership as managed mode.
            let result = unsafe {
                self.world.operation(
                    s.opcode,
                    fd,
                    s.addr,
                    s.len,
                    s.off,
                    s.op_flags,
                    s.file_index as i32,
                )
            };
            let Some((res, accepted)) = result else {
                next.push((s, due));
                continue;
            };
            if let Some(file) = accepted {
                self.accepted.insert(s.user_data, File::simulated(file));
            }
            let flags = if s.opcode == 47 { 2 } else { 0 };
            self.world.record(s.opcode, fd, res);
            self.cq.push_back(completion(s.user_data, res, flags));
            if flags != 0 {
                s.opcode = 255;
                next.push((s, self.world.delay()));
            }
        }
        self.pending = next;
    }
    /// Cancel future effects and deliver results/NOTIF without callbacks or ticks.
    pub fn quiesce(&mut self) {
        for (s, _) in self.staged.drain(..).chain(self.pending.drain(..)) {
            self.cq.push_back(Cqe {
                user_data: s.user_data,
                res: if matches!(s.opcode, 14 | 255) {
                    0
                } else {
                    -libc::ECANCELED
                },
                flags: if s.opcode == 255 { 8 } else { 0 },
            });
        }
        for (c, _) in self.completions.drain(..) {
            self.cq.push_back(c);
        }
    }
}

include!("simulation_contracts.rs");
