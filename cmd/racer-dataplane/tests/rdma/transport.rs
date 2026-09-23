// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn early_reply_waits_for_request_send_and_slot_generation_never_wraps() {
    let now = Instant::now();
    let mut slot = Slot::new(now);
    slot.uses = 254;
    slot.key = 123;
    assert_eq!(slot.begin(2, Phase::RequestSend, now).unwrap(), 1);
    slot.frame.request = 42;
    let reply = Frame {
        kind: 2,
        request: 42,
        key: 99,
        ..Frame::default()
    };
    slot.accept_reply(reply, Phase::GrantReady);
    assert!(slot.phase == Phase::RequestSend);
    assert_eq!(
        slot.frame.key, 0,
        "TLS still owns the request representation"
    );
    slot.request_sent();
    assert!(slot.phase == Phase::GrantReady);
    assert_eq!(slot.frame.key, 99);
    assert_eq!(slot.uses, 254, "a ticket generation cannot renew an rkey");
    assert_eq!(slot.key, 123);

    slot.generation = u64::MAX;
    assert!(slot.begin(3, Phase::Incoming, now).is_err());
    assert_eq!(slot.conn, 2);
    assert!(slot.phase == Phase::GrantReady);
    assert_eq!(slot.frame.key, 99);
}

#[test]
fn control_arena_preserves_wire_bytes_and_rejects_oversized_metadata() {
    let mut arena = ControlArena::new(1);
    let metadata = vec![0x5a; MAX_METADATA];
    let mut frame = Frame {
        kind: 1,
        request: 17,
        ..Frame::default()
    };
    let len = arena.encode(0, &mut frame, &metadata).unwrap();
    assert_eq!(len, CONTROL);
    let mut expected = vec![0; len];
    frame.encode(&mut expected);
    expected[HEADER..].copy_from_slice(&metadata);
    assert_eq!(arena.bytes(0).as_slice(), expected);
    let before = *arena.bytes(0);
    assert!(
        arena
            .encode(0, &mut frame, &vec![0; MAX_METADATA + 1])
            .is_err()
    );
    assert_eq!(*arena.bytes(0), before);
    assert_eq!(frame.metadata as usize, MAX_METADATA);
}

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
    /* Removed control opcodes reject before touching any device or QP. */
    assert(racer_post(NULL, NULL, 1, 1, NULL, 0, 0, 0, NULL) == EINVAL);
    assert(racer_post(NULL, NULL, 2, 1, NULL, 0, 0, 0, NULL) == EINVAL);
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
mod loopback_tests {
    use super::*;
    use crate::buffers::{self, BUFFER_SIZE, Buffer, Fill, Key, WorkerPool};
    fn fill(pool: &WorkerPool, value: [u8; 32]) -> Fill {
        pool.stage(Key::new(value)).unwrap()
    }
    fn checked(mut fill: Fill, len: usize) -> Buffer {
        let crc = crate::allocator::crc64(&fill.as_mut_slice()[..len]);
        fill.publish_checked(len, crc).unwrap()
    }
    fn assert_request(r: &Request, len: usize) {
        assert_eq!(r.value, [9; 32]);
        assert_eq!(r.len, len);
        assert_eq!(r.metadata, b"exact descriptor");
    }
    pub(crate) struct Pair {
        ap: WorkerPool,
        bp: WorkerPool,
        a: Transport,
        b: Transport,
        pub(crate) ac: Connection,
        pub(crate) bc: Connection,
    }
    impl Pair {
        fn source(&self, len: usize) -> Buffer {
            let mut f = fill(&self.bp, [9; 32]);
            f.as_mut_slice()[..len].fill(42);
            checked(f, len)
        }
        fn request(&self, len: usize) -> Ticket<Grant> {
            self.ac.request([9; 32], len, b"exact descriptor").unwrap()
        }
    }
    #[test]
    #[ignore = "requires RDMA hardware"]
    fn verbs_loopback() {
        let rail = discover().unwrap().into_iter().next().expect("no RNIC");
        loopback(rail);
    }
    #[test]
    #[ignore = "requires root and Soft-RoCE"]
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
        let (a, mut a_source) = Transport::new(&a_pool, rail.clone(), config.clone()).unwrap();
        let (b, mut b_source) = Transport::new(&b_pool, rail, config).unwrap();
        let mut a_ring =
            uring::Ring::http_test_ring(a_pool.clone(), uring::Config::default()).unwrap();
        let mut b_ring =
            uring::Ring::http_test_ring(b_pool.clone(), uring::Config::default()).unwrap();
        let aa = a.prepare([7; 16], 0, 1).unwrap();
        let bb = b.prepare([7; 16], 0, 1).unwrap();
        let ((oa, sa), (ob, sb)) =
            crate::negotiation::tests::tls_channels(aa.offer(), bb.offer(), &mut a_ring, false);
        let ac = aa.connect_authenticated(oa, sa, 0).unwrap();
        let bc = bb.connect_authenticated(ob, sb, 0).unwrap();
        let p = Pair {
            ap: a_pool,
            bp: b_pool,
            a,
            b,
            ac,
            bc,
        };
        let source = p.source(BUFFER_SIZE);
        let mut request = p.request(BUFFER_SIZE);
        let until = Instant::now() + Duration::from_secs(5);
        let (mut read, mut served) = (None, false);
        loop {
            assert!(Instant::now() < until, "loopback timed out");
            a_ring.progress().unwrap();
            b_ring.progress().unwrap();
            uring::CompletionSource::poll(&mut a_source, &mut a_ring, 32).unwrap();
            uring::CompletionSource::poll(&mut b_source, &mut b_ring, 32).unwrap();
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
