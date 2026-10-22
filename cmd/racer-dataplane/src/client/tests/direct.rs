//! `host-read` and `gpu-read` over the real client socket with a fake origin.
use super::*;
use crate::client::direct::Destination;
use crate::client::direct::DirectPage;
use crate::client::direct::DirectSink;
use crate::client::direct::GpuService;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::model::PAGE_BYTES;
use std::io::Seek;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::fd::RawFd;

static NAMES: AtomicUsize = AtomicUsize::new(0);

/// A memfd with a process-unique name, so open copies can be counted.
struct Memfd {
    file: File,
    name: String,
}

impl Memfd {
    fn new() -> Self {
        let name = format!(
            "racer-direct-{}-{}",
            std::process::id(),
            NAMES.fetch_add(1, Ordering::Relaxed)
        );
        let c_name = CString::new(name.clone()).unwrap();
        let fd = unsafe { libc::memfd_create(c_name.as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        Self {
            file: unsafe { File::from_raw_fd(fd) },
            name,
        }
    }

    /// Descriptors in this process that refer to this memfd, including ours.
    fn open_copies(&self) -> usize {
        let exact = format!("/memfd:{} (deleted)", self.name);
        fs::read_dir("/proc/self/fd")
            .unwrap()
            .filter_map(|entry| fs::read_link(entry.ok()?.path()).ok())
            .filter(|target| target.to_string_lossy() == exact)
            .count()
    }

    fn contents(&mut self) -> Vec<u8> {
        let mut output = Vec::new();
        self.file.seek(std::io::SeekFrom::Start(0)).unwrap();
        self.file.read_to_end(&mut output).unwrap();
        output
    }
}

/// Send `bytes` with `fds` attached as one `SCM_RIGHTS` message.
fn send_with_fds(socket: &UnixStream, bytes: &[u8], fds: &[RawFd]) {
    let payload = std::mem::size_of_val(fds);
    let space = unsafe { libc::CMSG_SPACE(payload as u32) } as usize;
    let mut control = vec![0u64; space.div_ceil(8)];
    let mut iov = libc::iovec {
        iov_base: bytes.as_ptr().cast_mut().cast(),
        iov_len: bytes.len(),
    };
    let mut header: libc::msghdr = unsafe { std::mem::zeroed() };
    header.msg_iov = &mut iov;
    header.msg_iovlen = 1;
    if !fds.is_empty() {
        header.msg_control = control.as_mut_ptr().cast();
        header.msg_controllen = space as _;
        unsafe {
            let cmsg = libc::CMSG_FIRSTHDR(&header);
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(payload as u32) as _;
            std::ptr::copy_nonoverlapping(
                fds.as_ptr().cast::<u8>(),
                libc::CMSG_DATA(cmsg),
                payload,
            );
        }
    }
    let sent = unsafe { libc::sendmsg(socket.as_raw_fd(), &header, 0) };
    assert_eq!(sent, bytes.len() as isize);
}

fn direct_request(suffix: &str, fields: &str) -> Vec<u8> {
    format!(
        "POST /v1/objects/{}/{suffix} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n{fields}\r\n",
        "0".repeat(64)
    )
    .into_bytes()
}

/// A fixture whose fake origin serves `body` as version "v1".
fn direct_fixture(body: Vec<u8>, gpu: Option<Rc<dyn GpuService>>) -> Fixture {
    let mut fixture = Fixture::new();
    let delivery = Rc::new(Delivery::new(
        fixture.reactor.clone(),
        Duration::from_secs(2),
    ));
    let mut responses = Responses::new(fixture.listeners.io.clone(), delivery.clone());
    if let Some(gpu) = gpu {
        responses = responses.with_gpu(gpu);
    }
    fixture.listeners.responses = Rc::new(responses);
    let worker = ReadWorker::new(
        definition(),
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: definition().id,
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            },
            length: body.len() as u64,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH + Duration::from_millis(1234))
                .unwrap(),
        },
        fixture.listeners.admission.clone(),
        fixture.reactor.clone(),
        delivery,
        1,
    );
    worker.origin.set_body(body);
    fixture.listeners.reads = worker.coordinator.clone();
    fixture.worker = Some(worker);
    fixture.reconcile(&[definition()]).unwrap();
    fixture
}

