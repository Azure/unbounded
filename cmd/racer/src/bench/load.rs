//! A block-device load generator.
//!
//! One io_uring per thread, `O_DIRECT`, registered file and registered buffers, `depth`
//! requests in flight and a new page issued the moment one completes.
//!
//! It lives here so the same generator drives both a raw device and a racer volume —
//! the only honest way to say how much of the cost is ours.

use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use io_uring::{IoUring, opcode, types};

/// The largest queue depth a thread may ask for.
const MAX_DEPTH: usize = 512;

/// What to run against one device.
#[derive(Clone)]
pub struct Job {
    /// Devices exporting the same volume; thread `i` takes path `i % len`, since every
    /// node of a group is an equal gateway to the same pages.
    pub paths: Vec<PathBuf>,
    /// Request size; also the alignment of every offset.
    pub bs: usize,
    /// Requests in flight per thread.
    pub depth: usize,
    /// One thread per cpu named here.
    pub cpus: Vec<usize>,
    /// Addressable bytes. Offsets are uniform over `span / bs` pages.
    pub span: u64,
    pub write: bool,
    /// Walk the pages in order, each thread taking its own stripe, and stop when the
    /// stripe ends. A one-shot fill of an immutable volume needs this: a page may be
    /// written once.
    pub sequential: bool,
    pub warmup: Duration,
    pub run: Duration,
}

impl Job {
    pub fn new(paths: &[PathBuf], bs: usize, span: u64) -> Job {
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
pub struct Report {
    pub ops: u64,
    pub errors: u64,
    /// The first errno seen, if any.
    pub errno: i32,
    pub secs: f64,
    pub bs: usize,
    pub hist: Hist,
}

impl Report {
    pub fn iops(&self) -> f64 {
        self.ops as f64 / self.secs
    }

    pub fn gib_s(&self) -> f64 {
        self.ops as f64 * self.bs as f64 / self.secs / (1u64 << 30) as f64
    }
}

/// Run `job`; the report is every thread's work together.
pub fn run(job: &Job) -> std::io::Result<Report> {
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

fn pin(cpu: usize) {
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        libc::CPU_SET(cpu, &mut set);
        libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set);
    }
}

// ---------------------------------------------------------------------------
// latency
// ---------------------------------------------------------------------------

/// A log-linear histogram: exact below 16 ns, then 16 buckets per octave, so under 7%
/// error everywhere and mergeable by addition.
pub struct Hist {
    buckets: Vec<u64>,
    count: u64,
    sum: u64,
    max: u64,
}

const BUCKETS: usize = 1024;

impl Hist {
    pub fn new() -> Hist {
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

    pub fn add(&mut self, ns: u64) {
        self.buckets[Self::index(ns)] += 1;
        self.count += 1;
        self.sum += ns;
        self.max = self.max.max(ns);
    }

    pub fn merge(&mut self, o: &Hist) {
        for (a, b) in self.buckets.iter_mut().zip(&o.buckets) {
            *a += b;
        }
        self.count += o.count;
        self.sum += o.sum;
        self.max = self.max.max(o.max);
    }

    pub fn mean(&self) -> f64 {
        if self.count == 0 {
            0.0
        } else {
            self.sum as f64 / self.count as f64
        }
    }

    pub fn pct(&self, p: f64) -> u64 {
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

    pub fn max(&self) -> u64 {
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
