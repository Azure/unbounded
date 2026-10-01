//! Real executable restart coverage. Run explicitly as root on Linux.
//! All writable state lives beneath this crate's target/.
#![cfg(target_os = "linux")]

#[path = "process_restart/throughput.rs"]
mod throughput;

use racer_dataplane as dataplane;
use racer_dataplane::{model::PAGE_BYTES, store::checkpoint};
#[path = "support/enrollment.rs"]
mod enrollment_io;
use enrollment_io::{fields, read_head};
use std::{
    collections::BTreeMap,
    ffi::CString,
    fs,
    io::{self, Read, Write},
    net::{SocketAddr, TcpListener, TcpStream},
    os::{
        fd::AsRawFd,
        unix::{
            fs::MetadataExt,
            net::{UnixListener, UnixStream},
            process::{CommandExt, ExitStatusExt},
        },
    },
    path::{Path, PathBuf},
    process::{Child, Command, ExitStatus, Stdio},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::{Duration, Instant},
};

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const NODE: &str = "22222222-2222-4222-8222-222222222222";
const CACHE: &str = "44444444-4444-4444-8444-444444444444";
const NAME: &str = "process-restart";
const P: u64 = PAGE_BYTES;
const LENGTH: u64 = 2 * P + 113;
const TIMEOUT: Duration = Duration::from_secs(30);

#[path = "../../../internal/racer-test/control.rs"]
mod control;

mod measurement {
    //! Client-side completion oracle. Partial bodies never contribute to goodput.
    use serde::Serialize;
    use std::{
        collections::BTreeMap,
        io::{self, BufReader, Read, Write},
        time::Duration,
    };

    #[derive(Debug, PartialEq, Eq)]
    pub enum Failure {
        Http(u16),
        Truncated,
        Transport(io::ErrorKind),
        Invalid,
        Corrupt,
    }
    impl From<io::Error> for Failure {
        fn from(error: io::Error) -> Self {
            if error.kind() == io::ErrorKind::UnexpectedEof {
                Self::Truncated
            } else {
                Self::Transport(error.kind())
            }
        }
    }

    pub struct Expected {
        pub start: u64,
        pub end: u64,
        pub length: u64,
    }

    pub fn response<S: Read + Write>(
        stream: &mut BufReader<S>,
        expected: &Expected,
        byte: impl FnMut(u64) -> u8,
        pause: Duration,
    ) -> Result<u64, Failure> {
        response_fields(stream, expected, byte, pause).map(|(length, _)| length)
    }

    pub fn response_fields<S: Read + Write>(
        stream: &mut BufReader<S>,
        expected: &Expected,
        mut byte: impl FnMut(u64) -> u8,
        pause: Duration,
    ) -> Result<(u64, BTreeMap<String, String>), Failure> {
        let mut head = Vec::new();
        // Bounded even for an unterminated malicious header.
        while !head.ends_with(b"\r\n\r\n") {
            if head.len() == 32768 {
                return Err(Failure::Invalid);
            }
            let mut b = [0];
            stream.read_exact(&mut b)?;
            head.push(b[0]);
        }
        let head = std::str::from_utf8(&head).map_err(|_| Failure::Invalid)?;
        let mut lines = head.split("\r\n");
        let mut status = lines.next().ok_or(Failure::Invalid)?.split_whitespace();
        if status.next() != Some("HTTP/1.1") {
            return Err(Failure::Invalid);
        }
        let status: u16 = status
            .next()
            .ok_or(Failure::Invalid)?
            .parse()
            .map_err(|_| Failure::Invalid)?;
        let mut fields = BTreeMap::new();
        for line in lines.filter(|line| !line.is_empty()) {
            let (name, value) = line.split_once(':').ok_or(Failure::Invalid)?;
            if fields
                .insert(name.to_ascii_lowercase(), value.trim().to_owned())
                .is_some()
            {
                return Err(Failure::Invalid);
            }
        }
        if status != 200 {
            return Err(Failure::Http(status));
        }
        let length = expected
            .end
            .checked_sub(expected.start)
            .ok_or(Failure::Invalid)?;
        let pages = if length == 0 {
            0
        } else {
            (expected.end - 1) / super::P - expected.start / super::P + 1
        };
        if expected.end > expected.length
            || fields
                .get("content-length")
                .and_then(|s| s.parse::<u64>().ok())
                != Some(length + 21 * (pages + 1))
            || fields.get("etag").map(String::as_str) != Some("\"restart-v1\"")
            || fields.get("racer-object-length") != Some(&expected.length.to_string())
            || fields.get("racer-range-start") != Some(&expected.start.to_string())
            || fields.get("racer-range-end") != Some(&expected.end.to_string())
            || fields.get("racer-expires-at") != Some(&"0".to_owned())
            || fields.get("content-type").map(String::as_str) != Some("application/octet-stream")
            || fields.get("connection").map(String::as_str) != Some("close")
            || fields.contains_key("content-range")
            || fields.contains_key("transfer-encoding")
        {
            return Err(Failure::Invalid);
        }
        let mut scratch = [0; 64 << 10];
        let mut offset = expected.start;
        while offset < expected.end {
            let number = offset / super::P;
            let end = expected.end.min((number + 1) * super::P);
            let length = (end - offset) as u32;
            let mut frame = [0; 21];
            stream.read_exact(&mut frame)?;
            if frame[0] != 1
                || u64::from_be_bytes(frame[1..9].try_into().unwrap()) != number
                || u64::from_be_bytes(frame[9..17].try_into().unwrap()) != offset
                || u32::from_be_bytes(frame[17..].try_into().unwrap()) != length
            {
                return Err(Failure::Invalid);
            }
            while offset < end {
                let n = scratch.len().min((end - offset) as usize);
                stream.read_exact(&mut scratch[..n])?;
                if scratch[..n]
                    .iter()
                    .enumerate()
                    .any(|(i, actual)| *actual != byte(offset + i as u64))
                {
                    return Err(Failure::Corrupt);
                }
                offset += n as u64;
                if !pause.is_zero() {
                    std::thread::sleep(pause);
                }
            }
            // Completion releases the final lease; earlier pages return exact credit.
            if offset < expected.end {
                stream.get_mut().write_all(&number.to_be_bytes())?;
                stream.get_mut().write_all(&length.to_be_bytes())?;
            }
        }
        let mut complete = [0; 21];
        stream.read_exact(&mut complete)?;
        if complete[0] != 2
            || u64::from_be_bytes(complete[1..9].try_into().unwrap()) != pages
            || u64::from_be_bytes(complete[9..17].try_into().unwrap()) != length
            || u32::from_be_bytes(complete[17..].try_into().unwrap()) != 0
        {
            return Err(Failure::Invalid);
        }
        let mut extra = [0];
        if stream.read(&mut extra)? != 0 {
            return Err(Failure::Invalid);
        }
        Ok((length, fields))
    }

