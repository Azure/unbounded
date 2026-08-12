//! Six-node cluster end-to-end. Every assertion goes through a ublk device, half through a
//! node that does not hold the page; one process per node, since only one runtime may exist
//! per process. Each store is a file on ext4 over a loop device on a memfd, so no real
//! storage is used. Peers attach to each other's fabric device; only nvme-of is untested.

// Real processes over real kernel interfaces; `sim` replaces them with an event queue.
#![cfg(not(feature = "sim"))]

use std::fs::{File, OpenOptions};
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;

use harness::last_error;
use racer::config::Config;

#[path = "../src/harness.rs"]
mod harness;

const NODES: u32 = 7;
const ROOT: &str = "/tmp/racer-e2e";
/// Room for two stores: a rebuild lays a fresh one down before the fs frees the old one.
const FS_BYTES: u64 = 3 << 30;
const IMG_BYTES: u64 = 1 << 30;
/// What the store is raised to partway through, to watch a start reserve the difference.
const GROWN_BYTES: u64 = IMG_BYTES + (256 << 20);
const PAGE: usize = 4096;
const HUGE: usize = 4 << 20;

/// The shared universe and the device ids the test opens. Device `n` maps extent `n`
/// alone, so device page and extent page match; `MIX` proves they need not.
const UNIVERSE: u32 = 1;
const LWW: u32 = 1;
const OCC: u32 = 2;
const IMM: u32 = 3;
const BIG: u32 = 4;
const MIX: u32 = 5;

/// Extent bases in the universe address space, placed by the control plane, not by devices.
const LWW_BASE: u64 = 0;
const OCC_BASE: u64 = 4096;
const IMM_BASE: u64 = 4608;

// --- the cluster ---

struct Node {
    /// Declared before the backing store: the process must be gone before its store is
    /// unmounted, and fields drop in order.
    proc: harness::Proc,
    _backing: harness::Backing,
    /// Store size the generations for this node ask for; raising it grows the file.
    store_bytes: u64,
}

impl Node {
    fn dev(&self, device: u32) -> Dev {
        Dev::open(self.proc.device(device))
    }

    /// Start `racer serve` on a config it must refuse and return stderr. Node must be down.
    fn serve_refuses(&self, text: &str) -> String {
        let cfg = Config::parse(text).expect("parse config");
        let path = self.proc.dir.join("node.refused");
        std::fs::write(&path, cfg.encode()).unwrap();
        let out = Command::new(self.proc.exe())
            .arg("serve")
            .arg(&path)
            .env("METRICS_ADDR", "127.0.0.1:0")
            .env(racer::config::STORE_PATH_ENV, &self.proc.store)
            .output()
            .expect("spawn racer serve");
        assert!(
            !out.status.success(),
            "racer started on a store it must have refused"
        );
        String::from_utf8_lossy(&out.stderr).into_owned()
    }
}

/// Stop the cluster together: a ublk device cannot be torn down while a peer holds it open.
fn shutdown(nodes: &mut [Node]) {
    harness::shutdown(nodes.iter_mut().map(|n| &mut n.proc));
}

// --- metrics ---

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

/// A parsed exposition. Parsing is the assertion: help, then type, then samples, no dupes.
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
    let (status, body) = http_get(&n.proc.metrics, "/metrics");
    assert_eq!(
        status, "HTTP/1.1 200 OK",
        "scrape of node {} failed",
        n.proc.id
    );
    Exposition::parse(&body)
}

// --- backing store: memfd -> loop -> ext4 ---

fn build_node(id: u32) -> Node {
    let dir = PathBuf::from(ROOT).join(format!("n{id}"));
    let backing = harness::Backing::new(&dir.join("mnt"), FS_BYTES, &id.to_string());
    // Deliberately not created: racer places and sizes its own store.
    let store = backing.path("store.img");
    let exe = PathBuf::from(env!("CARGO_BIN_EXE_racer"));
    Node {
        proc: harness::Proc::new(id, dir, store, exe),
        _backing: backing,
        store_bytes: IMG_BYTES,
    }
}

// --- client IO ---

/// A consumer device opened `O_DIRECT`: one request is one page, errors reach the caller.
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

/// Write, retrying `EAGAIN`, which is what an LWW round says when it loses to the sweep.
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

/// An OCC write that must land, re-observing on `EAGAIN`. Refusal is the register's only
/// answer to any conflict, and a repair round can raise the term and refuse a proposal with
/// no competing writer, so the client re-reads and retries. Asserted refusals use `write`.
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

