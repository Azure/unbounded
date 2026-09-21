// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn readiness25_native_blocked_jobs_event_ack_and_context_close() {
    use std::io::Write;
    use std::process::{Command, Stdio};

    let source = r#"/* Exercise the actual C job/tryjoin/event ownership code without an RNIC. */
#define _GNU_SOURCE
#include <infiniband/verbs.h>
#include <errno.h>
#include <assert.h>
#include <stdatomic.h>
#include <unistd.h>
#include <sched.h>
#include <pthread.h>

static atomic_int allow_error, allow_destroy, pending_event, event_acked;
static atomic_int error_calls, destroy_calls, close_calls, allow_close;
static atomic_int allow_mw;
static atomic_int mw_calls, fail_mw, fail_qp;
static pthread_mutex_t gate_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t gate_cond = PTHREAD_COND_INITIALIZER;
static void wait_gate(atomic_int *gate) {
    pthread_mutex_lock(&gate_mutex);
    while (!atomic_load(gate)) pthread_cond_wait(&gate_cond, &gate_mutex);
    pthread_mutex_unlock(&gate_mutex);
}
static void release_gate(atomic_int *gate) {
    pthread_mutex_lock(&gate_mutex);
    atomic_store(gate, 1);
    pthread_cond_broadcast(&gate_cond);
    pthread_mutex_unlock(&gate_mutex);
}
static struct ibv_qp qp;
static int mock_modify(struct ibv_qp *q, struct ibv_qp_attr *a, int mask) {
    (void)q; (void)a; (void)mask;
    atomic_fetch_add(&error_calls, 1);
    wait_gate(&allow_error);
    return 0;
}
static int mock_destroy(struct ibv_qp *q) {
    (void)q;
    int call = atomic_fetch_add(&destroy_calls, 1) + 1;
    wait_gate(&allow_destroy);
    wait_gate(&event_acked);
    if (call == atomic_load(&fail_qp)) return EIO;
    return 0;
}
static int mock_get(struct ibv_context *ctx, struct ibv_async_event *event) {
    assert(ctx && !atomic_load(&close_calls));
    if (!atomic_exchange(&pending_event, 0)) { errno = EAGAIN; return -1; }
    event->event_type = IBV_EVENT_QP_FATAL;
    event->element.qp = &qp;
    return 0;
}
static void mock_ack(struct ibv_async_event *event) {
    assert(event->element.qp == &qp);
    release_gate(&event_acked);
}
static int mock_close(struct ibv_context *ctx) {
    (void)ctx;
    atomic_fetch_add(&close_calls, 1);
    wait_gate(&allow_close);
    return 0;
}
static int mock_mw(struct ibv_mw *mw) {
    (void)mw;
    int call = atomic_fetch_add(&mw_calls, 1) + 1;
    wait_gate(&allow_mw);
    if (call == atomic_load(&fail_mw)) return EIO;
    return 0;
}
/* Explicitly join a permitted mock helper before observing its result. This
 * makes counts independent of OS scheduling; blocked-job probes still exercise
 * the real tryjoin. Production never uses this test-only blocking join. */
static int joined;
static int mock_tryjoin(pthread_t thread, void **result) {
    if (joined) { joined = 0; return 0; }
    return pthread_tryjoin_np(thread, result);
}
#define pthread_tryjoin_np mock_tryjoin
#define ibv_modify_qp mock_modify
#define ibv_destroy_qp mock_destroy
#define ibv_get_async_event mock_get
#define ibv_ack_async_event mock_ack
#define ibv_close_device mock_close
#define ibv_dealloc_mw mock_mw
#include "src/rdma_verbs.c"

static void finish(struct racer_device *d) {
    assert(d->job && !joined);
    assert(!pthread_join(d->job->thread, NULL));
    joined = 1;
}

static void capacity(size_t qps, size_t windows, int delayed) {
    struct racer_device *d = calloc(1, sizeof(*d));
    assert(d);
    struct ibv_context ctx = {0};
    d->ctx = &ctx;
    d->async_ready = 1;
    void **qs = calloc(qps ? qps : 1, sizeof(*qs));
    void **ws = calloc(windows, sizeof(*ws));
    assert(d && qs && ws);
    for (size_t i = 0; i < qps; ++i) qs[i] = (void *)(i + 1);
    for (size_t i = 0; i < windows; ++i) ws[i] = (void *)(i + 1);
    atomic_store(&error_calls, 0);
    atomic_store(&destroy_calls, 0);
    atomic_store(&mw_calls, 0);
    atomic_store(&close_calls, 0);
    atomic_store(&allow_destroy, !delayed);
    atomic_store(&event_acked, !delayed);
    atomic_store(&allow_mw, !delayed);
    unsigned stages = 0;
    if (qps) {
        assert(racer_destroy_qps(d, qs, qps) == EAGAIN);
        ++stages;
        assert(atomic_load(&destroy_jobs) == 1);
        if (delayed) {
            assert(racer_destroy_qps(d, qs, qps) == EAGAIN);
            atomic_store(&pending_event, 1);
            uint32_t qpn;
            assert(racer_event(d, 1, &qpn) == 2 && qpn == 42);
            release_gate(&allow_destroy);
        }
        finish(d);
        assert(!racer_destroy_qps(d, qs, qps));
        for (size_t i = 0; i < qps; ++i) assert(!qs[i]);
    }
    assert(atomic_load(&destroy_calls) == (int)qps);
    assert(atomic_load(&error_calls) == (int)qps);
    assert(racer_free_windows(d, ws, windows) == EAGAIN);
    ++stages;
    if (delayed) {
        assert(racer_free_windows(d, ws, windows) == EAGAIN);
        assert(racer_close(d) == EAGAIN); /* batch retains device ownership */
        release_gate(&allow_mw);
    }
    finish(d);
    assert(!racer_free_windows(d, ws, windows));
    for (size_t i = 0; i < windows; ++i) assert(!ws[i]);
    assert(atomic_load(&mw_calls) == (int)windows);
    /* Two close stages, with event consumption fenced before context close. */
    assert(racer_close(d) == EAGAIN);
    ++stages;
    finish(d);
    assert(racer_close(d) == EAGAIN);
    ++stages;
    assert(d->closing);
    finish(d);
    assert(!racer_close(d));
    assert(!atomic_load(&destroy_jobs));
    assert(stages == (qps ? 4u : 3u));
    free(qs);
    free(ws);
}

int main(void) {
    alarm(15); /* A synchronous-wait regression fails rather than hanging CI. */
    struct ibv_context ctx = {0};
    struct racer_device devices[33] = {0};
    uint32_t partial_qpn;
    assert(racer_event(&devices[0], 0, &partial_qpn) == -EAGAIN);
    assert(racer_event(&devices[0], 1, &partial_qpn) == -EAGAIN);
    qp.qp_num = 42;
    for (int i = 0; i < 33; ++i) {
        devices[i].ctx = &ctx;
        devices[i].async_ready = 1;
    }
    for (int i = 0; i < 32; ++i) {
        assert(racer_destroy_qp(&devices[i], &qp) == EAGAIN);
        assert(devices[i].job);
    }
    assert(racer_destroy_qp(&devices[32], &qp) == EAGAIN);
    assert(!devices[32].job); /* no queued helper/job allocation beyond cap */
    void *denied[] = {&qp};
    assert(racer_destroy_qps(&devices[32], denied, 1) == EAGAIN);
    assert(racer_free_windows(&devices[32], denied, 1) == EAGAIN);
    assert(!devices[32].job && denied[0] == &qp);
    for (int i = 0; i < 1000; ++i) {
        assert(racer_destroy_qp(&devices[0], &qp) == EAGAIN);
        assert(atomic_load(&destroy_jobs) == 32);
    }
    release_gate(&allow_error);
    atomic_store(&pending_event, 1);
    uint32_t qpn = 0;
    assert(racer_event(&devices[0], 1, &qpn) == 2 && qpn == 42);
    assert(atomic_load(&event_acked));
    assert(racer_destroy_qp(&devices[0], &qp) == EAGAIN);
    release_gate(&allow_destroy);
    for (int i = 0; i < 32; ++i) {
        int e;
        while ((e = racer_destroy_qp(&devices[i], &qp)) == EAGAIN) sched_yield();
        assert(!e && !devices[i].job);
    }
    assert(!atomic_load(&destroy_jobs));
    assert(atomic_load(&error_calls) == 32 && atomic_load(&destroy_calls) == 32);
    struct ibv_mw mw = {0};
    struct ibv_mw mw2 = {0};
    void *windows[] = {&mw, NULL, &mw2};
    atomic_store(&fail_mw, 2);
    assert(racer_free_windows(&devices[0], windows, 3) == EAGAIN);
    assert(racer_destroy_qp(&devices[0], &qp) == EAGAIN);
    assert(atomic_load(&destroy_jobs) == 1); /* one helper per device */
    release_gate(&allow_mw);
    finish(&devices[0]);
    assert(racer_free_windows(&devices[0], windows, 3) == EIO);
    assert(!windows[0] && !windows[1] && windows[2] == &mw2);
    assert(racer_free_windows(&devices[0], windows, 3) == EAGAIN);
    finish(&devices[0]);
    assert(!racer_free_windows(&devices[0], windows, 3));
    assert(!windows[2] && atomic_load(&mw_calls) == 3 && !atomic_load(&destroy_jobs));
    atomic_store(&fail_mw, 0);
    /* Adopt a pending single-QP destroy without double destruction. */
    struct ibv_qp qp2 = {0};
    void *qps[] = {&qp, &qp2};
    assert(racer_destroy_qp(&devices[0], &qp) == EAGAIN);
    finish(&devices[0]);
    atomic_store(&fail_qp, 34);
    assert(racer_destroy_qps(&devices[0], qps, 2) == EAGAIN);
    finish(&devices[0]);
    assert(racer_destroy_qps(&devices[0], qps, 2) == EIO);
    assert(!qps[0] && qps[1] == &qp2);
    assert(racer_destroy_qps(&devices[0], qps, 2) == EAGAIN);
    finish(&devices[0]);
    assert(!racer_destroy_qps(&devices[0], qps, 2));
    assert(!qps[0] && !qps[1] && atomic_load(&destroy_calls) == 35);
    atomic_store(&fail_qp, 0);
    /* The context-closing helper must never race an event consumer. */
    struct racer_device *d = calloc(1, sizeof(*d));
    d->ctx = &ctx;
    d->async_ready = 1;
    while (!atomic_load(&close_calls)) {
        assert(racer_close(d) == EAGAIN);
        sched_yield();
    }
    assert(d->closing);
    for (int i = 0; i < 1000; ++i) {
        assert(racer_event(d, 1, &qpn) == -EAGAIN);
        assert(racer_event(d, 0, &qpn) == -EAGAIN);
        assert(racer_close(d) == EAGAIN);
    }
    release_gate(&allow_close);
    int e;
    while ((e = racer_close(d)) == EAGAIN) sched_yield();
    assert(!e && !atomic_load(&destroy_jobs));
    for (int delayed = 0; delayed <= 1; ++delayed) {
        capacity(0, 32 * 16 * 4, delayed); /* reported zero-QP regression */
        capacity(32, 32 * 16 * 4, delayed); /* maximum daemon startup geometry */
        capacity(256, 256 * 16 * 4, delayed); /* suspected serial-QP limit */
        capacity(4096, 4096 * 128 * 4, delayed); /* actual public Config maximum */
    }
    return 0;
}
"#;
    let binary = std::env::temp_dir().join(format!("racer-rdma-teardown-{}", std::process::id()));
    let mut compiler = Command::new("cc")
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .args([
            "-std=c11",
            "-O2",
            "-Wall",
            "-Wextra",
            "-Werror",
            "-x",
            "c",
            "-",
            "-libverbs",
            "-pthread",
            "-o",
        ])
        .arg(&binary)
        .stdin(Stdio::piped())
        .spawn()
        .unwrap();
    compiler
        .stdin
        .take()
        .unwrap()
        .write_all(source.as_bytes())
        .unwrap();
    let status = compiler.wait().unwrap();
    assert!(
        status.success(),
        "compile native teardown fixture: {status}"
    );
    let status = Command::new(&binary).status().unwrap();
    std::fs::remove_file(binary).unwrap();
    assert!(status.success(), "native teardown fixture: {status}");
}

