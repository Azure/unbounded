//! Extent volume benchmark for `page-alloc`.
//!
//! Drives `Volume` (allocate, write, publish, read, commit, mount) the way the
//! racer dataplane does, on a production-like device plan, and reports where
//! time goes: device throughput and utilization, per-call latency, commit and
//! mount cost against index size, CPU cost of cached index operations, cold
//! and hot lookups through a bounded metadata page cache (`lookup`), and each
//! worker's CPU time and executor loop breakdown against wall time.
//! `--metadata-cache SIZE` sets the page cache budget of every volume.
//!
//! **`--devices` mode overwrites the named block devices. All data on them is
//! destroyed.** It requires `--destroy-data`.
//!
//! Build and run:
//!
//! ```text
//! cargo bench --locked --manifest-path cmd/racer-dataplane/Cargo.toml \
//!     -p page-alloc --bench volume --no-run
//! sudo ./cmd/racer-dataplane/target/release/deps/volume-<hash> \
//!     --devices /dev/nvme1n1,/dev/nvme2n1 --destroy-data --workers 2 --scenario write
//! ./cmd/racer-dataplane/target/release/deps/volume-<hash> \
//!     --file tmp/slab.dat --file-bytes 2G --scenario all --duration 2
//! ./cmd/racer-dataplane/target/release/deps/volume-<hash> \
//!     --file tmp/slab.dat --file-bytes 8G --scenario lookup \
//!     --lookup-entries 1000000 --metadata-cache 16M --duration 10
//! ```
//!
//! Run with `--help` for every option. Each result prints a table and one
//! `RESULT {json}` line.

mod args;
mod exec;
mod plan;
mod scenarios;
mod stats;

use std::fmt::Write as _;
use std::sync::{Arc, Barrier, mpsc};
use std::time::Instant;

use page_alloc::volume::SLOT_BYTES;

use args::Args;
use plan::Device;
use scenarios::{Job, Phase, Worker};
use stats::{DeviceSample, Report, json_f64, json_str};

fn main() {
    let args = match Args::parse() {
        Ok(args) => args,
        Err(message) => {
            if !message.is_empty() {
                eprintln!("error: {message}\n");
            }
            eprint!("{}", args::USAGE);
            std::process::exit(if message.is_empty() { 0 } else { 2 });
        }
    };
    if let Err(message) = run(&args) {
        eprintln!("error: {message}");
        std::process::exit(1);
    }
}