    #[derive(Default, Debug, Serialize)]
    pub struct Measurements {
        pub attempted: usize,
        pub completed: usize,
        pub completed_bytes: u64,
        pub failures: BTreeMap<String, usize>,
        pub elapsed_seconds: f64,
        pub goodput_bytes_per_second: f64,
        pub completed_p50_ms: Option<f64>,
        pub completed_p99_ms: Option<f64>,
        #[serde(skip)]
        latencies: Vec<Duration>,
    }
    impl Measurements {
        pub fn record(&mut self, result: Result<u64, Failure>, latency: Duration) {
            self.attempted += 1;
            match result {
                Ok(bytes) => {
                    self.completed += 1;
                    self.completed_bytes += bytes;
                    self.latencies.push(latency);
                }
                Err(error) => *self.failures.entry(format!("{error:?}")).or_default() += 1,
            }
        }
        pub fn finish(&mut self, elapsed: Duration) {
            self.elapsed_seconds = elapsed.as_secs_f64();
            self.goodput_bytes_per_second = if elapsed.is_zero() {
                0.
            } else {
                self.completed_bytes as f64 / self.elapsed_seconds
            };
            self.latencies.sort_unstable();
            let percentile = |p: usize| {
                self.latencies
                    .get((self.latencies.len() * p).div_ceil(100).saturating_sub(1))
                    .map(|d| d.as_secs_f64() * 1000.)
            };
            self.completed_p50_ms = percentile(50);
            self.completed_p99_ms = percentile(99);
        }
        pub fn accept(&self, scheduled: usize) -> bool {
            scheduled > 0
                && self.attempted == scheduled
                && self.completed == scheduled
                && self.completed_bytes > 0
                && self.failures.is_empty()
                && self.elapsed_seconds > 0.
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        fn check(bytes: &[u8]) -> Result<u64, Failure> {
            response(
                &mut BufReader::new(io::Cursor::new(bytes.to_vec())),
                &Expected {
                    start: 0,
                    end: 3,
                    length: 3,
                },
                |i| i as u8,
                Duration::ZERO,
            )
        }
        #[test]
        fn complete_error_truncated_malformed_and_corrupt_responses() {
            let head = b"HTTP/1.1 200 OK\r\nContent-Length: 45\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: 3\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 3\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n";
            let page = frame(1, 0, 0, 3);
            let complete = frame(2, 1, 3, 0);
            let valid = [head.as_slice(), &page, &[0, 1, 2], &complete].concat();
            assert_eq!(check(&valid), Ok(3));
            assert_eq!(
                check(&[head.as_slice(), &page, &[0, 1]].concat()),
                Err(Failure::Truncated)
            );
            assert_eq!(
                check(&[head.as_slice(), &page, &[0, 1, 7], &complete].concat()),
                Err(Failure::Corrupt)
            );
            assert_eq!(
                check(b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n"),
                Err(Failure::Http(503))
            );
            assert_eq!(check(b""), Err(Failure::Truncated));
            assert_eq!(check(&valid[..valid.len() - 1]), Err(Failure::Truncated));
            assert_eq!(
                check(&[valid.as_slice(), &[0]].concat()),
                Err(Failure::Invalid)
            );
            assert_eq!(
                check(&[head.as_slice(), &frame(1, 1, 0, 3), &[0, 1, 2], &complete].concat()),
                Err(Failure::Invalid)
            );
            assert_eq!(
                check(&[head.as_slice(), &page, &[0, 1, 2], &frame(2, 2, 3, 0)].concat()),
                Err(Failure::Invalid)
            );
            assert_eq!(check(&vec![b'x'; 32769]), Err(Failure::Invalid));
            assert_eq!(
                check(b"HTTP/1.1 206 OK\r\nContent-Length: 3\r\ncontent-length: 3\r\n\r\n"),
                Err(Failure::Invalid)
            );
        }
        fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
            let mut frame = [0; 21];
            frame[0] = kind;
            frame[1..9].copy_from_slice(&number.to_be_bytes());
            frame[9..17].copy_from_slice(&offset.to_be_bytes());
            frame[17..].copy_from_slice(&length.to_be_bytes());
            frame
        }

        #[test]
        fn partial_pages_return_exact_credit_and_empty_requires_completion() {
            let (mut client, mut server) = std::os::unix::net::UnixStream::pair().unwrap();
            client
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            server
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let worker = std::thread::spawn(move || {
                write!(server, "HTTP/1.1 200 OK\r\nContent-Length: 65\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: {}\r\nRacer-Range-Start: {}\r\nRacer-Range-End: {}\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n", super::super::P + 1, super::super::P - 1, super::super::P + 1).unwrap();
                server
                    .write_all(&frame(1, 0, super::super::P - 1, 1))
                    .unwrap();
                server.write_all(&[7]).unwrap();
                let mut release = [0; 12];
                server.read_exact(&mut release).unwrap();
                assert_eq!(&release[..8], &0u64.to_be_bytes());
                assert_eq!(&release[8..], &1u32.to_be_bytes());
                server.write_all(&frame(1, 1, super::super::P, 1)).unwrap();
                server.write_all(&[7]).unwrap();
                server.write_all(&frame(2, 2, 2, 0)).unwrap();
            });
            assert_eq!(
                response(
                    &mut BufReader::new(&mut client),
                    &Expected {
                        start: super::super::P - 1,
                        end: super::super::P + 1,
                        length: super::super::P + 1
                    },
                    |_| 7,
                    Duration::ZERO
                ),
                Ok(2)
            );
            worker.join().unwrap();
            let head = b"HTTP/1.1 200 OK\r\nContent-Length: 21\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: 0\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 0\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n";
            for complete in [true, false] {
                let mut raw = head.to_vec();
                if complete {
                    raw.extend_from_slice(&frame(2, 0, 0, 0));
                }
                assert_eq!(
                    response(
                        &mut BufReader::new(io::Cursor::new(raw)),
                        &Expected {
                            start: 0,
                            end: 0,
                            length: 0
                        },
                        |_| panic!("empty payload"),
                        Duration::ZERO
                    ),
                    if complete {
                        Ok(0)
                    } else {
                        Err(Failure::Truncated)
                    }
                );
            }
        }
        #[test]
        fn strict_gate_rejects_partial_success_empty_and_unfinished_runs() {
            let mut m = Measurements::default();
            m.finish(Duration::from_secs(1));
            assert!(!m.accept(0));
            m.record(Ok(3), Duration::from_millis(10));
            m.finish(Duration::from_secs(1));
            assert!(m.accept(1));
            assert!(!m.accept(2));
            m.record(Err(Failure::Truncated), Duration::from_millis(20));
            m.finish(Duration::from_secs(2));
            assert!(!m.accept(2));
            assert_eq!(m.completed_bytes, 3);
            assert_eq!(m.goodput_bytes_per_second, 1.5);
            assert_eq!(m.completed_p99_ms, Some(10.));
            m.finish(Duration::ZERO);
            assert!(!m.accept(1));
        }
    }
}

// Opt-in real Application + Go SDK sustained connection rotation gate.
// The flag records normal completion observed by the load loop. Drop must not
// probe/reap an unobserved exited supervisor before signaling its whole group.
struct SDKProcess(Child, bool);
impl Drop for SDKProcess {
    fn drop(&mut self) {
        // A normally completed timeout has already waited for its SDK child.
        if self.1 {
            return;
        }
        // Keep the supervisor alive during the grace period. Do not reap it
        // before escalation: its unreaped PID also pins the process-group ID.
        let group = -(self.0.id() as i32);
        unsafe {
            libc::kill(group, libc::SIGTERM);
        }
        thread::sleep(Duration::from_millis(200));
        unsafe {
            libc::kill(group, libc::SIGKILL);
        }
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            match self.0.try_wait() {
                Ok(Some(_)) => break,
                Err(error) => {
                    eprintln!("SDK supervisor reap failed: {error}");
                    break;
                }
                Ok(None) if Instant::now() < deadline => {
                    thread::sleep(Duration::from_millis(10));
                }
                Ok(None) => {
                    eprintln!("SDK supervisor did not exit after process-group SIGKILL");
                    break;
                }
            }
        }
    }
}