#[derive(Default)]
pub(super) struct Simulation {
    pub control_edit: Option<Box<dyn FnOnce(&mut Vec<u8>)>>,
    pub id: u64,
    pub effected: std::collections::HashSet<u64>,
    pub queued: std::collections::HashSet<u64>,
    pub receives: std::collections::BTreeMap<u64, ffi::Wc>,
    pub armed: bool,
    pub wake: Option<std::sync::Arc<uring::Wake>>,
    pub window_free_fails: bool,
    pub window_alloc_fails: bool,
    pub windows_freed: usize,
    pub windows_allocated: usize,
    pub posts: Vec<(usize, u32)>,
    pub reject: u32,
    pub destroy_fails: bool,
    pub destroy_blocked: bool,
    pub destroy_after: Option<Instant>,
    pub cleanup_delay: Option<Duration>,
    pub cleanup_stages: [Option<Instant>; 4],
    pub connect_error: i32,
    pub completions: std::collections::VecDeque<ffi::Wc>,
}
impl Simulation {
    // Native helpers always return pending on admission, including an instant
    // provider. Model one such turn per batch/stage, never per window or QP.
    pub(super) fn cleanup_turn(&mut self, stage: usize) -> io::Result<()> {
        let Some(delay) = self.cleanup_delay else {
            return Ok(());
        };
        let now = crate::environment::now();
        match self.cleanup_stages[stage] {
            Some(end) if now >= end => Ok(()),
            Some(_) => Err(full()),
            None => {
                self.cleanup_stages[stage] = Some(now + delay);
                Err(full())
            }
        }
    }
    pub(super) fn signal(&mut self) {
        if self.armed {
            self.armed = false;
            if let Some(wake) = &self.wake {
                crate::workers::Wake::wake(&**wake);
            }
        }
    }
}

impl Transport {
    pub(crate) fn test_cleanup_batches(&self, delay: Duration) {
        with(self, |core| {
            core.simulation.as_mut().unwrap().cleanup_delay = Some(delay)
        });
    }
}

pub(crate) fn transport_config(
    pool: &WorkerPool,
    connections: usize,
    depth: usize,
) -> (Transport, Connection) {
    let mut core = Core::new(
        pool,
        Rail {
            raw: unsafe { std::mem::zeroed() },
            name: "simulation".into(),
            numa_node: None,
        },
        Config {
            fabric: "test".into(),
            connections,
            depth,
            timeout: Duration::from_secs(30),
        },
    );
    core.simulation = Some(Simulation {
        id: TEST_TRANSPORT_ID.with(|id| {
            id.set(
                id.get()
                    .checked_add(1)
                    .expect("transport identity exhausted"),
            );
            id.get()
        }),
        ..Simulation::default()
    });
    core.serial = 1;
    for (i, s) in core.slots.iter_mut().enumerate() {
        s.mw = std::ptr::dangling_mut::<u8>().cast();
        s.key = 0x123400 + i as u32 * 256;
    }
    let endpoint = ffi::Endpoint {
        qpn: 7,
        ..ffi::Endpoint::default()
    };
    let mut session = Session::new(
        std::ptr::dangling_mut::<u8>().cast(),
        1,
        [1; 16],
        endpoint,
        crate::environment::now() + core.config.timeout,
    );
    session.peer = [2; 16];
    session.ready = true;
    core.connections.push(session);
    let transport = Transport {
        owner: Rc::new(RefCell::new(Owner {
            core: Some(Box::new(core)),
        })),
    };
    let connection = Connection {
        transport: transport.clone(),
        index: 0,
        serial: 1,
        cancelled: with(&transport, |core| core.connections[0].cancelled.clone()),
    };
    TEST_TRANSPORTS.with(|registry| {
        let mut registry = registry.borrow_mut();
        registry.retain(|owner| owner.strong_count() != 0);
        registry.push(Rc::downgrade(&transport.owner));
    });
    (transport, connection)
}
pub(super) fn with<T>(t: &Transport, f: impl FnOnce(&mut Core) -> T) -> T {
    f(t.owner.borrow_mut().core().unwrap())
}
pub(super) fn wc(core: &Core, i: usize) -> ffi::Wc {
    ffi::Wc {
        id: core.slots[i].wr,
        status: 0,
        opcode: core.slots[i].opcode,
        len: 0,
        qpn: core.connections[core.slots[i].conn].qpn,
    }
}
pub(super) fn complete(t: &Transport, i: usize) {
    let now = crate::environment::now();
    with(t, |c| c.completed(wc(c, i), now).unwrap());
}

pub(super) fn wire(t: &Transport, i: usize) -> Vec<u8> {
    with(t, |c| wire_bytes(c, i))
}
pub(super) fn wire_bytes(c: &Core, i: usize) -> Vec<u8> {
    (unsafe { c.control.bytes(i) })[..c.slots[i].wire_len].to_vec()
}
pub(super) fn deliver(t: &Transport, bytes: &[u8]) {
    with(t, |c| c.receive_bytes(0, bytes).unwrap());
}

impl Source {
    pub(crate) fn test_transport(&self) -> Transport {
        self.transport.clone()
    }
}

pub(crate) fn test_connection(pool: &WorkerPool) -> Connection {
    tests::transport_config(pool, 1, 4).1
}

thread_local! {
    // Weak transport owners, independent of runtime admission and draining lists.
    static TEST_TRANSPORTS: RefCell<Vec<std::rc::Weak<RefCell<Owner>>>> = const { RefCell::new(Vec::new()) };
    static TEST_TRANSPORT_ID: Cell<u64> = const { Cell::new(0) };
}