fn run(args: &Args) -> Result<(), String> {
    let devices: Vec<Device> = if let Some(path) = &args.file {
        vec![plan::open_file(path, args.file_bytes)?]
    } else {
        args.devices
            .iter()
            .map(|p| plan::open_device(p))
            .collect::<Result<_, _>>()?
    };
    let plans = plan::plan(&devices, args.workers, args.slots_per_worker)?;
    println!(
        "# backend: {}",
        devices
            .iter()
            .map(|d| format!(
                "{} ({:.1} GiB, align mem={} off={} len={})",
                d.path.display(),
                d.bytes as f64 / (1u64 << 30) as f64,
                d.alignment.memory(),
                d.alignment.offset(),
                d.alignment.length()
            ))
            .collect::<Vec<_>>()
            .join(", ")
    );
    for (index, plan) in plans.iter().enumerate() {
        println!(
            "# worker {index}: {} slots ({:.1} GiB): {}",
            plan.placements.len(),
            (plan.placements.len() as u64 * SLOT_BYTES) as f64 / (1u64 << 30) as f64,
            plan.runs
                .iter()
                .map(|(device, n)| format!("{device} x{n}"))
                .collect::<Vec<_>>()
                .join(", ")
        );
    }
    let mut jobs = Vec::new();
    for &scenario in &args.scenarios {
        if scenario.per_page_size() {
            for &page_bytes in &args.page_bytes {
                jobs.push(Job {
                    scenario,
                    page_bytes,
                });
            }
        } else {
            jobs.push(Job {
                scenario,
                page_bytes: 4096,
            });
        }
    }
    let barrier = Arc::new(Barrier::new(args.workers + 1));
    let (tx, rx) = mpsc::channel::<(usize, Result<Report, String>)>();
    let mut handles = Vec::new();
    for (index, plan) in plans.into_iter().enumerate() {
        let args = args.clone();
        let jobs = jobs.clone();
        let barrier = barrier.clone();
        let tx = tx.clone();
        let handle = std::thread::Builder::new()
            .name(format!("volume-bench-{index}"))
            .spawn(move || worker(index, &args, &plan, &jobs, &barrier, &tx))
            .map_err(|e| format!("spawn: {e}"))?;
        handles.push(handle);
    }
    drop(tx);
    let mut failed = false;
    for job in &jobs {
        barrier.wait();
        let before: Vec<DeviceSample> = devices.iter().map(|d| d.stat.sample()).collect();
        let t0 = Instant::now();
        barrier.wait();
        let wall = t0.elapsed();
        let after: Vec<DeviceSample> = devices.iter().map(|d| d.stat.sample()).collect();
        let mut merged = Report::default();
        let mut errors = Vec::new();
        for _ in 0..args.workers {
            match rx.recv() {
                Ok((_, Ok(report))) => merged.merge(&report),
                Ok((index, Err(error))) => errors.push(format!("worker {index}: {error}")),
                Err(_) => errors.push("worker exited".into()),
            }
        }
        failed |= !errors.is_empty();
        print_result(
            args,
            job,
            &merged,
            wall.as_secs_f64(),
            &devices,
            &before,
            &after,
            &errors,
        );
    }
    for handle in handles {
        let _ = handle.join();
    }
    drop(devices);
    if let Some(path) = &args.file
        && !args.keep_file
    {
        let _ = std::fs::remove_file(path);
    }
    if failed {
        Err("some jobs failed".into())
    } else {
        Ok(())
    }
}

fn pin(cpu: usize) {
    // SAFETY: cpu_set_t is plain data; sched_setaffinity reads it.
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_SET(cpu, &mut set);
        if libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set) != 0 {
            eprintln!(
                "warning: pin to CPU {cpu}: {}",
                std::io::Error::last_os_error()
            );
        }
    }
}

fn worker(
    index: usize,
    args: &Args,
    plan: &plan::WorkerPlan,
    jobs: &[Job],
    barrier: &Barrier,
    tx: &mpsc::Sender<(usize, Result<Report, String>)>,
) {
    if args.pin {
        pin(args.cpu_offset + index);
    }
    let worker = Worker::new(index, args, plan).map_err(|e| format!("reactor: {e}"));
    for &job in jobs {
        let mut phase = Phase::new(barrier);
        let result = match &worker {
            Ok(worker) => std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                worker.job(job, &mut phase).map_err(|e| e.to_string())
            }))
            .unwrap_or_else(|panic| {
                Err(panic
                    .downcast_ref::<String>()
                    .cloned()
                    .or_else(|| panic.downcast_ref::<&str>().map(|s| s.to_string()))
                    .unwrap_or_else(|| "panic".into()))
            }),
            Err(error) => Err(error.clone()),
        };
        phase.finish();
        let _ = tx.send((index, result));
    }
}

fn fmt_bytes(bytes: u64) -> String {
    match bytes {
        b if b >= 1 << 30 && b.is_multiple_of(1 << 30) => format!("{}GiB", b >> 30),
        b if b >= 1 << 20 && b.is_multiple_of(1 << 20) => format!("{}MiB", b >> 20),
        b if b >= 1 << 10 && b.is_multiple_of(1 << 10) => format!("{}KiB", b >> 10),
        b => format!("{b}B"),
    }
}

