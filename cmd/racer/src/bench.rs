//! `racer-bench` — what the dataplane costs, from the CPU up to a client's block device.
//!
//! ```text
//! racer-bench cpu    # per-core costs on the request path: CRC, copy (the default)
//! racer-bench e2e    # a real cluster through real ublk devices (needs root)
//! ```
//!
//! `cpu` says what a core can do at all; `e2e` says what a client gets. The ladder `e2e`
//! prints — raw ram disk, then one node alone, then a three node group — is what makes
//! the difference attributable: the raw device row is the load generator's own ceiling,
//! and every row below it is read against that.

#[path = "bench/cluster.rs"]
mod cluster;
#[path = "bench/load.rs"]
mod load;

use std::time::{Duration, Instant};

use cluster::{BIG, Cluster, LWW, Plan};
use load::Job;
use racer::layout::{crc32c, page_crc};

const SMALL: usize = 4096;
const HUGE: usize = 4 << 20;

fn main() {
    let mut args = std::env::args().skip(1);
    match args.next().as_deref() {
        None | Some("cpu") => cpu(),
        Some("e2e") => e2e(Args::parse(args)),
        Some(other) => {
            eprintln!("racer-bench: unknown command {other}");
            eprintln!(
                "usage: racer-bench [cpu | e2e [--cores N] [--jobs N] [--depth N] [--seconds S]]"
            );
            std::process::exit(2);
        }
    }
}

// ---------------------------------------------------------------------------
// cpu
// ---------------------------------------------------------------------------

/// One core, no device: the 4 KiB page CRC, the 4 MiB CRC, and the copy the small write
/// path pays before its CRC.
fn cpu() {
    let mut small = vec![0u8; SMALL];
    let mut huge = vec![0u8; HUGE];
    fill(&mut small);
    fill(&mut huge);

    println!("racer-bench cpu  (single core, warm cache except where noted)\n");

    let t = time(200_000, || page_crc(0x1234_5678, 42, &small));
    row("page_crc 4 KiB", t, SMALL);

    let t = time(200, || crc32c(&huge));
    row("crc32c 4 MiB", t, HUGE);

    // The 4 KiB write path stages guest bytes in our own registered memory before the
    // CRC runs, so it pays copy + CRC. This is the memcpy half; the `pread` on the ublk
    // char device that does it needs a live device and is measured by `e2e`.
    let mut dst = vec![0u8; SMALL];
    let t = time(200_000, || dst.copy_from_slice(&small));
    row("memcpy 4 KiB", t, SMALL);

    // The shape of the small write path: the CRC reads what the copy just left in L1,
    // so the pair costs less than the sum.
    let t = time(200_000, || {
        dst.copy_from_slice(&small);
        page_crc(0x1234_5678, 42, &dst)
    });
    row("memcpy + page_crc 4 KiB", t, SMALL);

    println!();
    let ns = time(200_000, || page_crc(0x1234_5678, 42, &small));
    println!(
        "  a core spending all its time on 4 KiB CRCs would sustain {:.1} M pages/s",
        1000.0 / ns
    );
    println!(
        "  at 1 M IOPS/core the CRC is {:.1}% of the core",
        ns / 10.0
    );
}

fn fill(b: &mut [u8]) {
    let mut x = 0x9e37_79b9u32;
    for (i, v) in b.iter_mut().enumerate() {
        x = x.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
        *v = (x >> 24) as u8 ^ i as u8;
    }
}

/// Nanoseconds per iteration, after a warmup of the same shape.
fn time<T>(iters: usize, mut f: impl FnMut() -> T) -> f64 {
    for _ in 0..iters / 10 {
        std::hint::black_box(f());
    }
    let start = Instant::now();
    for _ in 0..iters {
        std::hint::black_box(f());
    }
    start.elapsed().as_secs_f64() * 1e9 / iters as f64
}

fn row(name: &str, ns: f64, bytes: usize) {
    println!(
        "  {name:<26} {ns:>9.1} ns   {:>7.1} GB/s",
        bytes as f64 / ns
    );
}

// ---------------------------------------------------------------------------
// end to end
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