// --- configuration ---

/// Two groups over six members, node 7 spare. Every node maps every extent, so any page is
/// reachable through any node; for half of them that node is outside the owning group,
/// which exercises forwarding. The catalog is balanced: each of the six members holds one
/// of the six seats, so every store formats to the same size. Node 7 holds nothing and
/// sizes itself for the share it would inherit. Device `MIX` maps the extents of `OCC` and
/// `LWW` in the other order: an extent belongs to the universe, not to a device.
fn config_text(generation: u32, n: &Node, peers: &[(u32, PathBuf)]) -> String {
    let mut s = format!(
        "generation {generation}\nnode id={} zone=1 cohort={} size={}\n",
        n.proc.id,
        (n.proc.id - 1) % 3,
        n.store_bytes,
    );
    s += "universe 1 epoch=1\n";
    for (id, dev) in peers {
        s += &format!("peer id={id} device={}\n", dev.display());
    }
    for g in catalog() {
        s += &format!("group {} {} {}\n", g[0], g[1], g[2]);
    }
    // Admission is per extent: extent 1 admits on first sight, extent 3 never admits.
    s += "extent id=1 base=0    pages=4096 kind=lww          zone=1 cache_admit=1\n\
          extent id=2 base=4096 pages=512  kind=occ          zone=1 cache_admit=1\n\
          extent id=3 base=4608 pages=512  kind=immutable    zone=1\n\
          extent id=4 base=5120 pages=4    kind=immutable_4m zone=1 cache_admit=1\n\
          device 1 extents=1\n\
          device 2 extents=2\n\
          device 3 extents=3\n\
          device 4 extents=4\n\
          device 5 extents=2,1\n";
    s
}

/// The catalog every generation is written with; replacing a member rewrites it.
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
        .filter(|n| n.proc.id != id)
        .map(|n| (n.proc.id, n.proc.fabric.clone()))
        .collect()
}

/// Tell the nodes at `who` where every fabric device now lives, as a new generation.
fn wire(nodes: &mut [Node], generation: u32, who: &[usize]) {
    wire_without(nodes, generation, who, &[]);
}

/// As [`wire`], but `who` is told nothing of the peers in `omit` and must route via others.
fn wire_without(nodes: &mut [Node], generation: u32, who: &[usize], omit: &[u32]) {
    let fabrics: Vec<(u32, PathBuf)> = nodes
        .iter()
        .map(|n| (n.proc.id, n.proc.fabric.clone()))
        .collect();
    for &i in who {
        let n = &mut nodes[i];
        let peers: Vec<(u32, PathBuf)> = fabrics
            .iter()
            .filter(|(id, _)| *id != n.proc.id && !omit.contains(id))
            .cloned()
            .collect();
        let text = config_text(generation, n, &peers);
        n.proc.install(&text);
        n.proc.await_reload();
    }
}

/// First page of the extent at `base` that group `want` owns, so "holds it" is exact below.
fn page_in(cfg: &Config, base: u64, want: u32) -> u64 {
    (0..512)
        .find(|off| cfg.group(addr(base + off)) == group(want))
        .expect("some page hashes to every group")
}

/// A page's address in the shared universe.
fn addr(lba: u64) -> u64 {
    racer::config::addr_of(UNIVERSE, lba)
}

/// A group of that universe, by index into its catalog.
fn group(index: u32) -> racer::config::GroupId {
    racer::config::GroupId::new(UNIVERSE, index)
}

// --- rebuilding a member ---

/// Cut every link to node `i`. With no peers its quorum is one, so reads come from its own
/// slab: the only way from outside to tell "holds the value" from "can go and fetch it".
fn isolate(nodes: &mut [Node], i: usize, generation: &mut u32) {
    *generation += 1;
    let text = config_text(*generation, &nodes[i], &[]);
    nodes[i].proc.install(&text);
    nodes[i].proc.await_reload();
}

/// Undo [`isolate`].
fn rejoin(nodes: &mut [Node], i: usize, generation: &mut u32) {
    *generation += 1;
    wire(nodes, *generation, &[i]);
}

