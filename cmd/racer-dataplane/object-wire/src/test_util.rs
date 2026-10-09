//! Controllable local origin server for protocol and application integration tests.
//! Uses real HTTP bytes on a Unix socket; no application reactor or quotas are needed.

use crate::model::{ObjectMetadata, PAGE_BYTES};
use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant, UNIX_EPOCH};

/// Preserve the application's strict separator rules for opaque origin context.
struct Opaque;

impl http1::Opaque for Opaque {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}

type Codec = http1::Codec<Opaque>;

/// Origin operation observed by the fixture.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum RequestKind {
    Head,
    InitialGet,
    PinnedGet,
}

/// Recorded request identity, without sensitive context headers.
#[derive(Clone, Debug)]
pub struct Call {
    pub kind: RequestKind,

    pub page: u64,

    pub if_match: Option<Vec<u8>>,
}

/// Shared scripted state for accepted connections.
struct State {
    metadata: ObjectMetadata,

    missing: bool,

    body: Option<Vec<u8>>,

    calls: Vec<Call>,

    completed: Vec<Call>,

    rejections: BTreeMap<RequestKind, VecDeque<u16>>,

    rejected_pages: BTreeMap<u64, u16>,

    blocked: BTreeSet<RequestKind>,

    delays: BTreeMap<RequestKind, Duration>,
}

/// An owned origin socket server; drop stops and joins all connection threads.
pub struct AdapterOrigin {
    state: Arc<Mutex<State>>,

    stop: Arc<AtomicBool>,

    server: Option<JoinHandle<()>>,

    directory: PathBuf,

    // Keep the short /proc path valid even for long worktree names.
    _directory_fd: File,

    pub root: PathBuf,
}

impl AdapterOrigin {
    /// Without an explicit body, three-byte objects contain `abc`; other pages
    /// contain their page number repeated, without allocating the complete object.
    pub fn new(cache_name: &str, metadata: ObjectMetadata) -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        assert!(!cache_name.contains('/') && !cache_name.is_empty());
        let directory = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "adapter-origin-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(directory.join(cache_name).join("origin")).unwrap();
        let directory_fd = File::open(&directory).unwrap();
        let root = PathBuf::from(format!(
            "/proc/{}/fd/{}",
            std::process::id(),
            directory_fd.as_raw_fd()
        ));
        let listener = UnixListener::bind(root.join(cache_name).join("origin/socket")).unwrap();
        listener.set_nonblocking(true).unwrap();
        let state = Arc::new(Mutex::new(State {
            metadata,
            missing: false,
            body: None,
            calls: vec![],
            completed: vec![],
            rejections: BTreeMap::new(),
            rejected_pages: BTreeMap::new(),
            blocked: BTreeSet::new(),
            delays: BTreeMap::new(),
        }));
        let stop = Arc::new(AtomicBool::new(false));
        let server = {
            let state = state.clone();
            let stop = stop.clone();
            thread::spawn(move || {
                let mut connections = vec![];
                while !stop.load(Ordering::Acquire) {
                    match listener.accept() {
                        Ok((stream, _)) => {
                            let state = state.clone();
                            let stop = stop.clone();
                            connections.push(thread::spawn(move || serve(stream, &state, &stop)));
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            thread::sleep(Duration::from_millis(1))
                        }
                        Err(error) => panic!("adapter accept: {error}"),
                    }
                }
                for connection in connections {
                    connection.join().unwrap();
                }
            })
        };
        Self {
            state,
            stop,
            server: Some(server),
            directory,
            _directory_fd: directory_fd,
            root,
        }
    }

    /// Replace the current metadata returned to new requests.
    pub fn set_version(&self, metadata: ObjectMetadata) {
        self.state.lock().unwrap().metadata = metadata;
    }

    /// Missing current objects return 404; unsatisfied explicit pins return 412.
    pub fn set_missing(&self, missing: bool) {
        self.state.lock().unwrap().missing = missing;
    }

    /// Set explicit content matching the current version length.
    pub fn set_body(&self, body: Vec<u8>) {
        let mut state = self.state.lock().unwrap();
        assert_eq!(body.len() as u64, state.metadata.length);
        state.body = Some(body);
    }

    /// Return one scripted error for the next matching operation.
    pub fn reject_next(&self, kind: RequestKind, status: u16) {
        assert!((400..600).contains(&status));
        self.state
            .lock()
            .unwrap()
            .rejections
            .entry(kind)
            .or_default()
            .push_back(status);
    }

    /// Reject every GET for this page, including retries, without failing HEAD.
    pub fn reject_page(&self, page: u64, status: u16) {
        assert!((400..600).contains(&status));
        self.state
            .lock()
            .unwrap()
            .rejected_pages
            .insert(page, status);
    }

    /// Hold matching requests until released or the fixture is dropped.
    pub fn block(&self, kind: RequestKind) {
        self.state.lock().unwrap().blocked.insert(kind);
    }

    /// Resume held requests of this kind.
    pub fn release(&self, kind: RequestKind) {
        self.state.lock().unwrap().blocked.remove(&kind);
    }

    /// Delay responses of this kind by the supplied interval.
    pub fn delay(&self, kind: RequestKind, delay: Duration) {
        self.state.lock().unwrap().delays.insert(kind, delay);
    }

    /// Snapshot accepted requests.
    pub fn calls(&self) -> Vec<Call> {
        self.state.lock().unwrap().calls.clone()
    }

    /// Count accepted requests of one kind.
    pub fn count(&self, kind: RequestKind) -> usize {
        self.calls().iter().filter(|call| call.kind == kind).count()
    }

    /// Count requests that have passed their delay and blocking gates.
    pub fn completed(&self, kind: RequestKind) -> usize {
        self.state
            .lock()
            .unwrap()
            .completed
            .iter()
            .filter(|call| call.kind == kind)
            .count()
    }
}

