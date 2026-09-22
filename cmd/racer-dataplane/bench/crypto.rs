// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Run: cargo run --release --bin crypto-bench -- --compute-workers 1,2,4
//! Measures bounded NUMA checksum admission.
//! Bulk timing includes acquisition, fill, queueing, completion and publication.
use racer_dataplane::{buffers, crypto, uring, workers};
use std::{
    hint::black_box,
    io,
    num::NonZeroUsize,
    sync::{Arc, Mutex, OnceLock},
    task::{Context, Poll, Waker},
    thread,
    time::{Duration, Instant},
};

const DEPTH: usize = 8;
struct Bulk {
    worker: crypto::Worker,
    pool: buffers::WorkerPool,
    tickets: Vec<crypto::ChecksumTicket>,
    pending: Option<buffers::Fill>,
    waker: Waker,
    start: Arc<OnceLock<Instant>>,
    warmup: Duration,
    duration: Duration,
    len: usize,
    completed: u64,
    result: Arc<Mutex<Vec<u64>>>,
    draining: bool,
}
impl uring::Application for Bulk {
    fn poll(&mut self, _: &mut uring::Ring, _: usize) -> io::Result<uring::Work> {
        let Some(&start) = self.start.get() else {
            return Ok(uring::Work {
                runnable: true,
                deadline: None,
            });
        };
        let measure = start + self.warmup;
        let end = measure + self.duration;
        let mut i = 0;
        while i < self.tickets.len() {
            if let Some(result) = self.worker.take_checksum(&mut self.tickets[i]) {
                let (fill, len, crc) = result?;
                drop(black_box(fill.publish_checked(len, crc)?));
                self.tickets.swap_remove(i);
                if !self.draining && (measure..end).contains(&Instant::now()) {
                    self.completed += 1;
                }
            } else {
                i += 1;
            }
        }
        let mut runnable = false;
        while !self.draining && self.tickets.len() < DEPTH && Instant::now() < end {
            let mut cx = Context::from_waker(&self.waker);
            match self.worker.poll_capacity(&mut cx) {
                Poll::Pending => break,
                Poll::Ready(result) => result?,
            }
            let fill = match self.pending.take() {
                Some(fill) => fill,
                None => {
                    let mut fill = self.pool.private_fill().map_err(io::Error::other)?;
                    fill.as_mut_slice()[..self.len].fill(42);
                    fill
                }
            };
            match self.worker.checksum(fill, self.len, None) {
                Ok(ticket) => self.tickets.push(ticket),
                Err(e) if matches!(e.error, crypto::Error::WouldBlock) => {
                    self.pending = Some(e.resource);
                    runnable = match self.worker.poll_capacity(&mut cx) {
                        Poll::Pending => false,
                        Poll::Ready(result) => {
                            result?;
                            true
                        }
                    };
                    break;
                }
                Err(e) => return Err(e.error.into()),
            }
        }
        Ok(uring::Work {
            runnable,
            deadline: (Instant::now() < end).then_some(end),
        })
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.draining = true;
        self.pending.take();
        let deadline = Instant::now() + Duration::from_secs(30);
        while !self.tickets.is_empty() {
            self.poll(ring, 64)?;
            if Instant::now() >= deadline {
                return Err(io::Error::other("checksum drain timed out"));
            }
            thread::yield_now();
        }
        self.result.lock().unwrap().push(self.completed);
        Ok(())
    }
}
fn bulk(
    counts: workers::WorkerCounts,
    len: usize,
    warmup: Duration,
    duration: Duration,
) -> io::Result<()> {
    let cpus = thread::available_parallelism()?;
    let plan = workers::CpuPlan::discover(workers::Config { shard_count: cpus }, counts)?;
    let compute = Arc::new(crypto::Pool::start(
        plan.compute(),
        crypto::PoolConfig::default(),
    )?);
    let factory = compute.clone();
    let pools = buffers::Pools::new(buffers::Config::new(
        NonZeroUsize::new(cpus.get() * (DEPTH + 1)).unwrap(),
    ));
    let start = Arc::new(OnceLock::new());
    let factory_start = start.clone();
    let results = Arc::new(Mutex::new(Vec::new()));
    let factory_results = results.clone();
    let group = workers::Workers::start_planned(plan, move |placement| {
        let pool = pools.for_worker(placement)?;
        let ring = uring::Ring::new(placement, pool.clone(), uring::Config::default())?;
        let (worker, source) = factory.attach(placement, &pool, ring.wake_handle())?;
        let app = Bulk {
            worker,
            pool,
            tickets: Vec::with_capacity(DEPTH),
            pending: None,
            waker: Waker::from(ring.wake_handle()),
            start: factory_start.clone(),
            warmup,
            duration,
            len,
            completed: 0,
            result: factory_results.clone(),
            draining: false,
        };
        let mut driver = uring::Driver::new(ring, app, 64)?;
        driver.add_source(source);
        Ok(driver)
    })?;
    start.set(Instant::now()).unwrap();
    thread::sleep(warmup + duration);
    group.stop_handle().request_stop();
    let joined = group.join();
    Arc::try_unwrap(compute)
        .ok()
        .expect("compute pool still shared")
        .shutdown()?;
    joined?;
    report(
        "checksum_fill_publish",
        len,
        results.lock().unwrap().iter().sum(),
        duration,
    );
    Ok(())
}
fn report(name: &str, len: usize, completed: u64, duration: Duration) {
    assert!(completed > 0);
    let rate = completed as f64 / duration.as_secs_f64();
    println!(
        "RESULT case={name} bytes={len} completed={completed} ops_per_second={rate:.2} gib_per_second={:.3}",
        rate * len as f64 / (1u64 << 30) as f64
    );
}
fn main() -> io::Result<()> {
    let mut warmup = Duration::from_secs(2);
    let mut duration = Duration::from_secs(5);
    let mut io_per_node = None;
    let mut compute = vec![None];
    let mut args = std::env::args().skip(1);
    while let Some(flag) = args.next() {
        if flag == "--help" {
            println!(
                "crypto-bench [--io-workers N] [--compute-workers N,N,...] [--warmup SECONDS] [--duration SECONDS]\nMeasures metadata and 4 MiB checksum fill/queue/publication through production NUMA workers."
            );
            return Ok(());
        }
        // Retain the old checksum-only invocation for benchmark automation.
        if flag == "--bulk-only" {
            continue;
        }
        let value = args
            .next()
            .ok_or_else(|| io::Error::other("missing option value"))?;
        match flag.as_str() {
            "--warmup" | "--duration" => {
                let seconds: u64 = value.parse().map_err(io::Error::other)?;
                if !(1..=3600).contains(&seconds) {
                    return Err(io::Error::other("seconds must be 1..=3600"));
                }
                if flag == "--warmup" {
                    warmup = Duration::from_secs(seconds);
                } else {
                    duration = Duration::from_secs(seconds);
                }
            }
            "--io-workers" => {
                io_per_node = Some(value.parse::<NonZeroUsize>().map_err(io::Error::other)?)
            }
            "--compute-workers" => {
                compute = value
                    .split(',')
                    .map(|v| {
                        v.parse::<NonZeroUsize>()
                            .map(Some)
                            .map_err(io::Error::other)
                    })
                    .collect::<io::Result<_>>()?
            }
            _ => return Err(io::Error::other("unknown option")),
        }
    }
    for compute_per_node in compute {
        for len in [
            racer_dataplane::metadata::Metadata::SIZE,
            buffers::BUFFER_SIZE,
        ] {
            bulk(
                workers::WorkerCounts {
                    io_per_node,
                    compute_per_node,
                },
                len,
                warmup,
                duration,
            )?;
        }
    }
    Ok(())
}