#[test]
fn sdk_process_cleanup_terminates_term_ignoring_child() {
    use std::os::fd::AsRawFd;

    let mut sdk = SDKProcess(
        Command::new("timeout")
            .process_group(0)
            .args([
                "--signal=TERM",
                "--kill-after=10s",
                "60s",
                "sh",
                "-c",
                "trap '' TERM; printf '%s\\n' \"$$\"; exec sleep 300",
            ])
            .stdout(Stdio::piped())
            .spawn()
            .unwrap(),
        false,
    );
    let supervisor = sdk.0.id();
    let mut output = sdk.0.stdout.take().unwrap();
    // Bound the readiness handshake too; the child must install SIG_IGN before
    // cleanup begins, otherwise a passing test would not prove escalation.
    let flags = unsafe { libc::fcntl(output.as_raw_fd(), libc::F_GETFL) };
    assert!(flags >= 0);
    assert_eq!(
        unsafe { libc::fcntl(output.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) },
        0
    );
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut ready = Vec::new();
    loop {
        let mut byte = [0];
        match output.read(&mut byte) {
            Ok(1) if byte[0] == b'\n' => break,
            Ok(1) => ready.push(byte[0]),
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                thread::sleep(Duration::from_millis(5))
            }
            other => panic!("child readiness failed: {other:?}"),
        }
        assert!(Instant::now() < deadline, "child readiness timed out");
    }
    let child: u32 = std::str::from_utf8(&ready).unwrap().parse().unwrap();
    assert_ne!(child, supervisor);
    let started = Instant::now();
    drop(sdk);
    assert!(started.elapsed() < Duration::from_secs(3));
    assert!(
        !Path::new(&format!("/proc/{supervisor}")).exists(),
        "supervisor was not reaped"
    );
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        // An orphan may briefly be a zombie until init reaps it. It must no
        // longer execute; kill(pid, 0) alone cannot distinguish this state.
        match fs::read_to_string(format!("/proc/{child}/stat")) {
            Err(error) if error.kind() == io::ErrorKind::NotFound => break,
            Ok(stat) if stat.rsplit_once(") ").unwrap().1.starts_with('Z') => break,
            _ => assert!(
                Instant::now() < deadline,
                "TERM-ignoring child survived cleanup"
            ),
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn cpu(pid: u32) -> BTreeMap<String, u64> {
    fs::read_dir(format!("/proc/{pid}/task"))
        .unwrap()
        .map(|entry| {
            let path = entry.unwrap().path();
            let name = fs::read_to_string(path.join("comm")).unwrap();
            let stat = fs::read_to_string(path.join("schedstat")).unwrap();
            (
                format!(
                    "{}:{}",
                    name.trim(),
                    path.file_name().unwrap().to_string_lossy()
                ),
                stat.split_whitespace().next().unwrap().parse().unwrap(),
            )
        })
        .collect()
}

#[test]
#[ignore = "requires root, mount namespaces, io_uring, O_DIRECT and RACER_SDK_AGE_BINARY"]
fn real_sdk_connection_age_sustained() {
    let binary = std::env::var_os("RACER_SDK_AGE_BINARY").expect("prebuild the Go SDK test binary");
    let report_directory = Path::new(env!("CARGO_MANIFEST_DIR")).join("target");
    fs::create_dir_all(&report_directory).unwrap();
    let scratch = Scratch::new();
    let profile = throughput::Profile {
        pairs: 4,
        length: P + 113,
        caches: vec![
            (CACHE.into(), "age-bulk".into()),
            (
                "44444444-4444-4444-8444-444444444445".into(),
                "age-small".into(),
            ),
        ],
        peer: None,
        key_base: 0,
    };
    let control = control::Control::start_with(&scratch.0, profile.caches.clone());
    let (mut process, mut origins) = Process::start_profile(&scratch, &control, 0, Some(&profile));
    // Give the second cache genuinely small objects, not small ranges of bulk objects.
    drop(origins.remove(1));
    let small_origin = format!(
        "/proc/self/fd/{}/origin/socket",
        process.cache_directories[1].as_raw_fd()
    );
    fs::remove_file(&small_origin).unwrap();
    origins.push(Origin::start_with(Path::new(&small_origin), 113, true));
    for age in ["1h", "500ms"] {
        let output = report_directory.join(format!("sdk-age-{age}.log"));
        let log = fs::File::create(&output).unwrap();
        let mut sdk = SDKProcess(
            Command::new("timeout")
                .process_group(0)
                .args(["--signal=TERM", "--kill-after=10s", "60s"])
                .arg(&binary)
                .args([
                    "-test.run=^TestRealRuntimeConnectionAgeLoad$",
                    "-test.timeout=5m",
                    "-test.v",
                ])
                .env("RACER_SDK_AGE", age)
                .env(
                    "RACER_SDK_AGE_SOCKET",
                    format!(
                        "/proc/{}/fd/{}/client/socket",
                        std::process::id(),
                        process.cache_directories[0].as_raw_fd()
                    ),
                )
                .env(
                    "RACER_SDK_AGE_SMALL_SOCKET",
                    format!(
                        "/proc/{}/fd/{}/client/socket",
                        std::process::id(),
                        process.cache_directories[1].as_raw_fd()
                    ),
                )
                .stdout(log.try_clone().unwrap())
                .stderr(log)
                .spawn()
                .unwrap(),
            false,
        );
        let started = Instant::now();
        let before = cpu(process.child.id());
        let mut samples = Vec::new();
        let status = loop {
            process.assert_running();
            if let Some(status) = sdk.0.try_wait().unwrap() {
                sdk.1 = true;
                break status;
            }
            if started.elapsed() > Duration::from_secs(75) {
                // Allow timeout's full 60s + 10s escalation window. On a
                // stalled supervisor, Drop still escalates the entire group.
                panic!("SDK timed out");
            }
            let metrics = process.diagnostic("/metrics").unwrap();
            samples.push(serde_json::json!({"seconds": started.elapsed().as_secs_f64(), "cpu_ns": cpu(process.child.id()), "metrics": String::from_utf8(metrics.body).unwrap()}));
            thread::sleep(Duration::from_millis(500));
        };
        let after = cpu(process.child.id());
        let report = serde_json::json!({"age": age, "pairs": 4, "elapsed_seconds": started.elapsed().as_secs_f64(), "cpu_before_ns": before, "cpu_after_ns": after, "samples": samples,
            "limitations": ["thread CPU is a worker activity proxy, not request attribution", "server exports aggregate metrics, not per-worker requests or queue residence"]});
        fs::write(
            report_directory.join(format!("sdk-age-{age}.json")),
            serde_json::to_vec_pretty(&report).unwrap(),
        )
        .unwrap();
        println!(
            "SDK_AGE_LOG {}\n{}",
            output.display(),
            fs::read_to_string(&output).unwrap()
        );
        assert!(status.success(), "SDK load failed");
    }
    assert!(process.stop(libc::SIGTERM).success());
    drop(origins);
}

// Historical profiles call I/O shard counts "pairs". Preserve their 1/2/4-shard
// workloads, but budget shared crypto rather than doubling every shard count.
fn profile_thread_cap(io_shards: usize) -> usize {
    assert!([1, 2, 4].contains(&io_shards));
    io_shards + io_shards.div_ceil(2)
}

fn profile_plan(io_shards: usize) -> racer_dataplane::runtime::affinity::AffinityPlan {
    use racer_dataplane::{config::Config, runtime::affinity::AffinityPlan};
    let (config, _) = Config::from_lookup_with_fabric_ports(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some(CLUSTER.into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://controller.invalid:443".into()),
            "RACER_MAX_THREADS" => Some(profile_thread_cap(io_shards).to_string()),
            _ => None,
        })
    })
    .unwrap();
    let plan = AffinityPlan::discover(&config).unwrap();
    assert_eq!(
        plan.pairs.len(),
        io_shards,
        "host CPU/quota/NUMA placement cannot provide requested I/O shards"
    );
    plan
}

