//! `racer-bench` - what a client gets out of the dataplane, through real block devices.
//!
//! ```text
//! racer-bench e2e    # a real cluster through real ublk devices (needs root)
//! ```
//!
//! The ladder it prints - a raw ext4 file, then one node alone, then a three node group -
//! is what makes the numbers attributable: the raw row is what the load generator and one
//! store can do between them on this machine, and every row below it is read against that.
//! One store: a three node cluster has three, so the group rows can and do pass the raw
//! row on bandwidth. What the raw row bounds is a node, not a cluster.
//!
//! Storage is memory throughout, the same brd-ext4 stack `tests/cluster.rs` runs on, so
//! the CPU is the only thing that can be the limit, which is the question being asked.
//! A loop device would not do: it takes one hardware queue and one kernel worker, which
//! caps a store near 8 GiB/s however many cores ask, and a benchmark that measures that
//! is measuring the loop driver.

#[path = "harness.rs"]
mod harness;

use std::io::{Read, Write};
use std::net::TcpStream;
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use io_uring::{IoUring, opcode, types};

const HUGE: usize = 4 << 20;

fn main() {
    let mut args = std::env::args().skip(1);
    match args.next().as_deref() {
        Some("e2e") => e2e(Args::parse(args)),
        other => {
            if let Some(cmd) = other {
                eprintln!("racer-bench: unknown command {cmd}");
            }
            eprintln!(
                "usage: racer-bench e2e [--cores N] [--jobs N] [--depth N] [--seconds S]\n\
                 \x20                      [--groups N] [--pages N] [--only NAME] [--solo | --group]"
            );
            std::process::exit(2);
        }
    }
}

// ---------------------------------------------------------------------------
// command line
// ---------------------------------------------------------------------------

struct Args {
    cores: usize,
    jobs: usize,
    depth: usize,
    seconds: u64,
    groups: usize,
    pages: u64,
    ladder: Vec<u32>,
    /// Run only the workloads whose name contains this. Filtering out `4m write (fill)`
    /// leaves `4m randread` reading holes, which is a different amount of work.
    only: String,
}

impl Args {
    fn parse(mut it: impl Iterator<Item = String>) -> Args {
        let mut a = Args {
            cores: 2,
            jobs: 4,
            depth: 32,
            seconds: 5,
            groups: 12,
            pages: 262_144,
            ladder: vec![1, 3],
            only: String::new(),
        };
        while let Some(k) = it.next() {
            match k.as_str() {
                "--cores" => a.cores = num(&mut it),
                "--jobs" => a.jobs = num(&mut it),
                "--depth" => a.depth = num(&mut it),
                "--seconds" => a.seconds = num(&mut it) as u64,
                "--groups" => a.groups = num(&mut it),
                "--pages" => a.pages = num(&mut it) as u64,
                "--only" => a.only = it.next().expect("a workload name"),
                "--solo" => a.ladder = vec![1],
                "--group" => a.ladder = vec![3],
                other => panic!("unknown flag {other}"),
            }
        }
        a
    }
}

fn num(it: &mut impl Iterator<Item = String>) -> usize {
    it.next().expect("a value").parse().expect("a number")
}

// ---------------------------------------------------------------------------
// end to end
// ---------------------------------------------------------------------------