impl Drop for AdapterOrigin {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        self.server.take().unwrap().join().unwrap();
        std::fs::remove_dir_all(&self.directory).unwrap();
    }
}

/// Serve one bounded request and close the connection after its response.
fn serve(mut stream: UnixStream, state: &Mutex<State>, stop: &AtomicBool) {
    stream
        .set_read_timeout(Some(Duration::from_millis(20)))
        .unwrap();
    stream
        .set_write_timeout(Some(Duration::from_millis(100)))
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let mut request = Vec::new();
    while !request.ends_with(b"\r\n\r\n") {
        if stop.load(Ordering::Acquire) || Instant::now() >= deadline {
            return;
        }
        let mut byte = [0];
        match stream.read(&mut byte) {
            Ok(0) => return,
            Ok(_) => request.push(byte[0]),
            Err(error)
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                ) =>
            {
                continue;
            }
            Err(_) => return,
        }
        assert!(request.len() <= 32768, "oversized adapter request");
    }
    let request = Codec::new(32768).decode_head(&request).unwrap().unwrap().0;
    let head =
        matches!(&request.start, http1::StartLine::Request { method, .. } if method == "HEAD");
    let if_match = request.unique("If-Match").unwrap().map(<[u8]>::to_vec);
    let first = request
        .unique("Range")
        .unwrap()
        .map(|range| {
            std::str::from_utf8(range)
                .unwrap()
                .strip_prefix("bytes=")
                .unwrap()
                .split('-')
                .next()
                .unwrap()
                .parse::<u64>()
                .unwrap()
        })
        .unwrap_or(0);
    let kind = if head {
        RequestKind::Head
    } else if if_match.is_some() {
        RequestKind::PinnedGet
    } else {
        RequestKind::InitialGet
    };
    let call = Call {
        kind,
        page: first / PAGE_BYTES,
        if_match,
    };
    let started = Instant::now();
    state.lock().unwrap().calls.push(call.clone());
    loop {
        if stop.load(Ordering::Acquire) || Instant::now() >= deadline {
            return;
        }
        let state = state.lock().unwrap();
        let blocked = state.blocked.contains(&kind)
            || started.elapsed() < state.delays.get(&kind).copied().unwrap_or_default();
        drop(state);
        if !blocked {
            break;
        }
        thread::sleep(Duration::from_millis(1));
    }
    let mut state = state.lock().unwrap();
    state.completed.push(call.clone());
    let rejected = state
        .rejections
        .entry(kind)
        .or_default()
        .pop_front()
        .or_else(|| {
            (!head)
                .then(|| state.rejected_pages.get(&call.page).copied())
                .flatten()
        });
    let metadata = &state.metadata;
    let status = rejected.unwrap_or_else(|| {
        if state.missing {
            if call.if_match.is_some() { 412 } else { 404 }
        } else if call
            .if_match
            .as_deref()
            .is_some_and(|etag| etag != metadata.version.etag.as_bytes())
        {
            412
        } else if !head && first >= metadata.length && metadata.length != 0 {
            416
        } else if head || metadata.length == 0 {
            200
        } else {
            206
        }
    });
    let length = if head {
        metadata.length
    } else {
        metadata.length.saturating_sub(first).min(PAGE_BYTES)
    };
    let mut response = format!("HTTP/1.1 {status} Fixture\r\nConnection: close\r\n");
    let body = if status >= 400 {
        response.push_str("Content-Length: 0\r\n\r\n");
        vec![]
    } else {
        response.push_str(&format!(
            "Content-Length: {length}\r\nETag: {}\r\nRacer-Expires-At: {}\r\n",
            std::str::from_utf8(metadata.version.etag.as_bytes()).unwrap(),
            metadata
                .expires_at
                .as_system_time()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_millis()
        ));
        if let Some(content_type) = &metadata.content_type {
            response.push_str(&format!(
                "Racer-Content-Type: {}\r\n",
                content_type.as_str()
            ));
        }
        if !head {
            response.push_str("Content-Type: application/octet-stream\r\n");
        }
        if !head && length != 0 {
            response.push_str(&format!(
                "Content-Range: bytes {first}-{}/{}\r\n",
                first + length - 1,
                metadata.length
            ));
        }
        response.push_str("\r\n");
        if head {
            vec![]
        } else if let Some(body) = &state.body {
            body[first as usize..(first + length) as usize].to_vec()
        } else if metadata.length == 3 {
            b"abc".to_vec()
        } else {
            vec![call.page as u8; length as usize]
        }
    };
    drop(state);
    if stream.write_all(response.as_bytes()).is_ok() {
        let _ = stream.write_all(&body);
    }
}