/// Split a complete response into its lowercased head and body bytes.
fn split(output: &[u8]) -> (String, &[u8]) {
    let end = output
        .windows(4)
        .position(|part| part == b"\r\n\r\n")
        .expect("response head")
        + 4;
    (
        std::str::from_utf8(&output[..end])
            .unwrap()
            .to_ascii_lowercase(),
        &output[end..],
    )
}

/// Decode 21-byte frames: (kind, number, offset, length).
fn frames(body: &[u8]) -> Vec<(u8, u64, u64, u32)> {
    assert_eq!(body.len() % 21, 0, "body is whole frames");
    body.chunks(21)
        .map(|frame| {
            (
                frame[0],
                u64::from_be_bytes(frame[1..9].try_into().unwrap()),
                u64::from_be_bytes(frame[9..17].try_into().unwrap()),
                u32::from_be_bytes(frame[17..].try_into().unwrap()),
            )
        })
        .collect()
}

/// Send one request with `fds` and read the response to EOF.
fn exchange(fixture: &Fixture, request: &[u8], fds: &[RawFd]) -> Vec<u8> {
    let mut socket = fixture.connect();
    send_with_fds(&socket, request, fds);
    fixture.receive(&mut socket, true)
}

/// Pump until the connection and its descriptors are gone. Completions can
/// arrive late on a loaded host, so wait on a deadline, not a pump count.
fn settle(fixture: &Fixture) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        fixture.pump(16);
        if fixture.listeners.active_connections() == 0 {
            break;
        }
        assert!(Instant::now() < deadline, "connection did not close");
        std::thread::yield_now();
    }
    for _ in 0..8 {
        fixture.pump(16);
    }
}

#[test]
fn host_read_writes_the_range_at_the_destination_offset() {
    let fixture = direct_fixture(b"hello".to_vec(), None);
    let mut memfd = Memfd::new();
    memfd.file.write_all(&[b'.'; 16]).unwrap();
    let output = exchange(
        &fixture,
        &direct_request(
            "host-read",
            "Range: bytes=1-3\r\nIf-Match: \"v1\"\r\nRacer-Destination-Offset: 10\r\n\
             Racer-Destination-Length: 3\r\nRacer-Page-Credits: 1\r\n",
        ),
        &[memfd.file.as_raw_fd()],
    );
    let (head, body) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(head.contains("racer-delivery: host\r\n"), "{head}");
    assert!(head.contains("content-length: 42\r\n"), "{head}");
    assert!(head.contains("racer-range-start: 1\r\n"), "{head}");
    assert!(head.contains("racer-range-end: 4\r\n"), "{head}");
    assert!(head.contains("connection: close\r\n"), "{head}");
    assert_eq!(frames(body), [(1, 0, 1, 3), (2, 1, 3, 0)]);
    assert_eq!(memfd.contents(), b"..........ell...");
    settle(&fixture);
    assert_eq!(memfd.open_copies(), 1, "dataplane closed its copy");
    assert_no_body_leases(&fixture);
}

#[test]
fn host_read_spans_pages_and_whole_objects() {
    let length = PAGE_BYTES as usize + 3;
    let body: Vec<u8> = (0..length).map(|i| (i % 251) as u8).collect();
    let fixture = direct_fixture(body.clone(), None);
    let mut memfd = Memfd::new();
    let output = exchange(
        &fixture,
        &direct_request(
            "host-read",
            &format!("Racer-Destination-Offset: 0\r\nRacer-Destination-Length: {length}\r\n"),
        ),
        &[memfd.file.as_raw_fd()],
    );
    let (head, body_frames) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(head.contains("content-length: 63\r\n"), "{head}");
    assert_eq!(
        frames(body_frames),
        [
            (1, 0, 0, PAGE_BYTES as u32),
            (1, 1, PAGE_BYTES, 3),
            (2, 2, length as u64, 0)
        ]
    );
    assert!(memfd.contents() == body, "destination matches the object");
    settle(&fixture);
    assert_eq!(memfd.open_copies(), 1);
}