fn e2e(a: Args) {
    let root = unsafe { libc::geteuid() } == 0;
    assert!(
        root,
        "racer-bench e2e needs root for ublk, loop devices and mounts"
    );
    let run = Duration::from_secs(a.seconds);

    println!(
        "racer-bench e2e  ({} cores/node, {} client threads, depth {}, {} s per row)\n",
        a.cores, a.jobs, a.depth, a.seconds
    );
    header();

    for &nodes in &a.ladder {
        clear_root();
        let plan = Plan {
            nodes,
            cores: a.cores,
            groups: a.groups,
            small_pages: a.pages,
            ..Plan::default()
        };
        // Clients take the logical CPUs above the node processes' cores, spread over the
        // whole remaining range: blk-mq picks a device queue from the submitting CPU, so
        // bunched clients all land on one worker and the node looks single-threaded.
        let online = unsafe { libc::sysconf(libc::_SC_NPROCESSORS_ONLN) } as usize;
        let first = plan.client_first_core() * 2;
        // The nodes can ask for every core the machine has, and then there is no range
        // left to spread over and nowhere to put a client. Say so: the arithmetic below
        // answers that case with a CPU that does not exist, and a plan that cannot be
        // measured has to stop rather than print a number made somewhere unknown.
        assert!(
            first < online,
            "{} nodes of {} cores take all {online} logical CPUs and leave the clients \
             nowhere to run: ask for fewer cores, or fewer nodes",
            plan.nodes,
            plan.cores,
        );
        let cpus: Vec<usize> = (0..a.jobs)
            .map(|i| first + i * (online - first) / a.jobs)
            .collect();

        if nodes == 1 {
            // A file system of its own, thrown away before the cluster starts: a node
            // adopts any store whose superblock region is not zeros, and this one is
            // about to be written all over.
            let mnt = PathBuf::from(ROOT).join("baseline");
            let backing = harness::Backing::new(&mnt, FS_BYTES, RAM_BASE);
            let files = [reserve(&backing.path("store.img"))];

            // Memory is spent on a page when something stores it and the image file is
            // sparse, so reading blocks nobody wrote measures a zero fill rather than a
            // copy. Touch the whole span first.
            let span = plan.small_bytes().max(plan.huge_bytes());
            let mut j = Job::new(&files, HUGE, span);
            j.depth = 4;
            j.cpus = vec![cpus[0]];
            j.write = true;
            j.sequential = true;
            j.warmup = Duration::ZERO;
            j.run = Duration::from_secs(600);
            run_load(&j).expect("baseline fill");

            // The same generator, the same file system and the same block layer as the
            // store the nodes are about to use. Every row below is read against this one.
            let mut j = Job::new(&files, 4096, plan.small_bytes());
            j.depth = a.depth;
            j.cpus = cpus.clone();
            j.run = run;
            report(&a, "ext4 file", "4k randread", &plan, &j);
            j.write = true;
            report(&a, "ext4 file", "4k randwrite", &plan, &j);
            let mut j = Job::new(&files, HUGE, plan.huge_bytes());
            j.depth = 4.min(a.depth);
            j.cpus = cpus.clone();
            j.run = run;
            report(&a, "ext4 file", "4m randread", &plan, &j);
            let mut j = Job::new(&files, 4096, plan.small_bytes());
            j.depth = 1;
            j.cpus = vec![cpus[0]];
            j.run = run;
            report(&a, "ext4 file", "4k read  qd1", &plan, &j);
        }

        let c = Cluster::start(&plan);
        let tag: &str = if nodes == 1 { "1 node" } else { "3 nodes" };
        for n in &c.nodes {
            println!("  # node {} metrics {}", n.proc.id, n.proc.metrics);
        }
        // Every node exports the device and is a member of every group, so load is
        // spread over all of them; driving a single gateway would measure that node.
        let small: Vec<_> = c
            .nodes
            .iter()
            .map(|n| n.proc.device(minor(n.proc.id, LWW)).to_path_buf())
            .collect();
        let big: Vec<_> = c
            .nodes
            .iter()
            .map(|n| n.proc.device(minor(n.proc.id, BIG)).to_path_buf())
            .collect();

        // Fill the 4 KiB extent once, so reads find pages and writes overwrite rather
        // than allocate: a hole and a filled page are different amounts of work.
        let mut fill = Job::new(&small, 4096, plan.small_bytes());
        fill.depth = a.depth;
        fill.cpus = cpus.clone();
        fill.write = true;
        fill.sequential = true;
        fill.warmup = Duration::ZERO;
        fill.run = Duration::from_secs(600);
        let f = run_load(&fill).expect("fill");
        assert_eq!(
            f.errors, 0,
            "filling the 4 KiB extent failed with errno {}",
            f.errno
        );
        emit(tag, "4k fill (seq)", &plan, &f);

        let mut j = Job::new(&small, 4096, plan.small_bytes());
        j.depth = a.depth;
        j.cpus = cpus.clone();
        j.run = run;
        report_racer(&a, tag, "4k randread", &plan, &j, &c);
        j.write = true;
        report_racer(&a, tag, "4k randwrite", &plan, &j, &c);

        // An immutable 4 MiB page may be filled once per epoch, so its write row is a
        // single pass over the extent rather than a timed window.
        let mut j = Job::new(&big, HUGE, plan.huge_bytes());
        j.depth = 4.min(a.depth);
        j.cpus = cpus.clone();
        j.write = true;
        j.sequential = true;
        j.warmup = Duration::ZERO;
        j.run = Duration::from_secs(600);
        report_racer(&a, tag, "4m write (fill)", &plan, &j, &c);
        j.write = false;
        j.sequential = false;
        j.warmup = Duration::from_millis(500);
        j.run = run;
        report_racer(&a, tag, "4m randread", &plan, &j, &c);

        // One request in flight per thread: latency without the queueing every row
        // above is full of.
        let mut j = Job::new(&small, 4096, plan.small_bytes());
        j.depth = 1;
        j.cpus = vec![cpus[0]];
        j.run = run;
        report_racer(&a, tag, "4k read  qd1", &plan, &j, &c);
        j.write = true;
        report_racer(&a, tag, "4k write qd1", &plan, &j, &c);
    }
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

fn header() {
    println!(
        "  {:<9} {:<16} {:>10} {:>8} {:>9} {:>8} {:>8} {:>8} {:>8}  IOPS/core",
        "target", "workload", "IOPS", "GiB/s", "mean µs", "p50", "p99", "p99.9", "max"
    );
}

fn report(a: &Args, target: &str, name: &str, plan: &Plan, job: &Job) {
    if !name.contains(&a.only) {
        return;
    }
    let r = run_load(job).expect("load generator");
    assert_eq!(
        r.errors, 0,
        "{target} {name}: {} requests failed, errno {}",
        r.errors, r.errno
    );
    emit(target, name, plan, &r);
}

fn report_racer(a: &Args, target: &str, name: &str, plan: &Plan, job: &Job, c: &Cluster) {
    if !name.contains(&a.only) {
        return;
    }
    let mut timed = job.clone();
    if !job.warmup.is_zero() {
        let mut warm = job.clone();
        warm.run = job.warmup;
        warm.warmup = Duration::ZERO;
        let r = run_load(&warm).expect("warmup load generator");
        assert_eq!(
            r.errors, 0,
            "{target} {name} warmup: {} requests failed, errno {}",
            r.errors, r.errno
        );
        timed.warmup = Duration::ZERO;
    }
    // An idle worker may publish only when its 100 ms park expires. Settle both scrape
    // boundaries so the delta covers the counted run rather than its warmup or next row.
    std::thread::sleep(Duration::from_millis(110));
    let before = scrape_cluster(c);
    let r = run_load(&timed).expect("load generator");
    assert_eq!(
        r.errors, 0,
        "{target} {name}: {} requests failed, errno {}",
        r.errors, r.errno
    );
    std::thread::sleep(Duration::from_millis(110));
    let after = scrape_cluster(c);
    emit(target, name, plan, &r);
    emit_flush_metrics(
        after.saturating_sub(before),
        Duration::from_secs_f64(r.secs),
        plan,
        r.ops,
    );
}

fn emit(target: &str, name: &str, plan: &Plan, r: &Report) {
    let per_core = r.iops() / (plan.nodes as usize * plan.cores) as f64;
    println!(
        "  {target:<9} {name:<16} {:>10.0} {:>8.2} {:>9.1} {:>8.1} {:>8.1} {:>8.1} {:>8.1}  {per_core:>10.0}",
        r.iops(),
        r.gib_s(),
        r.hist.mean() / 1000.0,
        r.hist.pct(0.50) as f64 / 1000.0,
        r.hist.pct(0.99) as f64 / 1000.0,
        r.hist.pct(0.999) as f64 / 1000.0,
        r.hist.max() as f64 / 1000.0,
    );
}

#[derive(Clone, Copy, Default)]
struct FlushMetrics {
    commits: u64,
    flushes: u64,
    batch: u64,
    parks: u64,
    busy_us: u64,
    one_shot: u64,
    prepares: u64,
    rejected: u64,
    conflicts: u64,
    retries: u64,
    repairs: u64,
}

impl FlushMetrics {
    fn saturating_sub(self, old: FlushMetrics) -> FlushMetrics {
        FlushMetrics {
            commits: self.commits.saturating_sub(old.commits),
            flushes: self.flushes.saturating_sub(old.flushes),
            batch: self.batch.saturating_sub(old.batch),
            parks: self.parks.saturating_sub(old.parks),
            busy_us: self.busy_us.saturating_sub(old.busy_us),
            one_shot: self.one_shot.saturating_sub(old.one_shot),
            prepares: self.prepares.saturating_sub(old.prepares),
            rejected: self.rejected.saturating_sub(old.rejected),
            conflicts: self.conflicts.saturating_sub(old.conflicts),
            retries: self.retries.saturating_sub(old.retries),
            repairs: self.repairs.saturating_sub(old.repairs),
        }
    }

    fn add(&mut self, name: &str, value: u64) {
        match name {
            r#"racer_mblock_commits_total{class="small"}"# => self.commits += value,
            r#"racer_mblock_flushes_total{class="small"}"# => self.flushes += value,
            r#"racer_mblock_flush_batch_total{class="small"}"# => self.batch += value,
            r#"racer_commit_park_total{class="small"}"# => self.parks += value,
            r#"racer_flush_busy_us_total{class="small"}"# => self.busy_us += value,
            "racer_paxos_one_shot_total" => self.one_shot += value,
            "racer_paxos_prepare_total" => self.prepares += value,
            r#"racer_paxos_accept_total{result="rejected"}"# => self.rejected += value,
            "racer_paxos_guard_conflicts_total" => self.conflicts += value,
            "racer_paxos_lww_retry_total" => self.retries += value,
            "racer_paxos_repair_total" => self.repairs += value,
            _ => {}
        }
    }
}

fn scrape_cluster(c: &Cluster) -> FlushMetrics {
    let mut total = FlushMetrics::default();
    for n in &c.nodes {
        let mut s = TcpStream::connect(&n.proc.metrics).expect("connect metrics endpoint");
        s.write_all(b"GET /metrics HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n")
            .expect("request metrics");
        let mut response = String::new();
        s.read_to_string(&mut response).expect("read metrics");
        for line in response.split("\r\n\r\n").nth(1).unwrap_or("").lines() {
            let Some((name, value)) = line.split_once(' ') else {
                continue;
            };
            if let Ok(value) = value.parse() {
                total.add(name, value);
            }
        }
    }
    total
}

fn emit_flush_metrics(m: FlushMetrics, elapsed: Duration, plan: &Plan, ops: u64) {
    let batch = if m.flushes == 0 {
        0.0
    } else {
        m.batch as f64 / m.flushes as f64
    };
    let core_time = elapsed.as_secs_f64() * 1_000_000.0 * (plan.nodes as usize * plan.cores) as f64;
    let flight = if core_time == 0.0 {
        0.0
    } else {
        100.0 * m.busy_us as f64 / core_time
    };
    println!(
        "  # small mblock commits={} flushes={} batch={batch:.2} parks={} flight={flight:.1}%",
        m.commits, m.flushes, m.parks
    );
    // Consensus work per user operation. A healthy write is one one-shot accept and
    // nothing else; anything above that is a round the cluster paid for and threw away.
    let paxos = m.one_shot + m.prepares + m.rejected + m.conflicts + m.retries + m.repairs;
    if paxos == 0 {
        return;
    }
    let per = |v: u64| {
        if ops == 0 { 0.0 } else { v as f64 / ops as f64 }
    };
    println!(
        "  # paxos/op one_shot={:.2} prepares={:.2} rejected={:.2} conflicts={:.2} retries={:.2} repairs={:.2}",
        per(m.one_shot),
        per(m.prepares),
        per(m.rejected),
        per(m.conflicts),
        per(m.retries),
        per(m.repairs),
    );
}

// ---------------------------------------------------------------------------
// the cluster
// ---------------------------------------------------------------------------

const ROOT: &str = "/tmp/racer-bench";

/// Export roles: one device per extent of the universe, plus the fabric namespace. A role
/// is not an id. An id is a ublk minor, minors are host-wide, and these nodes share a host,
/// so the id a node asks for has to carry the node in it (see [`minor`]).
const LWW: u32 = 1;
const BIG: u32 = 2;
const FABRIC: u32 = 3;

/// The ublk minor node `id` exports `role` as. The block is above anything the tests ask
/// for, so a benchmark and a test suite may run at once.
fn minor(id: u32, role: u32) -> u32 {
    200 + 10 * id + role
}

/// The ram disk node `id` puts its file system on, and `RAM_BASE` itself for the raw
/// baseline, which runs before any node starts. Above the block the tests use, for the
/// same reason the minors are.
const RAM_BASE: u32 = 8;

fn ram(id: u32) -> u32 {
    RAM_BASE + id
}

/// Bytes of file system per node: room for the store image and the ext4 metadata around
/// it. Memory is spent only on what is written, so the slack costs nothing.
const FS_BYTES: u64 = 8 << 30;

/// Bytes of the store image on each node's file system: the default plan's extents plus
/// the metadata and over-provisioning around them, with the rest left to ext4.
const STORE_BYTES: u64 = FS_BYTES - (1 << 30);

/// How the cluster is laid out. `racer-bench e2e` overrides `nodes`, `cores`, `groups`
/// and `small_pages` from its flags and takes the rest from here.
#[derive(Clone)]
struct Plan {
    /// 1 for a node alone (no peers, so quorum is one and nothing crosses the fabric)
    /// or 3 for a real consensus group.
    nodes: u32,
    /// Physical cores per node process. The runtime runs one worker per physical core in
    /// its affinity mask, so this is the only knob there is.
    cores: usize,
    /// The first physical core to hand out. Nodes take `cores` each from here in order,
    /// and the load generator takes the ones above them.
    first_core: usize,
    /// 4 KiB pages in the LWW extent.
    small_pages: u64,
    /// 4 MiB pages in the immutable extent.
    huge_pages: u64,
    /// Consensus groups in the catalog. A group's allocator index shard and its
    /// consensus state both live on core `group % cores`, so fewer groups than cores
    /// leaves cores with nothing to do.
    groups: usize,
    /// DRAM the cache may spend on slot records. The cache takes whatever the slabs
    /// left in the store either way, so this is what actually sizes it.
    cache_index_bytes: u64,
    /// `cache_admit` for both extents. Zero caches nothing, which is what a member-only
    /// read wants; one caches on first sight.
    cache_admit: u32,
}

impl Default for Plan {
    fn default() -> Plan {
        Plan {
            nodes: 3,
            cores: 2,
            first_core: 0,
            small_pages: 262_144,
            huge_pages: 512,
            groups: 12,
            cache_index_bytes: 1 << 30,
            cache_admit: 0,
        }
    }
}

impl Plan {
    /// The bytes of each extent a workload touches: three quarters of it, so
    /// out-of-place writes always have free slots, since slabs carry only 5% spare.
    fn small_bytes(&self) -> u64 {
        self.small_pages / 4 * 3 * 4096
    }

    fn huge_bytes(&self) -> u64 {
        self.huge_pages / 4 * 3 * (4 << 20)
    }

    /// The first core above the node processes', which is the client's.
    fn client_first_core(&self) -> usize {
        self.first_core + self.nodes as usize * self.cores
    }
}

struct Node {
    /// Declared before the backing store: the process must be gone before its store is
    /// unmounted, and fields drop in order.
    proc: harness::Proc,
    _backing: harness::Backing,
}

struct Cluster {
    nodes: Vec<Node>,
}

impl Cluster {
    /// Lay down fresh stores, start the nodes and wire them to each other.
    fn start(plan: &Plan) -> Cluster {
        let racer = racer_bin();
        let mut nodes = Vec::new();
        for i in 0..plan.nodes {
            let id = i + 1;
            let dir = PathBuf::from(ROOT).join(format!("n{id}"));
            // A file system of its own per node, made here and gone when the cluster is:
            // every run formats a blank store rather than finding one left behind.
            let backing = harness::Backing::new(&dir.join("mnt"), FS_BYTES, ram(id));
            let store = reserve(&backing.path("store.img"));
            let mut proc = harness::Proc::new(id, dir, store, racer.clone());
            // Both SMT siblings of every core: the runtime folds them back to one worker
            // per physical core and parks the control thread on the spare sibling.
            let lo = plan.first_core + i as usize * plan.cores;
            proc.pin(
                (lo..lo + plan.cores)
                    .flat_map(|c| [c * 2, c * 2 + 1])
                    .collect(),
            );
            nodes.push(Node {
                proc,
                _backing: backing,
            });
        }

        // Generation 1 names no peer: a peer's fabric device does not exist until its
        // process is running.
        for n in &nodes {
            n.proc.install(&text(plan, n.proc.id, &[], 1));
        }
        for n in &mut nodes {
            n.proc.serve();
        }
        if plan.nodes > 1 {
            let fabrics: Vec<(u32, PathBuf)> = nodes
                .iter()
                .map(|n| (n.proc.id, n.proc.fabric.clone()))
                .collect();
            for n in &mut nodes {
                let peers: Vec<(u32, PathBuf)> = fabrics
                    .iter()
                    .filter(|(id, _)| *id != n.proc.id)
                    .cloned()
                    .collect();
                n.proc.install(&text(plan, n.proc.id, &peers, 2));
                n.proc.await_reload();
            }
        }
        Cluster { nodes }
    }
}

impl Drop for Cluster {
    fn drop(&mut self) {
        harness::shutdown(self.nodes.iter_mut().map(|n| &mut n.proc));
    }
}

/// An empty store image of `STORE_BYTES`, reserved rather than written: the file is
/// sparse until racer formats it, and memory backs only the pages actually stored.
fn reserve(path: &Path) -> PathBuf {
    std::fs::File::create(path)
        .and_then(|f| f.set_len(STORE_BYTES))
        .unwrap_or_else(|e| panic!("reserve {}: {e}", path.display()));
    path.to_path_buf()
}

/// Detach whatever an earlier run left mounted under `ROOT` and take the tree away. A run
/// killed before it could unwind leaves its mounts behind; the memory under them is
/// already freed, so there is nothing in them to keep.
fn clear_root() {
    let mounts = std::fs::read_to_string("/proc/self/mounts").unwrap_or_default();
    for at in mounts.lines().filter_map(|l| l.split(' ').nth(1)) {
        if at.starts_with(ROOT) {
            let at = at.replace("\\040", " ");
            unsafe { libc::umount2(harness::cstr(Path::new(&at)).as_ptr(), libc::MNT_DETACH) };
        }
    }
    let _ = std::fs::remove_dir_all(ROOT);
    std::fs::create_dir_all(ROOT).expect("create the bench directory");
}

/// The `racer` binary beside this one.
fn racer_bin() -> PathBuf {
    let me = std::env::current_exe().expect("current_exe");
    let p = me.parent().expect("a bin directory").join("racer");
    assert!(
        p.exists(),
        "{} is missing; build the `racer` binary too",
        p.display()
    );
    p
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

/// One universe holding an LWW extent of 4 KiB pages and an immutable extent of 4 MiB
/// ones: the two page classes, one device each. OCC is left out because its cost is the
/// read it requires, which the LWW extent already measures.
fn text(plan: &Plan, id: u32, peers: &[(u32, PathBuf)], generation: u32) -> String {
    let mut s = format!(
        "generation {generation}\nnode id={id} zone=1 cohort={} size={STORE_BYTES}\n",
        (id - 1) % 3,
    );
    s += &format!("policy cache_index_bytes={}\n", plan.cache_index_bytes);
    s += &format!(
        "universe 1 epoch=1 fabric_device_id={}\n",
        minor(id, FABRIC)
    );
    for (id, dev) in peers {
        s += &format!("peer id={id} device={}\n", dev.display());
    }
    // Every group is the same three nodes in a different order: one replica set, many
    // groups sharing it. A lone node is a member of all of them and, having no peers,
    // proposes and reads by itself at quorum one.
    const ORDER: [[u32; 3]; 6] = [
        [1, 2, 3],
        [2, 3, 1],
        [3, 1, 2],
        [1, 3, 2],
        [3, 2, 1],
        [2, 1, 3],
    ];
    for i in 0..plan.groups {
        let g = ORDER[i % ORDER.len()];
        s += &format!("group {} {} {}\n", g[0], g[1], g[2]);
    }
    // The huge extent starts on a 4 MiB boundary above the small one: a 4 MiB page is
    // 1024 blocks of the universe's flat address space.
    let huge_base = plan.small_pages.next_multiple_of(1024);
    s += &format!(
        "extent id=1 base=0 pages={} kind=lww zone=1 cache_admit={admit}\n\
         extent id=2 base={huge_base} pages={} kind=immutable_4m zone=1 cache_admit={admit}\n\
         device {lww} extents=1\ndevice {big} extents=2\n",
        plan.small_pages,
        plan.huge_pages,
        admit = plan.cache_admit,
        lww = minor(id, LWW),
        big = minor(id, BIG),
    );
    s
}

// ---------------------------------------------------------------------------
// load generation
// ---------------------------------------------------------------------------
//
// One io_uring per thread, `O_DIRECT`, registered file and registered buffers, `depth`
// requests in flight and a new page issued the moment one completes. The same generator
// drives a raw file and a racer device, which is the only honest way to say how much of
// the cost is ours.

/// The largest queue depth a thread may ask for.
const MAX_DEPTH: usize = 512;

/// What to run against one device.
#[derive(Clone)]
struct Job {
    /// Devices exporting the same extents; thread `i` takes path `i % len`, since every
    /// node of a group is an equal gateway to the same pages.
    paths: Vec<PathBuf>,
    /// Request size; also the alignment of every offset.
    bs: usize,
    /// Requests in flight per thread.
    depth: usize,
    /// One thread per cpu named here.
    cpus: Vec<usize>,
    /// Addressable bytes. Offsets are uniform over `span / bs` pages.
    span: u64,
    write: bool,
    /// Walk the pages in order, each thread taking its own stripe, and stop when the
    /// stripe ends. A one-shot fill of an immutable extent needs this: a page may be
    /// written once.
    sequential: bool,
    warmup: Duration,
    run: Duration,
}

impl Job {
    fn new(paths: &[PathBuf], bs: usize, span: u64) -> Job {
        Job {
            paths: paths.to_vec(),
            bs,
            depth: 32,
            cpus: vec![0],
            span,
            write: false,
            sequential: false,
            warmup: Duration::from_millis(500),
            run: Duration::from_secs(5),
        }
    }
}

/// What one job did.
struct Report {
    ops: u64,
    errors: u64,
    /// The first errno seen, if any.
    errno: i32,
    secs: f64,
    bs: usize,
    hist: Hist,
}

impl Report {
    fn iops(&self) -> f64 {
        self.ops as f64 / self.secs
    }

    fn gib_s(&self) -> f64 {
        self.ops as f64 * self.bs as f64 / self.secs / (1u64 << 30) as f64
    }
}

/// Run `job`; the report is every thread's work together.
fn run_load(job: &Job) -> std::io::Result<Report> {
    assert!(
        job.depth <= MAX_DEPTH,
        "depth {} is over {MAX_DEPTH}",
        job.depth
    );
    assert!(!job.cpus.is_empty(), "a job needs at least one thread");
    let mut threads = Vec::new();
    for (i, &cpu) in job.cpus.iter().enumerate() {
        let j = job.clone();
        threads.push(std::thread::spawn(move || one(j, i, cpu)));
    }
    let mut r = Report {
        ops: 0,
        errors: 0,
        errno: 0,
        secs: 0.0,
        bs: job.bs,
        hist: Hist::new(),
    };
    for t in threads {
        let one = t.join().expect("load thread panicked")?;
        r.ops += one.ops;
        r.errors += one.errors;
        if r.errno == 0 {
            r.errno = one.errno;
        }
        // Threads share one window, so the job's duration is the longest of them and
        // its rate is the sum of theirs over that.
        r.secs = r.secs.max(one.secs);
        r.hist.merge(&one.hist);
    }
    Ok(r)
}

/// One thread: `depth` requests in flight until the window closes.
fn one(j: Job, index: usize, cpu: usize) -> std::io::Result<Report> {
    pin(cpu);
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(j.write)
        .custom_flags(libc::O_DIRECT)
        .open(&j.paths[index % j.paths.len()])?;

    let mut ring: IoUring = IoUring::builder()
        .setup_single_issuer()
        .build(j.depth as u32 * 2)?;
    ring.submitter().register_files(&[file.as_raw_fd()])?;
    let mem = Aligned::new(j.depth * j.bs);
    let iovecs: Vec<libc::iovec> = (0..j.depth)
        .map(|i| libc::iovec {
            iov_base: unsafe { mem.ptr.add(i * j.bs) } as *mut _,
            iov_len: j.bs,
        })
        .collect();
    unsafe { ring.submitter().register_buffers(&iovecs)? };

    let pages = (j.span / j.bs as u64).max(1);
    let threads = j.cpus.len() as u64;
    let mut cursor = pages * index as u64 / threads;
    let last = pages * (index as u64 + 1) / threads;
    let mut rng = 0x9e37_79b9_7f4a_7c15u64 ^ (index as u64 + 1).wrapping_mul(0x2545_f491_4f6c_dd1d);

    let mut free: Vec<usize> = (0..j.depth).rev().collect();
    let mut sent = [Instant::now(); MAX_DEPTH];
    let mut r = Report {
        ops: 0,
        errors: 0,
        errno: 0,
        secs: 0.0,
        bs: j.bs,
        hist: Hist::new(),
    };

    let mut counted_from = Instant::now() + j.warmup;
    let mut stop_at = counted_from + j.run;
    let mut counting = false;

    loop {
        let now = Instant::now();
        if !counting && now >= counted_from {
            // Warmup is over: throw it away and restart the clock.
            counting = true;
            counted_from = now;
            stop_at = now + j.run;
            r.hist = Hist::new();
            r.ops = 0;
            r.errors = 0;
        }
        let done = now >= stop_at || (j.sequential && cursor >= last);
        while !done && let Some(slot) = free.pop() {
            let page = if j.sequential {
                cursor += 1;
                cursor - 1
            } else {
                rng ^= rng << 13;
                rng ^= rng >> 7;
                rng ^= rng << 17;
                rng % pages
            };
            let ptr = unsafe { mem.ptr.add(slot * j.bs) };
            let e = if j.write {
                opcode::WriteFixed::new(types::Fixed(0), ptr, j.bs as u32, slot as u16)
                    .offset(page * j.bs as u64)
                    .build()
            } else {
                opcode::ReadFixed::new(types::Fixed(0), ptr, j.bs as u32, slot as u16)
                    .offset(page * j.bs as u64)
                    .build()
            };
            sent[slot] = Instant::now();
            unsafe {
                ring.submission()
                    .push(&e.user_data(slot as u64))
                    .expect("sq space")
            };
            if j.sequential && cursor >= last {
                break;
            }
        }
        if free.len() == j.depth {
            if done {
                break;
            }
            continue;
        }
        ring.submit_and_wait(1)?;
        let now = Instant::now();
        let mut cq = ring.completion();
        cq.sync();
        for cqe in cq {
            let slot = cqe.user_data() as usize;
            if cqe.result() < 0 {
                r.errors += 1;
                if r.errno == 0 {
                    r.errno = -cqe.result();
                }
            } else {
                r.ops += 1;
                r.hist.add(now.duration_since(sent[slot]).as_nanos() as u64);
            }
            free.push(slot);
        }
    }
    r.secs = counted_from.elapsed().as_secs_f64();
    Ok(r)
}

/// Hold this thread on one logical CPU.
///
/// Checked, because the failure is silent otherwise and the run goes on: a client that
/// drifts takes its device queue with it, which is the one thing the spread above exists
/// to prevent, and the row it produces looks like an answer.
fn pin(cpu: usize) {
    let rc = unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        libc::CPU_SET(cpu, &mut set);
        libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set)
    };
    assert_eq!(
        rc,
        0,
        "pin a client to CPU {cpu}: {}",
        std::io::Error::last_os_error()
    );
}