/// Destroy node `i` and prove the group puts it back.
///
/// The node keeps its id and catalog seat but loses its data, so its empty digests make the
/// sweep replay every bucket. `want` is the group's contents, updated for the writes here.
fn rebuild(
    nodes: &mut [Node],
    i: usize,
    generation: &mut u32,
    want: &mut [(u64, Vec<u8>)],
    mark: u8,
) {
    let id = nodes[i].proc.id;
    let all: Vec<usize> = (0..nodes.len()).collect();

    // Remove the whole backing file, not just metadata: the restart places a blank store.
    nodes[i].proc.signal(libc::SIGKILL);
    nodes[i].proc.reap();
    std::fs::remove_file(&nodes[i].proc.store).expect("remove the store");
    nodes[i].proc.serve();
    assert_eq!(
        nodes[i].proc.store_len(),
        nodes[i].store_bytes,
        "a start must place a missing store at the configured size"
    );
    // The restarted node's fabric device is a fresh minor, so its peers need telling.
    *generation += 1;
    wire(nodes, *generation, &all);

    // Write through another member mid-join: replay and traffic must merge by version.
    {
        let d = nodes[(i + 1) % 3].dev(LWW);
        for (p, v) in want.iter_mut().take(8) {
            *v = pattern(mark, PAGE);
            write_lww(&d, *p * 4096, v);
        }
    }

    // Wait without sending: the sweep must restore the data, not a client read hitting a
    // hole. Each probe isolates the node (quorum one, so it cannot repair) and costs two
    // reloads, so back off rather than poll: `Live` keeps only one previous generation.
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

    // Holding the data is not membership: a replaying node forwards instead of proposing.
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

// --- the test ---

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
        n.proc.install(&text);
        n.proc.serve();
    }
    // Nothing created the store beforehand, and `size=` is all that sets its size.
    for n in &nodes {
        assert_eq!(
            n.proc.store_len(),
            IMG_BYTES,
            "node {} did not place its own store",
            n.proc.id
        );
    }

    // ---- second generation: now every node can name every peer's fabric device ----
    wire(&mut nodes, 2, &(0..NODES as usize).collect::<Vec<_>>());

    let cfg = Config::parse(&config_text(2, &nodes[0], &[])).unwrap();
    // Node 1 is in group 0, node 6 in group 1: for each page one holds the register and
    // the other must go through the fabric. Node 7 is spare and holds neither.
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
    // Node 1 is not in group 1, so it may cache `remote`. Repeated reads raise the width
    // past one and admit it; a metadata round confirms every hit on `(version, ballot)`.
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

    // ---- cache: an extent that opts out is never admitted, however hot it gets ------
    // Extent 3 opts out of admission, and node 1 does not hold this page, so every other
    // rule would cache it. A group member computes the width, so sum the counts everywhere.
    let cold = page_in(&cfg, IMM_BASE, 1);
    // Workers publish counters on a tick, so settle first or the scrape lags the work.
    let totals = |nodes: &[Node]| -> (u64, u64) {
        std::thread::sleep(std::time::Duration::from_millis(250));
        nodes
            .iter()
            .map(|n| {
                let m = scrape(n);
                (
                    m.get("racer_cache_reject_total{class=\"small\",reason=\"policy\"}"),
                    m.get("racer_cache_admit_total{class=\"small\"}"),
                )
            })
            .fold((0, 0), |(r, a), (dr, da)| (r + dr, a + da))
    };
    nodes[5]
        .dev(IMM)
        .write(cold * 4096, &pattern(0x21, PAGE))
        .unwrap();
    let (was_rejected, was_admitted) = totals(&nodes);
    let ca = nodes[0].dev(IMM);
    for _ in 0..64 {
        assert_eq!(
            ca.read(cold * 4096, PAGE).unwrap(),
            pattern(0x21, PAGE),
            "opting out of the cache must not change what a read returns"
        );
    }
    drop(ca);
    let (rejected, admitted) = totals(&nodes);
    assert!(
        rejected > was_rejected,
        "64 reads of an opted-out extent counted no policy rejection"
    );
    assert_eq!(
        admitted, was_admitted,
        "a page from an opted-out extent was admitted anyway"
    );

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
    // Device MIX concatenates extents 2 and 1, so its page 512 is device LWW's page 0. An
    // address belongs to its extent, so the device and its extent order do not matter.
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
    // Node 1 has proposed, read, cached and served a remote page, so its counters cover it.
    let m = scrape(&nodes[0]);
    assert!(
        m.get("racer_paxos_accept_total{result=\"ok\"}") > 0,
        "no accepts counted"
    );
    assert!(
        m.get("racer_cache_lookup_total{class=\"small\",result=\"hit\"}") > 0,
        "no cache hits counted"
    );
    let slots = m.get("racer_alloc_slots{class=\"small\"}");
    assert!(slots > 0, "the device has no small slots");
    assert!(
        m.get("racer_alloc_free_slots{class=\"small\"}") <= slots,
        "more free than there is"
    );
    // Written by core 0 alone; a second publisher would multiply each by the worker count.
    assert_eq!(
        m.get("racer_config_generation"),
        2,
        "the generation in force"
    );
    assert_eq!(m.get("racer_node_id"), nodes[0].proc.id as u64);
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
    // An equal share means identical slabs everywhere, so the spare needs no reformat.
    for (i, n) in nodes.iter().enumerate() {
        let their_slots = scrape(n).get("racer_alloc_slots{class=\"small\"}");
        assert_eq!(
            their_slots,
            slots,
            "node {} formatted {their_slots} small slots against node 1's {slots}",
            i + 1
        );
    }

    // A config the node must refuse: valid on its own, wrong only against the generation
    // already running. There is no caller to fail, so it surfaces as a metric or nowhere.
    nodes[0].proc.install(&config_text(1, &nodes[0], &[]));
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

    let (status, _) = http_get(&nodes[0].proc.metrics, "/nope");
    assert_eq!(status, "HTTP/1.1 404 Not Found", "only /metrics is served");

    // This node restarts below, so leave it a config it can boot from; the others rewire
    // at 3 separately, keeping generations monotonic per node.
    wire(&mut nodes, 3, &[0]);

    // ---- crash a node: the survivors serve on, and the node comes back -------------
    drop((a, b, oa, ob, ia, ib, ba, bb));
    nodes[0].proc.signal(libc::SIGKILL);
    nodes[0].proc.reap();
    // Group 0 is {1,2,3}, so two of the three members are still up.
    assert_eq!(
        nodes[1].dev(LWW).read(held * 4096, PAGE).unwrap(),
        pattern(2, PAGE),
        "a quorum keeps serving after a member dies"
    );
    // ---- the store grows on the way up, and never the other way --------------------
    // The control plane raises this node's share while it is down. A start reserves the
    // difference in place and leaves existing pages put; a lower `size=` is refused.
    assert_eq!(
        nodes[0].proc.store_len(),
        IMG_BYTES,
        "formatted at its share"
    );
    {
        let peers = peers_of(&nodes, nodes[0].proc.id);
        nodes[0].store_bytes = IMG_BYTES - PAGE as u64;
        let said = nodes[0].serve_refuses(&config_text(3, &nodes[0], &peers));
        assert!(
            said.contains(&IMG_BYTES.to_string())
                && said.contains(&(IMG_BYTES - PAGE as u64).to_string()),
            "a refused shrink must name both sizes: {said}"
        );
        assert_eq!(
            nodes[0].proc.store_len(),
            IMG_BYTES,
            "a refused start must leave the store alone"
        );

        nodes[0].store_bytes = GROWN_BYTES;
        nodes[0].proc.install(&config_text(3, &nodes[0], &peers));
    }
    nodes[0].proc.serve();
    assert_eq!(
        nodes[0].proc.store_len(),
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
    // Group 1 is {4,5,6}. Node 1 keeps only its link to 4, so the other two legs of a read
    // are forwarded through it. Without the forward a read gets one answer, never a pair.
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
    // Far more digest buckets than a steady-state sweep walks: the whole group must move.
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

    // All three members of group 0, one at a time, so no original copy survives to explain
    // the data. Two at once would leave no quorum, and losing the value then is correct.
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
    // Balance is enforced: two of the six seats across five nodes fits no equal split.
    set_catalog(&[[1, 2, 3], [4, 5, 1]]);
    let skewed = Config::parse(&config_text(generation + 1, &nodes[0], &[])).unwrap();
    assert!(
        skewed.validate().is_err(),
        "an unbalanced catalog must be refused"
    );

    // Node 7 takes every seat node 1 held and no others, so the zone stays balanced. The
    // catalog keeps its length, so groups keep their addresses and only members change.
    let before = scrape(&nodes[0]).get("racer_alloc_free_slots{class=\"small\"}");
    set_catalog(&[[7, 2, 3], [4, 5, 6]]);
    generation += 1;
    wire(
        &mut nodes,
        generation,
        &(0..NODES as usize).collect::<Vec<_>>(),
    );
    // The member count sets the share, so a swap needs no reformat on either node.
    assert_eq!(
        scrape(&nodes[6]).get("racer_alloc_slots{class=\"small\"}"),
        slots,
        "a replacement must not resize the zone"
    );

    // Node 7 replays group 0 from the surviving members; node 1 sheds only what it can see
    // all three new members holding, so the two halves finish in that order.
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