/// Derived rates from the merged counters.
fn derive(report: &Report, wall: f64, workers: usize) -> Vec<(String, f64)> {
    let mut out = Vec::new();
    let gib = (1u64 << 30) as f64;
    let get = |k: &str| report.get(k) as f64;
    if report.counters.contains_key("bytes") {
        out.push(("gib_per_s".into(), get("bytes") / gib / wall));
        out.push(("ops_per_s".into(), get("ops") / wall));
    }
    if report.counters.contains_key("read_bytes") {
        out.push(("read_gib_per_s".into(), get("read_bytes") / gib / wall));
        out.push(("reads_per_s".into(), get("reads") / wall));
    }
    let worker_wall = get("wall_ns").max(1.0);
    let cpu = get("cpu_user_ns") + get("cpu_system_ns");
    out.push(("cpu_util".into(), cpu / worker_wall));
    out.push(("cpu_user_util".into(), get("cpu_user_ns") / worker_wall));
    out.push(("cpu_system_util".into(), get("cpu_system_ns") / worker_wall));
    if report.counters.contains_key("exec_turns") {
        out.push(("exec_task_frac".into(), get("exec_task_ns") / worker_wall));
        out.push((
            "exec_reactor_frac".into(),
            get("exec_reactor_ns") / worker_wall,
        ));
        out.push(("exec_wait_frac".into(), get("exec_wait_ns") / worker_wall));
    }
    if report.get("commit_attempts") > 0 {
        let calls = get("commit_calls") + get("lazy_commits");
        out.push((
            "commit_calls_per_attempt".into(),
            calls / get("commit_attempts"),
        ));
        out.push(("commits_per_s".into(), get("commit_attempts") / wall));
    }
    for (key, &value) in &report.counters {
        if let Some(name) = key.strip_suffix("_ns")
            && let Some(&ops) = report.counters.get(&format!("{name}_ops"))
            && ops > 0
        {
            out.push((format!("{name}_ns_per_op"), value as f64 / ops as f64));
        }
    }
    out.push(("workers".into(), workers as f64));
    out
}

