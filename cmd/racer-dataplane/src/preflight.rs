// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Fail-closed checks for the http-small-v1 deployment profile.
//! Run in the dataplane container, after setting inherited RLIMIT_MEMLOCK.
use racer_dataplane::{buffers, crypto, uring, workers};
use std::{
    env, fs, io,
    io::Write,
    num::NonZeroUsize,
    os::{fd::AsRawFd, unix::fs::MetadataExt},
    path::{Component, Path, PathBuf},
    sync::Arc,
    time::{Duration, Instant},
};

const MIB: u64 = 1024 * 1024;
const MEMORY: u64 = 2048 * MIB;
const MEMLOCK: u64 = 256 * MIB;
const HEADROOM: u64 = 2048 * MIB;

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::other(message.into())
}
fn number(name: &str) -> io::Result<u64> {
    env::var(name)
        .map_err(|_| invalid(format!("{name} must be explicitly set")))?
        .parse()
        .map_err(|_| invalid(format!("invalid {name}")))
}
fn profile(shards: u64, io: u64, compute: u64, buffers: u64, size: u64) -> io::Result<()> {
    if (shards, io, compute, buffers) != (1, 1, 1, 8)
        || !(64 * MIB..=10 * 1024 * MIB).contains(&size)
        || !size.is_multiple_of(4 * MIB)
    {
        return Err(invalid(
            "http-small-v1 requires shards=1, IO=1, compute=1, buffers=8, slab=64MiB..10GiB aligned to 4MiB",
        ));
    }
    Ok(())
}

fn quota(text: &str) -> io::Result<()> {
    let fields: Vec<_> = text.split_whitespace().collect();
    if fields.len() != 2 {
        return Err(invalid("invalid cpu.max"));
    }
    let period: u64 = fields[1]
        .parse()
        .map_err(|_| invalid("invalid CPU period"))?;
    if period == 0 {
        return Err(invalid("zero CPU period"));
    }
    if fields[0] != "max" {
        let budget: u64 = fields[0]
            .parse()
            .map_err(|_| invalid("invalid CPU quota"))?;
        if budget / period < 3 {
            return Err(invalid(
                "CPU quota below 3 CPUs; affinity alone does not account for quota",
            ));
        }
    }
    Ok(())
}

fn cgroup_path(text: &str) -> io::Result<PathBuf> {
    let path = text
        .lines()
        .find_map(|l| l.strip_prefix("0::"))
        .ok_or_else(|| invalid("http-small-v1 requires cgroup v2"))?;
    if !path.starts_with('/')
        || Path::new(path)
            .components()
            .any(|c| matches!(c, Component::ParentDir))
    {
        return Err(invalid("cgroup path is outside the visible namespace"));
    }
    Ok(Path::new("/sys/fs/cgroup").join(path.trim_start_matches('/')))
}

fn cgroups() -> io::Result<()> {
    let leaf = cgroup_path(&fs::read_to_string("/proc/self/cgroup")?)?;
    // A finite leaf limit is part of this profile, not a claim that all accepted
    // control-plane configurations or traffic fit in it.
    if fs::read_to_string(leaf.join("memory.max"))?.trim() != MEMORY.to_string() {
        return Err(invalid(
            "http-small-v1 requires a 2GiB container memory.max",
        ));
    }
    for path in leaf
        .ancestors()
        .take_while(|p| p.starts_with("/sys/fs/cgroup"))
    {
        for (name, cpu) in [("cpu.max", true), ("memory.max", false)] {
            let text = match fs::read_to_string(path.join(name)) {
                Ok(text) => text,
                // The cgroup2 mount root has no resource controls.
                Err(e)
                    if e.kind() == io::ErrorKind::NotFound
                        && path == Path::new("/sys/fs/cgroup") =>
                {
                    continue;
                }
                Err(e) => return Err(e),
            };
            if cpu {
                quota(&text)?;
            } else if text.trim() != "max"
                && text
                    .trim()
                    .parse::<u64>()
                    .map_err(|_| invalid("invalid memory.max"))?
                    < MEMORY
            {
                return Err(invalid("ancestor memory.max below 2GiB"));
            }
        }
    }
    Ok(())
}

fn storage_needed(size: u64, allocated: u64) -> io::Result<u64> {
    size.saturating_sub(allocated)
        .checked_add(HEADROOM)
        .ok_or_else(|| invalid("storage budget overflow"))
}

