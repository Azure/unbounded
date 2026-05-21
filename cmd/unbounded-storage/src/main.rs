// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::process::ExitCode;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use unbounded_storage::backing::{BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
use unbounded_storage::runtime::{PinnedRuntime, Threading, WorkerIdx, WorkerSpec};
use unbounded_storage::topology::{self, Host, Plan, Role, Worker};

const SHUTDOWN_POLL: Duration = Duration::from_millis(100);

/// Per-shard buffer-pool sizing. The page size is fixed at 2 MiB
/// to match the hugepage allocator; the byte budget per shard is
/// configurable via `--bytes-per-shard` (default 128 MiB == 64
/// hugepages). The allocator rounds the requested size up to a
/// whole number of pages, so non-multiples of 2 MiB are tolerated
/// but discouraged.
const DEFAULT_BYTES_PER_SHARD: usize = 128 * 1024 * 1024;

/// Process-wide shutdown flag. Set by the signal handler (which
/// is restricted to async-signal-safe operations) and polled by
/// the main thread plus every shard thread.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

fn main() -> ExitCode {
    let cli = match Cli::parse(std::env::args().skip(1)) {
        Ok(CliAction::Run(cli)) => cli,
        Ok(CliAction::Help) => {
            print_help();
            return ExitCode::SUCCESS;
        }
        Err(e) => {
            eprintln!("{e}");
            eprintln!();
            print_help();
            return ExitCode::FAILURE;
        }
    };

    let host = Host::discover();
    let plan = Plan::for_host(&host, &topology::PlanConfig::default());

    let counts = RoleCounts::from_plan(&plan);
    eprintln!(
        "topology plan: workers={} progress={} handlers={} nvme={} numa_pools={:?}",
        plan.workers.len(),
        counts.progress,
        counts.handlers,
        counts.nvme,
        plan.numa_pools,
    );

    // Only RDMA progress workers are spawned in this phase; handler
    // and NVMe placements are computed and logged for observability
    // but not yet wired up.
    let progress: Vec<Worker> = plan.rdma_progress().cloned().collect();
    if progress.is_empty() {
        eprintln!("topology plan produced no RDMA progress workers; exiting");
        return ExitCode::FAILURE;
    }

    let specs: Vec<WorkerSpec> = progress
        .iter()
        .map(|w| WorkerSpec::new(w.cpu, w.numa))
        .collect();
    let runtime = PinnedRuntime::new(specs);
    install_signal_handler();

    // Each shard thread reports either a successful bring-up or an
    // error on this channel. The main thread waits for every shard
    // to report so a partial bring-up produces a coherent error
    // path.
    let (ready_tx, ready_rx) = mpsc::channel::<ShardReady>();
    let bytes_per_shard = cli.bytes_per_shard / progress.len();
    let mut joins = Vec::with_capacity(progress.len());
    for (i, worker) in progress.iter().enumerate() {
        let widx = WorkerIdx(u16::try_from(i).expect("worker index fits in u16"));
        let dev_name = match worker.role {
            Role::RdmaProgress { hca } if hca != usize::MAX => Some(host.hcas[hca].dev_name.clone()),
            _ => None,
        };
        let worker = worker.clone();
        let runtime = runtime.clone();
        let tx = ready_tx.clone();
        let backing_kind = cli.backing_kind;
        joins.push(
            thread::Builder::new()
                .name(format!("ub-storage-shard-{i}"))
                .spawn(move || {
                    let rt = runtime.clone();
                    rt.run_worker(
                        widx,
                        Box::new(move || {
                            run_shard(widx, worker, dev_name, tx, backing_kind, bytes_per_shard);
                        }),
                    );
                })
                .expect("spawn shard thread"),
        );
    }
    drop(ready_tx);

    let mut up = 0usize;
    let mut errors: Vec<String> = Vec::new();
    for msg in ready_rx {
        match msg {
            ShardReady::Up => up += 1,
            ShardReady::Failed(err) => {
                eprintln!("shard failed: {err}");
                errors.push(err);
                SHUTDOWN.store(true, Ordering::Relaxed);
            }
        }
    }

    if errors.is_empty() && up > 0 {
        eprintln!("shards up: {up}");
    }

    // Wait for shutdown
    wait_for_shutdown();
    eprintln!("shutdown signaled; tearing down shards");

    // (reverse order so the last-built shard tears down first)
    for h in joins.into_iter().rev() {
        if let Err(e) = h.join() {
            eprintln!("shard thread panicked: {e:?}");
            errors.push(format!("panic: {e:?}"));
        }
    }

    if errors.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

/// Body of one shard thread. Runs on the pinned executor: allocates
/// a NUMA-local backing region, reports readiness, then idles until
/// shutdown. The transport that previously consumed this backing
/// has been removed; this is the bring-up shell that the next
/// transport will plug into.
fn run_shard(
    widx: WorkerIdx,
    worker: Worker,
    dev_name: Option<String>,
    tx: mpsc::Sender<ShardReady>,
    backing_kind: BackingKind,
    bytes_per_shard: usize,
) {
    let backing = match allocate(BackingRequest {
        kind: backing_kind,
        bytes: bytes_per_shard,
        numa: worker.numa,
    }) {
        Ok(b) => b,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: backing allocation failed: {e}",
                widx.0,
            )));
            return;
        }
    };

    println!(
        "shard up: worker={} dev={} numa={} cpu={} backing_bytes={}",
        widx.0,
        dev_name.as_deref().unwrap_or("tcp-fallback"),
        worker
            .numa
            .map(|n| n.to_string())
            .unwrap_or_else(|| "none".into()),
        worker.cpu,
        backing.page_size * backing.page_count,
    );

    let _ = tx.send(ShardReady::Up);

    wait_for_shutdown();

    drop(backing);
}

