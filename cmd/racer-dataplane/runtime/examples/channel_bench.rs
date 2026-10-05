//! Two-thread SPSC benchmark. Run with --help for configuration.
//! Throughput and sampled send-attempt-to-receive latency use separate passes.
use std::{
    hint::{black_box, spin_loop},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};
use uring_runtime::{Error, channel};

type Result<T> = std::result::Result<T, String>;
const HELP: &str = "channel_bench [--transfers N] [--capacities 1,3,256] [--repeats N]
  [--sample-every N] [--timeout-ms N] [--cpus PRODUCER,CONSUMER]
Defaults: transfers=1000000 capacities=1,3,256 repeats=3 sample-every=1024
  timeout-ms=10000 cpus=unpinned. Each capacity gets a 10000-transfer warmup.
Bounds: transfers<=1000000000 capacities<=1048576 (at most 32 entries)
  repeats<=100 timeout-ms<=60000 sampled messages/pass<=1000000.
Two dedicated threads busy-spin, yielding on retry at 1024-attempt checkpoints.
Throughput has no per-message timestamps; latency is a separate sparse pass.
Latency includes sender backpressure from FIRST send attempt to receive.
CPU IDs must be distinct and allowed by Linux sched_getaffinity; affinity is
validated again inside each worker. Use a release build and an external timeout.";

#[derive(Clone, Debug)]
struct Config {
    transfers: usize,
    capacities: Vec<usize>,
    repeats: usize,
    sample_every: usize,
    timeout: Duration,
    cpus: Option<[usize; 2]>,
    #[cfg(test)]
    hooks: std::sync::Arc<tests::Hooks>,
}

fn number(value: &str, name: &str, max: usize) -> Result<usize> {
    let n = value
        .parse::<usize>()
        .map_err(|_| format!("invalid {name}: {value}"))?;
    if n == 0 || n > max {
        return Err(format!("{name} must be in 1..={max}"));
    }
    Ok(n)
}

fn parse(args: impl IntoIterator<Item = String>) -> Result<Option<Config>> {
    let mut config = Config {
        transfers: 1_000_000,
        capacities: vec![1, 3, 256],
        repeats: 3,
        sample_every: 1024,
        timeout: Duration::from_secs(10),
        cpus: None,
        #[cfg(test)]
        hooks: std::sync::Arc::default(),
    };
    let mut args = args.into_iter();
    while let Some(flag) = args.next() {
        if flag == "--help" || flag == "-h" {
            return Ok(None);
        }
        let value = args
            .next()
            .ok_or_else(|| format!("missing value for {flag}"))?;
        match flag.as_str() {
            "--transfers" => config.transfers = number(&value, "transfers", 1_000_000_000)?,
            "--capacities" => {
                config.capacities = value
                    .split(',')
                    .map(|v| number(v, "capacity", 1_048_576))
                    .collect::<Result<_>>()?;
                if config.capacities.len() > 32 {
                    return Err("at most 32 capacities are allowed".into());
                }
            }
            "--repeats" => config.repeats = number(&value, "repeats", 100)?,
            "--sample-every" => {
                config.sample_every = number(&value, "sample-every", 1_000_000_000)?
            }
            "--timeout-ms" => {
                config.timeout = Duration::from_millis(number(&value, "timeout-ms", 60_000)? as u64)
            }
            "--cpus" => {
                let cpus = value
                    .split(',')
                    .map(|v| v.parse::<usize>().map_err(|_| format!("invalid CPU: {v}")))
                    .collect::<Result<Vec<_>>>()?;
                if cpus.len() != 2 || cpus[0] == cpus[1] {
                    return Err("--cpus requires two distinct CPU IDs".into());
                }
                config.cpus = Some([cpus[0], cpus[1]]);
            }
            _ => return Err(format!("unknown option: {flag}")),
        }
    }
    if config.transfers.div_ceil(config.sample_every) > 1_000_000 {
        return Err("at most 1000000 latency samples per pass are allowed".into());
    }
    Ok(Some(config))
}

#[cfg(target_os = "linux")]
fn allowed_cpus() -> Result<Vec<usize>> {
    // SAFETY: zero is a valid empty cpu_set_t; syscall writes the entire supplied
    // fixed-size mask. CPU_ISSET only receives indices within CPU_SETSIZE.
    unsafe {
        let mut mask: libc::cpu_set_t = std::mem::zeroed();
        if libc::sched_getaffinity(0, std::mem::size_of_val(&mask), &mut mask) != 0 {
            return Err(format!(
                "sched_getaffinity: {}",
                std::io::Error::last_os_error()
            ));
        }
        Ok((0..libc::CPU_SETSIZE as usize)
            .filter(|cpu| libc::CPU_ISSET(*cpu, &mask))
            .collect())
    }
}