/// Opaque generation-fenced QP identity; holds no connection/cancellation owner.
#[derive(Clone)]
pub(crate) struct TestQp {
    transport: Transport,
    index: usize,
    serial: u64,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(crate) struct TestPost {
    pub id: u64,
    pub opcode: u32,
    pub kind: u8,
    pub value: [u8; 32],
    owner: u64,
    serial: u64,
}
/// Snapshot every still-owned QP, including pending activation and failed destroy.
pub(crate) fn test_qps() -> Vec<TestQp> {
    TEST_TRANSPORTS.with(|registry| {
        let mut result = Vec::new();
        for owner in registry.borrow().iter().filter_map(|w| w.upgrade()) {
            let transport = Transport { owner };
            let borrowed = transport.owner.borrow();
            let Some(core) = &borrowed.core else {
                continue;
            };
            for (index, c) in core.connections.iter().enumerate() {
                if !c.qp.is_null() {
                    result.push(TestQp {
                        transport: transport.clone(),
                        index,
                        serial: c.serial,
                    });
                }
            }
        }
        result
    })
}
impl Transport {
    /// Attach this source to the ordinary uring Driver; no synthetic verbs fd.
    pub(crate) fn test_source(&self) -> Source {
        tests::with(self, |c| assert!(c.simulation.is_some()));
        Source {
            transport: self.clone(),
            files: None,
            polls: [None, None],
        }
    }
    pub(crate) fn test_progress(&self, budget: usize) -> io::Result<uring::Work> {
        self.owner.borrow_mut().core()?.progress(budget)
    }
    pub(crate) fn test_block_destroy(&self, blocked: bool) {
        tests::with(self, |c| {
            c.simulation.as_mut().unwrap().destroy_blocked = blocked
        });
    }
    pub(crate) fn test_destroy_after(&self, after: Instant) {
        tests::with(self, |c| {
            c.simulation.as_mut().unwrap().destroy_after = Some(after)
        });
    }
    /// Shared conformance oracle: never exports storage, keys, pointers or Core.
    pub(crate) fn test_invariants(&self) -> (usize, usize, usize) {
        let owner = self.owner.borrow();
        let Some(c) = &owner.core else {
            return (0, 0, 0);
        };
        c.assert_invariants(&c.free);
        (
            c.slots.len(),
            c.free.len(),
            c.slots.iter().filter(|s| s.owns_dma()).count(),
        )
    }
    pub(crate) fn test_expire(&self) {
        tests::with(self, |c| {
            for s in &mut c.slots {
                s.deadline = crate::environment::now();
            }
            for s in &mut c.connections {
                s.deadline = crate::environment::now();
            }
            c.renew_after = None;
        });
    }
    pub(crate) fn test_free_indices(&self, indices: &[usize]) {
        tests::with(self, |c| c.assert_invariants(indices));
    }
    /// Provider fault controls; production quiescence/retention still handles them.
    pub(crate) fn test_faults(&self, reject: u32, destroy: bool, free: bool, allocate: bool) {
        tests::with(self, |c| {
            let s = c.simulation.as_mut().unwrap();
            s.reject = reject;
            s.destroy_fails = destroy;
            s.window_free_fails = free;
            s.window_alloc_fails = allocate;
        });
    }
    pub(crate) fn test_edit_control(&self, edit: impl FnOnce(&mut Vec<u8>) + 'static) {
        tests::with(self, |c| {
            c.simulation.as_mut().unwrap().control_edit = Some(Box::new(edit))
        });
    }
    pub(crate) fn test_observe(&self) -> TestObservation {
        tests::with(self, |c| {
            let sim = c.simulation.as_ref().unwrap();
            TestObservation {
                windows: c
                    .slots
                    .iter()
                    .map(|s| (s.uses, s.key, !s.mw.is_null()))
                    .collect(),
                sends: c
                    .slots
                    .iter()
                    .enumerate()
                    .filter_map(|(i, s)| {
                        (s.wr != 0 && s.opcode == 1).then(|| (s.wr, tests::wire_bytes(c, i)))
                    })
                    .collect(),
                qps: c.connections.iter().filter(|s| !s.qp.is_null()).count(),
                wrs: c.slots.iter().filter(|s| s.wr != 0).count(),
                allocated: sim.windows_allocated,
                freed: sim.windows_freed,
                pending: c
                    .slots
                    .iter()
                    .filter_map(|s| s.send_pending.then_some(s.signed))
                    .collect(),
            }
        })
    }
    pub(crate) fn test_inject(&self, bytes: &[u8]) -> io::Result<()> {
        tests::with(self, |c| c.receive_bytes(0, bytes))
    }
    pub(crate) fn test_borrow(&self, f: impl FnOnce()) {
        let _owner = self.owner.borrow_mut();
        f();
    }
    pub(crate) fn test_connect_error(&self) {
        tests::with(self, |c| {
            c.simulation.as_mut().unwrap().connect_error = libc::EIO
        });
    }
    pub(crate) fn test_reset_fabric(&self) {
        tests::with(self, |c| c.config.fabric.clear());
    }
    pub(crate) fn test_stale_cqe(&self, id: u64) {
        tests::with(self, |c| {
            let before: Vec<_> = c.slots.iter().map(|s| s.wr).collect();
            let wc = ffi::Wc {
                id,
                ..ffi::Wc::default()
            };
            c.completed(wc, crate::environment::now()).unwrap();
            assert_eq!(before, c.slots.iter().map(|s| s.wr).collect::<Vec<_>>());
        });
    }
}
impl TestQp {
    pub(crate) fn same(&self, other: &Self) -> bool {
        Rc::ptr_eq(&self.transport.owner, &other.transport.owner)
            && self.index == other.index
            && self.serial == other.serial
    }
    pub(crate) fn belongs_to(&self, transport: &Transport) -> bool {
        Rc::ptr_eq(&self.transport.owner, &transport.owner)
    }
    /// Exact reciprocal endpoint and nonce pairing; never pair by node or QPN alone.
    pub(crate) fn pairs_with(&self, other: &Self) -> bool {
        if self.same(other) {
            return false;
        }
        let a = self.transport.owner.borrow();
        let b = other.transport.owner.borrow();
        let (Some(a), Some(b)) = (&a.core, &b.core) else {
            return false;
        };
        let (Ok(a), Ok(b)) = (
            a.connection(self.index, self.serial),
            b.connection(other.index, other.serial),
        ) else {
            return false;
        };
        !a.qp.is_null()
            && !b.qp.is_null()
            && a.ready
            && b.ready
            && !a.failed
            && !b.failed
            && !a.cancelled.get()
            && !b.cancelled.get()
            && a.binding.is_some()
            && b.binding.is_some()
            && a.remote_endpoint == Some(b.endpoint)
            && b.remote_endpoint == Some(a.endpoint)
            && a.local == b.peer
            && a.peer == b.local
    }
    pub(crate) fn posts(&self) -> Vec<TestPost> {
        let owner = self.transport.owner.borrow();
        let Some(core) = &owner.core else {
            return Vec::new();
        };
        if core.connection(self.index, self.serial).is_err() {
            return Vec::new();
        }
        let mut posts = core
            .slots
            .iter()
            .filter(|s| s.conn == self.index && s.wr != 0 && s.opcode != 2)
            .map(|s| self.post_token(core, s.wr))
            .collect::<Vec<_>>();
        posts.sort_by_key(|p| p.id);
        posts
    }
    pub(crate) fn receives(&self) -> Vec<TestPost> {
        let owner = self.transport.owner.borrow();
        let Some(c) = &owner.core else {
            return vec![];
        };
        if c.connection(self.index, self.serial).is_err() {
            return vec![];
        }
        c.simulation
            .as_ref()
            .unwrap()
            .receives
            .values()
            .filter(|wc| c.slots[wc.id as u32 as usize].conn == self.index)
            .map(|wc| self.post_token(c, wc.id))
            .collect()
    }
    fn post_token(&self, c: &Core, id: u64) -> TestPost {
        let s = &c.slots[id as u32 as usize];
        TestPost {
            id,
            opcode: s.opcode,
            kind: s.frame.kind,
            value: s.frame.value,
            owner: c.simulation.as_ref().unwrap().id,
            serial: self.serial,
        }
    }
    /// Apply one legal SQ effect. CQE delivery is a separate scheduler choice.
    /// `false` means stale, already effected, SQ-order blocked, or no receive credit.
    pub(crate) fn effect(&self, peer: &Self, post: TestPost, corrupt: bool) -> io::Result<bool> {
        if post.serial != self.serial {
            return Ok(false);
        }
        if !self.pairs_with(peer) {
            return Ok(false);
        }
        // Same-PD loopback is supported by hardware; this scheduler uses separate
        // transport borrows and rejects that unsupported simulated shape explicitly.
        if Rc::ptr_eq(&self.transport.owner, &peer.transport.owner) {
            return Err(invalid());
        }
        let mut owner = self.transport.owner.borrow_mut();
        let Some(core) = owner.core.as_mut() else {
            return Ok(false);
        };
        if post.owner != core.simulation.as_ref().unwrap().id {
            return Ok(false);
        }
        let i = post.id as u32 as usize;
        let Some(s) = core.slots.get(i) else {
            return Ok(false);
        };
        let sim = core.simulation.as_ref().unwrap();
        if s.conn != self.index
            || s.wr != post.id
            || s.opcode != post.opcode
            || sim.effected.contains(&post.id)
            || sim.queued.contains(&post.id)
            || core.slots.iter().any(|s| {
                s.conn == self.index
                    && s.opcode != 2
                    && s.wr != 0
                    && s.wr < post.id
                    && !sim.effected.contains(&s.wr)
            })
        {
            return Ok(false);
        }
        let frame = s.frame;
        match post.opcode {
            1 => {
                let mut remote_owner = peer.transport.owner.borrow_mut();
                let remote = remote_owner.core()?;
                let receive = remote
                    .slots
                    .iter()
                    .enumerate()
                    .filter(|(_, s)| {
                        s.conn == peer.index
                            && s.opcode == 2
                            && s.wr != 0
                            && !remote.simulation.as_ref().unwrap().effected.contains(&s.wr)
                            && !remote.simulation.as_ref().unwrap().queued.contains(&s.wr)
                    })
                    .min_by_key(|(_, s)| s.wr)
                    .map(|(i, _)| i);
                let Some(receive) = receive else {
                    return Ok(false);
                };
                let bytes = unsafe { core.control.bytes(i) };
                let len = core.slots[i].wire_len;
                if len > CONTROL {
                    return Err(protocol());
                }
                // Both transport owners are borrowed through the synchronous copy.
                // No pointer escapes this effect; WRs keep arenas pinned until CQE
                // processing or successful QP destruction. Distinct transports own
                // distinct control arenas, so the ranges cannot overlap.
                unsafe {
                    ptr::copy_nonoverlapping(bytes.as_ptr(), remote.control.pointer(receive), len);
                }
                if let Some(world) = crate::simulation::current() {
                    world.trace_bytes(&bytes[..len]);
                }
                let mut wc = tests::wc(remote, receive);
                wc.len = len as u32;
                let sim = remote.simulation.as_mut().unwrap();
                sim.effected.insert(wc.id);
                sim.receives.insert(wc.id, wc);
            }
            3 => {
                let remote_owner = peer.transport.owner.borrow();
                let remote = remote_owner.core.as_ref().ok_or_else(invalid)?;
                let source = remote
                    .slots
                    .iter()
                    .find(|s| {
                        s.conn == peer.index
                            && !s.mw.is_null()
                            && s.binding == Some((peer.serial, frame.key))
                            && matches!(s.phase, Phase::Advertise | Phase::AwaitAck)
                            && s.frame.grant == frame.grant
                            && s.frame.address == frame.address
                            && s.frame.key == frame.key
                            && s.frame.value == frame.value
                            && s.frame.len == frame.len
                    })
                    .ok_or_else(protocol)?;
                let bytes = source.buffer.as_ref().ok_or_else(protocol)?.as_slice();
                if bytes.len() != frame.len as usize {
                    return Err(protocol());
                }
                let destination = core.slots[i]
                    .fill
                    .as_mut()
                    .ok_or_else(protocol)?
                    .as_mut_slice();
                if destination.len() < bytes.len() {
                    return Err(protocol());
                }
                destination[..bytes.len()].copy_from_slice(bytes);
                if corrupt {
                    destination[0] ^= 1;
                }
                if let Some(world) = crate::simulation::current() {
                    world.trace_bytes(bytes);
                }
            }
            4 => {
                let s = &mut core.slots[i];
                if s.mw.is_null()
                    || s.binding.is_some()
                    || s.phase != Phase::Bind
                    || s.buffer.is_none()
                {
                    return Err(protocol());
                }
                s.binding = Some((self.serial, frame.key));
            }
            5 => {
                let s = &mut core.slots[i];
                if s.binding != Some((self.serial, frame.key))
                    || s.phase != Phase::Invalidate
                    || s.buffer.is_none()
                {
                    return Err(protocol());
                }
                s.binding = None;
            }
            _ => return Err(invalid()),
        }
        core.simulation.as_mut().unwrap().effected.insert(post.id);
        Ok(true)
    }
    /// Enqueue the exact effect's CQE; Source::poll alone runs production completion.
    pub(crate) fn complete(&self, post: TestPost, status: u32) -> io::Result<bool> {
        if post.serial != self.serial {
            return Ok(false);
        }
        let mut owner = self.transport.owner.borrow_mut();
        let Some(core) = owner.core.as_mut() else {
            return Ok(false);
        };
        if post.owner != core.simulation.as_ref().unwrap().id {
            return Ok(false);
        }
        if core.connection(self.index, self.serial).is_err() {
            return Ok(false);
        }
        let i = post.id as u32 as usize;
        if core
            .slots
            .get(i)
            .is_none_or(|s| s.wr != post.id || s.conn != self.index || s.opcode != post.opcode)
        {
            return Ok(false);
        }
        let mut wc = core
            .simulation
            .as_ref()
            .unwrap()
            .receives
            .get(&post.id)
            .copied()
            .unwrap_or_else(|| tests::wc(core, i));
        wc.status = status;
        let sim = core.simulation.as_mut().unwrap();
        if (status == 0 && !sim.effected.contains(&post.id))
            || core.slots.iter().any(|s| {
                s.conn == self.index
                    && (s.opcode == 2) == (post.opcode == 2)
                    && s.wr != 0
                    && s.wr < post.id
                    && !sim.queued.contains(&s.wr)
            })
            || !sim.queued.insert(post.id)
        {
            return Ok(false);
        }
        sim.completions.push_back(wc);
        sim.signal();
        Ok(true)
    }
    pub(crate) fn disconnect(&self) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        if core.connection(self.index, self.serial).is_err() {
            return Ok(());
        }
        core.fail(self.index, io::ErrorKind::ConnectionAborted)
    }
    pub(crate) fn source_ready(&self) -> bool {
        self.transport
            .owner
            .borrow()
            .core
            .as_ref()
            .is_some_and(|c| !c.simulation.as_ref().unwrap().completions.is_empty())
    }
}

