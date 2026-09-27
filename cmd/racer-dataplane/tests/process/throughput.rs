//! Production executable gates, deliberately separate from custom hotpath graphs.
use super::*;
use measurement::{Expected, Failure, Measurements};
use serde_json::json;
use std::{io::BufReader, sync::Barrier};

#[derive(Clone)]
pub struct Profile {
    pub pairs: usize,
    pub length: u64,
    pub caches: Vec<(String, String)>,
    pub peer: Option<SocketAddr>,
    pub key_base: u64,
}
impl Profile {
    fn new(pairs: usize, length: u64, caches: usize) -> Self {
        assert!([1, 2, 4].contains(&pairs));
        Self {
            pairs,
            length,
            peer: None,
            key_base: 0,
            caches: (0..caches)
                .map(|i| {
                    (
                        format!("44444444-4444-4444-8444-{i:012x}"),
                        format!("throughput-{i}"),
                    )
                })
                .collect(),
        }
    }
}
pub fn object_byte(key: u64, offset: u64) -> u8 {
    ((u64::from(byte(offset)) + key % 251) % 251) as u8
}
fn socket_path(process: &Process, cache: usize) -> PathBuf {
    format!(
        "/proc/self/fd/{}/client/socket",
        process.cache_directories[cache].as_raw_fd()
    )
    .into()
}
fn connect(path: &Path) -> io::Result<UnixStream> {
    let socket = UnixStream::connect(path)?;
    socket.set_read_timeout(Some(Duration::from_secs(35)))?;
    socket.set_write_timeout(Some(Duration::from_secs(35)))?;
    Ok(socket)
}
fn request(path: &Path, key: u64, range: &Expected, slow: bool) -> Result<u64, Failure> {
    let mut socket = connect(path)?;
    write!(
        socket,
        "GET /v1/objects/{key:064x} HTTP/1.1\r\nHost: racer\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\nIf-Match: \"restart-v1\"\r\nRange: bytes={}-{}\r\nConnection: close\r\n\r\n",
        range.start,
        range.end - 1
    )?;
    measurement::response(
        &mut BufReader::with_capacity(64 << 10, socket),
        range,
        |offset| object_byte(key, offset),
        if slow {
            Duration::from_millis(1)
        } else {
            Duration::ZERO
        },
    )
}

