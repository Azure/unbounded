//! Opt-in real Application + Go SDK sustained connection rotation gate.
use super::*;

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