pub(crate) fn test_transport(pool: &WorkerPool) -> Transport {
    test_transport_config(pool, 1, 4)
}
pub(crate) fn test_transport_multi(pool: &WorkerPool) -> Transport {
    test_transport_config(pool, 8, 4)
}
/// Reduced queue geometry only: pool leases and 4MiB registered buffers stay real.
pub(crate) fn test_transport_config(pool: &WorkerPool, qps: usize, depth: usize) -> Transport {
    assert!((1..=4096).contains(&qps) && (1..=128).contains(&depth));
    let (t, c) = tests::transport_config(pool, qps, depth);
    drop(c);
    tests::with(&t, |core| core.rail.raw.max_read = depth as u32);
    t
}

impl Connection {
    pub(crate) fn test_window_renewal(&self, allocation_fails: bool) {
        tests::with(&self.transport, |core| {
            core.renewing = true;
            core.simulation.as_mut().unwrap().window_alloc_fails = allocation_fails;
            core.renew_after = None;
        });
    }

    pub(crate) fn test_endpoint(&self) -> (Transport, TestQp) {
        (
            self.transport.clone(),
            TestQp {
                transport: self.transport.clone(),
                index: self.index,
                serial: self.serial,
            },
        )
    }

    pub(crate) fn test_deliver_request(&self, destination: &Connection) -> Ticket<Grant> {
        let ticket = self.request([3; 32], 3, b"invalid descriptor").unwrap();
        tests::deliver(
            &destination.transport,
            &tests::wire(&self.transport, ticket.index),
        );
        tests::complete(&self.transport, ticket.index);
        ticket
    }
    pub(crate) fn test_grant(&self, ticket: &Ticket<Grant>) {
        tests::complete(&self.transport, ticket.index);
        tests::with(&self.transport, |core| {
            let frame =
                crate::negotiation::control_wire::test_grant(core.slots[ticket.index].frame);
            core.received(0, frame, 15).unwrap();
        })
    }

    pub(crate) fn test_read<B: Writable>(&self, ticket: &Ticket<Read<B>>, bytes: &[u8]) {
        tests::with(&self.transport, |core| {
            core.slots[ticket.index]
                .fill
                .as_mut()
                .unwrap()
                .as_mut_slice()[..bytes.len()]
                .copy_from_slice(bytes);
        });
        tests::complete(&self.transport, ticket.index);
        tests::complete(&self.transport, ticket.index);
    }
}

impl Rail {
    pub(crate) fn test_candidate(device: &str, port: u8, gid: u8) -> Self {
        let mut raw = unsafe { std::mem::zeroed::<ffi::Rail>() };
        raw.port = port;
        raw.gid_index = gid;
        raw.windows = 1;
        raw.max_read = 16;
        Self {
            raw,
            name: device.into(),
            numa_node: None,
        }
    }
}

mod policy {
    use super::*;
    use std::env;

    fn policy(settings: &[(&str, &str)]) -> io::Result<StartupPolicy> {
        StartupPolicy::parse(|name| {
            settings
                .iter()
                .find(|s| s.0 == name)
                .map(|s| s.1.to_owned())
                .ok_or(env::VarError::NotPresent)
        })
    }

    #[test]
    fn readiness24_disabled_never_discovers_and_config_is_strict() {
        let disabled = policy(&[]).unwrap();
        assert!(
            disabled
                .catalog_with(|| panic!("HTTP profile discovered RNICs"))
                .is_empty()
        );
        assert_eq!(disabled.validate_workers(4096).unwrap(), 0);
        for settings in [
            vec![("RACER_RDMA_MODE", "auto")],
            vec![("RACER_RDMA_MODE", "enabled")],
            vec![("RACER_RDMA_RAILS", "mlx5_0:1:0")],
            vec![("RACER_RDMA_DEPTH", "0")],
        ] {
            assert!(policy(&settings).is_err());
        }
        for rails in [
            "",
            "mlx5_0",
            "mlx5_0:0:0",
            "mlx5_0:1:256",
            "mlx5_0:1:0,mlx5_0:01:00",
            "mlx5_0:1:0,",
            "../dev:1:0",
        ] {
            assert!(
                policy(&[("RACER_RDMA_MODE", "enabled"), ("RACER_RDMA_RAILS", rails)]).is_err(),
                "{rails}"
            );
        }
        for (name, value) in [
            ("RACER_RDMA_CONNECTIONS", "33"),
            ("RACER_RDMA_DEPTH", "17"),
            ("RACER_RDMA_DEPTH", "0"),
        ] {
            assert!(
                policy(&[
                    ("RACER_RDMA_MODE", "enabled"),
                    ("RACER_RDMA_RAILS", "mlx5_0:1:0"),
                    (name, value)
                ])
                .is_err()
            );
        }
    }

    #[test]
    fn readiness24_multiport_gid_selection_canonical_holes_and_discovery_failure() {
        let policy = policy(&[
            ("RACER_RDMA_MODE", "enabled"),
            ("RACER_RDMA_RAILS", "z:2:7,a:1:3,z:1:1"),
        ])
        .unwrap();
        let candidates = vec![
            Rail::test_candidate("z", 1, 0),
            Rail::test_candidate("z", 2, 7),
            Rail::test_candidate("a", 1, 2),
            Rail::test_candidate("a", 1, 3),
            Rail::test_candidate("z", 2, 1),
        ];
        for reverse in [false, true] {
            let mut candidates = candidates.clone();
            if reverse {
                candidates.reverse();
            }
            let selected = policy.catalog_with(|| Ok(candidates));
            assert_eq!(selected.len(), 3);
            let a = selected[0].as_ref().unwrap();
            assert_eq!((&*a.name, a.raw.port, a.raw.gid_index), ("a", 1, 3));
            assert!(selected[1].is_none());
            let z = selected[2].as_ref().unwrap();
            assert_eq!((&*z.name, z.raw.port, z.raw.gid_index), ("z", 2, 7));
        }
        let missing = policy.catalog_with(|| Err(io::Error::other("device unavailable")));
        assert_eq!(missing.len(), 3);
        assert!(missing.iter().all(Option::is_none));
    }

    #[test]
    fn readiness24_bounded_process_provisioning_and_auth_gate() {
        let policy = policy(&[
            ("RACER_RDMA_MODE", "enabled"),
            ("RACER_RDMA_RAILS", "a:1:0"),
        ])
        .unwrap();
        assert_eq!(policy.validate_workers(1).unwrap(), 64);
        assert_eq!(policy.control_bytes(1).unwrap(), 256 * 1024);
        assert_eq!(policy.validate_workers(1024).unwrap(), 65536);
        assert!(policy.validate_workers(1025).is_err());
        assert!(policy.validate_workers(usize::MAX).is_err());
        for (fabric, auth) in [(false, false), (true, false), (false, true), (true, true)] {
            assert_eq!(StartupPolicy::eligible(fabric, auth), fabric && auth);
        }
    }
}

pub(crate) struct TestObservation {
    pub windows: Vec<(u16, u32, bool)>,
    pub sends: Vec<(u64, Vec<u8>)>,
    pub qps: usize,
    pub wrs: usize,
    pub allocated: usize,
    pub freed: usize,
    pub pending: Vec<bool>,
}
impl TestObservation {
    fn retained_sends(&self, after: &Self) {
        for (id, bytes) in &self.sends {
            if let Some((_, current)) = after.sends.iter().find(|(wr, _)| wr == id) {
                assert_eq!(bytes, current, "NIC-owned SEND mutated before retirement");
            }
        }
    }
}

