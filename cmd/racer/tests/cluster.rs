//! Six nodes, six processes, one cluster: everything a client can observe. Every
//! assertion goes through a ublk device, half of them through a node that does not hold
//! the page. Separate processes because only one runtime may exist per process.
//!
//! Each node's store is a file on its own ext4, on a loop device backed by a memfd, so
//! the test never touches real storage. Peers are wired straight to each other's fabric
//! block device, so only the nvme-of transport is left out.

// Real processes over real kernel interfaces; `sim` replaces them with an event queue.
#![cfg(not(feature = "sim"))]

use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdout, Command, Stdio};

use racer::config::Config;

const NODES: u32 = 7;
const ROOT: &str = "/tmp/racer-e2e";
/// Room for the store twice over: a rebuild lays a fresh one down before the file
/// system has finished accounting for the one it just removed.
const FS_BYTES: u64 = 3 << 30;
const IMG_BYTES: u64 = 1 << 30;
/// What the store is raised to partway through, to watch a start reserve the difference.
const GROWN_BYTES: u64 = IMG_BYTES + (256 << 20);
const PAGE: usize = 4096;
const HUGE: usize = 4 << 20;

/// The one universe every node shares, and the device ids the test opens. Device `n`
/// is composed of extent `n` alone, so a device page and its extent page are the same
/// number; `MIX` is the exception, and exists to prove they need not be.
const UNIVERSE: u32 = 1;
const LWW: u32 = 1;
const OCC: u32 = 2;
const IMM: u32 = 3;
const BIG: u32 = 4;
const MIX: u32 = 5;

/// Where each extent sits in the universe's address space. The control plane places
/// them; nothing about the layout is derived from the device that maps them.
const LWW_BASE: u64 = 0;
const OCC_BASE: u64 = 4096;
const IMM_BASE: u64 = 4608;

const LOOP_CTL_GET_FREE: libc::c_ulong = 0x4c82;
const LOOP_SET_FD: libc::c_ulong = 0x4c00;
const LOOP_CLR_FD: libc::c_ulong = 0x4c01;
const LOOP_SET_BLOCK_SIZE: libc::c_ulong = 0x4c09;

// ---------------------------------------------------------------------------
// the cluster
// ---------------------------------------------------------------------------

struct Node {
    id: u32,
    dir: PathBuf,
    img: PathBuf,
    mnt: PathBuf,
    /// Declared before `_memfd`, so the loop device detaches before its backing file
    /// closes.
    loop_dev: Option<OwnedFd>,
    _loop_path: String,
    _memfd: OwnedFd,
    child: Option<Child>,
    out: Option<BufReader<ChildStdout>>,
    devices: Vec<(u32, PathBuf)>,
    fabric: PathBuf,
    /// What the generations written for this node say its store must be. The control
    /// plane's to raise, one node at a time, which is how the test asks for a bigger
    /// file.
    store_bytes: u64,
    /// Where this node's prometheus endpoint landed; the port is ephemeral.
    metrics: String,
}

impl Node {
    /// Start `racer serve` and read back the devices it published.
    fn serve(&mut self) {
        let mut child = Command::new(env!("CARGO_BIN_EXE_racer"))
            .arg("serve")
            .arg(self.dir.join("node.pb"))
            .env("METRICS_ADDR", "127.0.0.1:0")
            // The store's path is this process's own, so it never appears in a
            // generation: the control plane places the file, racer sizes it.
            .env(racer::config::STORE_PATH_ENV, &self.img)
            .stdout(Stdio::piped())
            .spawn()
            .expect("spawn racer serve");
        let mut out = BufReader::new(child.stdout.take().unwrap());
        self.devices.clear();
        loop {
            let line = next_line(&mut out, self.id);
            if let Some(rest) = line.strip_prefix("metrics -> ") {
                self.metrics = rest.to_string();
            } else if let Some(rest) = line.strip_prefix("device ") {
                let (id, path) = rest.split_once(" -> ").expect("device line");
                self.devices
                    .push((id.parse().unwrap(), PathBuf::from(path)));
            } else if let Some(rest) = line.strip_prefix("universe ") {
                // One universe here, so its fabric device is the last line of the
                // banner and the one the peers attach to.
                let (_, path) = rest.split_once(" fabric -> ").expect("universe line");
                self.fabric = PathBuf::from(path);
                break;
            }
        }
        for (_, p) in &self.devices {
            wait_for(p);
        }
        wait_for(&self.fabric);
        self.child = Some(child);
        self.out = Some(out);
    }

    fn signal(&mut self, sig: i32) {
        if let Some(c) = &self.child {
            unsafe { libc::kill(c.id() as i32, sig) };
        }
    }

    fn reap(&mut self) {
        if let Some(mut c) = self.child.take() {
            let _ = c.wait();
        }
        self.out = None;
    }

    /// Install a generation as the control plane does: write beside the live file and
    /// rename over it, so the watcher sees one atomic replacement.
    fn install(&self, text: &str) {
        let cfg = Config::parse(text).expect("parse config");
        cfg.validate().expect("validate config");
        let tmp = self.dir.join("node.next");
        std::fs::write(&tmp, cfg.encode()).unwrap();
        std::fs::rename(&tmp, self.dir.join("node.pb")).unwrap();
    }

    fn await_reload(&mut self) {
        let out = self.out.as_mut().unwrap();
        loop {
            if next_line(out, self.id) == "racer: configuration applied" {
                return;
            }
        }
    }

    fn dev(&self, device: u32) -> Dev {
        let p = self
            .devices
            .iter()
            .find(|(id, _)| *id == device)
            .expect("device")
            .1
            .clone();
        Dev::open(&p)
    }