#[cfg(not(target_os = "linux"))]
fn allowed_cpus() -> Result<Vec<usize>> {
    Err("CPU affinity requires Linux".into())
}

fn pin(cpu: Option<usize>) -> Result<()> {
    let Some(cpu) = cpu else { return Ok(()) };
    if !allowed_cpus()?.contains(&cpu) {
        return Err(format!(
            "CPU {cpu} is outside the calling thread's allowed mask"
        ));
    }
    #[cfg(target_os = "linux")]
    {
        // SAFETY: CPU was validated against the bounded kernel mask above; pid 0
        // changes only this worker, never the coordinator or other workers.
        unsafe {
            let mut mask: libc::cpu_set_t = std::mem::zeroed();
            libc::CPU_SET(cpu, &mut mask);
            if libc::sched_setaffinity(0, std::mem::size_of_val(&mask), &mask) != 0 {
                return Err(format!(
                    "sched_setaffinity: {}",
                    std::io::Error::last_os_error()
                ));
            }
        }
        if allowed_cpus()? != [cpu] {
            return Err(format!("CPU {cpu} affinity did not take effect"));
        }
    }
    Ok(())
}

struct Message {
    sequence: usize,
    sent: Option<Instant>,
}

// Check wall time sparsely on both success and retry paths. No shared cancellation
// atomic on the hot path: endpoint destruction reports peer failure instead.
struct Guard {
    deadline: Instant,
    attempts: usize,
}

impl Guard {
    fn check(&mut self) -> Result<()> {
        self.attempts = self.attempts.wrapping_add(1);
        if self.attempts & 1023 == 0 && Instant::now() >= self.deadline {
            return Err("pass deadline exceeded".into());
        }
        Ok(())
    }

    fn retry(&self) {
        if self.attempts & 1023 == 0 {
            thread::yield_now();
        } else {
            spin_loop();
        }
    }
}