pub(crate) mod rdma_ownership_corpus {
    use super::*;
    use crate::crypto::auth;
    type Event = (TestQp, TestPost);
    type Reading = Ticket<rdma::Read>;
    fn assert_shutdown(t: &Transport) {
        t.shutdown().unwrap();
        assert_eq!(t.test_invariants(), (0, 0, 0));
    }
    fn fill(pool: &WorkerPool, value: [u8; 32]) -> Fill {
        pool.stage(Key::new(value)).unwrap()
    }
    fn checked(mut fill: Fill, len: usize) -> Buffer {
        let crc = crate::allocator::crc64(&fill.as_mut_slice()[..len]);
        fill.publish_checked(len, crc).unwrap()
    }
    fn negative() -> PeerFailure {
        PeerFailure {
            identity: [7; 32],
            candidate: 3,
            reason: crate::http_client::attempt::PeerReason::OwnerUnavailable,
            evidence: None,
        }
    }
    fn failure_reply(c: &Connection) {
        c.respond_error(c.next_request().unwrap().unwrap(), negative())
            .unwrap();
    }
    fn assert_request(r: &Request, len: usize) {
        assert_eq!(r.value, [9; 32]);
        assert_eq!(r.len, len);
        assert_eq!(r.metadata, b"exact descriptor");
    }
    fn assert_blocked(pool: &WorkerPool, key: u8) {
        assert!(pool.stage(Key::new([key; 32])).is_err());
    }
    #[test]
    fn authenticated_controls_reject_malformed_negative_tamper_replay_and_downgrade() {
        // Negative semantic attacks are signed by the genuine session. The
        // remaining cases corrupt each control kind after signing or replay it.
        for kind in 1..=4 {
            for attack in 0..25 {
                if (kind == 1 && attack < 8)
                    || (kind == 2 && matches!(attack, 5..=7))
                    || (kind != 4 && attack == 25)
                {
                    continue;
                }
                let p = Pair::new(1);
                let mut ticket = p.request(4);
                let read = p.control_stage((kind, attack), &mut ticket);
                let (sender, _) = p.side(kind % 2 == 0);
                let (receiver, connection) = p.side(kind % 2 != 0);
                let mut wire = sender.test_observe().sends[0].1.clone();
                if (8..21).contains(&attack) {
                    let offsets = [0, 8, 16, 24, 40, 48, 80, 84, 92, 100, 104, 128];
                    let offset = offsets.get(attack - 8).copied().unwrap_or(wire.len() - 1);
                    wire[offset] ^= 1;
                }
                if attack == 22 {
                    wire = wire[16..wire.len() - 96].to_vec();
                }
                if attack == 23 || attack == 24 {
                    let foreign = Pair::new(1);
                    let (t, c) = if attack == 23 {
                        (&foreign.b, &foreign.bc)
                    } else {
                        p.side(kind % 2 == 0)
                    };
                    t.test_inject(&wire).unwrap();
                    assert!(!c.is_healthy());
                    assert!(connection.is_healthy());
                    continue;
                }
                let before = sender.test_observe();
                receiver.test_faults(0, true, false, false);
                if attack == 21 {
                    assert!(receiver.test_inject(&wire).is_ok());
                }
                assert!(
                    receiver.test_inject(&wire).is_err(),
                    "kind={kind} attack={attack}"
                );
                assert!(!connection.is_healthy());
                before.retained_sends(&sender.test_observe());
                if kind == 3 {
                    assert_eq!(receiver.test_invariants().2, 1);
                }
                receiver.test_faults(0, false, false, false);
                receiver.test_progress(32).unwrap();
                if kind == 1 {
                    assert!(p.bc.next_request().is_err());
                }
                if kind == 2 || kind == 4 {
                    assert!(p.ac.take_reply(&mut ticket).is_err());
                }
                drop(read);
            }
        }
    }
    #[test]
    fn dropped_read_releases_storage_after_shutdown_quiescence() {
        let p = Pair::new(1);
        let ticket = p.read();
        assert_blocked(&p.ap, 9);
        drop(p.ac);
        drop(ticket);
        p.a.shutdown().unwrap();
        drop(fill(&p.ap, [8; 32]));
    }
    #[test]
    fn terminal_send_and_advertised_owner_corpus() {
        for kind in [2, 4] {
            for early in [false, true] {
                let p = Pair::new(1);
                let ticket = p.request(4);
                let _send = p.effect(false, 1);
                if kind == 4 {
                    failure_reply(&p.bc);
                } else {
                    p.respond(4);
                    p.pump(true, 4);
                }
                if early {
                    let _ = p.effect(true, 1);
                }
                if kind == 2 && early {
                    p.b.test_faults(0, true, false, false);
                    drop(p.bc);
                }
                for t in [&p.a, &p.b] {
                    t.test_faults(0, true, false, false);
                    t.test_expire();
                    let owned = usize::from(kind == 2 && std::ptr::eq(t, &p.b));
                    Pair::quiesce_failure(t, owned);
                }
                drop(ticket);
            }
        }
        let p = Pair::new(1);
        let read = p.read();
        p.pump(false, 3);
        p.b.test_faults(5, true, false, false);
        let (q, peer) = p.qps();
        let post = q.posts()[0];
        assert!(q.effect(&peer, post, false).unwrap());
        assert!(peer.complete(peer.receives()[0], 0).unwrap());
        Pair::quiesce_failure(&p.b, 1);
        drop(read);
    }
    fn pending(pool: &WorkerPool) -> (Transport, rdma::Connecting) {
        let t = test_transport(pool);
        let q = t.prepare([7; 16], 0, 1).unwrap();
        (t, q)
    }
    fn retired(t: &Transport) {
        let o = t.test_observe();
        assert_eq!((o.qps, o.wrs), (0, 0));
    }
    fn session_pair(a: &Offer, b: &Offer) -> (auth::Session, auth::Session, crypto::Snapshot) {
        let snapshot = crypto::tests::trust(7).1;
        let peers = auth::PeerContext::new([1; 32], [2; 32]).unwrap();
        let duration = Duration::from_secs(30);
        let (i, hello) =
            auth::Initiator::start(snapshot.clone(), peers.clone(), Some(a), duration).unwrap();
        let (r, reply) =
            auth::Responder::accept(snapshot.clone(), peers, hello, Some(b), duration).unwrap();
        let (i, finish) = i.finish(reply).unwrap();
        (i, r.finish(finish).unwrap(), snapshot)
    }
    #[test]
    fn renewal_and_negative_reuse_corpus() {
        let mut p = Pair::new(1);
        let mut previous = None;
        for epoch in 0..3 {
            let mut keys = std::collections::HashSet::new();
            let mut slot = None;
            for bind in 1..=255 {
                let grant = p.grant(4);
                let observed = p.b.test_observe();
                let i = observed.windows.iter().position(|w| w.0 != 0).unwrap();
                let window = observed.windows[i];
                assert_eq!(*slot.get_or_insert(i), i);
                assert_eq!(window.0, bind);
                assert!(window.2 && keys.insert(window.1), "no live key wrap");
                p.read_all(p.ac.read(grant, fill(&p.ap, [9; 32])).unwrap());
                if bind < 255 {
                    assert!(p.ac.is_healthy() && p.bc.is_healthy());
                }
            }
            assert!(p.bc.needs_http_recovery(), "epoch {epoch}");
            assert_ne!(
                p.bc.request([9; 32], 4, b"").err().unwrap().kind(),
                io::ErrorKind::WouldBlock
            );
            if let Some(old) = previous.replace(keys.clone()) {
                assert_eq!(old, keys);
            }
            p.ac.disconnect().unwrap();
            p.bc.disconnect().unwrap();
            (p.ac, p.bc) = Pair::connect(&p.a, &p.b);
            assert_eq!(p.a.test_invariants(), (8, 6, 0));
            assert_eq!(p.b.test_invariants(), (8, 6, 0));
            for t in [&p.a, &p.b] {
                let o = t.test_observe();
                assert_eq!(o.allocated, 8 * (epoch + 2));
                assert!(o.windows.iter().all(|w| w.0 == 0 && w.2));
            }
        }
        for disposition in 0..4 {
            let mut ticket = Some(p.request(4));
            let send = p.effect(false, 1);
            failure_reply(&p.bc);
            let reply_send = p.effect(true, 1);
            let uses = p.b.test_observe().windows.iter().map(|w| w.0).sum::<u16>();
            assert!(p.ac.take_reply(ticket.as_mut().unwrap()).unwrap().is_none());
            match disposition {
                1 => drop(ticket.take()),
                2 => p.ac.cancel(ticket.as_ref().unwrap()).unwrap(),
                _ => (),
            }
            p.progress();
            assert!(!p.a.test_observe().sends.is_empty());
            p.finish(send);
            p.finish(reply_send);
            assert_eq!(
                p.b.test_observe().windows.iter().map(|w| w.0).sum::<u16>(),
                uses
            );
            if disposition == 0 {
                let Some(GrantReply::Failure(actual)) =
                    p.ac.take_reply(ticket.as_mut().unwrap()).unwrap()
                else {
                    panic!()
                };
                assert_eq!(actual, negative());
            } else {
                if disposition == 3 {
                    p.a.test_expire();
                }
                p.progress();
            }
            assert!(p.ac.is_healthy() && p.bc.is_healthy());
            assert_eq!(p.a.test_invariants().2, 0);
            p.read_all(p.read());
        }
    }
    #[test]
    fn capacity_defaults_pending_drop_and_authentication_corpus() {
        let d = Config::default();
        assert_eq!((d.connections, d.depth), (32, 16));
        for (qps, depth, valid) in [
            (0, 1, false),
            (4097, 1, false),
            (1, 0, false),
            (1, 129, false),
            (4096, 128, true),
        ] {
            let c = Config {
                connections: qps,
                depth,
                ..Config::default()
            };
            assert_eq!(c.validate().is_ok(), valid);
        }
        let pool = buffers::io_test_pool(1);
        let a = test_transport_config(&pool, 32, 16);
        assert_eq!(a.test_invariants(), (2048, 2048, 0));
        for _ in 0..300 {
            let pending: Vec<_> = (0..32).map(|_| a.prepare([7; 16], 0, 1).unwrap()).collect();
            assert!(a.prepare([7; 16], 0, 1).is_err());
            drop(pending);
            a.test_progress(32).unwrap();
            assert_eq!(a.test_invariants(), (2048, 2048, 0));
        }
    }
    #[test]
    fn pending_connect_fabric_and_reentrant_owner_corpus() {
        let pool = buffers::io_test_pool(1);
        for case in 0..15 {
            let (a, aa) = pending(&pool);
            let b = test_transport(&pool);
            let challenge = [if case == 0 { 8 } else { 7 }; 16];
            let bb = b.prepare(challenge, 0, 1).unwrap();
            let (mut sa, mut sb, snapshot) = session_pair(aa.offer(), bb.offer());
            if case >= 10 {
                let (mut other, _, _) = session_pair(aa.offer(), bb.offer());
                other.take_offer(&snapshot).unwrap();
                sb.take_offer(&snapshot).unwrap();
                let connected = aa
                    .connect(sa.take_offer(&snapshot).unwrap().unwrap(), 0)
                    .unwrap();
                match case {
                    12 => connected.cancel().unwrap(),
                    13 => a.shutdown().unwrap(),
                    14 => a.test_inject(b"bad").unwrap(),
                    _ => (),
                }
                let session = match case {
                    10 => other,
                    11 => sb,
                    _ => sa,
                };
                assert!(connected.authenticate_session(session, snapshot).is_err());
                if case != 13 {
                    a.test_progress(32).unwrap();
                    retired(&a);
                }
                continue;
            }
            if case < 4 {
                match case {
                    1 => a.test_connect_error(),
                    2 => a.test_expire(),
                    3 => aa.cancel().unwrap(),
                    _ => (),
                }
                assert!(aa.connect_authenticated(sa, snapshot, 0).is_err());
                assert_eq!(a.test_observe().qps, 0);
            } else {
                match case {
                    4 => {
                        std::mem::forget(aa);
                        a.test_expire();
                        a.test_progress(32).unwrap();
                        retired(&a);
                    }
                    5 => {
                        aa.cancel().unwrap();
                        aa.cancel().unwrap();
                        let replacement = a.prepare([8; 16], 0, 1).unwrap();
                        drop(aa);
                        assert_eq!(a.test_observe().qps, 1);
                        replacement.close().unwrap();
                    }
                    _ => {
                        let fail = case % 2 == 1;
                        a.test_faults(0, fail, false, false);
                        if case < 8 {
                            a.test_borrow(|| {
                                blocked(aa.cancel());
                                drop(aa);
                            });
                        } else {
                            let connected = aa
                                .connect(sa.take_offer(&snapshot).unwrap().unwrap(), 0)
                                .unwrap();
                            a.test_borrow(|| drop(connected));
                        }
                        assert_eq!(a.test_observe().wrs, 8);
                        assert_eq!(a.test_progress(32).is_err(), fail);
                        if fail {
                            assert!(a.shutdown().is_err());
                            assert!(a.prepare([8; 16], 0, 1).is_err());
                        }
                        a.test_faults(0, false, false, false);
                        if !fail {
                            a.test_progress(32).unwrap();
                            retired(&a);
                        }
                    }
                }
            }
            assert_shutdown(&a);
        }
        let p = Pair::new(1);
        let ac = Rc::new(p.ac);
        drop(ac.clone());
        assert!(ac.is_healthy());
        p.a.test_borrow(|| {
            assert!(!ac.is_healthy());
            blocked(ac.disconnect());
        });
        ac.disconnect().unwrap();
        ac.disconnect().unwrap();
        assert!(ac.request([9; 32], 4, b"").is_err());
        p.a.shutdown().unwrap();
        ac.disconnect().unwrap();
        p.bc.close().unwrap();
        let a = test_transport_config(&pool, 2, 4);
        let old = a.prepare_for_fabric("old", [7; 16], 0, 1).unwrap();
        let new = a.prepare_for_fabric("new", [7; 16], 0, 1).unwrap();
        assert_eq!((old.offer().fabric(), new.offer().fabric()), ("old", "new"));
        let (sa, _, snapshot) = session_pair(old.offer(), new.offer());
        assert!(old.connect_authenticated(sa, snapshot, 0).is_err());
        drop(new);
        assert_eq!(a.prepare([7; 16], 0, 1).unwrap().offer().fabric(), "test");
        for fabric in ["".to_string(), "x".repeat(65536)] {
            assert!(a.prepare_for_fabric(&fabric, [7; 16], 0, 1).is_err());
        }
        a.test_reset_fabric();
        assert!(a.prepare([7; 16], 0, 1).is_err());
        assert!(a.prepare_for_fabric("live", [7; 16], 0, 1).is_ok());
    }
    fn blocked(r: io::Result<()>) {
        assert_eq!(r.unwrap_err().kind(), io::ErrorKind::WouldBlock);
    }
    /// Shared real-buffer fixture and ownership oracle for every WR stage.
    pub(crate) struct Pair {
        ap: WorkerPool,
        bp: WorkerPool,
        a: Transport,
        b: Transport,
        ac: Connection,
        pub(crate) bc: Connection,
    }
    impl Pair {
        fn side(&self, reverse: bool) -> (&Transport, &Connection) {
            if reverse {
                (&self.b, &self.bc)
            } else {
                (&self.a, &self.ac)
            }
        }
        fn edit(&self, kind: u8, attack: usize) {
            let (t, _) = self.side(kind != 3);
            t.test_edit_control(move |b| {
                let offsets = if kind == 4 {
                    [112, 31, 32, 67, 8, 180, 0, 83]
                } else {
                    [32, 31, 32, 67, 8, 87, 75, 83]
                };
                if kind == 4 && attack == 6 {
                    b.pop();
                    let n = (b.len() - 112) as u16;
                    b[88..90].copy_from_slice(&n.to_be_bytes());
                } else if kind == 4 && attack == 5 {
                    b[180] = 255;
                } else {
                    b[offsets[attack]] ^= 1;
                }
            });
        }
        fn control_stage(&self, case: (u8, usize), ticket: &mut Ticket<Grant>) -> Option<Reading> {
            let (kind, attack) = case;
            if kind == 1 {
                return None;
            }
            let send = self.effect(false, 1);
            if kind == 3 {
                self.finish(send);
            }
            if attack < 8 {
                self.edit(kind, attack);
            }
            if kind == 4 {
                failure_reply(&self.bc);
                return None;
            }
            self.respond(4);
            self.pump(true, 4);
            if kind == 2 {
                return None;
            }
            self.pump(true, 1);
            let grant = self.ac.take_grant(ticket).unwrap().unwrap();
            let read = self.ac.read(grant, self.fill()).unwrap();
            self.pump(false, 3);
            Some(read)
        }
        fn quiesce_failure(t: &Transport, owned: usize) {
            let before = t.test_observe();
            assert!(t.test_progress(32).is_err());
            assert!(t.test_observe().qps > 0);
            before.retained_sends(&t.test_observe());
            assert_eq!(t.test_invariants().2, owned);
            assert!(t.shutdown().is_err());
            t.test_faults(0, false, false, false);
            assert_shutdown(t);
        }
        pub(crate) fn new(depth: usize) -> Self {
            let ap = buffers::io_test_pool(1);
            let bp = buffers::io_test_pool(1);
            let a = test_transport_config(&ap, 2, depth);
            let b = test_transport_config(&bp, 2, depth);
            Self::from_transports(ap, bp, a, b)
        }
        fn from_transports(ap: WorkerPool, bp: WorkerPool, a: Transport, b: Transport) -> Self {
            let (ac, bc) = Self::connect(&a, &b);
            Self {
                ap,
                bp,
                a,
                b,
                ac,
                bc,
            }
        }
        fn connect(a: &Transport, b: &Transport) -> (Connection, Connection) {
            let aa = a.prepare([7; 16], 0, 1).unwrap();
            let bb = b.prepare([7; 16], 0, 1).unwrap();
            let (mut sa, mut sb, snapshot) = session_pair(aa.offer(), bb.offer());
            let ready = sb
                .sign(&snapshot, auth::Control::new(0, b"Ready".to_vec()).unwrap())
                .unwrap();
            sa.verify(&snapshot, ready).unwrap();
            (
                aa.connect_authenticated(sa, snapshot.clone(), 0).unwrap(),
                bb.connect_authenticated(sb, snapshot, 0).unwrap(),
            )
        }
        fn qps(&self) -> (TestQp, TestQp) {
            (self.ac.test_endpoint().1, self.bc.test_endpoint().1)
        }
        fn pump(&self, reverse: bool, opcode: u32) {
            self.finish(self.effect(reverse, opcode));
        }
        fn read_all(&self, mut read: Reading) {
            for op in [3, 1] {
                assert!(self.ac.take_read(&mut read).unwrap().is_none());
                self.pump(false, op);
            }
            let b = self.ac.take_read(&mut read).unwrap().unwrap();
            assert!(b.as_slice().iter().all(|b| *b == 42));
            assert!(self.ac.take_read(&mut read).is_err());
            drop(b);
            self.pump(true, 5);
        }
        fn abandon<B: buffers::Writable>(&self, ticket: Ticket<rdma::Read<B>>, disposition: usize) {
            match disposition {
                0 | 4 => drop(ticket),
                1 | 7 => {
                    std::mem::forget(ticket);
                    self.a.test_expire();
                }
                _ => {
                    match disposition {
                        2 => assert!(self.ac.cancel(&ticket).is_err()),
                        3 | 6 => drop(self.a.test_source()),
                        _ => assert!(self.ac.disconnect().is_err()),
                    }
                    drop(ticket);
                }
            }
        }
        fn progress(&self) {
            for t in [&self.a, &self.b] {
                t.test_progress(32).unwrap();
                t.test_invariants();
            }
        }
        fn effect(&self, reverse: bool, opcode: u32) -> Event {
            let before = [self.a.test_observe(), self.b.test_observe()];
            let (a, b) = self.qps();
            let (a, b) = if reverse { (b, a) } else { (a, b) };
            let post = a.posts().into_iter().find(|p| p.opcode == opcode).unwrap();
            assert!(!a.complete(post, 0).unwrap(), "success requires an effect");
            assert!(!b.effect(&a, post, false).unwrap(), "foreign QP token");
            assert!(a.effect(&b, post, false).unwrap());
            assert!(!a.effect(&b, post, true).unwrap(), "no duplicate writes");
            for receive in b.receives() {
                assert!(b.complete(receive, 0).unwrap());
            }
            self.progress();
            for (old, t) in before.iter().zip([&self.a, &self.b]) {
                old.retained_sends(&t.test_observe());
            }
            (a, post)
        }
        fn finish(&self, event: Event) {
            assert!(event.0.complete(event.1, 0).unwrap());
            assert!(!event.0.complete(event.1, 0).unwrap());
            self.progress();
            let (t, _) = self.side(!event.0.belongs_to(&self.a));
            t.test_stale_cqe(event.1.id);
        }
        fn receive(&self) -> Request {
            self.bc.next_request().unwrap().unwrap()
        }
        fn respond(&self, len: usize) {
            self.bc.respond(self.receive(), self.source(len)).unwrap();
        }
        fn request(&self, len: usize) -> Ticket<Grant> {
            self.ac.request([9; 32], len, b"exact descriptor").unwrap()
        }
        fn source(&self, len: usize) -> Buffer {
            let mut f = fill(&self.bp, [9; 32]);
            f.as_mut_slice()[..len].fill(42);
            checked(f, len)
        }
        fn read(&self) -> Reading {
            self.ac.read(self.grant(4), self.fill()).unwrap()
        }
        fn fill(&self) -> Fill {
            fill(&self.ap, [9; 32])
        }
        fn grant(&self, len: usize) -> RemoteGrant {
            let (grant, send) = self.early_grant(len);
            self.finish(send);
            grant
        }
        fn early_grant(&self, len: usize) -> (RemoteGrant, Event) {
            let mut ticket = self.request(len);
            assert!(self.ac.request([8; 32], len, b"depth").is_err());
            let send = self.effect(false, 1);
            let request = self.receive();
            assert_request(&request, len);
            let buffer = self.source(len);
            let crc = buffer.checksum();
            self.bc.respond(request, buffer).unwrap();
            assert!(self.ac.take_grant(&mut ticket).unwrap().is_none());
            self.pump(true, 4);
            let reply = self.effect(true, 1);
            assert!(self.ac.take_grant(&mut ticket).unwrap().is_none());
            self.finish(send);
            let grant = self.ac.take_grant(&mut ticket).unwrap().unwrap();
            assert_eq!(grant.checksum(), crc);
            (grant, reply)
        }
    }
    #[test]
    fn readiness25_delayed_destroy_retains_both_dma_owners_and_bounds_admission() {
        use crate::simulation::World;
        for shutdown in [false, true] {
            let world = World::new(2502);
            let _scope = world.enter();
            let p = Pair::new(1);
            let mut read = p.read();
            let event = p.effect(false, 3); // DMA happened; CQE is delayed.
            let before = [p.a.test_observe(), p.b.test_observe()];
            for t in [&p.a, &p.b] {
                t.test_block_destroy(true);
            }
            p.ac.disconnect().unwrap();
            p.bc.disconnect().unwrap();
            assert!(p.ac.take_read(&mut read).is_err());
            drop(read);
            assert!(event.0.complete(event.1, 0).unwrap());
            for _ in 0..100 {
                world.advance(Duration::from_secs(1));
                for (n, t) in [&p.a, &p.b].into_iter().enumerate() {
                    if shutdown {
                        assert_eq!(t.shutdown().unwrap_err().kind(), io::ErrorKind::WouldBlock);
                    } else {
                        let work = t.test_progress(16).unwrap();
                        assert!(!work.runnable, "pending destruction must not spin");
                        assert!(work.deadline.unwrap() > world.now());
                    }
                    assert_eq!(t.test_invariants().2, 1);
                    assert_eq!(t.test_observe().windows, before[n].windows);
                    assert_eq!(t.test_observe().qps, 1);
                }
                assert_blocked(&p.ap, 9);
                // The advertised immutable source may be read locally, but its
                // only pool slot remains pinned against replacement/reuse.
                assert!(p.bp.stage(Key::new([8; 32])).is_err());
            }
            // With a full fixed QP table, repeated pending-handshake cancellation
            // cannot create an unbounded retired-session/helper list.
            if !shutdown {
                let extra = p.a.prepare([8; 16], 0, 1).unwrap();
                drop(extra);
                for _ in 0..100 {
                    assert!(p.a.prepare([8; 16], 0, 1).is_err());
                }
                assert_eq!(p.a.test_observe().qps, 2);
            }
            for t in [&p.a, &p.b] {
                t.test_block_destroy(false);
                world.advance(Duration::from_millis(100));
                t.shutdown().unwrap();
                assert_eq!(t.test_invariants(), (0, 0, 0));
            }
            drop(fill(&p.ap, [9; 32]));
            drop(fill(&p.bp, [9; 32]));
            drop(p);
            world.assert_clean();
        }
    }