#[test]
fn capped_profiles_count_io_shards_and_unique_crypto_threads() {
    use racer_dataplane::{
        config::Config,
        runtime::affinity::{AffinityPlan, CpuLocation, EffectiveTopology},
    };
    let (mut config, _) = Config::from_lookup_with_fabric_ports(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some(CLUSTER.into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://controller.invalid:443".into()),
            _ => None,
        })
    })
    .unwrap();
    for (io, crypto, cap) in [(1, 1, 2), (2, 1, 3), (4, 2, 6)] {
        config.max_threads = profile_thread_cap(io);
        assert_eq!(config.max_threads, cap);
        let plan = AffinityPlan::from_topology(
            &config,
            EffectiveTopology {
                cpus: (0..8)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: Some(0),
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap();
        assert_eq!(plan.pairs.len(), io);
        assert_eq!(plan.crypto_groups().len(), crypto);
        assert_eq!(io + crypto, cap);
    }
}

struct Scratch(PathBuf);
impl Scratch {
    fn new() -> Self {
        assert_eq!(
            unsafe { libc::geteuid() },
            0,
            "requires root and mount namespaces"
        );
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target");
        fs::create_dir_all(&root).unwrap();
        let path = root.join(format!(
            "restart-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        Self(path)
    }
}
impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

struct Process {
    child: Child,
    log: PathBuf,
    diagnostics: SocketAddr,
    socket_directory: fs::File,
    cache_directories: Vec<fs::File>,
}
impl Process {
    fn start(scratch: &Scratch, control: &control::Control, incarnation: usize) -> (Self, Origin) {
        let (process, mut origins) = Self::start_profile(scratch, control, incarnation, None);
        (process, origins.remove(0))
    }
    fn start_profile(
        scratch: &Scratch,
        control: &control::Control,
        incarnation: usize,
        profile: Option<&throughput::Profile>,
    ) -> (Self, Vec<Origin>) {
        let expected_plan = profile.map(|profile| profile_plan(profile.pairs));
        // This fixture gives each process fresh runtime sockets and shares identity/
        // and slabs/. It does not cover stale sockets on deployment's hostPath.
        let run = scratch.0.join(format!("run-{incarnation}"));
        let names = profile.map_or_else(
            || vec![NAME.to_owned()],
            |p| p.caches.iter().map(|(_, name)| name.clone()).collect(),
        );
        let mut cache_directories = Vec::new();
        let mut origins = Vec::new();
        for name in names {
            let cache = run.join("racer").join(name);
            fs::create_dir_all(cache.join("origin")).unwrap();
            fs::create_dir_all(cache.join("client")).unwrap();
            let directory = fs::File::open(&cache).unwrap();
            let alias = PathBuf::from(format!("/proc/self/fd/{}", directory.as_raw_fd()));
            origins.push(Origin::start_with(
                &alias.join("origin/socket"),
                profile.map_or(LENGTH, |p| p.length),
                profile.is_some(),
            ));
            cache_directories.push(directory);
        }
        let socket_directory = cache_directories[0].try_clone().unwrap();
        let peer = TcpListener::bind(
            profile
                .and_then(|p| p.peer)
                .unwrap_or_else(|| "127.0.0.1:0".parse().unwrap()),
        )
        .unwrap();
        let diagnostic = TcpListener::bind("127.0.0.1:0").unwrap();
        let diagnostics = diagnostic.local_addr().unwrap();
        let mut command = Command::new(env!("CARGO_BIN_EXE_racer-dataplane"));
        for (key, _) in std::env::vars_os() {
            if key.as_encoded_bytes().starts_with(b"RACER_") {
                command.env_remove(key);
            }
        }
        command.envs([
            ("RACER_CLUSTER_ID", CLUSTER.to_owned()),
            ("RACER_CONTROL_ENDPOINT", control.endpoint.clone()),
            ("RACER_PEER_LISTEN", peer.local_addr().unwrap().to_string()),
            ("RACER_DIAGNOSTICS_LISTEN", diagnostics.to_string()),
        ]);
        for (name, path) in [
            ("RACER_TRUST_BUNDLE", "trust.pem"),
            ("RACER_SERVICE_ACCOUNT_TOKEN", "token"),
            ("RACER_IDENTITY_DIRECTORY", "identity"),
            ("RACER_SLAB_DIRECTORY", "slabs"),
        ] {
            command.env(name, scratch.0.join(path));
        }
        if let Some(plan) = &expected_plan {
            command.env("RACER_MAX_THREADS", plan.max_threads.to_string());
        } else {
            command.envs([
                ("RACER_MAX_THREADS", "2"),
                ("RACER_ENABLE_RDMA", "false"),
                ("RACER_PLAINTEXT_BYTES", "67108864"),
                ("RACER_CIPHERTEXT_BYTES", "167772160"),
                ("RACER_DIRTY_BYTES", "67108864"),
                ("RACER_REGISTERED_BYTES", "1"),
                // Current signed-peer envelope progress floor exceeds 1 MiB.
                ("RACER_REQUEST_CONTEXT_BYTES", "16777216"),
                ("RACER_SLAB_BYTES", "268435456"),
                ("RACER_SEGMENT_BYTES", "67108864"),
                ("RACER_FREE_SEGMENT_RESERVE", "1"),
                ("RACER_QUEUE_ENTRIES", "16"),
                // Three independent control slots require twelve total connections.
                ("RACER_CLIENT_CONNECTIONS", "12"),
                ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "2"),
                ("RACER_METADATA_ENTRIES", "32"),
                ("RACER_FLIGHTS", "8"),
                ("RACER_PIPES", "2"),
                ("RACER_RANGE_WINDOW_PAGES", "1"),
                ("RACER_REQUEST_TIMEOUT_MS", "20000"),
                ("RACER_READER_STALL_TIMEOUT_MS", "10000"),
                ("RACER_SHUTDOWN_TIMEOUT_MS", "10000"),
            ]);
        }
        let source = CString::new(run.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: the child performs only async-signal-safe syscalls before exec.
        // Make propagation private before mounting worktree-local storage over /run.
        unsafe {
            command.pre_exec(move || {
                if libc::unshare(libc::CLONE_NEWNS) != 0
                    || libc::mount(
                        std::ptr::null(),
                        c"/".as_ptr(),
                        std::ptr::null(),
                        libc::MS_REC | libc::MS_PRIVATE,
                        std::ptr::null(),
                    ) != 0
                    || libc::mount(
                        source.as_ptr(),
                        c"/run".as_ptr(),
                        std::ptr::null(),
                        libc::MS_BIND,
                        std::ptr::null(),
                    ) != 0
                {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let log = scratch.0.join(format!("process-{incarnation}.log"));
        let output = fs::File::create(&log).unwrap();
        command
            .stdin(Stdio::null())
            .stdout(output.try_clone().unwrap())
            .stderr(output);
        drop(peer);
        drop(diagnostic);
        let child = command
            .spawn()
            .expect("exec dataplane in private mount namespace");
        let mut process = Self {
            child,
            log,
            diagnostics,
            socket_directory,
            cache_directories,
        };
        let deadline = Instant::now() + TIMEOUT;
        loop {
            process.assert_running();
            if process
                .diagnostic("/readyz")
                .is_ok_and(|reply| reply.status == 200)
            {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "readiness timed out: {}",
                process.logs()
            );
            thread::sleep(Duration::from_millis(20));
        }
        if let Some(plan) = &expected_plan {
            let names: Vec<_> = fs::read_dir(format!("/proc/{}/task", process.child.id()))
                .unwrap()
                .map(|task| fs::read_to_string(task.unwrap().path().join("comm")).unwrap())
                .collect();
            assert_eq!(
                names
                    .iter()
                    .filter(|name| name.starts_with("racer-crypto-"))
                    .count(),
                plan.crypto_groups().len(),
                "crypto execution groups differ from planned placement: {names:?}"
            );
            assert_eq!(
                names
                    .iter()
                    .filter(|name| name.starts_with("racer-io-"))
                    .count(),
                plan.pairs.len() - 1,
                "caller owns worker zero; progress floors must fund requested I/O shards: {names:?}"
            );
            assert_eq!(
                // Linux can expose io_uring's kernel workers in the task list.
                // They are not application threads selected by AffinityPlan.
                names
                    .iter()
                    .filter(|name| !name.starts_with("iou-wrk-"))
                    .count(),
                plan.pairs.len() + plan.crypto_groups().len(),
                "application thread count must match the capped plan: {names:?}"
            );
        }
        (process, origins)
    }
    fn logs(&self) -> String {
        fs::read_to_string(&self.log).unwrap_or_default()
    }
    fn assert_running(&mut self) {
        assert!(
            self.child.try_wait().unwrap().is_none(),
            "dataplane exited: {}",
            self.logs()
        );
    }
    fn connect(&self) -> UnixStream {
        let socket = UnixStream::connect(format!(
            "/proc/self/fd/{}/client/socket",
            self.socket_directory.as_raw_fd()
        ))
        .unwrap();
        socket.set_read_timeout(Some(TIMEOUT)).unwrap();
        socket.set_write_timeout(Some(TIMEOUT)).unwrap();
        socket
    }
    fn request(&self, pin: Option<&str>, start: u64, end: u64) -> Reply {
        let mut socket = self.connect();
        write_request(&mut socket, pin, start, end);
        let mut body = Vec::new();
        let (_, fields) = measurement::response_fields(
            &mut io::BufReader::new(socket),
            &measurement::Expected {
                start,
                end,
                length: LENGTH,
            },
            |offset| {
                let value = byte(offset);
                body.push(value);
                value
            },
            Duration::ZERO,
        )
        .expect("complete client subscription before deadline");
        Reply {
            status: 200,
            fields,
            body,
        }
    }
    fn diagnostic(&self, path: &str) -> io::Result<Reply> {
        let mut socket = TcpStream::connect_timeout(&self.diagnostics, Duration::from_millis(200))?;
        socket.set_read_timeout(Some(Duration::from_secs(1)))?;
        socket.set_write_timeout(Some(Duration::from_secs(1)))?;
        write!(
            socket,
            "GET {path} HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n"
        )?;
        read_reply(&mut socket)
    }
    fn metric(&self, name: &str) -> u64 {
        let reply = self.diagnostic("/metrics").unwrap();
        assert_eq!(reply.status, 200);
        String::from_utf8(reply.body)
            .unwrap()
            .lines()
            .find_map(|line| line.strip_prefix(&format!("{name} ")))
            .unwrap()
            .parse()
            .unwrap()
    }
    fn stop(&mut self, signal: i32) -> ExitStatus {
        self.assert_running();
        assert_eq!(unsafe { libc::kill(self.child.id() as i32, signal) }, 0);
        let deadline = Instant::now() + TIMEOUT;
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status;
            }
            assert!(
                Instant::now() < deadline,
                "process failed to exit: {}",
                self.logs()
            );
            thread::sleep(Duration::from_millis(10));
        }
    }
}
impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        if thread::panicking() {
            eprintln!("dataplane log: {}", self.logs());
        }
    }
}

struct Reply {
    status: u16,
    fields: BTreeMap<String, String>,
    body: Vec<u8>,
}
fn read_reply(stream: &mut impl Read) -> io::Result<Reply> {
    let head = read_head(stream)?;
    let fields = fields(&head);
    let status = head.split_whitespace().nth(1).unwrap().parse().unwrap();
    let length: usize = fields["content-length"].parse().unwrap();
    assert!(length <= LENGTH as usize);
    let mut body = vec![0; length];
    stream.read_exact(&mut body)?;
    Ok(Reply {
        status,
        fields,
        body,
    })
}
fn write_request(socket: &mut UnixStream, pin: Option<&str>, start: u64, end: u64) {
    let pin = pin.map_or(String::new(), |pin| format!("If-Match: {pin}\r\n"));
    write!(socket, "POST /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: {P}\r\nRacer-Ordered: 1\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\n{pin}Range: bytes={start}-{}\r\nConnection: close\r\n\r\n", "ab".repeat(32), end - 1).unwrap();
}
fn byte(offset: u64) -> u8 {
    ((offset * 31 + offset / P * 17) % 251) as u8
}
fn check(reply: &Reply, start: u64, end: u64) {
    assert_eq!(reply.status, 200);
    assert_eq!(reply.fields["etag"], "\"restart-v1\"");
    assert_eq!(reply.fields["racer-object-length"], LENGTH.to_string());
    assert_eq!(reply.fields["racer-range-start"], start.to_string());
    assert_eq!(reply.fields["racer-range-end"], end.to_string());
    assert_eq!(reply.body.len() as u64, end - start);
    for (i, actual) in reply.body.iter().enumerate() {
        assert_eq!(
            *actual,
            byte(start + i as u64),
            "corrupt byte at {}",
            start + i as u64
        );
    }
}

fn enrolled_identity(
    scratch: &Scratch,
    control: &control::Control,
    processes: usize,
) -> racer_dataplane::control::enrollment::LocalSigningIdentity {
    use racer_dataplane::{
        control::{enrollment::Enrollment, wire},
        model::ClusterId,
    };
    assert_eq!(
        control.enrollments.load(Ordering::Acquire),
        2 * processes,
        "each process authenticates at pre-worker bootstrap and control-worker startup"
    );
    assert!(
        !scratch.0.join("identity/pending.json").exists(),
        "readiness requires committed enrollment and completed pending cleanup"
    );
    let enrollment = Enrollment::new(
        ClusterId(CLUSTER.into()),
        scratch.0.join("token"),
        scratch.0.join("identity"),
    );
    let bundle = wire::decode_bundle(&control.bundle).unwrap();
    enrollment
        .set_peer_trust_roots(bundle.peer_trust_roots)
        .unwrap();
    // Verify the persisted certificate's chain, SAN, validity, request correlation,
    // and local key pairing rather than comparing opaque identity.json bytes.
    let reactor = enrollment_io::reactor();
    enrollment.attach_reactor(reactor.clone());
    let scope = racer_dataplane::runtime::deadline::RequestScope::new(
        racer_dataplane::model::RequestId([9; 16]),
        Instant::now() + Duration::from_secs(15),
    )
    .unwrap();
    let identity = enrollment_io::drive(&reactor, enrollment.load_identity_async(&scope))
        .unwrap()
        .unwrap();
    assert_eq!(identity.cluster().0, CLUSTER);
    assert_eq!(identity.node().0, NODE);
    assert!(identity.valid_now());
    identity
}

#[derive(Clone, Debug)]
struct Call {
    method: String,
    pin: Option<String>,
    range: Option<String>,
}
struct Origin {
    stop: Arc<AtomicBool>,
    offline: Arc<AtomicBool>,
    pause: Arc<AtomicBool>,
    partial: Arc<AtomicBool>,
    calls: Arc<Mutex<Vec<Call>>>,
    thread: Option<thread::JoinHandle<()>>,
}
impl Origin {
    fn start_with(path: &Path, length: u64, distinct: bool) -> Self {
        let listener = UnixListener::bind(path).unwrap();
        listener.set_nonblocking(true).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let offline = Arc::new(AtomicBool::new(false));
        let pause = Arc::new(AtomicBool::new(false));
        let partial = Arc::new(AtomicBool::new(false));
        let calls = Arc::new(Mutex::new(Vec::new()));
        let (stopping, disabled, paused, sent, observed) = (
            stop.clone(),
            offline.clone(),
            pause.clone(),
            partial.clone(),
            calls.clone(),
        );
        let thread = thread::spawn(move || {
            let mut connections = Vec::new();
            while !stopping.load(Ordering::Acquire) {
                match listener.accept() {
                    Ok((mut socket, _)) => {
                        let (stop, offline, pause, partial, calls) = (
                            stopping.clone(),
                            disabled.clone(),
                            paused.clone(),
                            sent.clone(),
                            observed.clone(),
                        );
                        connections.push(thread::spawn(move || {
                            socket.set_read_timeout(Some(Duration::from_millis(200))).unwrap();
                            socket.set_write_timeout(Some(TIMEOUT)).unwrap();
                            while !stop.load(Ordering::Acquire) {
                                let head = match read_head(&mut socket) {
                                    Ok(head) => head,
                                    Err(error) if matches!(error.kind(), io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut) => continue,
                                    Err(_) => return,
                                };
                                let fields = fields(&head);
                                let method = head.split_whitespace().next().unwrap();
                                let target = head.split_whitespace().nth(1).unwrap();
                                let key = target.strip_prefix("/v1/objects/").unwrap();
                                assert_eq!(key.len(), 64);
                                assert!(key.bytes().all(|b| b.is_ascii_hexdigit()));
                                if !distinct { assert_eq!(key, "ab".repeat(32)); }
                                assert_eq!(fields["authorization"], "fixture-credential");
                                assert_eq!(fields["racer-metadata"], "fixture-metadata");
                                calls.lock().unwrap().push(Call { method: method.into(), pin: fields.get("if-match").cloned(), range: fields.get("range").cloned() });
                                if offline.load(Ordering::Acquire) {
                                    let _ = write!(socket, "HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n");
                                    continue;
                                }
                                assert!(fields.get("if-match").is_none_or(|pin| pin == "\"restart-v1\""));
                                if method == "HEAD" {
                                    if write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {length}\r\nContent-Type: application/octet-stream\r\nETag: \"restart-v1\"\r\nRacer-Expires-At: 0\r\n\r\n").is_err() { return; }
                                    continue;
                                }
                                assert_eq!(method, "GET");
                                if length == 0 {
                                    if write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"restart-v1\"\r\nRacer-Expires-At: 0\r\n\r\n").is_err() { return; }
                                    continue;
                                }
                                let (first, last) = fields["range"].strip_prefix("bytes=").unwrap().split_once('-').unwrap();
                                let first: u64 = first.parse().unwrap();
                                let last: u64 = last.parse().unwrap();
                                assert_eq!(first % P, 0);
                                assert_eq!(last, first + P - 1);
                                let end = (last + 1).min(length);
                                if write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes {first}-{}/{length}\r\nETag: \"restart-v1\"\r\nRacer-Expires-At: 0\r\n\r\n", end - first, end - 1).is_err() { return; }
                                let mut offset = first;
                                let mut chunk = [0; 65536];
                                while offset < end {
                                    let n = chunk.len().min((end - offset) as usize);
                                    for (i, b) in chunk[..n].iter_mut().enumerate() { *b = if distinct { throughput::object_byte(u64::from_str_radix(&key[48..], 16).unwrap(), offset + i as u64) } else { byte(offset + i as u64) }; }
                                    if socket.write_all(&chunk[..n]).is_err() { return; }
                                    offset += n as u64;
                                    if pause.load(Ordering::Acquire) {
                                        partial.store(true, Ordering::Release);
                                        while pause.load(Ordering::Acquire) {
                                            if stop.load(Ordering::Acquire) { return; }
                                            thread::sleep(Duration::from_millis(2));
                                        }
                                    }
                                }
                            }
                        }));
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(2))
                    }
                    Err(error) => panic!("origin accept: {error}"),
                }
            }
            for connection in connections {
                connection.join().unwrap();
            }
        });
        Self {
            stop,
            offline,
            pause,
            partial,
            calls,
            thread: Some(thread),
        }
    }
    fn calls(&self) -> Vec<Call> {
        self.calls.lock().unwrap().clone()
    }
}
impl Drop for Origin {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            thread.join().unwrap();
        }
    }
}