fn start_worker(
    cpu: Option<usize>,
    ready: mpsc::Sender<Result<()>>,
    start: mpsc::Receiver<Instant>,
    timeout: Duration,
    #[cfg(test)] hooks: &tests::Hooks,
    #[cfg(test)] worker: usize,
) -> Result<Instant> {
    let result = pin(cpu);
    #[cfg(test)]
    if result.is_ok() {
        hooks.setup_ok[worker].fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
    let _ = ready.send(result.clone());
    result?;
    start
        .recv_timeout(timeout)
        .map_err(|e| format!("start handshake: {e}"))
}

struct Measurement {
    elapsed: Duration,
    samples: Vec<u64>,
}

fn pass<const LATENCY: bool>(
    config: &Config,
    capacity: usize,
    transfers: usize,
) -> Result<Measurement> {
    let (sender, mut receiver) =
        channel::bounded::<Message>(capacity).map_err(|e| e.to_string())?;
    thread::scope(|scope| {
        let (ready_tx, ready_rx) = mpsc::channel();
        let (producer_go, producer_start) = mpsc::channel();
        let (consumer_go, consumer_start) = mpsc::channel();
        let producer_ready = ready_tx.clone();
        let producer = thread::Builder::new()
            .name("channel-producer".into())
            .spawn_scoped(scope, move || -> Result<()> {
                #[cfg(test)]
                let _exit = tests::WorkerExit(&config.hooks, 0);
                let start = start_worker(
                    config.cpus.map(|c| c[0]),
                    producer_ready,
                    producer_start,
                    config.timeout,
                    #[cfg(test)]
                    &config.hooks,
                    #[cfg(test)]
                    0,
                )?;
                let mut guard = Guard {
                    deadline: start + config.timeout,
                    attempts: 0,
                };
                for sequence in 0..transfers {
                    #[cfg(test)]
                    config.hooks.inject(0, sequence, &mut guard)?;
                    let mut message = Message {
                        sequence,
                        sent: if LATENCY && sequence % config.sample_every == 0 {
                            Some(Instant::now())
                        } else {
                            None
                        },
                    };
                    loop {
                        guard.check()?;
                        match sender.try_send(message) {
                            Ok(()) => break,
                            Err(failed) if failed.error == Error::Overloaded => {
                                message = failed.command;
                                guard.retry();
                            }
                            Err(failed) => {
                                return Err(format!("send at {sequence}: {}", failed.error));
                            }
                        }
                    }
                }
                #[cfg(test)]
                if let Some(closed) = &config.hooks.notify_closed {
                    drop(sender);
                    closed.send(()).map_err(|e| e.to_string())?;
                }
                Ok(())
            })
            .map_err(|e| format!("spawn producer: {e}"))?;
        let consumer = thread::Builder::new()
            .name("channel-consumer".into())
            .spawn_scoped(scope, move || -> Result<Measurement> {
                #[cfg(test)]
                let _exit = tests::WorkerExit(&config.hooks, 1);
                // Allocate samples before signaling readiness, outside measured time.
                let mut samples = Vec::with_capacity(if LATENCY {
                    transfers.div_ceil(config.sample_every)
                } else {
                    0
                });
                let start = start_worker(
                    config.cpus.map(|c| c[1]),
                    ready_tx,
                    consumer_start,
                    config.timeout,
                    #[cfg(test)]
                    &config.hooks,
                    #[cfg(test)]
                    1,
                )?;
                let mut guard = Guard {
                    deadline: start + config.timeout,
                    attempts: 0,
                };
                for expected in 0..transfers {
                    #[cfg(test)]
                    config.hooks.inject(1, expected, &mut guard)?;
                    let message = loop {
                        guard.check()?;
                        let observed = receiver.receive().map_err(|e| format!("receive: {e}"))?;
                        #[cfg(test)]
                        config.hooks.after_receive(observed.is_none())?;
                        match observed {
                            Some(message) => break message,
                            None if receiver.is_closed() => {
                                // The first empty observation can race the final send.
                                break receiver
                                    .receive()
                                    .map_err(|e| e.to_string())?
                                    .ok_or_else(|| format!("sender closed early at {expected}"))?;
                            }
                            None => guard.retry(),
                        }
                    };
                    if LATENCY && let Some(sent) = message.sent {
                        samples.push(sent.elapsed().as_nanos() as u64);
                    }
                    if black_box(message.sequence) != expected {
                        return Err(format!("FIFO mismatch at {expected}: {}", message.sequence));
                    }
                }
                Ok(Measurement {
                    elapsed: start.elapsed(),
                    samples,
                })
            })
            .map_err(|e| format!("spawn consumer: {e}"))?;
        // Failed setup drops go senders, releasing the other worker. No barrier
        // can strand a peer when pinning, allocation, or thread startup fails.
        for _ in 0..2 {
            ready_rx
                .recv_timeout(config.timeout)
                .map_err(|e| format!("ready handshake: {e}"))??;
        }
        let start = Instant::now();
        producer_go.send(start).map_err(|e| e.to_string())?;
        consumer_go.send(start).map_err(|e| e.to_string())?;
        // Join both even if either failed or panicked. Dropping an endpoint makes
        // the peer terminate; the sparse deadline also bounds unexpected stalls.
        let sent = producer.join().map_err(|_| "producer panicked".to_string());
        let received = consumer.join().map_err(|_| "consumer panicked".to_string());
        sent??;
        let measurement = received??;
        if measurement.elapsed > config.timeout {
            return Err("pass exceeded deadline at completion".into());
        }
        Ok(measurement)
    })
}

fn percentile(sorted: &[u64], percent: usize) -> u64 {
    sorted[(sorted.len() * percent).div_ceil(100).saturating_sub(1)]
}

fn run(config: &Config) -> Result<()> {
    if let Some(cpus) = config.cpus {
        let allowed = allowed_cpus()?;
        for cpu in cpus {
            if !allowed.contains(&cpu) {
                return Err(format!(
                    "requested CPU {cpu} not in allowed mask {allowed:?}"
                ));
            }
        }
    }
    println!(
        "config={config:?} payload_bytes={} warmup_transfers={} deadline_check_attempts=1024 mode=busy-spin debug_assertions={}",
        std::mem::size_of::<Message>(),
        config.transfers.min(10_000),
        cfg!(debug_assertions)
    );
    println!("allowed_cpus={:?}", allowed_cpus()?);
    for &capacity in &config.capacities {
        pass::<false>(config, capacity, config.transfers.min(10_000))?;
        for repeat in 1..=config.repeats {
            let throughput = pass::<false>(config, capacity, config.transfers)?;
            let mut latency = pass::<true>(config, capacity, config.transfers)?;
            latency.samples.sort_unstable();
            println!(
                "capacity={capacity} repeat={repeat} transfers={} throughput_mmsg_s={:.3} throughput_ms={:.3} latency_samples={} latency_p50_ns={} latency_p95_ns={} latency_p99_ns={} latency_max_ns={}",
                config.transfers,
                config.transfers as f64 / throughput.elapsed.as_secs_f64() / 1e6,
                throughput.elapsed.as_secs_f64() * 1e3,
                latency.samples.len(),
                percentile(&latency.samples, 50),
                percentile(&latency.samples, 95),
                percentile(&latency.samples, 99),
                latency.samples.last().unwrap()
            );
        }
    }
    Ok(())
}

fn main() -> std::process::ExitCode {
    let result = parse(std::env::args().skip(1)).and_then(|config| match config {
        Some(config) => run(&config),
        None => {
            println!("{HELP}");
            Ok(())
        }
    });
    match result {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("channel_bench: {error}");
            std::process::ExitCode::FAILURE
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    };

    #[derive(Debug, Default)]
    pub(super) struct Hooks {
        fault: Option<(usize, Fault)>,
        fired: AtomicUsize,
        exited: [AtomicUsize; 2],
        pub(super) setup_ok: [AtomicUsize; 2],
        // Test-only handshake forces empty receive, final send/close, recheck.
        empty: Option<mpsc::Sender<()>>,
        closed: Option<Mutex<mpsc::Receiver<()>>>,
        await_empty: Option<Mutex<mpsc::Receiver<()>>>,
        pub(super) notify_closed: Option<mpsc::Sender<()>>,
    }

    #[derive(Clone, Copy, Debug)]
    enum Fault {
        Deadline,
        Fail,
    }

    impl Hooks {
        pub(super) fn inject(
            &self,
            worker: usize,
            sequence: usize,
            guard: &mut Guard,
        ) -> Result<()> {
            if worker == 0
                && sequence == 0
                && let Some(empty) = &self.await_empty
            {
                empty
                    .lock()
                    .unwrap()
                    .recv_timeout(Duration::from_secs(1))
                    .map_err(|e| e.to_string())?;
            }
            if sequence == 8
                && let Some((target, fault)) = self.fault
                && target == worker
            {
                let error = match fault {
                    Fault::Deadline => {
                        // Eight messages have already crossed this endpoint. Expire
                        // the real sparse guard at its next checkpoint, no sleeps.
                        guard.deadline = Instant::now();
                        guard.attempts = 1023;
                        guard.check().expect_err("injected deadline must expire")
                    }
                    Fault::Fail => "injected worker failure".into(),
                };
                self.fired.fetch_add(1, Ordering::SeqCst);
                return Err(error);
            }
            Ok(())
        }

        pub(super) fn after_receive(&self, empty: bool) -> Result<()> {
            if empty && let Some(notify) = &self.empty {
                notify.send(()).map_err(|e| e.to_string())?;
                self.closed
                    .as_ref()
                    .unwrap()
                    .lock()
                    .unwrap()
                    .recv_timeout(Duration::from_secs(1))
                    .map_err(|e| e.to_string())?;
                self.fired.fetch_add(1, Ordering::SeqCst);
            }
            Ok(())
        }
    }

    pub(super) struct WorkerExit<'a>(pub(super) &'a Hooks, pub(super) usize);

    impl Drop for WorkerExit<'_> {
        fn drop(&mut self) {
            self.0.exited[self.1].fetch_add(1, Ordering::SeqCst);
        }
    }

    fn assert_joined(config: &Config, start: Instant) {
        // pass uses scoped workers: returning already implies both joins finished.
        // Counters additionally assert both worker bodies exited, not just an early
        // configuration rejection. External timeout kills the whole test process
        // on regressions; never detach a watchdog thread or abandon a child.
        assert_eq!(config.hooks.exited[0].load(Ordering::SeqCst), 1);
        assert_eq!(config.hooks.exited[1].load(Ordering::SeqCst), 1);
        assert!(start.elapsed() < Duration::from_secs(2));
    }

    fn args(s: &str) -> Result<Option<Config>> {
        parse(s.split_whitespace().map(str::to_string))
    }

    #[test]
    fn configuration_bounds_and_help() {
        assert!(args("--help").unwrap().is_none());
        assert_eq!(args("").unwrap().unwrap().capacities, [1, 3, 256]);
        for invalid in [
            "--transfers 0",
            "--capacities 0",
            "--capacities 1,",
            "--transfers 1000000001",
            "--repeats 101",
            "--timeout-ms 60001",
            "--cpus 0,0",
            "--cpus 0",
            "--sample-every 0",
            "--transfers 1000001 --sample-every 1",
            "--unknown 1",
            "--transfers",
        ] {
            assert!(args(invalid).is_err(), "accepted {invalid}");
        }
    }

    #[test]
    fn small_transfers_and_nearest_rank_percentiles() {
        let config = args("--transfers 33 --sample-every 4 --repeats 1")
            .unwrap()
            .unwrap();
        for capacity in [1, 3, 256] {
            assert!(
                pass::<false>(&config, capacity, 33)
                    .unwrap()
                    .samples
                    .is_empty()
            );
            assert_eq!(
                pass::<true>(&config, capacity, 33).unwrap().samples.len(),
                9
            );
        }
        assert_eq!(percentile(&[10], 99), 10);
        assert_eq!(percentile(&[1, 2, 3, 4], 50), 2);
        assert_eq!(percentile(&[1, 2, 3, 4], 99), 4);
    }

    #[test]
    fn deadline_and_setup_failures_terminate() {
        let mut guard = Guard {
            deadline: Instant::now(),
            attempts: 1023,
        };
        assert!(guard.check().is_err());
        let mut config = args("--transfers 1 --repeats 1").unwrap().unwrap();
        config.cpus = Some([usize::MAX, usize::MAX - 1]);
        assert!(pass::<false>(&config, 1, 1).is_err());
    }

    #[test]
    fn active_transfer_deadline_and_peer_failure_join_both_workers() {
        for worker in [0, 1] {
            for fault in [Fault::Deadline, Fault::Fail] {
                let mut config = args("--transfers 10000 --timeout-ms 1000")
                    .unwrap()
                    .unwrap();
                config.hooks = Arc::new(Hooks {
                    fault: Some((worker, fault)),
                    ..Hooks::default()
                });
                let start = Instant::now();
                let error = pass::<false>(&config, 1, config.transfers)
                    .err()
                    .expect("injected failure must fail pass");
                assert_eq!(config.hooks.fired.load(Ordering::SeqCst), 1);
                if worker == 0 {
                    assert_eq!(
                        error,
                        match fault {
                            Fault::Deadline => "pass deadline exceeded",
                            Fault::Fail => "injected worker failure",
                        }
                    );
                } else {
                    // Receiver destruction must interrupt the producer, rather
                    // than making it retry a terminal error until its deadline.
                    assert!(
                        error.starts_with("send at ") && error.ends_with("resource unavailable"),
                        "{error}"
                    );
                }
                assert_joined(&config, start);
            }
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn asymmetric_affinity_failure_releases_valid_worker() {
        let allowed = allowed_cpus().expect("Linux affinity discovery");
        let Some(&valid) = allowed.first() else {
            return;
        };
        for cpus in [[valid, usize::MAX], [usize::MAX, valid]] {
            let mut config = args("--transfers 1 --timeout-ms 1000").unwrap().unwrap();
            config.cpus = Some(cpus);
            let start = Instant::now();
            let error = pass::<false>(&config, 1, 1)
                .err()
                .expect("invalid peer CPU must fail setup");
            assert!(
                error.contains("outside the calling thread's allowed mask"),
                "{error}"
            );
            assert_joined(&config, start);
            for (worker, cpu) in cpus.into_iter().enumerate() {
                assert_eq!(
                    config.hooks.setup_ok[worker].load(Ordering::SeqCst),
                    usize::from(cpu == valid)
                );
            }
            assert_eq!(
                allowed_cpus().unwrap(),
                allowed,
                "coordinator affinity changed"
            );
        }
    }

    #[test]
    fn empty_observation_before_final_send_rechecks_after_close() {
        let (empty_tx, empty_rx) = mpsc::channel();
        let (closed_tx, closed_rx) = mpsc::channel();
        let mut config = args("--transfers 1 --timeout-ms 1000").unwrap().unwrap();
        config.hooks = Arc::new(Hooks {
            empty: Some(empty_tx),
            closed: Some(Mutex::new(closed_rx)),
            await_empty: Some(Mutex::new(empty_rx)),
            notify_closed: Some(closed_tx),
            ..Hooks::default()
        });
        let start = Instant::now();
        pass::<false>(&config, 1, 1).expect("closure must not hide the final value");
        assert_eq!(
            config.hooks.fired.load(Ordering::SeqCst),
            1,
            "forced exactly one stale empty observation"
        );
        assert_joined(&config, start);
    }
}