#[test]
fn host_read_descriptor_errors_are_400_and_close_descriptors() {
    let fields = "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\n";
    let fixture = direct_fixture(b"hello".to_vec(), None);
    let memfd = Memfd::new();
    let fd = memfd.file.as_raw_fd();
    let plain = request("POST", "");
    for (request, fds) in [
        // Missing descriptor.
        (direct_request("host-read", fields), vec![]),
        // Two descriptors.
        (direct_request("host-read", fields), vec![fd, fd]),
        // A descriptor on a plain Get.
        (plain.clone(), vec![fd]),
        // A descriptor on a HEAD.
        (request("HEAD", ""), vec![fd]),
        // Missing destination field.
        (
            direct_request("host-read", "Racer-Destination-Offset: 0\r\n"),
            vec![fd],
        ),
    ] {
        let output = exchange(&fixture, &request, &fds);
        let (head, body) = split(&output);
        assert!(
            head.starts_with("http/1.1 400"),
            "{head} {:?}",
            String::from_utf8_lossy(&request)
        );
        assert!(body.is_empty());
        settle(&fixture);
        assert_eq!(
            memfd.open_copies(),
            1,
            "{}",
            String::from_utf8_lossy(&request)
        );
    }
    // Plain requests without descriptors still work.
    let output = exchange(&fixture, &plain, &[]);
    assert!(split(&output).0.starts_with("http/1.1 200"));
}

#[test]
fn host_read_rejects_non_regular_and_read_only_descriptors() {
    let fields = "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\n";
    let fixture = direct_fixture(b"hello".to_vec(), None);
    let mut fds = [0; 2];
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
    let mut read = unsafe { File::from_raw_fd(fds[0]) };
    let write = unsafe { OwnedFd::from_raw_fd(fds[1]) };
    let output = exchange(
        &fixture,
        &direct_request("host-read", fields),
        &[write.as_raw_fd()],
    );
    assert!(split(&output).0.starts_with("http/1.1 400"));
    settle(&fixture);
    drop(write);
    let mut byte = [0];
    assert_eq!(read.read(&mut byte).unwrap(), 0, "pipe write end leaked");

    let memfd = Memfd::new();
    let path = format!("/proc/self/fd/{}", memfd.file.as_raw_fd());
    let read_only = File::open(path).unwrap();
    let output = exchange(
        &fixture,
        &direct_request("host-read", fields),
        &[read_only.as_raw_fd()],
    );
    assert!(split(&output).0.starts_with("http/1.1 400"));
    settle(&fixture);
    drop(read_only);
    assert_eq!(memfd.open_copies(), 1);
}

#[test]
fn host_read_range_and_version_errors_keep_get_statuses() {
    let fixture = direct_fixture(b"hello".to_vec(), None);
    let mut memfd = Memfd::new();
    for (fields, status) in [
        // Destination smaller than the range.
        (
            "Range: bytes=0-4\r\nRacer-Destination-Offset: 0\r\nRacer-Destination-Length: 4\r\n",
            "400",
        ),
        (
            "If-Match: \"v2\"\r\nRacer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\n",
            "412",
        ),
        (
            "Range: bytes=9-\r\nRacer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\n",
            "416",
        ),
    ] {
        let output = exchange(
            &fixture,
            &direct_request("host-read", fields),
            &[memfd.file.as_raw_fd()],
        );
        let (head, _) = split(&output);
        assert!(head.starts_with(&format!("http/1.1 {status}")), "{head}");
        settle(&fixture);
        assert_eq!(memfd.open_copies(), 1, "{fields}");
    }
    assert!(memfd.contents().is_empty(), "nothing was written");
    assert_no_body_leases(&fixture);
}

#[test]
fn host_read_client_disconnect_closes_descriptor_and_releases_pages() {
    let length = 2 * PAGE_BYTES as usize + 1;
    let fixture = direct_fixture(vec![b'x'; length], None);
    let memfd = Memfd::new();
    let mut socket = fixture.connect();
    send_with_fds(
        &socket,
        &direct_request(
            "host-read",
            &format!("Racer-Destination-Offset: 0\r\nRacer-Destination-Length: {length}\r\n"),
        ),
        &[memfd.file.as_raw_fd()],
    );
    let output = fixture.receive(&mut socket, false);
    assert!(split(&output).0.starts_with("http/1.1 200"));
    drop(socket);
    settle(&fixture);
    assert_eq!(memfd.open_copies(), 1);
    fixture.listeners.admission.reclaim_buffers();
    assert_no_body_leases(&fixture);
}