fn counters(process: &Process) -> BTreeMap<String, u64> {
    let reply = process.diagnostic("/metrics").unwrap();
    assert_eq!(reply.status, 200);
    String::from_utf8(reply.body)
        .unwrap()
        .lines()
        .filter(|line| !line.starts_with('#'))
        .map(|line| {
            let (name, value) = line.split_once(' ').unwrap();
            (name.into(), value.parse().unwrap())
        })
        .collect()
}
fn persisted(process: &mut Process, count: u64) {
    let deadline = Instant::now() + TIMEOUT;
    loop {
        process.assert_running();
        let metrics = counters(process);
        if metrics["racer_disk_publications_total"] >= count
            && metrics["racer_pending_disk_writes"] == 0
        {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "preload did not persist: {metrics:?}"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

#[derive(Default, serde::Serialize)]
struct Observation {
    rss_kib: u64,
    socket_fds: usize,
    /// Linux schedstat execution nanoseconds, not scheduler wait/queue time.
    threads: BTreeMap<String, u64>,
}
fn observe(pid: u32) -> Observation {
    let root = PathBuf::from(format!("/proc/{pid}"));
    let mut result = Observation::default();
    let status = fs::read_to_string(root.join("status")).unwrap();
    result.rss_kib = status
        .lines()
        .find_map(|line| line.strip_prefix("VmRSS:"))
        .unwrap()
        .split_whitespace()
        .next()
        .unwrap()
        .parse()
        .unwrap();
    result.socket_fds = fs::read_dir(root.join("fd"))
        .unwrap()
        .filter_map(|entry| fs::read_link(entry.ok()?.path()).ok())
        .filter(|path| path.as_os_str().as_encoded_bytes().starts_with(b"socket:"))
        .count();
    for task in fs::read_dir(root.join("task")).unwrap() {
        let task = task.unwrap();
        let stat = fs::read_to_string(task.path().join("stat")).unwrap();
        let end = stat.rfind(')').unwrap();
        let name = &stat[stat.find('(').unwrap() + 1..end];
        let schedstat = fs::read_to_string(task.path().join("schedstat")).unwrap();
        result.threads.insert(
            format!("{name}:{}", task.file_name().to_string_lossy()),
            schedstat
                .split_whitespace()
                .next()
                .unwrap()
                .parse()
                .unwrap(),
        );
    }
    result
}

fn measure(
    process: &Process,
    profile: &Profile,
    mode: &str,
    count: usize,
    concurrency: usize,
    shared: bool,
    range: &Expected,
    slow: bool,
) -> (Measurements, BTreeMap<String, u64>) {
    let before = counters(process);
    let cpu_before = observe(process.child.id());
    let paths: Vec<_> = (0..profile.caches.len())
        .map(|i| socket_path(process, i))
        .collect();
    let barrier = Barrier::new(concurrency + 1);
    let mut measured = Measurements::default();
    let start = thread::scope(|threads| {
        let handles: Vec<_> = (0..concurrency)
            .map(|client| {
                let (paths, barrier) = (&paths, &barrier);
                threads.spawn(move || {
                    barrier.wait();
                    (client..count)
                        .step_by(concurrency)
                        .map(|i| {
                            let start = Instant::now();
                            let key = profile.key_base + if shared { 0 } else { i as u64 };
                            let result = request(&paths[i % paths.len()], key, range, slow);
                            (result, start.elapsed())
                        })
                        .collect::<Vec<_>>()
                })
            })
            .collect();
        let start = Instant::now();
        barrier.wait();
        for handle in handles {
            for (result, latency) in handle.join().unwrap() {
                measured.record(result, latency);
            }
        }
        start
    });
    measured.finish(start.elapsed());
    let cpu_after = observe(process.child.id());
    let utilization: BTreeMap<_, _> = cpu_after
        .threads
        .iter()
        .map(|(name, value)| {
            (
                name.clone(),
                value.saturating_sub(*cpu_before.threads.get(name).unwrap_or(&0)) as f64
                    / 1_000_000_000.
                    / measured.elapsed_seconds,
            )
        })
        .collect();
    let after = counters(process);
    let deltas: BTreeMap<_, _> = after
        .iter()
        .filter(|(name, _)| name.ends_with("_total"))
        .map(|(name, value)| (name.clone(), value - before[name]))
        .collect();
    let report = json!({"mode": mode, "pairs": profile.pairs, "caches": profile.caches.len(), "object_bytes": profile.length, "range": [range.start, range.end], "concurrency": concurrency, "shared": shared, "slow_reader": slow, "budgets": "production-defaults", "measurement": measured, "thread_cpu_fraction": utilization, "thread_cpu_unit": "schedstat execution nanoseconds", "build": if cfg!(debug_assertions) { "test-optimized" } else { "release" }, "before": cpu_before, "after": cpu_after, "counter_delta": deltas, "unavailable": ["per-worker reservation failures", "queue residence", "reactor lag", "allocation bytes", "copy bytes", "ranking misses", "exact RSS peak", "exact connection peak"]});
    println!("THROUGHPUT {report}");
    // Persist nonsecret measurement evidence across fixture cleanup, including
    // failed gates. Append one JSON object per line under the worktree only.
    let mut output = fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(Path::new(env!("CARGO_MANIFEST_DIR")).join("target/throughput-results.jsonl"))
        .unwrap();
    writeln!(output, "{report}").unwrap();
    (measured, deltas)
}

fn case(
    pairs: usize,
    length: u64,
    caches: usize,
    mode: &str,
    shared: bool,
    concurrency: usize,
    slow: bool,
    short: bool,
) {
    run_case(
        pairs,
        length,
        caches,
        mode,
        shared,
        concurrency,
        slow,
        short,
        if shared { 8 } else { 4 },
    );
}

fn run_case(
    pairs: usize,
    length: u64,
    caches: usize,
    mode: &str,
    shared: bool,
    concurrency: usize,
    slow: bool,
    short: bool,
    count: usize,
) {
    assert!(count > 0 && count <= 4096 && concurrency > 0 && concurrency <= count);
    let scratch = Scratch::new();
    let profile = Profile::new(pairs, length, caches);
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let (mut process, mut origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    let range = Expected {
        start: if short { 17 } else { 0 },
        end: if short { 97 } else { length },
        length,
    };
    if mode != "origin" {
        let preload = if shared { caches } else { count };
        let mut stored = 0;
        for i in 0..preload {
            if mode == "disk" {
                // Isolate preload from the measurement: persist each page before
                // submitting the next, so dirty pressure cannot silently omit it.
                for page in 0..if short { 1 } else { length.div_ceil(P) } {
                    let page_range = Expected {
                        start: page * P,
                        end: ((page + 1) * P).min(length),
                        length,
                    };
                    assert_eq!(
                        request(
                            &socket_path(&process, i % caches),
                            if shared { 0 } else { i as u64 },
                            &page_range,
                            false
                        ),
                        Ok(page_range.end - page_range.start)
                    );
                    stored += 1;
                    persisted(&mut process, stored);
                }
                continue;
            }
            assert_eq!(
                request(
                    &socket_path(&process, i % caches),
                    if shared { 0 } else { i as u64 },
                    &range,
                    false
                ),
                Ok(range.end - range.start)
            );
        }
        if mode == "disk" {
            persisted(
                &mut process,
                (preload as u64) * (if short { 1 } else { length.div_ceil(P) }),
            );
            assert!(process.stop(libc::SIGTERM).success(), "{}", process.logs());
            drop(origins);
            (process, origins) = Process::start_profile(&scratch, &control, 1, Some(&profile));
        }
        for origin in &origins {
            origin.offline.store(true, Ordering::Release);
        }
    }
    let calls_before: usize = origins.iter().map(|o| o.calls().len()).sum();
    let label = if shared && mode != "memory" {
        format!("{mode}-cold-to-warm")
    } else {
        mode.into()
    };
    let (measured, deltas) = measure(
        &process,
        &profile,
        &label,
        count,
        concurrency,
        shared,
        &range,
        slow,
    );
    assert!(measured.accept(count), "zero-failure gate: {measured:?}");
    let event = match mode {
        "memory" => "racer_memory_hits_total",
        "disk" => "racer_disk_hits_total",
        _ => "racer_origin_fills_total",
    };
    assert!(
        deltas[event] > 0,
        "selected path was not exercised: {deltas:?}"
    );
    let pages = if short { 1 } else { length.div_ceil(P) };
    if !shared && mode != "memory" {
        assert_eq!(
            deltas[event],
            count as u64 * pages,
            "every distinct request must exercise selected cold path"
        );
    }
    if mode != "origin" {
        assert_eq!(deltas["racer_origin_fills_total"], 0);
        assert_eq!(
            origins.iter().map(|o| o.calls().len()).sum::<usize>(),
            calls_before
        );
    }
    assert_eq!(deltas["racer_request_errors_total"], 0);
    assert!(process.stop(libc::SIGTERM).success(), "{}", process.logs());
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and 8 CPU capacity; explicit production matrix"]
fn production_progress_matrix() {
    for pairs in [1, 2, 4] {
        for mode in ["origin", "memory", "disk"] {
            for length in [113, P, 2 * P + 113] {
                // Long recovered values verify cold-to-warm streaming. Distinct
                // full-page disk cases below require a disk hit for every read.
                case(
                    pairs,
                    length,
                    1,
                    mode,
                    mode == "memory" || mode == "disk" && length > P,
                    1,
                    false,
                    false,
                );
            }
        }
        case(pairs, 113, 2, "origin", false, 2, false, false);
        case(pairs, 113, 2, "memory", true, 2, false, false);
        case(pairs, P, 1, "memory", true, 2, true, false);
        case(pairs, 2 * P + 113, 1, "memory", true, 2, false, true);
        case(pairs, P, 1, "origin", true, 2, false, false);
    }
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring and O_DIRECT; focused one-pair baseline"]
fn production_progress_smoke() {
    for mode in ["origin", "memory", "disk"] {
        case(1, P, 1, mode, mode == "memory", 1, false, false);
    }
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and requested CPU capacity; configurable measurement"]
fn production_configured_measurement() {
    let number = |name: &str, default: usize| {
        std::env::var(name).map_or(default, |value| {
            value.parse().expect("unsigned throughput parameter")
        })
    };
    let pairs = number("RACER_THROUGHPUT_PAIRS", 4);
    let length = number("RACER_THROUGHPUT_BYTES", P as usize) as u64;
    let caches = number("RACER_THROUGHPUT_CACHES", 1);
    let count = number("RACER_THROUGHPUT_REQUESTS", 32);
    let concurrency = number("RACER_THROUGHPUT_CONCURRENCY", 1);
    let mode = std::env::var("RACER_THROUGHPUT_MODE").unwrap_or_else(|_| "origin".into());
    assert!(["memory", "disk", "origin"].contains(&mode.as_str()));
    assert!((1..=4).contains(&caches) && (113..=4 * P).contains(&length));
    run_case(
        pairs,
        length,
        caches,
        &mode,
        number("RACER_THROUGHPUT_SHARED", usize::from(mode == "memory")) != 0,
        concurrency,
        number("RACER_THROUGHPUT_SLOW", 0) != 0,
        number("RACER_THROUGHPUT_SHORT", 0) != 0,
        count,
    );
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and 8 CPU capacity; churn diagnostic, strict mode may expose baseline overload"]
fn production_churn_diagnostic() {
    for pairs in [1, 2, 4] {
        let scratch = Scratch::new();
        let profile = Profile::new(pairs, 113, 2);
        let control = control::Control::start_with(&scratch.0, profile.caches.clone());
        let (mut process, _origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
        let range = Expected {
            start: 0,
            end: 113,
            length: 113,
        };
        let (measured, deltas) = measure(
            &process,
            &profile,
            "distinct-churn-diagnostic",
            512,
            2,
            false,
            &range,
            false,
        );
        assert_eq!(measured.attempted, 512);
        assert!(measured.completed > 0);
        assert!(
            measured
                .failures
                .keys()
                .all(|failure| failure == "Http(503)"),
            "unexpected failure: {measured:?}"
        );
        assert_eq!(
            deltas["racer_origin_fills_total"],
            measured.completed as u64
        );
        assert_eq!(
            deltas["racer_request_errors_total"],
            (512 - measured.completed) as u64
        );
        assert!(process.stop(libc::SIGTERM).success());
        if std::env::var_os("RACER_THROUGHPUT_STRICT_BASELINE").is_some() {
            assert!(measured.accept(512), "strict churn gate: {measured:?}");
        }
    }
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and 8 CPU capacity; expected ingress limitation"]
fn production_ingress_baseline() {
    let scratch = Scratch::new();
    let profile = Profile::new(4, 113, 1);
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let (mut process, _origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    let path = socket_path(&process, 0);
    let before = observe(process.child.id());
    let mut held = Vec::new();
    // Partial heads retain accepted leases without issuing data requests.
    for _ in 0..32 {
        let mut socket = connect(&path).unwrap();
        socket.write_all(b"GET /v1/objects/").unwrap();
        held.push(socket);
    }
    let deadline = Instant::now() + Duration::from_secs(5);
    while observe(process.child.id()).socket_fds < before.socket_fds + 32 {
        assert!(
            Instant::now() < deadline,
            "accepted connections never saturated"
        );
        thread::sleep(Duration::from_millis(10));
    }
    let mut excess = connect(&path).unwrap();
    excess
        .set_read_timeout(Some(Duration::from_millis(200)))
        .unwrap();
    write!(
        excess,
        "GET /v1/objects/{:064x} HTTP/1.1\r\nHost: racer\r\nRange: bytes=0-112\r\n\r\n",
        0
    )
    .unwrap();
    let mut byte = [0];
    let outcome = excess.read(&mut byte);
    assert!(
        matches!(&outcome, Err(error) if matches!(error.kind(), io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut)),
        "expected ingress stall: {outcome:?}"
    );
    assert_eq!(process.diagnostic("/readyz").unwrap().status, 200);
    println!(
        "BASELINE {}",
        json!({"diagnostic": "worker-zero-ingress", "pairs": 4, "held": held.len(), "aggregate_connection_budget": 128, "result": format!("{outcome:?}"), "accepted_socket_delta": observe(process.child.id()).socket_fds - before.socket_fds})
    );
    drop(excess);
    drop(held);
    thread::sleep(Duration::from_millis(100));
    assert_eq!(
        request(
            &path,
            0,
            &Expected {
                start: 0,
                end: 113,
                length: 113
            },
            false
        ),
        Ok(113)
    );
    assert!(process.stop(libc::SIGTERM).success());
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring and O_DIRECT; blocked control publication with live data"]
fn production_blocked_control_progress() {
    let scratch = Scratch::new();
    let profile = Profile::new(1, 113, 1);
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let (mut process, _origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    control.blocked.store(true, Ordering::Release);
    let polls = control.polls.load(Ordering::Acquire);
    let deadline = Instant::now() + TIMEOUT;
    while control.polls.load(Ordering::Acquire) == polls {
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(10));
    }
    let range = Expected {
        start: 0,
        end: 113,
        length: 113,
    };
    let (measured, _) = measure(
        &process,
        &profile,
        "blocked-control",
        8,
        1,
        true,
        &range,
        false,
    );
    assert!(measured.accept(8));
    assert_eq!(process.diagnostic("/readyz").unwrap().status, 200);
    control.blocked.store(false, Ordering::Release);
    let polls = control.polls.load(Ordering::Acquire);
    let deadline = Instant::now() + TIMEOUT;
    while control.polls.load(Ordering::Acquire) == polls {
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(10));
    }
    assert!(process.stop(libc::SIGTERM).success());
}

fn owner_key(cache: &str, page: u64, owner: u16) -> u64 {
    use racer_dataplane::{
        model::identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag, WorkerId,
        },
        runtime::worker::WorkerMap,
    };
    let map = WorkerMap::new((0..4).map(WorkerId).collect()).unwrap();
    (0..10000)
        .find(|key| {
            let object = ObjectId {
                cache: CacheId(cache.into()),
                key: CacheKey::parse_hex(format!("{key:064x}").as_bytes()).unwrap(),
            };
            map.owner(&PageId {
                version: ObjectVersion {
                    object,
                    etag: StrongEtag::parse(b"\"restart-v1\"").unwrap(),
                },
                number: PageNumber(page),
            })
            .unwrap()
                == WorkerId(owner)
        })
        .unwrap()
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring and O_DIRECT; blocked listener publication and recovery"]
fn production_blocked_listener_publication() {
    use racer_dataplane::control::wire::PublicationSequence;
    let scratch = Scratch::new();
    let profile = Profile::new(2, 113, 2);
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let added = control.publication.lock().unwrap().caches.pop().unwrap();
    let (mut process, _origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    let blocked = scratch.0.join("run-0/racer/throughput-1/client/socket");
    fs::write(&blocked, b"fixture blocks publication").unwrap();
    let polls = control.polls.load(Ordering::Acquire);
    {
        let mut publication = control.publication.lock().unwrap();
        publication.sequence = PublicationSequence(2);
        publication.caches.push(added);
    }
    let deadline = Instant::now() + TIMEOUT;
    while control.polls.load(Ordering::Acquire) == polls {
        assert!(Instant::now() < deadline);
        process.assert_running();
        thread::sleep(Duration::from_millis(10));
    }
    thread::sleep(Duration::from_millis(100));
    let range = Expected {
        start: 0,
        end: 113,
        length: 113,
    };
    let mut old = profile.clone();
    old.caches.truncate(1);
    let (measured, _) = measure(
        &process,
        &old,
        "blocked-listener-publication",
        8,
        1,
        true,
        &range,
        false,
    );
    assert!(measured.accept(8));
    assert_eq!(fs::read(&blocked).unwrap(), b"fixture blocks publication");
    fs::remove_file(&blocked).unwrap();
    let deadline = Instant::now() + TIMEOUT;
    while !blocked.exists() {
        assert!(Instant::now() < deadline, "publication did not recover");
        process.assert_running();
        thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(
        request(&socket_path(&process, 1), 0, &range, false),
        Ok(113)
    );
    assert!(process.stop(libc::SIGTERM).success());
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and 8 CPU capacity"]
fn production_multicache_disk_baseline() {
    let scratch = Scratch::new();
    let profile = Profile::new(4, P + 113, 2);
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let (mut process, mut origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    let first = owner_key(&profile.caches[0].0, 0, 0);
    let full = Expected {
        start: 0,
        end: P,
        length: P + 113,
    };
    assert_eq!(
        request(&socket_path(&process, 0), first, &full, false),
        Ok(P)
    );
    persisted(&mut process, 1);
    assert!(process.stop(libc::SIGTERM).success());
    drop(origins);
    (process, origins) = Process::start_profile(&scratch, &control, 1, Some(&profile));
    // Retain a second active cache on the same worker with a short tail, while
    // the full page is cold and present only in the recovered slab index.
    let second = owner_key(&profile.caches[1].0, 1, 0);
    let tail = Expected {
        start: P,
        end: P + 113,
        length: P + 113,
    };
    assert_eq!(
        request(&socket_path(&process, 1), second, &tail, false),
        Ok(113)
    );
    persisted(&mut process, 1);
    for origin in &origins {
        origin.offline.store(true, Ordering::Release);
    }
    let before = counters(&process);
    let start = Instant::now();
    let outcome = request(&socket_path(&process, 0), first, &full, false);
    let mut measured = Measurements::default();
    measured.record(outcome, start.elapsed());
    measured.finish(start.elapsed());
    let after = counters(&process);
    println!(
        "BASELINE {}",
        json!({"diagnostic": "multicache-full-page-disk", "pairs": 4, "caches": 2, "ciphertext_bytes_per_worker": 64 << 20, "measurement": measured, "before": before, "after": after})
    );
    assert!(measured.accept(1), "multicache disk progress: {measured:?}");
    assert_eq!(
        after["racer_overloads_total"],
        before["racer_overloads_total"]
    );
    assert_eq!(
        after["racer_disk_hits_total"],
        before["racer_disk_hits_total"] + 1
    );
    assert_eq!(
        after["racer_origin_fills_total"],
        before["racer_origin_fills_total"]
    );
    assert_eq!(
        request(&socket_path(&process, 1), second, &tail, false),
        Ok(113)
    );
    assert!(process.stop(libc::SIGTERM).success());
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring and O_DIRECT; two actual Applications with authenticated TCP peers"]
fn production_peer_and_failed_neighbor_progress() {
    use racer_dataplane::{
        control::wire,
        model::identity::{CacheId, CacheKey, MembershipVersion, NodeId, ObjectId, PageNumber},
        topology::{
            membership::{Member, Membership},
            placement::Placement,
        },
    };
    const OTHER: &str = "33333333-3333-4333-8333-333333333333";
    for pairs in [1, 2, 4] {
        let first_root = Scratch::new();
        let second_root = Scratch::new();
        let mut profile = Profile::new(pairs, P, 1);
        let control = control::Control::start_with(&first_root.0, profile.caches.clone());
        // One authority and one key domain, separate persistent node identities.
        fs::copy(
            first_root.0.join("trust.pem"),
            second_root.0.join("trust.pem"),
        )
        .unwrap();
        fs::write(
            second_root.0.join("token"),
            format!("fixture.token.{OTHER}"),
        )
        .unwrap();
        fs::create_dir_all(second_root.0.join("secrets/epoch")).unwrap();
        fs::copy(
            first_root.0.join("secrets/epoch/bundle.json"),
            second_root.0.join("secrets/epoch/bundle.json"),
        )
        .unwrap();
        std::os::unix::fs::symlink("epoch", second_root.0.join("secrets/..data")).unwrap();
        let sockets = [
            TcpListener::bind("127.0.0.1:0").unwrap(),
            TcpListener::bind("127.0.0.1:0").unwrap(),
        ];
        let members: Vec<_> = [NODE, OTHER]
            .iter()
            .zip(&sockets)
            .map(|(node, socket)| Member {
                node: NodeId((*node).into()),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: socket.local_addr().unwrap().to_string(),
                rails: vec![],
                alignment_enabled: false,
            })
            .collect();
        control.publication.lock().unwrap().members = members.clone();
        let membership =
            Arc::new(Membership::validate(MembershipVersion(1), members.clone()).unwrap());
        let placement = Placement::new(128);
        let rank = |key| {
            placement
                .rank(
                    membership.clone(),
                    &ObjectId {
                        cache: CacheId(profile.caches[0].0.clone()),
                        key: CacheKey::parse_hex(format!("{key:064x}").as_bytes()).unwrap(),
                    },
                    PageNumber(0),
                )
                .unwrap()
                .ordered
        };
        let source = usize::from(rank(0)[0].0 == OTHER);
        let next_key = (1..10000).find(|key| rank(*key)[0] == rank(0)[0]).unwrap();
        profile.peer = Some(sockets[0].local_addr().unwrap());
        let mut second_profile = profile.clone();
        second_profile.peer = Some(sockets[1].local_addr().unwrap());
        drop(sockets);
        let (first, first_origins) =
            Process::start_profile(&first_root, &control, 0, Some(&profile));
        let (second, second_origins) =
            Process::start_profile(&second_root, &control, 0, Some(&second_profile));
        let mut processes = [first, second];
        let origins = [first_origins, second_origins];
        let range = Expected {
            start: 0,
            end: P,
            length: P,
        };
        assert_eq!(
            request(&socket_path(&processes[source], 0), 0, &range, false),
            Ok(P)
        );
        for origin in origins.iter().flatten() {
            origin.offline.store(true, Ordering::Release);
        }
        let calls_before: usize = origins.iter().flatten().map(|o| o.calls().len()).sum();
        let receiver = 1 - source;
        let (measured, deltas) = measure(
            &processes[receiver],
            &profile,
            "peer-cold-to-warm",
            8,
            1,
            true,
            &range,
            false,
        );
        assert!(measured.accept(8), "{measured:?}");
        assert_eq!(deltas["racer_peer_hits_total"], 1);
        assert_eq!(deltas["racer_origin_fills_total"], 0);
        assert_eq!(
            origins
                .iter()
                .flatten()
                .map(|o| o.calls().len())
                .sum::<usize>(),
            calls_before
        );
        assert_eq!(
            processes[source].stop(libc::SIGKILL).signal(),
            Some(libc::SIGKILL)
        );
        // Preserve failed neighbor membership: deletion would hide the failure.
        assert_eq!(
            control.publication.lock().unwrap().sequence,
            wire::PublicationSequence(1)
        );
        for origin in &origins[receiver] {
            origin.offline.store(false, Ordering::Release);
        }
        profile.key_base = next_key;
        let (measured, deltas) = measure(
            &processes[receiver],
            &profile,
            "failed-neighbor-origin-fallback",
            8,
            1,
            true,
            &range,
            false,
        );
        assert!(measured.accept(8), "{measured:?}");
        assert_eq!(deltas["racer_origin_fills_total"], 1);
        assert_eq!(deltas["racer_request_errors_total"], 0);
        assert!(processes[receiver].stop(libc::SIGTERM).success());
    }
}