    /// How much of the filesystem the store is holding, which is what `size=` asked for
    /// the last time this node started.
    fn store_len(&self) -> u64 {
        std::fs::metadata(&self.img).expect("stat the store").len()
    }

    /// Start `racer serve` on a configuration it must refuse and return what it said on
    /// the way out. The node has to be down: this is the same store.
    fn serve_refuses(&self, text: &str) -> String {
        let cfg = Config::parse(text).expect("parse config");
        let path = self.dir.join("node.refused");
        std::fs::write(&path, cfg.encode()).unwrap();
        let out = Command::new(env!("CARGO_BIN_EXE_racer"))
            .arg("serve")
            .arg(&path)
            .env("METRICS_ADDR", "127.0.0.1:0")
            .env(racer::config::STORE_PATH_ENV, &self.img)
            .output()
            .expect("spawn racer serve");
        assert!(
            !out.status.success(),
            "racer started on a store it must have refused"
        );
        String::from_utf8_lossy(&out.stderr).into_owned()
    }
}

impl Drop for Node {
    fn drop(&mut self) {
        self.signal(libc::SIGKILL);
        self.reap();
        unsafe { libc::umount2(cstr(&self.mnt).as_ptr(), libc::MNT_DETACH) };
        if let Some(d) = self.loop_dev.take() {
            unsafe { libc::ioctl(d.as_raw_fd(), LOOP_CLR_FD) };
        }
    }
}

/// Stop the cluster. Every node holds its peers' fabric devices open and a ublk device
/// cannot be torn down while open, so the nodes go down together.
fn shutdown(nodes: &mut [Node]) {
    for n in nodes.iter_mut() {
        n.signal(libc::SIGTERM);
    }
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
    for n in nodes.iter_mut() {
        while std::time::Instant::now() < deadline {
            match n.child.as_mut().map(|c| c.try_wait().unwrap()) {
                None | Some(Some(_)) => break,
                Some(None) => std::thread::sleep(std::time::Duration::from_millis(20)),
            }
        }
        n.signal(libc::SIGKILL);
        n.reap();
    }
}

/// One line of a node's stdout, or a panic if it died.
fn next_line(out: &mut BufReader<ChildStdout>, id: u32) -> String {
    let mut s = String::new();
    let n = out.read_line(&mut s).expect("read node stdout");
    assert!(n > 0, "node {id} exited before publishing its devices");
    s.trim_end().to_string()
}