#[test]
#[ignore = "requires root, private mount namespaces, io_uring and O_DIRECT"]
fn graceful_process_restart_recovers_encrypted_multipage_pin_without_origin() {
    let scratch = Scratch::new();
    let control = control::Control::start(&scratch.0);
    let (mut first, origin) = Process::start(&scratch, &control, 0);
    check(&first.request(None, 0, P), 0, P);
    check(&first.request(Some("\"restart-v1\""), P, LENGTH), P, LENGTH);
    let calls = origin.calls();
    assert_eq!(
        calls.len(),
        4,
        "metadata HEAD plus three page GETs: {calls:?}"
    );
    assert_eq!(calls[0].method, "HEAD");
    assert_eq!(calls[0].pin, None);
    for (i, call) in calls.iter().skip(1).enumerate() {
        assert_eq!(call.method, "GET");
        assert_eq!(
            call.range.as_deref(),
            Some(format!("bytes={}-{}", i as u64 * P, (i as u64 + 1) * P - 1).as_str())
        );
        assert_eq!(call.pin.as_deref(), Some("\"restart-v1\""));
    }
    let identity = enrolled_identity(&scratch, &control, 1);
    assert!(
        first.stop(libc::SIGTERM).success(),
        "graceful exit: {}",
        first.logs()
    );
    assert!(
        !scratch
            .0
            .join(format!("run-0/racer/{NAME}/client/socket"))
            .exists(),
        "graceful shutdown must unlink its listener"
    );
    let checkpoint = fs::read(scratch.0.join("slabs/checkpoint.0")).unwrap();
    let image = checkpoint::decode(&checkpoint).unwrap();
    assert_eq!(image.shards.len(), 1);
    assert_eq!(
        image.shards[0].index.entries.len(),
        3,
        "graceful cut must contain every encrypted page"
    );
    for (page, entry) in &image.shards[0].index.entries {
        assert_eq!(page.version.object.cache.0, CACHE);
        assert_eq!(entry.key_id.0, [7; 16]);
    }
    drop(origin);
    let (mut second, origin) = Process::start(&scratch, &control, 1);
    origin.offline.store(true, Ordering::Release);
    assert_ne!(first.child.id(), second.child.id());
    let renewed = enrolled_identity(&scratch, &control, 2);
    assert_ne!(
        renewed.certificate_chain(),
        identity.certificate_chain(),
        "restart must persist fresh issuance for the authenticated Node binding"
    );
    assert_eq!(second.metric("racer_disk_hits_total"), 0);
    check(
        &second.request(Some("\"restart-v1\""), 0, LENGTH),
        0,
        LENGTH,
    );
    assert_eq!(
        second.metric("racer_disk_hits_total"),
        3,
        "fresh process must decrypt every recovered slab page"
    );
    assert_eq!(second.metric("racer_origin_fills_total"), 0);
    assert!(
        origin.calls().is_empty(),
        "recovered pin contacted origin: {:?}",
        origin.calls()
    );
    assert!(second.stop(libc::SIGTERM).success(), "{}", second.logs());
}