// ---------------------------------------------------------------------------
// latency
// ---------------------------------------------------------------------------

/// A log-linear histogram: exact below 16 ns, then 16 buckets per octave, so under 7%
/// error everywhere and mergeable by addition.
struct Hist {
    buckets: Vec<u64>,
    count: u64,
    sum: u64,
    max: u64,
}

const BUCKETS: usize = 1024;

impl Hist {
    fn new() -> Hist {
        Hist {
            buckets: vec![0; BUCKETS],
            count: 0,
            sum: 0,
            max: 0,
        }
    }

    fn index(v: u64) -> usize {
        if v < 16 {
            return v as usize;
        }
        let k = 63 - v.leading_zeros() as usize;
        let sub = ((v >> (k - 4)) & 15) as usize;
        (16 + (k - 4) * 16 + sub).min(BUCKETS - 1)
    }

    /// The low edge of bucket `i`, which is what a percentile reports.
    fn value(i: usize) -> u64 {
        if i < 16 {
            return i as u64;
        }
        let shift = ((i - 16) / 16) as u32;
        (16 + (i % 16) as u64) << shift.min(59)
    }

    fn add(&mut self, ns: u64) {
        self.buckets[Self::index(ns)] += 1;
        self.count += 1;
        self.sum += ns;
        self.max = self.max.max(ns);
    }

