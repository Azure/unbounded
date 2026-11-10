//! NUMA and SMT placement benchmark for the fused host-read decrypt.
//!
//! The receiver copies each RDMA-received page out of its registered buffer
//! into the ciphertext store and decrypts it into the client's memfd in one
//! chunked pass (`DerivedPageKey::open_copy_into` with
//! `FusedOutput::Scratch` and `page_alloc::stream::copy_nontemporal`). This
//! measures that pass with each buffer bound to a chosen NUMA node and the
//! threads pinned to a chosen node, so the cost of a remote source (the
//! registered buffer) or a remote destination (store and memfd) can be
//! measured on a multi-socket host. It also measures each non-temporal copy
//! kind, and the effect of a busy SMT sibling on every decrypting core.
//!
//! Build once and copy the binary to the benchmark host:
//!
//! ```text
//! cargo test -p racer-crypto --release --test fused_numa --no-run
//! ```
//!
//! Cargo prints the binary path (`target/release/deps/fused_numa-<hash>`).
//! Run it with `--ignored --nocapture --test-threads=1 fused_numa_bench`.
//! Environment: `RACER_FUSED_THREADS` (threads per run, default one per
//! physical core of the CPU node), `RACER_FUSED_PAGES` (16 MiB pages per
//! thread, default 4), `RACER_FUSED_ROUNDS` (timed passes, default 3),
//! `RACER_FUSED_KINDS=all` (every copy kind instead of SSE2 and the default).
#![allow(unsafe_code)]

use page_alloc::stream::{CopyKind, copy_nontemporal_as};
use racer_crypto::{DerivedPageKey, FusedOutput, TAG_LEN};
use std::{
    collections::BTreeSet,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

const PAGE: usize = 16 << 20;
const CHUNK: usize = racer_crypto::FUSED_CHUNK;
const AAD: &[u8] = b"fused numa bench";

/// Anonymous memory bound to one NUMA node before it is first touched.
struct NodeBuffer {
    base: *mut u8,
    len: usize,
}

// SAFETY: the mapping is owned and only accessed through `&`/`&mut self`.
unsafe impl Send for NodeBuffer {}

impl NodeBuffer {
    /// Map `len` bytes, bind them to `node` (when given), and touch them.
    fn new(len: usize, node: Option<usize>) -> Self {
        // SAFETY: anonymous private mapping with no address hint.
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        assert_ne!(base, libc::MAP_FAILED, "mmap");
        if let Some(node) = node {
            const MPOL_BIND: libc::c_ulong = 2;
            const MPOL_MF_STRICT: libc::c_ulong = 1;
            const MPOL_MF_MOVE: libc::c_ulong = 2;
            let mut mask = [0u64; 16];
            mask[node / 64] |= 1 << (node % 64);
            // SAFETY: binds this fresh mapping; `mask` covers 1024 nodes.
            let status = unsafe {
                libc::syscall(
                    libc::SYS_mbind,
                    base,
                    len,
                    MPOL_BIND,
                    mask.as_ptr(),
                    mask.len() * 64 + 1,
                    MPOL_MF_STRICT | MPOL_MF_MOVE,
                )
            };
            assert_eq!(
                status,
                0,
                "mbind to node {node}: {}",
                std::io::Error::last_os_error()
            );
        }
        let mut this = Self {
            base: base.cast(),
            len,
        };
        this.bytes_mut().fill(1);
        this
    }

    fn bytes(&self) -> &[u8] {
        // SAFETY: live mapping of `len` initialized bytes.
        unsafe { std::slice::from_raw_parts(self.base, self.len) }
    }

    fn bytes_mut(&mut self) -> &mut [u8] {
        // SAFETY: live exclusive mapping of `len` bytes.
        unsafe { std::slice::from_raw_parts_mut(self.base, self.len) }
    }

    /// Node of the first page, as the kernel reports it.
    fn node(&self) -> Option<usize> {
        const MPOL_F_NODE: libc::c_ulong = 1;
        const MPOL_F_ADDR: libc::c_ulong = 2;
        let mut node: libc::c_int = -1;
        // SAFETY: writes one int; no node mask is read.
        let status = unsafe {
            libc::syscall(
                libc::SYS_get_mempolicy,
                &raw mut node,
                std::ptr::null_mut::<libc::c_ulong>(),
                0usize,
                self.base,
                MPOL_F_NODE | MPOL_F_ADDR,
            )
        };
        (status == 0 && node >= 0).then_some(node as usize)
    }
}

impl Drop for NodeBuffer {
    fn drop(&mut self) {
        // SAFETY: unmaps exactly this owned mapping once.
        unsafe { libc::munmap(self.base.cast(), self.len) };
    }
}

fn parse_list(text: &str) -> Vec<usize> {
    let mut out = Vec::new();
    for part in text.trim().split(',').filter(|p| !p.is_empty()) {
        match part.split_once('-') {
            Some((a, b)) => out.extend(a.parse::<usize>().unwrap()..=b.parse().unwrap()),
            None => out.push(part.parse().unwrap()),
        }
    }
    out
}

fn read(path: &str) -> Option<String> {
    std::fs::read_to_string(path).ok()
}

/// One logical CPU and its SMT siblings.
#[derive(Clone)]
struct Cpu {
    cpu: usize,
    siblings: Vec<usize>,
}

/// Online NUMA nodes with their allowed CPUs, one entry per physical core
/// (its first allowed CPU) followed by that core's other siblings.
fn topology() -> Vec<(usize, Vec<Cpu>)> {
    let allowed: BTreeSet<usize> = {
        // SAFETY: zeroed cpu_set_t is valid; sched_getaffinity fills it.
        let mut set: libc::cpu_set_t = unsafe { std::mem::zeroed() };
        // SAFETY: valid pointer and size for the calling thread.
        let status =
            unsafe { libc::sched_getaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &mut set) };
        assert_eq!(status, 0);
        (0..libc::CPU_SETSIZE as usize)
            // SAFETY: index below CPU_SETSIZE.
            .filter(|cpu| unsafe { libc::CPU_ISSET(*cpu, &set) })
            .collect()
    };
    let nodes = read("/sys/devices/system/node/online").map_or_else(|| vec![0], |t| parse_list(&t));
    nodes
        .into_iter()
        .filter_map(|node| {
            let cpus = read(&format!("/sys/devices/system/node/node{node}/cpulist"))
                .map_or_else(|| allowed.iter().copied().collect(), |t| parse_list(&t));
            let mut seen = BTreeSet::new();
            let mut cores = Vec::new();
            for cpu in cpus.into_iter().filter(|c| allowed.contains(c)) {
                if seen.contains(&cpu) {
                    continue;
                }
                let siblings = read(&format!(
                    "/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list"
                ))
                .map_or_else(|| vec![cpu], |t| parse_list(&t));
                let siblings: Vec<_> = siblings
                    .into_iter()
                    .filter(|s| *s != cpu && allowed.contains(s))
                    .collect();
                seen.insert(cpu);
                seen.extend(siblings.iter().copied());
                cores.push(Cpu { cpu, siblings });
            }
            (!cores.is_empty()).then_some((node, cores))
        })
        .collect()
}