/// Status a shard thread reports once it has either come up or
/// failed during bring-up.
enum ShardReady {
    Up,
    Failed(String),
}

/// Per-role worker counts derived from a [`Plan`]; used for the
/// startup observability line.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
struct RoleCounts {
    progress: usize,
    handlers: usize,
    nvme: usize,
}

impl RoleCounts {
    fn from_plan(plan: &Plan) -> Self {
        let mut c = Self::default();
        for w in &plan.workers {
            match w.role {
                Role::RdmaProgress { .. } => c.progress += 1,
                Role::RdmaHandler { .. } => c.handlers += 1,
                Role::NvmeIoUring { .. } => c.nvme += 1,
            }
        }
        c
    }
}

/// Parsed command-line options for one run of the daemon.
#[derive(Copy, Clone, Debug)]
struct Cli {
    backing_kind: BackingKind,
    bytes_per_shard: usize,
}

enum CliAction {
    Run(Cli),
    Help,
}

impl Cli {
    fn parse<I: IntoIterator<Item = String>>(args: I) -> Result<CliAction, String> {
        let mut backing_kind = BackingKind::Hugepage2Mb;
        let mut bytes_per_shard = DEFAULT_BYTES_PER_SHARD;
        let mut it = args.into_iter();
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "-h" | "--help" => return Ok(CliAction::Help),
                "--no-hugepages" => backing_kind = BackingKind::Heap,
                s if s.starts_with("--bytes-per-shard=") => {
                    let v = &s["--bytes-per-shard=".len()..];
                    bytes_per_shard = parse_bytes(v)?;
                }
                "--bytes-per-shard" => {
                    let v = it
                        .next()
                        .ok_or_else(|| "--bytes-per-shard requires a value".to_string())?;
                    bytes_per_shard = parse_bytes(&v)?;
                }
                other => return Err(format!("unknown argument: {other}")),
            }
        }
        if bytes_per_shard == 0 {
            return Err("--bytes-per-shard must be > 0".into());
        }
        Ok(CliAction::Run(Cli {
            backing_kind,
            bytes_per_shard,
        }))
    }
}

/// Parse a byte count with an optional `K`/`M`/`G` suffix (powers
/// of 1024). Bare integers are bytes.
fn parse_bytes(s: &str) -> Result<usize, String> {
    let s = s.trim();
    if s.is_empty() {
        return Err("empty byte count".into());
    }
    let (num, mult) = match s.as_bytes().last().copied() {
        Some(b'K') | Some(b'k') => (&s[..s.len() - 1], 1024usize),
        Some(b'M') | Some(b'm') => (&s[..s.len() - 1], 1024 * 1024),
        Some(b'G') | Some(b'g') => (&s[..s.len() - 1], 1024 * 1024 * 1024),
        _ => (s, 1usize),
    };
    let n: usize = num
        .parse()
        .map_err(|e| format!("invalid byte count {s:?}: {e}"))?;
    n.checked_mul(mult)
        .ok_or_else(|| format!("byte count {s:?} overflows usize"))
}

fn print_help() {
    let default_mib = DEFAULT_BYTES_PER_SHARD / (1024 * 1024);
    let hp_mib = HUGEPAGE_2MB / (1024 * 1024);
    eprintln!("Usage: unbounded-storage [OPTIONS]");
    eprintln!();
    eprintln!("Options:");
    eprintln!("  --no-hugepages              Allocate the per-shard backing with the");
    eprintln!("                              global allocator instead of {hp_mib} MiB");
    eprintln!("                              hugepages. The default requires reserved");
    eprintln!("                              hugepages on the host; there is no");
    eprintln!("                              automatic fallback.");
    eprintln!("  --bytes-per-shard=<BYTES>   Per-shard buffer pool size. Accepts a");
    eprintln!("                              K/M/G suffix (powers of 1024). Rounded");
    eprintln!("                              up to a multiple of the {hp_mib} MiB page");
    eprintln!("                              size. Default: {default_mib} MiB.");
    eprintln!("  -h, --help                  Print this help and exit.");
}