fn storage(path: &Path, size: u64) -> io::Result<fs::File> {
    let parent = path
        .parent()
        .filter(|p| !p.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    let directory = fs::File::open(parent)?;
    let fd = directory.as_raw_fd();
    let fdinfo = fs::read_to_string(format!("/proc/self/fdinfo/{fd}"))?;
    let mount = fdinfo
        .lines()
        .find_map(|l| l.strip_prefix("mnt_id:").map(str::trim))
        .ok_or_else(|| invalid("missing cache mount ID"))?;
    let mounts = fs::read_to_string("/proc/self/mountinfo")?;
    let ext4 = mounts.lines().any(|line| {
        line.split_whitespace().next() == Some(mount)
            && line
                .split_once(" - ")
                .is_some_and(|(_, fs)| fs.starts_with("ext4 "))
    });
    // SAFETY: no pointer arguments.
    if !ext4 || unsafe { libc::sysconf(libc::_SC_PAGESIZE) } != 4096 {
        return Err(invalid(
            "cache mount must be ext4 with 4KiB base pages; hostPath does not provision a filesystem",
        ));
    }
    let (logical, allocated) = match fs::symlink_metadata(path) {
        Ok(m) if m.is_file() && m.len() == size => (m.len(), m.blocks().saturating_mul(512)),
        Ok(_) => {
            return Err(invalid(
                "existing slab must be a regular file matching RACER_SLAB_SIZE; never resize/reformat automatically",
            ));
        }
        Err(e) if e.kind() == io::ErrorKind::NotFound => (size, 0),
        Err(e) => return Err(e),
    };
    let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
    // SAFETY: live fd and writable statvfs storage.
    if unsafe { libc::fstatvfs(fd, stat.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let stat = unsafe { stat.assume_init() };
    let available = stat.f_bavail.saturating_mul(stat.f_frsize);
    let needed = storage_needed(logical, allocated)?;
    if available < needed {
        return Err(invalid(format!(
            "cache free bytes {available} below {needed}: unallocated slab + 2GiB headroom required"
        )));
    }
    // An unlinked scratch inode tests the actual mounted storage without touching
    // the slab, and is automatically reclaimed even on later probe failure.
    let mut random = [0; 16];
    getrandom::getrandom(&mut random).map_err(|e| invalid(e.to_string()))?;
    let scratch = parent.join(format!(
        ".racer-preflight-{:032x}",
        u128::from_ne_bytes(random)
    ));
    let mut file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&scratch)?;
    fs::remove_file(scratch)?;
    file.write_all(&[0xa5; 4096])?;
    file.sync_all()?;
    // SAFETY: live scratch descriptor, aligned range entirely owned by this probe.
    if unsafe {
        libc::fallocate(
            file.as_raw_fd(),
            libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
            0,
            4096,
        )
    } != 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(file)
}

struct Probe(uring::Ring);
impl workers::Driver for Probe {
    type Wake = uring::Wake;
    fn wake_handle(&self) -> Arc<Self::Wake> {
        self.0.wake_handle()
    }
    fn turn(&mut self) -> io::Result<()> {
        std::thread::park_timeout(Duration::from_millis(1));
        Ok(())
    }
    fn shutdown(&mut self) -> io::Result<()> {
        self.0.shutdown()
    }
}

fn run() -> io::Result<()> {
    let size = number("RACER_SLAB_SIZE")?;
    profile(
        number("RACER_SHARDS")?,
        number("RACER_IO_WORKERS")?,
        number("RACER_COMPUTE_WORKERS")?,
        number("RACER_BUFFERS_PER_NODE")?,
        size,
    )?;
    cgroups()?;
    let mut limit = std::mem::MaybeUninit::<libc::rlimit>::uninit();
    // SAFETY: writable rlimit storage.
    if unsafe { libc::getrlimit(libc::RLIMIT_MEMLOCK, limit.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { limit.assume_init() }.rlim_cur < MEMLOCK {
        return Err(invalid(
            "inherited soft memlock must be at least 256MiB (ulimit -l 262144)",
        ));
    }
    let path = env::var("RACER_SLAB_PATH").map_err(|_| invalid("RACER_SLAB_PATH must be set"))?;
    let scratch = storage(Path::new(&path), size)?;
    let one = NonZeroUsize::new(1).unwrap();
    let plan = workers::CpuPlan::discover(
        workers::Config { shard_count: one },
        workers::WorkerCounts {
            io_per_node: Some(one),
            compute_per_node: Some(one),
        },
    )?;
    for p in plan.io() {
        eprintln!(
            "preflight: IO CPU {:?}, NUMA {:?}",
            p.cpu_id(),
            p.numa_node_id()
        );
    }
    eprintln!("preflight: compute CPUs {:?}", plan.compute().cpus());
    let compute = crypto::Pool::start(plan.compute(), crypto::PoolConfig::default())
        .map_err(io::Error::other)?;
    let pools = buffers::Pools::new(buffers::Config::new(NonZeroUsize::new(8).unwrap()));
    let workers = workers::Workers::start_planned(plan, move |placement| {
        // Production mbind + prefault, full-pool registration, fixed file table,
        // io_uring_setup/register/enter and a real FSYNC completion. No fallback.
        let pool = pools.for_worker(placement)?;
        let mut ring = uring::Ring::new(placement, pool, uring::Config::default())?;
        let file = uring::File::new(scratch.try_clone()?.into());
        let fixed = ring.register_file(file)?;
        let mut ticket = ring.sync_data(uring::Descriptor::Fixed(fixed))?;
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            ring.progress()?;
            if let Some(done) = ring.take_control(&mut ticket)? {
                done.result?;
                break;
            }
            if Instant::now() >= deadline {
                return Err(invalid("io_uring FSYNC timed out"));
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        Ok(Probe(ring))
    })?;
    workers.stop_handle().request_stop();
    workers.join()?;
    compute.shutdown().map_err(io::Error::other)?;
    eprintln!(
        "preflight passed: http-small-v1 startup prerequisites (not a traffic or RDMA certification)"
    );
    Ok(())
}

fn main() -> io::Result<()> {
    run().map_err(|e| invalid(format!("racer-preflight: {e}")))
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/bin/preflight.rs"
));