fn pin(cpu: usize) {
    // SAFETY: zeroed cpu_set_t is valid; one in-range CPU is set.
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_SET(cpu, &mut set);
        assert_eq!(
            libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set),
            0,
            "pin to CPU {cpu}"
        );
    }
}

fn env(name: &str) -> Option<usize> {
    std::env::var(name).ok().and_then(|v| v.parse().ok())
}

/// One measured configuration.
struct Run {
    cpu_node: usize,
    source_node: usize,
    destination_node: usize,
    kind: CopyKind,
    busy_sibling: bool,
    threads: usize,
}

fn fixture() -> (DerivedPageKey, Vec<u8>, Vec<u8>) {
    let mut key = [0u8; 32];
    let mut nonce = [0u8; 24];
    getrandom::getrandom(&mut key).unwrap();
    getrandom::getrandom(&mut nonce).unwrap();
    let plaintext: Vec<u8> = (0..PAGE).map(|i| (i * 13 + 5) as u8).collect();
    let mut sealed = vec![0; PAGE + TAG_LEN];
    racer_crypto::seal(&key, &nonce, AAD, &plaintext, &mut sealed).unwrap();
    (racer_crypto::xaes_derive(&key, &nonce), sealed, plaintext)
}

/// Time `rounds` fused passes over `pages` pages on each of `run.threads`
/// cores of the CPU node. Returns aggregate and per-thread GB/s of plaintext.
fn measure(
    run: &Run,
    cores: &[Cpu],
    key: &DerivedPageKey,
    sealed: &[u8],
    plaintext: &[u8],
    pages: usize,
    rounds: usize,
) -> (f64, f64, String) {
    let stop = Arc::new(AtomicBool::new(false));
    let barrier = std::sync::Barrier::new(run.threads);
    let mut placement = String::new();
    let elapsed = std::thread::scope(|scope| {
        let mut spinners = Vec::new();
        if run.busy_sibling {
            for core in &cores[..run.threads] {
                let Some(&sibling) = core.siblings.first() else {
                    continue;
                };
                let stop = stop.clone();
                spinners.push(scope.spawn(move || {
                    pin(sibling);
                    // Integer work with no PAUSE, like a busy I/O worker.
                    let mut x = 1u64;
                    while !stop.load(Ordering::Relaxed) {
                        for _ in 0..1024 {
                            x = std::hint::black_box(
                                x.wrapping_mul(6364136223846793005).wrapping_add(1),
                            );
                        }
                    }
                }));
            }
        }
        let workers: Vec<_> = cores[..run.threads]
            .iter()
            .map(|core| {
                let barrier = &barrier;
                scope.spawn(move || {
                    pin(core.cpu);
                    let mut sources: Vec<_> = (0..pages)
                        .map(|_| {
                            let mut b = NodeBuffer::new(PAGE + TAG_LEN, Some(run.source_node));
                            b.bytes_mut().copy_from_slice(sealed);
                            b
                        })
                        .collect();
                    let mut stores: Vec<_> = (0..pages)
                        .map(|_| NodeBuffer::new(PAGE + TAG_LEN, Some(run.destination_node)))
                        .collect();
                    let mut outs: Vec<_> = (0..pages)
                        .map(|_| NodeBuffer::new(PAGE, Some(run.destination_node)))
                        .collect();
                    let placed = format!(
                        "source on {:?}, store on {:?}, destination on {:?}",
                        sources[0].node(),
                        stores[0].node(),
                        outs[0].node()
                    );
                    let kind = run.kind;
                    let mut pass = |i: usize, sources: &mut Vec<NodeBuffer>| {
                        key.open_copy_into(
                            AAD,
                            sources[i].bytes(),
                            stores[i].bytes_mut(),
                            FusedOutput::Scratch {
                                out: outs[i].bytes_mut(),
                                start: 0,
                            },
                            CHUNK,
                            &mut |d, s| copy_nontemporal_as(kind, d, s),
                        )
                        .unwrap();
                    };
                    for i in 0..pages {
                        pass(i, &mut sources);
                    }
                    barrier.wait();
                    let start = std::time::Instant::now();
                    for _ in 0..rounds {
                        for i in 0..pages {
                            pass(i, &mut sources);
                        }
                    }
                    let elapsed = start.elapsed();
                    assert!(outs[0].bytes() == plaintext);
                    (elapsed, placed)
                })
            })
            .collect();
        let mut slowest = std::time::Duration::ZERO;
        for worker in workers {
            let (elapsed, placed) = worker.join().unwrap();
            slowest = slowest.max(elapsed);
            placement = placed;
        }
        stop.store(true, Ordering::Relaxed);
        for spinner in spinners {
            spinner.join().unwrap();
        }
        slowest
    });
    let bytes = (run.threads * pages * rounds * PAGE) as f64;
    let aggregate = bytes / elapsed.as_secs_f64() / 1e9;
    (aggregate, aggregate / run.threads as f64, placement)
}