fn wait_for(p: &Path) {
    for _ in 0..500 {
        if p.exists() {
            return;
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
    panic!("{} never appeared", p.display());
}

// ---------------------------------------------------------------------------
// metrics
// ---------------------------------------------------------------------------

/// One request, one connection, no keep-alive. Returns the status line and the body.
fn http_get(addr: &str, path: &str) -> (String, String) {
    use std::io::{Read, Write};
    let mut c = std::net::TcpStream::connect(addr).expect("connect to the metrics endpoint");
    c.set_read_timeout(Some(std::time::Duration::from_secs(10)))
        .unwrap();
    write!(
        c,
        "GET {path} HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n\r\n"
    )
    .unwrap();
    let mut raw = String::new();
    c.read_to_string(&mut raw).expect("read the response");
    let (head, body) = raw.split_once("\r\n\r\n").expect("headers end");
    (head.lines().next().unwrap().to_string(), body.to_string())
}

/// A parsed exposition. Parsing is the assertion: help before type before samples, no
/// duplicate series, so every scrape checks the shape and the tests only read numbers.
struct Exposition {
    samples: Vec<(String, u64)>,
}

impl Exposition {
    fn parse(body: &str) -> Exposition {
        let mut samples = Vec::new();
        let mut named = String::new();
        let mut typed = String::new();
        for line in body.lines() {
            if let Some(rest) = line.strip_prefix("# HELP ") {
                let (name, help) = rest.split_once(' ').expect("help text");
                assert!(!help.is_empty(), "{name} has an empty help string");
                named = name.to_string();
            } else if let Some(rest) = line.strip_prefix("# TYPE ") {
                let (name, kind) = rest.split_once(' ').expect("type");
                assert_eq!(name, named, "a type without its help");
                assert!(kind == "counter" || kind == "gauge", "{name} is a {kind}");
                typed = name.to_string();
            } else {
                let (series, value) = line.split_once(' ').expect("sample");
                let name = series.split_once('{').map_or(series, |(n, _)| n);
                assert_eq!(name, typed, "{name} arrived before its type");
                samples.push((series.to_string(), value.parse().expect("a u64 sample")));
            }
        }
        assert!(!samples.is_empty(), "an empty exposition");
        samples
            .iter()
            .fold(&mut std::collections::HashSet::new(), |seen, (s, _)| {
                assert!(seen.insert(s.clone()), "{s} exported twice");
                seen
            });
        Exposition { samples }
    }

    fn get(&self, series: &str) -> u64 {
        self.samples
            .iter()
            .find(|(s, _)| s == series)
            .unwrap_or_else(|| panic!("{series} is not exported"))
            .1
    }
}

fn scrape(n: &Node) -> Exposition {
    let (status, body) = http_get(&n.metrics, "/metrics");
    assert_eq!(status, "HTTP/1.1 200 OK", "scrape of node {} failed", n.id);
    Exposition::parse(&body)
}

// ---------------------------------------------------------------------------
// backing store: memfd -> loop -> ext4
// ---------------------------------------------------------------------------

fn build_node(id: u32) -> Node {
    let dir = PathBuf::from(ROOT).join(format!("n{id}"));
    let mnt = dir.join("mnt");
    std::fs::create_dir_all(&mnt).unwrap();

    let name = cstr(Path::new(&format!("racer-{id}")));
    let fd = unsafe { libc::memfd_create(name.as_ptr(), 0) };
    assert!(fd >= 0, "memfd_create: {}", last_error());
    let memfd = unsafe { OwnedFd::from_raw_fd(fd) };
    assert_eq!(
        unsafe { libc::ftruncate(fd, FS_BYTES as i64) },
        0,
        "ftruncate: {}",
        last_error()
    );

    let (loop_dev, loop_path) = loop_attach(fd);
    let ok = Command::new("mkfs.ext4")
        .args(["-q", "-F", "-b", "4096", &loop_path])
        .status()
        .expect("mkfs.ext4");
    assert!(ok.success(), "mkfs.ext4 failed on {loop_path}");
    let rc = unsafe {
        libc::mount(
            cstr(Path::new(&loop_path)).as_ptr(),
            cstr(&mnt).as_ptr(),
            c"ext4".as_ptr(),
            0,
            std::ptr::null(),
        )
    };
    assert_eq!(rc, 0, "mount {loop_path}: {}", last_error());

    // Deliberately not created: racer places and sizes its own store, and the first
    // start of the cluster is where that is proved.
    let img = mnt.join("store.img");
    Node {
        id,
        dir,
        img,
        mnt,
        loop_dev: Some(loop_dev),
        _loop_path: loop_path,
        _memfd: memfd,
        child: None,
        out: None,
        devices: Vec::new(),
        fabric: PathBuf::new(),
        store_bytes: IMG_BYTES,
        metrics: String::new(),
    }
}

/// Bind a free loop device to `backing`. The kernel picks the minor and udev creates
/// the node a moment later.
fn loop_attach(backing: RawFd) -> (OwnedFd, String) {
    let ctl = File::open("/dev/loop-control").expect("/dev/loop-control");
    let n = unsafe { libc::ioctl(ctl.as_raw_fd(), LOOP_CTL_GET_FREE) };
    assert!(n >= 0, "LOOP_CTL_GET_FREE: {}", last_error());
    let path = format!("/dev/loop{n}");
    wait_for(Path::new(&path));
    let dev = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&path)
        .expect("open loop device");
    let rc = unsafe { libc::ioctl(dev.as_raw_fd(), LOOP_SET_FD, backing) };
    assert_eq!(rc, 0, "LOOP_SET_FD: {}", last_error());
    unsafe { libc::ioctl(dev.as_raw_fd(), LOOP_SET_BLOCK_SIZE, 4096) };
    (OwnedFd::from(dev), path)
}

fn cstr(p: &Path) -> std::ffi::CString {
    std::ffi::CString::new(p.as_os_str().as_encoded_bytes()).unwrap()
}

fn last_error() -> std::io::Error {
    std::io::Error::last_os_error()
}

// ---------------------------------------------------------------------------
// client IO
// ---------------------------------------------------------------------------

/// A consumer device opened as a consumer would: `O_DIRECT`, so one request is one page
/// and an error reaches the call that caused it.
struct Dev(File);

impl Dev {
    fn open(p: &Path) -> Dev {
        Dev(OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(libc::O_DIRECT)
            .open(p)
            .unwrap_or_else(|e| panic!("open {}: {e}", p.display())))
    }

    fn write(&self, off: u64, data: &[u8]) -> std::io::Result<()> {
        let buf = Aligned::from(data);
        let n = unsafe {
            libc::pwrite(
                self.0.as_raw_fd(),
                buf.ptr as *const _,
                data.len(),
                off as i64,
            )
        };
        if n as usize == data.len() {
            Ok(())
        } else {
            Err(last_error())
        }
    }

    fn read(&self, off: u64, len: usize) -> std::io::Result<Vec<u8>> {
        let buf = Aligned::new(len);
        let n = unsafe { libc::pread(self.0.as_raw_fd(), buf.ptr as *mut _, len, off as i64) };
        if n as usize == len {
            Ok(buf.to_vec())
        } else {
            Err(last_error())
        }
    }

    fn discard(&self, off: u64, len: u64) -> std::io::Result<()> {
        let range = [off, len];
        // BLKDISCARD
        let rc = unsafe { libc::ioctl(self.0.as_raw_fd(), 0x1277, range.as_ptr()) };
        if rc == 0 { Ok(()) } else { Err(last_error()) }
    }
}

/// A page-aligned buffer, which `O_DIRECT` requires.
struct Aligned {
    ptr: *mut u8,
    len: usize,
}

impl Aligned {
    fn new(len: usize) -> Aligned {
        let l = std::alloc::Layout::from_size_align(len, PAGE).unwrap();
        let ptr = unsafe { std::alloc::alloc_zeroed(l) };
        assert!(!ptr.is_null());
        Aligned { ptr, len }
    }

    fn from(data: &[u8]) -> Aligned {
        let a = Aligned::new(data.len());
        unsafe { std::ptr::copy_nonoverlapping(data.as_ptr(), a.ptr, data.len()) };
        a
    }

    fn to_vec(&self) -> Vec<u8> {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len) }.to_vec()
    }
}

impl Drop for Aligned {
    fn drop(&mut self) {
        let l = std::alloc::Layout::from_size_align(self.len, PAGE).unwrap();
        unsafe { std::alloc::dealloc(self.ptr, l) };
    }
}

fn pattern(seed: u8, len: usize) -> Vec<u8> {
    (0..len)
        .map(|i| seed ^ (i as u8).wrapping_mul(31))
        .collect()
}

