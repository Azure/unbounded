//! Real executable restart coverage. Run explicitly as root on Linux; see
//! process/README.md. All writable state lives beneath this crate's target/.
#![cfg(target_os = "linux")]

#[path = "process/control.rs"]
mod control;

use racer_dataplane::{model::range::PAGE_BYTES, store::checkpoint_format::CheckpointCodec};
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
}
impl Process {
    fn start(scratch: &Scratch, control: &control::Control, incarnation: usize) -> (Self, Origin) {
        // This fixture gives each process fresh runtime sockets and shares identity/
        // and slabs/. It does not cover stale sockets on deployment's hostPath.
        let run = scratch.0.join(format!("run-{incarnation}"));
        let cache = run.join("racer").join(NAME);
        fs::create_dir_all(cache.join("origin")).unwrap();
        fs::create_dir_all(cache.join("client")).unwrap();
        let socket_directory = fs::File::open(&cache).unwrap();
        let alias = PathBuf::from(format!("/proc/self/fd/{}", socket_directory.as_raw_fd()));
        let origin = Origin::start(&alias.join("origin/socket"));
        let peer = TcpListener::bind("127.0.0.1:0").unwrap();
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
            ("RACER_SECRET_DIRECTORY", "secrets"),
            ("RACER_IDENTITY_DIRECTORY", "identity"),
            ("RACER_SLAB_DIRECTORY", "slabs"),
        ] {
            command.env(name, scratch.0.join(path));
        }
        command.envs([
            ("RACER_MAX_THREADS", "2"),
            ("RACER_ENABLE_RDMA", "false"),
            ("RACER_PLAINTEXT_BYTES", "67108864"),
            ("RACER_CIPHERTEXT_BYTES", "167772160"),
            ("RACER_DIRTY_BYTES", "67108864"),
            ("RACER_REGISTERED_BYTES", "1"),
            ("RACER_REQUEST_CONTEXT_BYTES", "1048576"),
            ("RACER_SLAB_BYTES", "268435456"),
            ("RACER_SEGMENT_BYTES", "67108864"),
            ("RACER_FREE_SEGMENT_RESERVE", "1"),
            ("RACER_QUEUE_ENTRIES", "16"),
            ("RACER_CLIENT_CONNECTIONS", "8"),
            ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "2"),
            ("RACER_METADATA_ENTRIES", "32"),
            ("RACER_FLIGHTS", "8"),
            ("RACER_PIPES", "2"),
            ("RACER_RANGE_WINDOW_PAGES", "1"),
            ("RACER_REQUEST_TIMEOUT_MS", "20000"),
            ("RACER_READER_STALL_TIMEOUT_MS", "10000"),
            ("RACER_SHUTDOWN_TIMEOUT_MS", "10000"),
        ]);
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
        (process, origin)
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
        read_reply(&mut socket).expect("complete client response before deadline")
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

fn read_head(stream: &mut impl Read) -> io::Result<String> {
    let mut head = Vec::new();
    while !head.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        stream.read_exact(&mut byte)?;
        head.push(byte[0]);
        assert!(head.len() <= 32768, "oversized HTTP head");
    }
    Ok(String::from_utf8(head).unwrap())
}
fn fields(head: &str) -> BTreeMap<String, String> {
    head.lines()
        .skip(1)
        .filter_map(|line| line.split_once(':'))
        .map(|(key, value)| (key.to_ascii_lowercase(), value.trim().to_owned()))
        .collect()
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
    write!(socket, "GET /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\n{pin}Range: bytes={start}-{}\r\nConnection: close\r\n\r\n", "ab".repeat(32), end - 1).unwrap();
}
fn byte(offset: u64) -> u8 {
    ((offset * 31 + offset / P * 17) % 251) as u8
}
fn check(reply: &Reply, start: u64, end: u64) {
    assert_eq!(reply.status, 206);
    assert_eq!(reply.fields["etag"], "\"restart-v1\"");
    assert_eq!(
        reply.fields["content-range"],
        format!("bytes {start}-{}/{LENGTH}", end - 1)
    );
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
        model::identity::ClusterId,
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
    let bundle =
        wire::decode_bundle(&fs::read(scratch.0.join("secrets/epoch/bundle.json")).unwrap())
            .unwrap();
    enrollment
        .set_peer_trust_roots(bundle.peer_trust_roots)
        .unwrap();
    // Verify the persisted certificate's chain, SAN, validity, request correlation,
    // and local key pairing rather than comparing opaque identity.json bytes.
    let identity = enrollment.load_identity().unwrap().unwrap();
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
    fn start(path: &Path) -> Self {
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
                                assert_eq!(head.split_whitespace().nth(1).unwrap(), format!("/v1/objects/{}", "ab".repeat(32)));
                                assert_eq!(fields["authorization"], "fixture-credential");
                                assert_eq!(fields["racer-metadata"], "fixture-metadata");
                                calls.lock().unwrap().push(Call { method: method.into(), pin: fields.get("if-match").cloned(), range: fields.get("range").cloned() });
                                if offline.load(Ordering::Acquire) {
                                    let _ = write!(socket, "HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n");
                                    continue;
                                }
                                assert!(fields.get("if-match").is_none_or(|pin| pin == "\"restart-v1\""));
                                if method == "HEAD" {
                                    if write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {LENGTH}\r\nContent-Type: application/octet-stream\r\nETag: \"restart-v1\"\r\nRacer-Expires-At: 0\r\n\r\n").is_err() { return; }
                                    continue;
                                }
                                assert_eq!(method, "GET");
                                let (first, last) = fields["range"].strip_prefix("bytes=").unwrap().split_once('-').unwrap();
                                let first: u64 = first.parse().unwrap();
                                let last: u64 = last.parse().unwrap();
                                assert_eq!(first % P, 0);
                                assert_eq!(last, first + P - 1);
                                let end = (last + 1).min(LENGTH);
                                if write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes {first}-{}/{LENGTH}\r\nETag: \"restart-v1\"\r\nRacer-Expires-At: 0\r\n\r\n", end - first, end - 1).is_err() { return; }
                                let mut offset = first;
                                let mut chunk = [0; 65536];
                                while offset < end {
                                    let n = chunk.len().min((end - offset) as usize);
                                    for (i, b) in chunk[..n].iter_mut().enumerate() { *b = byte(offset + i as u64); }
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
#[ignore = "requires root, private mount namespaces, io_uring and O_DIRECT; see process/README.md"]
fn graceful_process_restart_recovers_encrypted_multipage_pin_without_origin() {
    let scratch = Scratch::new();
    let control = control::Control::start(&scratch.0);
    let (mut first, origin) = Process::start(&scratch, &control, 0);
    check(&first.request(None, 0, P), 0, P);
    check(&first.request(Some("\"restart-v1\""), P, LENGTH), P, LENGTH);
    let calls = origin.calls();
    assert_eq!(calls.len(), 3);
    assert_eq!(calls[0].pin, None);
    for (i, call) in calls.iter().enumerate() {
        assert_eq!(call.method, "GET");
        assert_eq!(
            call.range.as_deref(),
            Some(format!("bytes={}-{}", i as u64 * P, (i as u64 + 1) * P - 1).as_str())
        );
        if i != 0 {
            assert_eq!(call.pin.as_deref(), Some("\"restart-v1\""));
        }
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
    let image = CheckpointCodec.decode(&checkpoint).unwrap();
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
#[ignore = "requires root, private mount namespaces, io_uring and O_DIRECT; see process/README.md"]
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