#[test]
#[ignore = "requires root, private mount namespaces, io_uring and O_DIRECT"]
fn interrupted_process_restart_refetches_safely_after_partial_origin_body() {
    let scratch = Scratch::new();
    let control = control::Control::start(&scratch.0);
    let (mut first, origin) = Process::start(&scratch, &control, 0);
    let identity = enrolled_identity(&scratch, &control, 1);
    check(&first.request(None, 0, P), 0, P);
    let deadline = Instant::now() + TIMEOUT;
    while fs::metadata(scratch.0.join("slabs/worker-0-slab-0.dat"))
        .unwrap()
        .blocks()
        == 0
    {
        first.assert_running();
        assert!(
            Instant::now() < deadline,
            "first page never reached slab storage"
        );
        thread::sleep(Duration::from_millis(10));
    }
    origin.pause.store(true, Ordering::Release);
    let mut interrupted = first.connect();
    write_request(&mut interrupted, Some("\"restart-v1\""), P, LENGTH);
    let deadline = Instant::now() + TIMEOUT;
    while !origin.partial.load(Ordering::Acquire) {
        first.assert_running();
        assert!(Instant::now() < deadline, "origin never sent partial page");
        thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(first.stop(libc::SIGKILL).signal(), Some(libc::SIGKILL));
    drop(interrupted);
    drop(origin);
    assert!(!scratch.0.join("slabs/checkpoint.0").exists());
    assert!(!scratch.0.join("slabs/checkpoint.1").exists());
    let (mut second, origin) = Process::start(&scratch, &control, 1);
    assert_ne!(first.child.id(), second.child.id());
    let renewed = enrolled_identity(&scratch, &control, 2);
    assert_ne!(
        renewed.certificate_chain(),
        identity.certificate_chain(),
        "crash recovery must reauthenticate rather than reuse the disk certificate"
    );
    check(
        &second.request(Some("\"restart-v1\""), 0, LENGTH),
        0,
        LENGTH,
    );
    let calls = origin.calls();
    assert!(
        calls
            .iter()
            .all(|call| call.pin.as_deref() == Some("\"restart-v1\""))
    );
    let mut ranges: Vec<_> = calls
        .iter()
        .filter(|call| call.method == "GET")
        .map(|call| call.range.clone().unwrap())
        .collect();
    ranges.sort();
    let mut expected: Vec<_> = (0..3)
        .map(|i| format!("bytes={}-{}", i * P, (i + 1) * P - 1))
        .collect();
    expected.sort();
    assert_eq!(
        ranges, expected,
        "uncheckpointed pages must be safe misses/refetches"
    );
    assert_eq!(second.metric("racer_disk_hits_total"), 0);
    assert_eq!(second.metric("racer_origin_fills_total"), 3);
    origin.offline.store(true, Ordering::Release);
    let before = origin.calls().len();
    check(
        &second.request(Some("\"restart-v1\""), P - 17, P + 113),
        P - 17,
        P + 113,
    );
    assert_eq!(
        origin.calls().len(),
        before,
        "refetched bytes were not cached"
    );
    assert!(second.stop(libc::SIGTERM).success(), "{}", second.logs());
}

#[test]
#[ignore = "requires root, private mount namespaces, io_uring and O_DIRECT"]
fn periodic_checkpoint_sigkill_recovers_older_pages_and_bounds_recent_loss() {
    let scratch = Scratch::new();
    let control = control::Control::start(&scratch.0);
    let (mut first, origin) = Process::start(&scratch, &control, 0);
    check(&first.request(None, 0, P), 0, P);
    let deadline = Instant::now() + TIMEOUT;
    while first.metric("racer_checkpoint_sequence") == 0 {
        first.assert_running();
        assert!(
            Instant::now() < deadline,
            "periodic checkpoint stalled: {}",
            first.logs()
        );
        thread::sleep(Duration::from_millis(20));
    }
    // A new completed page and a partial page are outside the recent cut.
    check(&first.request(Some("\"restart-v1\""), P, 2 * P), P, 2 * P);
    origin.pause.store(true, Ordering::Release);
    let mut interrupted = first.connect();
    write_request(&mut interrupted, Some("\"restart-v1\""), 2 * P, LENGTH);
    assert_eq!(first.stop(libc::SIGKILL).signal(), Some(libc::SIGKILL));
    drop(interrupted);
    drop(origin);
    let (mut second, origin) = Process::start(&scratch, &control, 1);
    origin.offline.store(true, Ordering::Release);
    check(&second.request(Some("\"restart-v1\""), 0, P), 0, P);
    assert_eq!(second.metric("racer_disk_hits_total"), 1);
    assert!(origin.calls().is_empty(), "checkpointed page refetched");
    origin.offline.store(false, Ordering::Release);
    check(
        &second.request(Some("\"restart-v1\""), P, LENGTH),
        P,
        LENGTH,
    );
    assert!(
        origin.calls().len() <= 2,
        "loss exceeded post-checkpoint pages"
    );
    assert!(second.stop(libc::SIGTERM).success(), "{}", second.logs());
}