#[allow(clippy::too_many_arguments)]
fn print_result(
    args: &Args,
    job: &Job,
    report: &Report,
    wall: f64,
    devices: &[Device],
    before: &[DeviceSample],
    after: &[DeviceSample],
    errors: &[String],
) {
    let derived = derive(report, wall, args.workers);
    let page = if job.scenario.per_page_size() {
        format!(" page={}", fmt_bytes(job.page_bytes))
    } else {
        String::new()
    };
    let mut text = String::new();
    let _ = writeln!(
        text,
        "\n== {}{page} workers={} qd={} wall={wall:.2}s",
        job.scenario.name(),
        args.workers,
        args.qd
    );
    for error in errors {
        let _ = writeln!(text, "  ERROR {error}");
    }
    for note in &report.notes {
        let _ = writeln!(text, "  note: {note}");
    }
    let mut line = String::from(" ");
    for (name, value) in &derived {
        let piece = if name.ends_with("_util") || name.ends_with("_frac") {
            format!(" {name}={:.1}%", value * 100.0)
        } else {
            format!(" {name}={value:.3}")
        };
        if line.len() + piece.len() > 110 {
            let _ = writeln!(text, "{line}");
            line = String::from(" ");
        }
        line.push_str(&piece);
    }
    let _ = writeln!(text, "{line}");
    if !report.hists.is_empty() {
        let _ = writeln!(
            text,
            "  {:<34} {:>9} {:>11} {:>11} {:>11} {:>11} {:>11}",
            "histogram (us, or raw if not _ns)", "count", "mean", "p50", "p99", "p99.9", "max"
        );
        for (name, hist) in &report.hists {
            let scale = if name.ends_with("_ns") { 1000.0 } else { 1.0 };
            let _ = writeln!(
                text,
                "  {:<34} {:>9} {:>11.1} {:>11.1} {:>11.1} {:>11.1} {:>11.1}",
                name,
                hist.count(),
                hist.mean() / scale,
                hist.quantile(0.5) as f64 / scale,
                hist.quantile(0.99) as f64 / scale,
                hist.quantile(0.999) as f64 / scale,
                hist.max() as f64 / scale
            );
        }
    }
    let mut line = String::from("  counters:");
    for (name, value) in &report.counters {
        let piece = format!(" {name}={value}");
        if line.len() + piece.len() > 110 {
            let _ = writeln!(text, "{line}");
            line = String::from("           ");
        }
        line.push_str(&piece);
    }
    let _ = writeln!(text, "{line}");
    let mut device_json = Vec::new();
    for ((device, b), a) in devices.iter().zip(before).zip(after) {
        let gib = (1u64 << 30) as f64;
        let read = (a.read_sectors - b.read_sectors) * 512;
        let written = (a.write_sectors - b.write_sectors) * 512;
        let util = (a.io_ticks_ms - b.io_ticks_ms) as f64 / 1000.0 / wall;
        let flushes = a.flushes - b.flushes;
        let flush_ms = a.flush_ticks_ms - b.flush_ticks_ms;
        let reads = a.reads - b.reads;
        let writes = a.writes - b.writes;
        let _ = writeln!(
            text,
            "  device {:<12} read {:>8.3} GiB/s ({reads} ios) write {:>8.3} GiB/s ({writes} ios) util {:>5.1}% flushes {flushes} ({flush_ms} ms)",
            device.stat.name,
            read as f64 / gib / wall,
            written as f64 / gib / wall,
            util * 100.0
        );
        device_json.push(format!(
            "{{\"name\":{},\"read_bytes\":{read},\"write_bytes\":{written},\"reads\":{reads},\"writes\":{writes},\"flushes\":{flushes},\"flush_ms\":{flush_ms},\"util\":{}}}",
            json_str(&device.stat.name),
            json_f64(util)
        ));
    }
    print!("{text}");
    let mut json = String::new();
    let _ = write!(
        json,
        "{{\"scenario\":{},\"page_bytes\":{},\"workers\":{},\"qd\":{},\"duration_s\":{},\"wall_s\":{},\"backend\":{}",
        json_str(job.scenario.name()),
        job.page_bytes,
        args.workers,
        args.qd,
        json_f64(args.duration.as_secs_f64()),
        json_f64(wall),
        json_str(if args.file.is_some() {
            "file"
        } else {
            "devices"
        })
    );
    json.push_str(",\"derived\":{");
    json.push_str(
        &derived
            .iter()
            .map(|(k, v)| format!("{}:{}", json_str(k), json_f64(*v)))
            .collect::<Vec<_>>()
            .join(","),
    );
    json.push_str("},\"counters\":{");
    json.push_str(
        &report
            .counters
            .iter()
            .map(|(k, v)| format!("{}:{v}", json_str(k)))
            .collect::<Vec<_>>()
            .join(","),
    );
    json.push_str("},\"hist\":{");
    json.push_str(
        &report
            .hists
            .iter()
            .map(|(k, h)| {
                format!(
                    "{}:{{\"count\":{},\"mean\":{},\"p50\":{},\"p90\":{},\"p99\":{},\"p999\":{},\"max\":{},\"sum\":{}}}",
                    json_str(k),
                    h.count(),
                    json_f64(h.mean()),
                    h.quantile(0.5),
                    h.quantile(0.9),
                    h.quantile(0.99),
                    h.quantile(0.999),
                    h.max(),
                    h.sum()
                )
            })
            .collect::<Vec<_>>()
            .join(","),
    );
    json.push_str("},\"devices\":[");
    json.push_str(&device_json.join(","));
    json.push_str("],\"notes\":[");
    json.push_str(
        &report
            .notes
            .iter()
            .chain(errors)
            .map(|n| json_str(n))
            .collect::<Vec<_>>()
            .join(","),
    );
    json.push_str("]}");
    println!("RESULT {json}");
}