fn e2e(a: Args) {
    let root = unsafe { libc::geteuid() } == 0;
    assert!(root, "racer-bench e2e needs root for ublk and brd");
    let run = Duration::from_secs(a.seconds);

    println!(
        "racer-bench e2e  ({} cores/node, {} client threads, depth {}, {} s per row)\n",
        a.cores, a.jobs, a.depth, a.seconds
    );
    header();

    for &nodes in &a.ladder {
        let plan = Plan {
            nodes,
            cores: a.cores,
            groups: a.groups,
            small_pages: a.pages,
            ..Plan::default()
        };
        // Provision the ram disks before anything opens one: `brd` cannot be reloaded
        // while a disk is in use, and the baseline row below opens one.
        let devices = cluster::ram_disks(nodes as usize).expect("ram disks");
        // Clients take the logical CPUs above the node processes' cores, spread over the
        // whole remaining range: blk-mq picks a device queue from the submitting CPU, so
        // bunched clients all land on one worker and the node looks single-threaded.
        let online = unsafe { libc::sysconf(libc::_SC_NPROCESSORS_ONLN) } as usize;
        let first = plan.client_first_core() * 2;
        let cpus: Vec<usize> = (0..a.jobs)
            .map(|i| first + i * (online - first) / a.jobs)
            .collect();

        if nodes == 1 {
            // A ram disk allocates a page on first write, so reading sectors nobody
            // wrote measures a zero fill rather than a copy. Touch the whole span first.
            let span = plan.small_bytes().max(plan.huge_bytes());
            let mut j = Job::new(&devices[..1], HUGE, span);
            j.depth = 4;
            j.cpus = vec![cpus[0]];
            j.write = true;
            j.sequential = true;
            j.warmup = Duration::ZERO;
            j.run = Duration::from_secs(600);
            load::run(&j).expect("baseline fill");

            // The raw device the nodes are about to use, same generator, same block
            // layer. Every row below is read against this one.
            let mut j = Job::new(&devices[..1], 4096, plan.small_bytes());
            j.depth = a.depth;
            j.cpus = cpus.clone();
            j.run = run;
            report(&a, "ram disk", "4k randread", &plan, &j);
            j.write = true;
            report(&a, "ram disk", "4k randwrite", &plan, &j);
            let mut j = Job::new(&devices[..1], HUGE, plan.huge_bytes());
            j.depth = 4.min(a.depth);
            j.cpus = cpus.clone();
            j.run = run;
            report(&a, "ram disk", "4m randread", &plan, &j);
            let mut j = Job::new(&devices[..1], 4096, plan.small_bytes());
            j.depth = 1;
            j.cpus = vec![cpus[0]];
            j.run = run;
            report(&a, "ram disk", "4k read  qd1", &plan, &j);
        }

        let c = Cluster::start(&plan).expect("cluster");
        let tag: &str = if nodes == 1 { "1 node" } else { "3 nodes" };
        for n in &c.nodes {
            println!("  # node {} metrics {}", n.id, n.metrics);
        }
        // Every node exports the volume and is a member of every group, so load is
        // spread over all of them; driving a single gateway would measure that node.
        let small: Vec<_> = c
            .nodes
            .iter()
            .map(|n| n.volume(LWW).to_path_buf())
            .collect();
        let big: Vec<_> = c
            .nodes
            .iter()
            .map(|n| n.volume(BIG).to_path_buf())
            .collect();

        // Fill the 4 KiB volume once, so reads find pages and writes overwrite rather
        // than allocate: a hole and a filled page are different amounts of work.
        let mut fill = Job::new(&small, 4096, plan.small_bytes());
        fill.depth = a.depth;
        fill.cpus = cpus.clone();
        fill.write = true;
        fill.sequential = true;
        fill.warmup = Duration::ZERO;
        fill.run = Duration::from_secs(600);
        let f = load::run(&fill).expect("fill");
        assert_eq!(
            f.errors, 0,
            "filling the 4 KiB volume failed with errno {}",
            f.errno
        );
        emit(tag, "4k fill (seq)", &plan, &f);

        let mut j = Job::new(&small, 4096, plan.small_bytes());
        j.depth = a.depth;
        j.cpus = cpus.clone();
        j.run = run;
        report(&a, tag, "4k randread", &plan, &j);
        j.write = true;
        report(&a, tag, "4k randwrite", &plan, &j);

        // An immutable 4 MiB page may be filled once per epoch, so its write row is a
        // single pass over the volume rather than a timed window.
        let mut j = Job::new(&big, HUGE, plan.huge_bytes());
        j.depth = 4.min(a.depth);
        j.cpus = cpus.clone();
        j.write = true;
        j.sequential = true;
        j.warmup = Duration::ZERO;
        j.run = Duration::from_secs(600);
        report(&a, tag, "4m write (fill)", &plan, &j);
        j.write = false;
        j.sequential = false;
        j.warmup = Duration::from_millis(500);
        j.run = run;
        report(&a, tag, "4m randread", &plan, &j);

        // One request in flight per thread: latency without the queueing every row
        // above is full of.
        let mut j = Job::new(&small, 4096, plan.small_bytes());
        j.depth = 1;
        j.cpus = vec![cpus[0]];
        j.run = run;
        report(&a, tag, "4k read  qd1", &plan, &j);
        j.write = true;
        report(&a, tag, "4k write qd1", &plan, &j);
    }
}

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
    let r = load::run(job).expect("load generator");
    assert_eq!(
        r.errors, 0,
        "{target} {name}: {} requests failed, errno {}",
        r.errors, r.errno
    );
    emit(target, name, plan, &r);
}

fn emit(target: &str, name: &str, plan: &Plan, r: &load::Report) {
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