/// See the module documentation for how to build and run this on a host.
#[test]
#[ignore = "benchmark"]
fn fused_numa_bench() {
    let nodes = topology();
    let pages = env("RACER_FUSED_PAGES").unwrap_or(4);
    let rounds = env("RACER_FUSED_ROUNDS").unwrap_or(3);
    let (key, sealed, plaintext) = fixture();
    let kinds: Vec<CopyKind> = if std::env::var("RACER_FUSED_KINDS").is_ok_and(|v| v == "all") {
        CopyKind::ALL
            .into_iter()
            .filter(|k| k.available())
            .collect()
    } else {
        let mut kinds = vec![CopyKind::Sse2, CopyKind::best()];
        kinds.dedup();
        kinds
    };
    println!(
        "nodes: {}; default copy kind {:?}; {pages} x 16 MiB pages per thread, {rounds} rounds",
        nodes
            .iter()
            .map(|(n, c)| format!("node{n}={} cores", c.len()))
            .collect::<Vec<_>>()
            .join(" "),
        CopyKind::best()
    );
    let (cpu_node, cores) = &nodes[0];
    let other = nodes.get(1).map(|(n, _)| *n);
    if other.is_none() {
        println!("one NUMA node: remote-memory rows are skipped");
    }
    let threads_max = env("RACER_FUSED_THREADS")
        .unwrap_or(cores.len())
        .min(cores.len());
    let has_siblings = cores.iter().any(|c| !c.siblings.is_empty());
    // (label, source node, destination node)
    let mut layouts = vec![("local", *cpu_node, *cpu_node)];
    if let Some(remote) = other {
        layouts.push(("remote source", remote, *cpu_node));
        layouts.push(("remote destination", *cpu_node, remote));
        layouts.push(("remote both", remote, remote));
    }
    println!(
        "{:<20} {:<16} {:<8} {:>7} {:>12} {:>14}",
        "layout", "copy", "sibling", "threads", "GB/s/thread", "GB/s aggregate"
    );
    for (label, source_node, destination_node) in layouts {
        for &kind in &kinds {
            for busy_sibling in [false, true] {
                if busy_sibling && (!has_siblings || label != "local" && kind != CopyKind::best()) {
                    continue;
                }
                for threads in [1, threads_max] {
                    let run = Run {
                        cpu_node: *cpu_node,
                        source_node,
                        destination_node,
                        kind,
                        busy_sibling,
                        threads,
                    };
                    let (aggregate, per_thread, placement) =
                        measure(&run, cores, &key, &sealed, &plaintext, pages, rounds);
                    println!(
                        "{label:<20} {:<16} {:<8} {threads:>7} {per_thread:>12.2} {aggregate:>14.1}   (CPUs on node {}; {placement})",
                        format!("{kind:?}"),
                        if busy_sibling { "busy" } else { "idle" },
                        run.cpu_node,
                    );
                    if threads_max == 1 {
                        break;
                    }
                }
            }
        }
    }
}