#[test]
fn gpu_read_is_validated_then_501_without_a_backend() {
    let fixture = direct_fixture(b"hello".to_vec(), None);
    let memfd = Memfd::new();
    let fd = memfd.file.as_raw_fd();
    for (fields, fds, status) in [
        (
            "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\nRacer-Allocation-Size: 5\r\n",
            vec![fd],
            "501",
        ),
        // Validation precedes the 501.
        (
            "Racer-Destination-Offset: 1\r\nRacer-Destination-Length: 5\r\nRacer-Allocation-Size: 5\r\n",
            vec![fd],
            "400",
        ),
        (
            "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\nRacer-Allocation-Size: 5\r\n",
            vec![],
            "400",
        ),
    ] {
        let output = exchange(&fixture, &direct_request("gpu-read", fields), &fds);
        let (head, body) = split(&output);
        assert!(head.starts_with(&format!("http/1.1 {status}")), "{head}");
        assert!(body.is_empty());
        settle(&fixture);
        assert_eq!(memfd.open_copies(), 1);
    }
}

/// Records what a GPU backend would be asked to do.
#[derive(Default)]
struct Recorded {
    opened: Vec<(Destination, u64)>,
    writes: Vec<(u64, Vec<u8>)>,
    finished: usize,
}

struct FakeGpu(Rc<RefCell<Recorded>>);

struct FakeSink(Rc<RefCell<Recorded>>, #[allow(dead_code)] OwnedFd);

impl GpuService for FakeGpu {
    fn open(
        &self,
        descriptor: OwnedFd,
        destination: Destination,
        allocation_size: u64,
    ) -> Result<Box<dyn DirectSink>> {
        self.0
            .borrow_mut()
            .opened
            .push((destination, allocation_size));
        Ok(Box::new(FakeSink(self.0.clone(), descriptor)))
    }
}

impl DirectSink for FakeSink {
    fn deliver<'a>(
        &'a mut self,
        page: DirectPage<'a>,
        destination_offset: u64,
        _scope: &'a RequestScope,
    ) -> crate::error::Operation<'a, ()> {
        let DirectPage::Plaintext(reader) = page;
        let bytes = reader.slice_bytes().to_vec();
        let recorded = self.0.clone();
        Box::pin(async move {
            // Complete on a later poll, as a helper thread would.
            let mut ready = false;
            std::future::poll_fn(|cx| {
                if std::mem::replace(&mut ready, true) {
                    Poll::Ready(())
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await;
            recorded
                .borrow_mut()
                .writes
                .push((destination_offset, bytes));
            Ok(())
        })
    }

    fn finish<'a>(&'a mut self, _scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        self.0.borrow_mut().finished += 1;
        Box::pin(async { Ok(()) })
    }
}

#[test]
fn gpu_read_delivers_pages_through_the_backend_seam() {
    let recorded = Rc::new(RefCell::new(Recorded::default()));
    let fixture = direct_fixture(b"hello".to_vec(), Some(Rc::new(FakeGpu(recorded.clone()))));
    let memfd = Memfd::new();
    let output = exchange(
        &fixture,
        &direct_request(
            "gpu-read",
            "Range: bytes=2-\r\nRacer-Destination-Offset: 100\r\n\
             Racer-Destination-Length: 3\r\nRacer-Allocation-Size: 4096\r\n",
        ),
        &[memfd.file.as_raw_fd()],
    );
    let (head, body) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(head.contains("racer-delivery: gpu\r\n"), "{head}");
    assert_eq!(frames(body), [(1, 0, 2, 3), (2, 1, 3, 0)]);
    {
        let recorded = recorded.borrow();
        assert_eq!(
            recorded.opened,
            [(
                Destination {
                    offset: 100,
                    length: 3
                },
                4096
            )]
        );
        assert_eq!(recorded.writes, [(100, b"llo".to_vec())]);
        assert_eq!(recorded.finished, 1);
    }
    settle(&fixture);
    assert_eq!(memfd.open_copies(), 1);
}

mod gpu;