fn wait_for_shutdown() {
    while !SHUTDOWN.load(Ordering::Acquire) {
        thread::sleep(SHUTDOWN_POLL);
    }
}

/// Install a SIGINT/SIGTERM handler that flips [`SHUTDOWN`]. The
/// handler is async-signal-safe (only a relaxed atomic store).
/// All threads observe the flag via their poll loops.
fn install_signal_handler() {
    unsafe extern "C" fn handler(_sig: libc::c_int) {
        // SAFETY: AtomicBool::store with Relaxed compiles to a
        // single machine store on every supported arch; it is
        // async-signal-safe. Readers synchronize via their own
        // Acquire loads.
        SHUTDOWN.store(true, Ordering::Relaxed);
    }
    // SAFETY: sigaction is invoked once at startup with a
    // zero-initialized sigaction whose handler does not touch
    // any non-async-signal-safe state.
    unsafe {
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = handler as *const () as usize;
        libc::sigemptyset(&mut sa.sa_mask);
        sa.sa_flags = 0;
        for sig in [libc::SIGINT, libc::SIGTERM] {
            if libc::sigaction(sig, &sa, std::ptr::null_mut()) != 0 {
                let e = std::io::Error::last_os_error();
                eprintln!("failed to install signal handler for sig={sig}: {e}");
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use unbounded_storage::topology::NumaPool;

    fn parse(args: &[&str]) -> Result<Cli, String> {
        match Cli::parse(args.iter().map(|s| s.to_string()))? {
            CliAction::Run(c) => Ok(c),
            CliAction::Help => Err("unexpected help".into()),
        }
    }

    #[test]
    fn defaults_to_hugepages() {
        let c = parse(&[]).unwrap();
        assert!(matches!(c.backing_kind, BackingKind::Hugepage2Mb));
        assert_eq!(c.bytes_per_shard, DEFAULT_BYTES_PER_SHARD);
    }

    #[test]
    fn no_hugepages_selects_heap() {
        let c = parse(&["--no-hugepages"]).unwrap();
        assert!(matches!(c.backing_kind, BackingKind::Heap));
    }

    #[test]
    fn bytes_per_shard_equals_form() {
        let c = parse(&["--bytes-per-shard=64M"]).unwrap();
        assert_eq!(c.bytes_per_shard, 64 * 1024 * 1024);
    }

    #[test]
    fn bytes_per_shard_space_form() {
        let c = parse(&["--bytes-per-shard", "2G"]).unwrap();
        assert_eq!(c.bytes_per_shard, 2 * 1024 * 1024 * 1024);
    }

    #[test]
    fn bytes_plain_integer_is_bytes() {
        let c = parse(&["--bytes-per-shard=4194304"]).unwrap();
        assert_eq!(c.bytes_per_shard, 4 * 1024 * 1024);
    }

    #[test]
    fn help_flag_returns_help_action() {
        let action = Cli::parse(["--help".to_string()].into_iter()).unwrap();
        assert!(matches!(action, CliAction::Help));
    }

    #[test]
    fn unknown_arg_is_rejected() {
        let err = parse(&["--nope"]).err().unwrap();
        assert!(err.contains("unknown argument"), "got: {err}");
    }

    #[test]
    fn zero_bytes_rejected() {
        let err = parse(&["--bytes-per-shard=0"]).err().unwrap();
        assert!(err.contains("must be > 0"), "got: {err}");
    }

    #[test]
    fn role_counts_aggregate_per_role() {
        // Synthetic plan: 2 progress, 3 handlers, 1 nvme. We do not
        // route this through `Plan::for_host`; we just want to
        // confirm `RoleCounts::from_plan` walks the worker list
        // correctly because main.rs feeds that into the startup
        // observability line.
        let plan = Plan {
            workers: vec![
                Worker { cpu: 1, numa: Some(0), role: Role::NvmeIoUring { nvme: 0 } },
                Worker { cpu: 2, numa: Some(0), role: Role::RdmaProgress { hca: 0 } },
                Worker { cpu: 3, numa: Some(0), role: Role::RdmaProgress { hca: 1 } },
                Worker { cpu: 4, numa: Some(0), role: Role::RdmaHandler { hca: 0 } },
                Worker { cpu: 5, numa: Some(0), role: Role::RdmaHandler { hca: 0 } },
                Worker { cpu: 6, numa: Some(0), role: Role::RdmaHandler { hca: 1 } },
            ],
            numa_pools: vec![NumaPool { numa: 0, workers: 6 }],
        };
        let c = RoleCounts::from_plan(&plan);
        assert_eq!(c, RoleCounts { progress: 2, handlers: 3, nvme: 1 });
    }
}