/// Write, retrying `EAGAIN`. A bulk write races the anti-entropy sweep for the same
/// pages, and an LWW round that exhausts its internal retries surfaces that conflict as
/// `EAGAIN`: ordinary here, not a failure.
fn write_lww(dev: &Dev, off: u64, page: &[u8]) {
    for _ in 0..64 {
        match dev.write(off, page) {
            Ok(()) => return,
            Err(e) if e.raw_os_error() == Some(libc::EAGAIN) => {
                std::thread::sleep(std::time::Duration::from_millis(5));
            }
            Err(e) => panic!("write at {off}: {e}"),
        }
    }
    panic!("write at {off}: still EAGAIN after 64 tries");
}

/// An OCC write that must land, re-observing on `EAGAIN`. A refusal is the register's
/// only answer for every kind of conflict, and a background repair round produces one
/// with no competing writer at all — a term the group raised under a sweep outranks the
/// proposal even when the version never moved. Reading again and retrying is what an OCC
/// client does. The refusals this test asserts stay plain `write` calls.
fn write_occ(dev: &Dev, off: u64, page: &[u8]) {
    for _ in 0..64 {
        match dev.write(off, page) {
            Ok(()) => return,
            Err(e) if e.raw_os_error() == Some(libc::EAGAIN) => {
                std::thread::sleep(std::time::Duration::from_millis(5));
                dev.read(off, page.len()).unwrap();
            }
            Err(e) => panic!("write at {off}: {e}"),
        }
    }
    panic!("write at {off}: still EAGAIN after 64 tries");
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

/// Two groups over six members, with node 7 spare. Every node maps every extent, so any
/// page is reachable through any node - and for half of them that node is not in the
/// owning group, which is what the forwarding rules exist for.
///
/// The catalog is balanced, as a zone's catalog must be: each of the six members holds
/// one of the six seats, so every store is formatted to the same size off one set of
/// extents. Node 7 holds nothing yet and sizes itself for the share it would inherit.
///
/// Device `MIX` maps the same two extents device `OCC` and device `LWW` do, in the other
/// order: an extent is a member of the universe, not of a device, so mapping it twice
/// over is the control plane's business and changes nothing about the address.
fn config_text(generation: u32, n: &Node, peers: &[(u32, PathBuf)]) -> String {
    let mut s = format!(
        "generation {generation}\nnode id={} zone=1 cohort={} size={}\n",
        n.id,
        (n.id - 1) % 3,
        n.store_bytes,
    );
    // A nonzero target rate is what turns the cache on at all. Low, so a handful of
    // reads makes a page hot.
    s += "policy cache_target_rate=1\n\
          universe 1 epoch=1\n";
    for (id, dev) in peers {
        s += &format!("peer id={id} device={}\n", dev.display());
    }
    for g in catalog() {
        s += &format!("group {} {} {}\n", g[0], g[1], g[2]);
    }
    s += "extent id=1 base=0    pages=4096 kind=lww          zone=1\n\
          extent id=2 base=4096 pages=512  kind=occ          zone=1\n\
          extent id=3 base=4608 pages=512  kind=immutable    zone=1\n\
          extent id=4 base=5120 pages=4    kind=immutable_4m zone=1\n\
          device 1 extents=1\n\
          device 2 extents=2\n\
          device 3 extents=3\n\
          device 4 extents=4\n\
          device 5 extents=2,1\n";
    s
}

/// The catalog every generation is written with. Replacing a member rewrites it, so it is
/// state the control plane holds rather than a constant, and the test is its control
/// plane.
static CATALOG: std::sync::Mutex<Vec<[u32; 3]>> = std::sync::Mutex::new(Vec::new());

fn catalog() -> Vec<[u32; 3]> {
    CATALOG.lock().unwrap().clone()
}

fn set_catalog(groups: &[[u32; 3]]) {
    *CATALOG.lock().unwrap() = groups.to_vec();
}

/// Every other node's fabric device, as `config_text` wants them.
fn peers_of(nodes: &[Node], id: u32) -> Vec<(u32, PathBuf)> {
    nodes
        .iter()
        .filter(|n| n.id != id)
        .map(|n| (n.id, n.fabric.clone()))
        .collect()
}

/// Tell the nodes at `who` where everyone's fabric device currently lives, as the
/// control plane would: a new generation, dropped in atomically.
fn wire(nodes: &mut [Node], generation: u32, who: &[usize]) {
    wire_without(nodes, generation, who, &[]);
}

/// As [`wire`], but the nodes at `who` are told nothing about the peers in `omit`. A
/// group member a node cannot name is one it has to reach through another.
fn wire_without(nodes: &mut [Node], generation: u32, who: &[usize], omit: &[u32]) {
    let fabrics: Vec<(u32, PathBuf)> = nodes.iter().map(|n| (n.id, n.fabric.clone())).collect();
    for &i in who {
        let n = &mut nodes[i];
        let peers: Vec<(u32, PathBuf)> = fabrics
            .iter()
            .filter(|(id, _)| *id != n.id && !omit.contains(id))
            .cloned()
            .collect();
        let text = config_text(generation, n, &peers);
        n.install(&text);
        n.await_reload();
    }
}

/// The first page of the extent at `base` that group `want` owns, so "a node that holds
/// it" and "a node that does not" are exact below.
fn page_in(cfg: &Config, base: u64, want: u32) -> u64 {
    (0..512)
        .find(|off| cfg.group(addr(base + off)) == group(want))
        .expect("some page hashes to every group")
}

/// A page's address in the universe every node here shares.
fn addr(lba: u64) -> u64 {
    racer::config::addr_of(UNIVERSE, lba)
}

/// A group of that universe, by index into its catalog.
fn group(index: u32) -> racer::config::GroupId {
    racer::config::GroupId::new(UNIVERSE, index)
}

// ---------------------------------------------------------------------------
// rebuilding a member
// ---------------------------------------------------------------------------

/// Cut every link to node `i`. With no peers its quorum is one, so a read is answered
/// from its own slab — the only way from outside to tell "holds the value" from "can go
/// and fetch it".
fn isolate(nodes: &mut [Node], i: usize, generation: &mut u32) {
    *generation += 1;
    let text = config_text(*generation, &nodes[i], &[]);
    nodes[i].install(&text);
    nodes[i].await_reload();
}

/// Undo [`isolate`].
fn rejoin(nodes: &mut [Node], i: usize, generation: &mut u32) {
    *generation += 1;
    wire(nodes, *generation, &[i]);
}

/// Destroy node `i` and prove the group puts it back.
///
/// The node keeps its id and its place in the catalog but loses everything else, so its
/// digests for the group come up empty while its peers' do not. The sweep reads that as
/// a node joining and replays every bucket instead of the handful a steady-state sweep
/// walks. `want` is the group's contents, updated here for the writes this makes.
fn rebuild(
    nodes: &mut [Node],
    i: usize,
    generation: &mut u32,
    want: &mut [(u64, Vec<u8>)],
    mark: u8,
) {
    let id = nodes[i].id;
    let all: Vec<usize> = (0..nodes.len()).collect();

    // Remove the whole backing file, not just the metadata region a reformat would
    // rewrite: what comes back is a blank store, placed and sized by the restart.
    nodes[i].signal(libc::SIGKILL);
    nodes[i].reap();
    std::fs::remove_file(&nodes[i].img).expect("remove the store");
    nodes[i].serve();
    assert_eq!(
        nodes[i].store_len(),
        nodes[i].store_bytes,
        "a start must place a missing store at the configured size"
    );
    // The restarted node's fabric device is a fresh minor, so its peers need telling.
    *generation += 1;
    wire(nodes, *generation, &all);

    // Write to the group while the join is still in flight, through a member other than
    // the wiped one. Replay and ordinary traffic must merge by version rather than one
    // clobbering the other, and this is the window where that is decided.
    {
        let d = nodes[(i + 1) % 3].dev(LWW);
        for (p, v) in want.iter_mut().take(8) {
            *v = pattern(mark, PAGE);
            write_lww(&d, *p * 4096, v);
        }
    }

    // Now wait, sending nothing: what puts the data back must be the sweep, not a client
    // read tripping over a hole. Each probe isolates the node (quorum one, so it answers
    // from its own slab and cannot repair) and costs two reloads, so back off rather than
    // poll — `Live` keeps only one previous generation for a sweep to still hold.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    let mut wait = std::time::Duration::from_secs(5);
    loop {
        std::thread::sleep(wait);
        isolate(nodes, i, generation);
        let holes = {
            let d = nodes[i].dev(LWW);
            want.iter()
                .filter(|(p, v)| d.read(p * 4096, PAGE).ok().as_ref() != Some(v))
                .count()
        };
        rejoin(nodes, i, generation);
        if holes == 0 {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "node {id} still missing {holes} of {} pages after the replay window",
            want.len()
        );
        wait = std::time::Duration::from_secs(10);
    }

    // Holding the data is not the same as being a member again: a node still replaying
    // forwards proposals instead of making them. Write at the rebuilt node, read back
    // from a node outside the group.
    let (p, v) = want.last_mut().expect("some pages");
    *v = pattern(!mark, PAGE);
    write_lww(&nodes[i].dev(LWW), *p * 4096, v);
    assert_eq!(
        &nodes[5].dev(LWW).read(*p * 4096, PAGE).unwrap(),
        v,
        "node {id} must propose for itself once it has rejoined"
    );
}

fn privileged() -> bool {
    (unsafe { libc::geteuid() } == 0)
        && File::open("/dev/ublk-control").is_ok()
        && File::open("/dev/loop-control").is_ok()
}

// ---------------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------------

#[test]
fn six_node_cluster() {
    if !privileged() {
        eprintln!("skipping: needs root, ublk_drv and loop");
        return;
    }
    let _ = std::fs::remove_dir_all(ROOT);
    std::fs::create_dir_all(ROOT).unwrap();
    set_catalog(&[[1, 2, 3], [4, 5, 6]]);

    // ---- bring seven nodes up with no peers: their fabric devices do not exist yet ----
    let mut nodes: Vec<Node> = (1..=NODES).map(build_node).collect();
    for n in &mut nodes {
        let text = config_text(1, n, &[]);
        n.install(&text);
        n.serve();
    }
    // Nothing created the store: the file did not exist before the start that formatted
    // it, and `size=` is the whole of what says how big it is.
    for n in &nodes {
        assert_eq!(
            n.store_len(),
            IMG_BYTES,
            "node {} did not place its own store",
            n.id
        );
    }

    // ---- second generation: now every node can name every peer's fabric device ----
    wire(&mut nodes, 2, &(0..NODES as usize).collect::<Vec<_>>());

    let cfg = Config::parse(&config_text(2, &nodes[0], &[])).unwrap();
    // Node 1 is in group 0 and node 6 in group 1, so for each of these pages one of them
    // holds the register and the other must go through the fabric. Node 7 is spare and
    // holds neither.
    let held = page_in(&cfg, LWW_BASE, 0);
    let remote = page_in(&cfg, LWW_BASE, 1);

    // ---- LWW: last write wins, from any node, and a hole reads as zeroes ----------
    let a = nodes[0].dev(LWW);
    let b = nodes[5].dev(LWW);
    for &p in &[held, remote] {
        assert_eq!(
            a.read(p * 4096, PAGE).unwrap(),
            vec![0u8; PAGE],
            "hole must read as zeroes"
        );
        assert_eq!(
            b.read(p * 4096, PAGE).unwrap(),
            vec![0u8; PAGE],
            "hole must read as zeroes"
        );

        write_lww(&a, p * 4096, &pattern(1, PAGE));
        assert_eq!(
            b.read(p * 4096, PAGE).unwrap(),
            pattern(1, PAGE),
            "write must be visible"
        );
        // Both the member and the non-member drive a round for this page.
        write_lww(&b, p * 4096, &pattern(2, PAGE));
        assert_eq!(
            a.read(p * 4096, PAGE).unwrap(),
            pattern(2, PAGE),
            "last write must win"
        );
    }

    // ---- cache: a hot page is cached, and a cached read is never stale ------------
    // Node 1 is not in group 1, so `remote` is a page it may cache. Repeated reads raise
    // the width past one and admit it locally; the mandatory metadata round confirms
    // every hit on `(version, ballot)`, which is the whole invalidation protocol.
    for _ in 0..64 {
        assert_eq!(
            a.read(remote * 4096, PAGE).unwrap(),
            pattern(2, PAGE),
            "a hot read"
        );
    }
    write_lww(&b, remote * 4096, &pattern(9, PAGE));
    assert_eq!(
        a.read(remote * 4096, PAGE).unwrap(),
        pattern(9, PAGE),
        "a cached copy that lost its version is dropped, not served"
    );
    write_lww(&b, remote * 4096, &pattern(2, PAGE));

    // ---- OCC: a write is refused unless this node read the current version ---------
    let occ_page = page_in(&cfg, OCC_BASE, 1);
    let oa = nodes[0].dev(OCC);
    let ob = nodes[5].dev(OCC);
    assert!(
        oa.write(occ_page * 4096, &pattern(3, PAGE)).is_err(),
        "OCC write with no prior read must be refused"
    );
    oa.read(occ_page * 4096, PAGE).unwrap();
    write_occ(&oa, occ_page * 4096, &pattern(3, PAGE));
    assert!(
        oa.write(occ_page * 4096, &pattern(4, PAGE)).is_err(),
        "OCC write on a stale observation must be refused"
    );
    // Both nodes read the same version; the second writer loses.
    oa.read(occ_page * 4096, PAGE).unwrap();
    assert_eq!(ob.read(occ_page * 4096, PAGE).unwrap(), pattern(3, PAGE));
    write_occ(&oa, occ_page * 4096, &pattern(5, PAGE));
    assert!(
        ob.write(occ_page * 4096, &pattern(6, PAGE)).is_err(),
        "OCC must reject a write whose read was overtaken"
    );
    assert_eq!(ob.read(occ_page * 4096, PAGE).unwrap(), pattern(5, PAGE));

    // ---- immutable: filled once, trimmed once, not refilled until the epoch moves ---
    let imm_page = page_in(&cfg, IMM_BASE, 0);
    let ia = nodes[0].dev(IMM);
    let ib = nodes[5].dev(IMM);
    ib.write(imm_page * 4096, &pattern(7, PAGE)).unwrap();
    assert_eq!(ia.read(imm_page * 4096, PAGE).unwrap(), pattern(7, PAGE));
    assert!(
        ia.write(imm_page * 4096, &pattern(8, PAGE)).is_err(),
        "a filled immutable page must refuse a second write"
    );
    ia.discard(imm_page * 4096, 4096).unwrap();
    assert_eq!(
        ib.read(imm_page * 4096, PAGE).unwrap(),
        vec![0u8; PAGE],
        "a trimmed page is a hole"
    );
    assert!(
        ib.write(imm_page * 4096, &pattern(9, PAGE)).is_err(),
        "a trimmed immutable page must not be refilled before the epoch advances"
    );

    // ---- 4 MiB immutable: filled once, whole pages only ---------------------------
    let big = pattern(0x5a, HUGE);
    let ba = nodes[0].dev(BIG);
    let bb = nodes[5].dev(BIG);
    ba.write(0, &big).unwrap();
    assert_eq!(
        bb.read(0, HUGE).unwrap(),
        big,
        "a 4 MiB fill must be visible cluster-wide"
    );
    assert_eq!(
        bb.read(0, HUGE).unwrap(),
        big,
        "a 4 MiB fill must be visible cluster-wide"
    );
    assert_eq!(
        ba.read(0, HUGE).unwrap(),
        big,
        "a non-member must fetch, not read zeroes"
    );
    assert!(
        ba.write(0, &big).is_err(),
        "a filled 4 MiB page must refuse a second write"
    );
    assert!(
        bb.write(4096, &pattern(1, PAGE)).is_err(),
        "a 4 MiB page is written whole or not at all"
    );
    assert_eq!(
        ba.read(HUGE as u64, HUGE).unwrap(),
        vec![0u8; HUGE],
        "an unfilled 4 MiB page reads as zeroes"
    );

    // ---- one extent, two devices --------------------------------------------------
    // Device MIX concatenates extents 2 and 1 in that order, so its page 512 is the
    // page device LWW calls page 0. An address belongs to its extent, so which device a
    // request arrives through, and in what order that device stacked its extents, are
    // invisible to everything below the block layer.
    let mix = nodes[3].dev(MIX);
    let both = 4000;
    let shared = pattern(0x3c, PAGE);
    write_lww(&mix, (512 + both) * 4096, &shared);
    assert_eq!(
        nodes[0].dev(LWW).read(both * 4096, PAGE).unwrap(),
        shared,
        "an extent is one page space however a device concatenates it"
    );
    assert_eq!(
        mix.read(occ_page * 4096, PAGE).unwrap(),
        nodes[0].dev(OCC).read(occ_page * 4096, PAGE).unwrap(),
        "and the extent a device leads with is the same extent"
    );
    drop(mix);

    // ---- metrics: what the work above did, as a scraper sees it -------------------
    // Node 1 has by now proposed, read, cached and served a page it does not hold, so
    // its counters cover the whole dataplane.
    let m = scrape(&nodes[0]);
    assert!(
        m.get("racer_paxos_accept_total{result=\"ok\"}") > 0,
        "no accepts counted"
    );
    assert!(
        m.get("racer_cache_lookup_total{result=\"hit\"}") > 0,
        "no cache hits counted"
    );
    let slots = m.get("racer_alloc_slots{class=\"small\"}");
    assert!(slots > 0, "the device has no small slots");
    assert!(
        m.get("racer_alloc_free_slots{class=\"small\"}") <= slots,
        "more free than there is"
    );
    // Written by core 0 alone, so these also prove the single-writer rule: a second core
    // publishing them would multiply each by the worker count.
    assert_eq!(
        m.get("racer_config_generation"),
        2,
        "the generation in force"
    );
    assert_eq!(m.get("racer_node_id"), nodes[0].id as u64);
    assert_eq!(m.get("racer_universes"), 1);
    assert_eq!(m.get("racer_devices"), 5);
    assert_eq!(m.get("racer_extents"), 4);
    assert_eq!(m.get("racer_peers"), NODES as u64 - 1);
    assert_eq!(
        m.get("racer_config_rejected_total"),
        0,
        "nothing rejected yet"
    );

    // ---- capacity: the nodes of a zone are homogeneous, so every device is the same --
    // Off one set of extents, an equal share means an identical slab on every node,
    // members and spare alike: the spare is sized for the share it would inherit, which
    // is what lets it stand in for any member without reformatting.
    for (i, n) in nodes.iter().enumerate() {
        let their_slots = scrape(n).get("racer_alloc_slots{class=\"small\"}");
        assert_eq!(
            their_slots,
            slots,
            "node {} formatted {their_slots} small slots against node 1's {slots}",
            i + 1
        );
    }

    // A config the node must refuse: it parses and validates on its own and is wrong
    // only against the generation already running. Nobody is left to return an error to,
    // so it surfaces as a metric or not at all.
    nodes[0].install(&config_text(1, &nodes[0], &[]));
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    while scrape(&nodes[0]).get("racer_config_rejected_total") != 1 {
        assert!(
            std::time::Instant::now() < deadline,
            "a refused config was never counted"
        );
        std::thread::sleep(std::time::Duration::from_millis(50));
    }
    assert_eq!(
        scrape(&nodes[0]).get("racer_config_generation"),
        2,
        "refusing changed nothing"
    );

    let (status, _) = http_get(&nodes[0].metrics, "/nope");
    assert_eq!(status, "HTTP/1.1 404 Not Found", "only /metrics is served");

    // The refused file is still on disk and this node is killed and restarted below, so
    // leave it one it can boot from. The others are rewired at 3 in a moment without
    // this one, so generations stay monotonic per node.
    wire(&mut nodes, 3, &[0]);

    // ---- crash a node: the survivors serve on, and the node comes back -------------
    drop((a, b, oa, ob, ia, ib, ba, bb));
    nodes[0].signal(libc::SIGKILL);
    nodes[0].reap();
    // Group 0 is {1,2,3}, so two of the three members are still up.
    assert_eq!(
        nodes[1].dev(LWW).read(held * 4096, PAGE).unwrap(),
        pattern(2, PAGE),
        "a quorum keeps serving after a member dies"
    );
    // ---- the store grows on the way up, and never the other way --------------------
    // While the node is down, the control plane raises its share. A start reserves the
    // difference in place; the pages already in the file stay where they were, which the
    // reads below check. A start on a lower `size=` than the file already holds is the
    // one case racer refuses outright: shrinking would cut the slabs off at the end.
    assert_eq!(nodes[0].store_len(), IMG_BYTES, "formatted at its share");
    {
        let peers = peers_of(&nodes, nodes[0].id);
        nodes[0].store_bytes = IMG_BYTES - PAGE as u64;
        let said = nodes[0].serve_refuses(&config_text(3, &nodes[0], &peers));
        assert!(
            said.contains(&IMG_BYTES.to_string())
                && said.contains(&(IMG_BYTES - PAGE as u64).to_string()),
            "a refused shrink must name both sizes: {said}"
        );
        assert_eq!(
            nodes[0].store_len(),
            IMG_BYTES,
            "a refused start must leave the store alone"
        );

        nodes[0].store_bytes = GROWN_BYTES;
        nodes[0].install(&config_text(3, &nodes[0], &peers));
    }
    nodes[0].serve();
    assert_eq!(
        nodes[0].store_len(),
        GROWN_BYTES,
        "a raised size= must be reserved at the next start"
    );
    // The restarted node's fabric device is a fresh minor, so its peers need telling.
    wire(&mut nodes, 3, &(1..NODES as usize).collect::<Vec<_>>());
    let a = nodes[0].dev(LWW);
    assert_eq!(
        a.read(held * 4096, PAGE).unwrap(),
        pattern(2, PAGE),
        "durable across restart"
    );
    assert_eq!(
        a.read(remote * 4096, PAGE).unwrap(),
        pattern(2, PAGE),
        "reachable across restart"
    );
    assert_eq!(
        nodes[0].dev(IMM).read(imm_page * 4096, PAGE).unwrap(),
        vec![0u8; PAGE]
    );
    drop(a);

    // ---- partial mesh: a member we cannot name is one we route through -----------
    // Group 1 is {4,5,6}. Node 1 keeps only its link to 4, so the other two legs of a
    // read of a group-1 page are forwarded through it. Without the forward the read has
    // one answer, never a matching pair, and cannot complete.
    wire_without(&mut nodes, 4, &[0], &[5, 6]);
    let a = nodes[0].dev(LWW);
    assert_eq!(
        a.read(remote * 4096, PAGE).unwrap(),
        pattern(2, PAGE),
        "read via a forward"
    );
    write_lww(&a, remote * 4096, &pattern(11, PAGE));
    assert_eq!(
        a.read(remote * 4096, PAGE).unwrap(),
        pattern(11, PAGE),
        "write via a forward"
    );
    assert_eq!(
        nodes[5].dev(LWW).read(remote * 4096, PAGE).unwrap(),
        pattern(11, PAGE),
        "the member we could not name still holds the value"
    );
    drop(a);

    // ---- a wiped member replays the group from its peers -------------------------
    let mut generation = 5;
    wire(
        &mut nodes,
        generation,
        &(0..NODES as usize).collect::<Vec<_>>(),
    );
    // Enough pages to spread over far more digest buckets than a steady-state sweep
    // walks in the window below: the test is that the whole group moves at once, not
    // that anti-entropy eventually gets there.
    let mut want: Vec<(u64, Vec<u8>)> = (0..4096)
        .filter(|off| cfg.group(addr(LWW_BASE + *off)) == group(0))
        .take(128)
        .enumerate()
        .map(|(i, p)| (p, pattern(0x20u8.wrapping_add(i as u8), PAGE)))
        .collect();
    {
        let a = nodes[0].dev(LWW);
        for (p, v) in &want {
            write_lww(&a, p * 4096, v);
        }
    }

    // All three members of group 0, one at a time. By the end every node that answered
    // the writes above has been destroyed and rebuilt, so no surviving original copy
    // explains the data still being there. One at a time is the point: wiping two
    // together leaves no quorum holding a value, and losing it then would be correct.
    for i in 0..3 {
        rebuild(&mut nodes, i, &mut generation, &mut want, 0x40 + i as u8);
        // The client-visible property, read through a node outside the group.
        let d = nodes[5].dev(LWW);
        for (p, v) in &want {
            assert_eq!(
                &d.read(p * 4096, PAGE).unwrap(),
                v,
                "page {p} lost rebuilding node {}",
                i + 1
            );
        }
    }

    // Group 1 was never disturbed by any of it.
    assert_eq!(
        nodes[5].dev(LWW).read(remote * 4096, PAGE).unwrap(),
        pattern(11, PAGE),
        "rebuilding one group must not touch another"
    );

    // ---- replacement: a member is swapped for the spare, at the same share ---------
    // Balance is enforced, not advisory. Handing node 1 the seat node 6 held leaves it
    // with two of the six and five nodes to share them, which no equal split fits, so
    // the zone's nodes would no longer be interchangeable.
    set_catalog(&[[1, 2, 3], [4, 5, 1]]);
    let skewed = Config::parse(&config_text(generation + 1, &nodes[0], &[])).unwrap();
    assert!(
        skewed.validate().is_err(),
        "an unbalanced catalog must be refused"
    );

    // Node 7 takes over from node 1 wholesale: every seat node 1 held, and no others, so
    // the zone stays balanced at one seat each. The catalog keeps its length, so the
    // groups keep every address they had and only their members change.
    let before = scrape(&nodes[0]).get("racer_alloc_free_slots{class=\"small\"}");
    set_catalog(&[[7, 2, 3], [4, 5, 6]]);
    generation += 1;
    wire(
        &mut nodes,
        generation,
        &(0..NODES as usize).collect::<Vec<_>>(),
    );
    // The share never moved: it is the member count that sets it, and a swap leaves that
    // alone. Neither node has to reformat to trade places.
    assert_eq!(
        scrape(&nodes[6]).get("racer_alloc_slots{class=\"small\"}"),
        slots,
        "a replacement must not resize the zone"
    );

    // Node 7 replays group 0 from its surviving members; node 1 gives up only what it can
    // see all three of the group's new members holding, so the two halves finish in that
    // order.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(90);
    loop {
        let out = scrape(&nodes[0]);
        let dropped = out.get("racer_heal_dropped_total");
        let shedding = out.get("racer_heal_groups_shedding");
        let replaying = scrape(&nodes[6]).get("racer_heal_groups_replaying");
        if dropped > 0 && shedding == 0 && replaying == 0 {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "replacement stalled: node 1 dropped {dropped} with {shedding} groups left, \
             node 7 replaying {replaying}"
        );
        std::thread::sleep(std::time::Duration::from_secs(2));
    }

    // The space came back, and none of the value went with it.
    let after = scrape(&nodes[0]).get("racer_alloc_free_slots{class=\"small\"}");
    assert!(
        after > before,
        "shedding a group must return slots: {before} then {after}"
    );
    let outside = nodes[5].dev(LWW);
    let shed = nodes[0].dev(LWW);
    for (p, v) in &want {
        assert_eq!(
            &outside.read(p * 4096, PAGE).unwrap(),
            v,
            "page {p} lost in the replacement"
        );
        // And the node that gave its share away still serves the pages, by forwarding.
        assert_eq!(
            &shed.read(p * 4096, PAGE).unwrap(),
            v,
            "page {p} unreachable through node 1"
        );
    }
    drop((outside, shed));

    shutdown(&mut nodes);
    drop(nodes);
    let _ = std::fs::remove_dir_all(ROOT);
}