    #[test]
    fn legal_events_early_reply_read_ack_and_publication_matrix() {
        for len in [4, BUFFER_SIZE] {
            let p = Pair::new(1);
            let (grant, grant_send) = p.early_grant(len);
            let (authority, destination) = fill(&p.ap, [9; 32]).split_destination();
            let mut read = p.ac.read(grant, destination).unwrap();
            let effect = p.effect(false, 3);
            assert!(p.ac.take_read(&mut read).unwrap().is_none());
            assert_blocked(&p.ap, 9);
            p.finish(effect);
            let ack = p.effect(false, 1);
            assert_eq!(p.b.test_invariants().2, 1, "early ACK retains owner");
            p.finish(grant_send);
            let invalidate = p.effect(true, 5);
            assert_eq!(p.b.test_invariants().2, 1, "effect retains owner");
            p.finish(invalidate);
            assert_eq!(p.b.test_invariants().2, 0);
            p.finish(ack);
            let (destination, n) = p.ac.take_read_unpublished(&mut read).unwrap().unwrap();
            let mut f = authority.reunite(destination).ok().unwrap();
            assert_eq!(n, len);
            assert!(f.as_mut_slice()[..n].iter().all(|b| *b == 42));
            assert_blocked(&p.ap, 9);
            drop(f);
            drop(fill(&p.ap, [9; 32]));
            assert!(p.ac.take_read(&mut read).is_err());
            assert!(p.ac.is_healthy() && p.bc.is_healthy());
            p.read_all(p.ac.read(p.grant(len), fill(&p.ap, [9; 32])).unwrap());
        }
    }
    #[test]
    fn retention_cancellation_and_stale_event_matrix() {
        // Before/after READ effect, dropped/forgotten/cancelled ticket, failed
        // destroy and source shutdown all share the same DMA ownership oracle.
        fn run<B: buffers::Writable>(
            p: &Pair,
            ticket: Ticket<rdma::Read<B>>,
            mode: usize,
            effected: bool,
        ) {
            let (qp, peer) = p.qps();
            let post = qp.posts()[0];
            if effected {
                assert!(qp.effect(&peer, post, false).unwrap());
            }
            p.a.test_faults(0, mode < 4 || mode == 5, false, false);
            p.abandon(ticket, mode);
            if mode < 4 || mode == 5 {
                assert!(p.ap.stage(Key::new([8; 32])).is_err());
                Pair::quiesce_failure(&p.a, 1);
            } else {
                if mode != 6 {
                    p.a.test_progress(32).unwrap();
                }
                assert_eq!(p.a.test_invariants().2, 0);
                assert_shutdown(&p.a);
            }
            assert!(!qp.effect(&peer, post, true).unwrap());
            assert!(!qp.complete(post, 0).unwrap());
            drop(fill(&p.ap, [9; 32]));
        }
        for mode in 0..8 {
            for effected in [false, true] {
                for restricted in [false, true] {
                    let p = Pair::new(1);
                    let grant = p.grant(4);
                    let f = fill(&p.ap, [9; 32]);
                    if restricted {
                        let (a, d) = f.split_destination();
                        drop(a);
                        run(&p, p.ac.read(grant, d).unwrap(), mode, effected);
                    } else {
                        run(&p, p.ac.read(grant, f).unwrap(), mode, effected);
                    }
                }
            }
        }
    }
    #[test]
    fn read_rejection_crc_failure_and_error_cqe_matrix() {
        fn rejected<B: buffers::Writable>(p: &Pair, grant: RemoteGrant, mut f: B) -> B {
            f.as_mut_slice()[..4].copy_from_slice(b"keep");
            let mut f = p.ac.read(grant, f).err().unwrap().resource;
            assert_eq!(&f.as_mut_slice()[..4], b"keep");
            assert!(p.qps().0.posts().is_empty());
            f
        }
        for fault in 0..8 {
            let p = Pair::new(1);
            if fault == 7 {
                let ticket = p.request(4);
                p.pump(false, 1);
                let wrong = checked(fill(&p.bp, [8; 32]), 4);
                assert!(p.bc.respond(p.receive(), wrong).is_err());
                assert!(p.qps().1.posts().is_empty());
                drop(ticket);
                continue;
            }
            let grant = p.grant(4);
            let mut f = fill(&p.ap, [if fault == 4 { 8 } else { 9 }; 32]);
            let other = buffers::io_test_pool(1);
            if fault == 5 {
                drop(f);
                f = fill(&other, [9; 32]);
            }
            if matches!(fault, 0 | 4 | 5 | 6) {
                if matches!(fault, 0 | 6) {
                    p.a.test_faults(3, false, false, false);
                }
                if fault != 6 {
                    drop(rejected(&p, grant, f));
                    continue;
                }
                let (authority, destination) = f.split_destination();
                let d = rejected(&p, grant, destination);
                drop(authority.reunite(d).ok().unwrap());
                continue;
            }
            let mut ticket = p.ac.read(grant, f).unwrap();
            let (q, peer) = p.qps();
            let post = q.posts()[0];
            assert!(q.effect(&peer, post, fault == 1).unwrap());
            if fault == 3 {
                p.a.test_expire();
            }
            assert!(q.complete(post, u32::from(fault == 2)).unwrap());
            p.progress();
            if fault == 1 {
                p.finish(p.effect(false, 1));
                p.finish(p.effect(true, 5));
            }
            let error = p.ac.take_read(&mut ticket).err().unwrap();
            if fault == 3 {
                assert_eq!(error.kind(), io::ErrorKind::TimedOut);
            }
            assert!(!q.complete(post, 0).unwrap());
            drop(fill(&p.ap, [9; 32]));
            assert!(
                q.posts().iter().all(|p| p.kind != 3),
                "no ACK after failure"
            );
        }
    }
    #[test]
    fn source_arm_registry_pairing_and_ordered_cqe_corpus() {
        use crate::workers::Driver as _;
        struct Idle;
        impl uring::Application for Idle {
            fn poll(&mut self, _: &mut uring::Ring, _: usize) -> io::Result<uring::Work> {
                Ok(uring::Work::default())
            }
            fn shutdown(&mut self, _: &mut uring::Ring) -> io::Result<()> {
                Ok(())
            }
        }
        let world = crate::simulation::World::new(811);
        let _scope = world.enter();
        world.enable_scheduler();
        let p = Pair::new(2);
        let extra = p.a.prepare([8; 16], 0, 1).unwrap();
        let (local, peer) = p.qps();
        assert_eq!(test_qps().iter().filter(|q| q.pairs_with(&peer)).count(), 1);
        let ring = uring::Ring::http_test_ring(p.bp.clone(), uring::Config::default()).unwrap();
        let mut driver = uring::Driver::new(ring, Idle, 1).unwrap();
        driver.add_source(p.b.test_source());
        driver.turn().unwrap();
        assert!(driver.parked() && !driver.ready());
        assert!(
            p.ac.request([9; 32], 4, &vec![0; MAX_METADATA + 1])
                .is_err()
        );
        let one = p.ac.request([9; 32], 4, &vec![42; MAX_METADATA]).unwrap();
        assert_eq!(p.a.test_observe().sends[0].1.len(), 4096);
        let two = p.ac.request([8; 32], 4, b"second").unwrap();
        let posts = local.posts();
        assert!(!local.effect(&peer, posts[1], false).unwrap());
        assert!(local.effect(&peer, posts[0], false).unwrap());
        assert!(local.effect(&peer, posts[1], false).unwrap());
        assert!(!local.complete(posts[1], 0).unwrap());
        assert!(!driver.ready(), "DMA effect is not CQ readiness");
        let receives = peer.receives();
        assert!(!peer.complete(receives[1], 0).unwrap());
        assert!(peer.complete(receives[0], 0).unwrap());
        assert!(driver.ready());
        let work = p.b.test_progress(1).unwrap();
        assert!(work.runnable && work.deadline.is_some());
        driver.turn().unwrap();
        let work = p.b.test_progress(1).unwrap();
        assert!(work.deadline.is_some());
        let request = p.bc.next_request().unwrap().unwrap();
        assert_eq!(request.metadata, vec![42; MAX_METADATA]);
        assert!(p.bc.authenticated_received());
        driver.shutdown().unwrap();
        assert!(!peer.complete(receives[1], 0).unwrap());
        drop((one, two, request, extra));
    }
    #[test]
    fn renewal_faults_are_bounded_and_recover_after_quiescence() {
        for fault in 0..3 {
            let p = Pair::new(1);
            let mut read = p.read();
            let (qp, peer) = p.qps();
            let other = p.a.prepare([8; 16], 0, 1).unwrap();
            let post = qp.posts()[0];
            assert!(qp.effect(&peer, post, false).unwrap());
            p.a.test_faults(0, fault == 0, fault == 1, fault == 2);
            p.ac.test_window_renewal(fault == 2);
            let baseline = p.a.test_observe();
            for _ in 0..20 {
                p.a.test_expire();
                let work = p.a.test_progress(32).unwrap();
                assert!(!work.runnable && work.deadline.is_some());
                assert!(!p.ac.is_healthy());
                let (slots, _, owned) = p.a.test_invariants();
                assert_eq!((slots, owned), (8, usize::from(fault == 0)));
                let o = p.a.test_observe();
                assert_eq!(o.allocated, baseline.allocated);
                assert_eq!(o.freed, baseline.freed + if fault == 2 { 8 } else { 0 });
                assert_eq!(o.qps, if fault == 0 { 2 } else { 0 });
                assert!(p.a.prepare([8; 16], 0, 1).is_err());
            }
            p.a.test_faults(0, false, false, false);
            p.a.test_expire();
            p.a.test_progress(32).unwrap();
            assert_eq!(p.a.test_invariants(), (8, 8, 0));
            let replacement = p.a.prepare([8; 16], 0, 1).unwrap();
            assert!(p.ac.take_read(&mut read).is_err());
            drop(other);
            assert_eq!(p.a.test_observe().qps, 1);
            assert!(!qp.complete(post, 0).unwrap());
            p.a.test_stale_cqe(post.id);
            assert!(!qp.effect(&peer, post, true).unwrap());
            drop(replacement);
        }
    }
    #[test]
    fn negotiation_versions_and_negative_send_pressure_corpus() {
        let pool = buffers::io_test_pool(1);
        {
            let (_a, aa) = pending(&pool);
            let (_b, bb) = pending(&pool);
            let (sa, sb, snapshot) = session_pair(aa.offer(), bb.offer());
            let ac = aa.connect_authenticated(sa, snapshot.clone(), 0).unwrap();
            let bc = bb.connect_authenticated(sb, snapshot, 0).unwrap();
            assert!(ac.is_authenticated());
            assert!(bc.is_authenticated());
            let ticket = ac.request([9; 32], 4, b"descriptor").unwrap();
            ac.test_pump(&bc, false);
            assert_eq!(
                bc.respond_error(bc.next_request().unwrap().unwrap(), negative())
                    .is_ok(),
                true
            );
            drop(ticket);
        }
        for expire in [false, true] {
            let p = Pair::new(2);
            let mut tickets = [p.request(4), p.ac.request([8; 32], 4, b"two").unwrap()];
            p.finish(p.effect(false, 1));
            p.finish(p.effect(false, 1));
            p.b.test_faults(1, false, false, false);
            for _ in 0..2 {
                failure_reply(&p.bc);
            }
            assert_eq!(p.b.test_invariants().2, 0);
            p.b.test_faults(0, false, false, false);
            let mut pending = p.b.test_observe().pending;
            pending.sort();
            assert_eq!(pending, [false, true]);
            if expire {
                p.b.test_expire();
                p.progress();
                assert!(!p.bc.is_healthy());
            } else {
                p.progress();
                p.finish(p.effect(true, 1));
                p.finish(p.effect(true, 1));
                for ticket in &mut tickets {
                    assert!(matches!(
                        p.ac.take_reply(ticket).unwrap(),
                        Some(GrantReply::Failure(_))
                    ));
                }
                assert!(p.ac.is_healthy() && p.bc.is_healthy());
            }
        }
    }
    use crate::{
        buffers::{self, BUFFER_SIZE, Buffer, Fill, Key, WorkerPool},
        rdma,
    };
    /// Run explicitly with the `verbs_loopback` filter and `--ignored`.
    #[test]
    #[ignore = "requires accessible RDMA hardware with type-2B memory windows"]
    fn verbs_loopback() {
        let rail = discover().unwrap().into_iter().next().expect("no RNIC");
        loopback(rail);
    }
    /// Run as root with iproute2, rdma-core, rdma_rxe and dummy-interface support.
    #[test]
    #[ignore = "configures Soft-RoCE; requires root, iproute2 and rdma_rxe"]
    fn soft_roce_loopback() {
        fn command(line: &str) -> io::Result<()> {
            let mut words = line.split_whitespace();
            let output = std::process::Command::new(words.next().unwrap())
                .args(words)
                .output()?;
            if output.status.success() {
                Ok(())
            } else {
                Err(io::Error::other(format!("{line}: {output:?}")))
            }
        }
        struct SoftRoce(Vec<String>);
        impl SoftRoce {
            fn cleanup(&mut self) -> io::Result<()> {
                while let Some(line) = self.0.last() {
                    command(line)?;
                    self.0.pop();
                }
                Ok(())
            }
            fn add(&mut self, command_line: String, undo: String) {
                command(&command_line).unwrap();
                self.0.push(undo);
            }
        }
        impl Drop for SoftRoce {
            fn drop(&mut self) {
                if let Err(e) = self.cleanup() {
                    eprintln!("Soft-RoCE cleanup failed: {e}");
                }
            }
        }
        command("modprobe rdma_rxe").unwrap();
        let mut setup = SoftRoce(Vec::new());
        let netdev = format!("rcr{}", std::process::id());
        let device = format!("rcr_rxe{}", std::process::id());
        setup.add(
            format!("ip link add {netdev} type dummy"),
            format!("ip link delete {netdev}"),
        );
        command(&format!("ip address add 192.0.2.1/32 dev {netdev}")).unwrap();
        command(&format!("ip link set {netdev} up")).unwrap();
        setup.add(
            format!("rdma link add {device} type rxe netdev {netdev}"),
            format!("rdma link delete {device}"),
        );
        let until = Instant::now() + Duration::from_secs(5);
        let rail = loop {
            if let Some(rail) = discover().unwrap().into_iter().find(|r| r.name == device) {
                break rail;
            }
            assert!(Instant::now() < until, "{device} lacks type-2B support");
            std::thread::sleep(Duration::from_millis(20));
        };
        loopback(rail);
        setup.cleanup().unwrap();
    }
    fn loopback(rail: Rail) {
        let a_pool = buffers::io_test_pool(2);
        let b_pool = buffers::io_test_pool(2);
        let config = Config {
            fabric: "loopback".into(),
            connections: 1,
            depth: 2,
            ..Config::default()
        };
        let (a, _a_source) = Transport::new(&a_pool, rail.clone(), config.clone()).unwrap();
        let (b, _b_source) = Transport::new(&b_pool, rail, config).unwrap();
        let p = Pair::from_transports(a_pool, b_pool, a, b);
        let source = p.source(BUFFER_SIZE);
        let mut request = p.request(BUFFER_SIZE);
        let until = Instant::now() + Duration::from_secs(5);
        let (mut read, mut served) = (None, false);
        loop {
            assert!(Instant::now() < until, "loopback timed out");
            p.a.test_progress(32).unwrap();
            p.b.test_progress(32).unwrap();
            if !served && let Some(r) = p.bc.next_request().unwrap() {
                assert_request(&r, BUFFER_SIZE);
                p.bc.respond(r, source.clone()).unwrap();
                served = true;
            }
            if read.is_none()
                && let Some(g) = p.ac.take_grant(&mut request).unwrap()
            {
                assert_eq!(g.checksum(), source.checksum());
                read = Some(p.ac.read(g, fill(&p.ap, [9; 32])).unwrap());
            }
            if let Some(r) = &mut read
                && let Some((mut received, len)) = p.ac.take_read_unpublished(r).unwrap()
            {
                assert_eq!(&received.as_mut_slice()[..len], source.as_slice());
                break;
            }
        }
        p.ac.close().unwrap();
        p.bc.close().unwrap();
        p.a.shutdown().unwrap();
        p.b.shutdown().unwrap();
    }
}