    fn merge(&mut self, o: &Hist) {
        for (a, b) in self.buckets.iter_mut().zip(&o.buckets) {
            *a += b;
        }
        self.count += o.count;
        self.sum += o.sum;
        self.max = self.max.max(o.max);
    }

    fn mean(&self) -> f64 {
        if self.count == 0 {
            0.0
        } else {
            self.sum as f64 / self.count as f64
        }
    }

    fn pct(&self, p: f64) -> u64 {
        if self.count == 0 {
            return 0;
        }
        let want = (self.count as f64 * p).ceil() as u64;
        let mut seen = 0;
        for (i, &n) in self.buckets.iter().enumerate() {
            seen += n;
            if seen >= want {
                return Self::value(i);
            }
        }
        self.max
    }

    fn max(&self) -> u64 {
        self.max
    }
}

// ---------------------------------------------------------------------------
// aligned memory
// ---------------------------------------------------------------------------

struct Aligned {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

impl Aligned {
    fn new(len: usize) -> Aligned {
        let layout = std::alloc::Layout::from_size_align(len, 4096).unwrap();
        let ptr = unsafe { std::alloc::alloc(layout) };
        assert!(!ptr.is_null(), "out of memory");
        // Touch every page: a first fault inside the timed window is not what is
        // measured.
        unsafe { std::ptr::write_bytes(ptr, 0xa5, len) };
        Aligned { ptr, layout }
    }
}

impl Drop for Aligned {
    fn drop(&mut self) {
        unsafe { std::alloc::dealloc(self.ptr, self.layout) };
    }
}
