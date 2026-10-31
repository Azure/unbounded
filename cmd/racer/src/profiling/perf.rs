//! Small Linux perf ABI surface. Kernel rings contain addresses, never stack bytes.
use super::{MAX_DEPTH, MAX_SAMPLES, ProfileError as Error, Sample, profile};
use std::{
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    sync::atomic::{AtomicBool, Ordering, fence},
    time::{Duration, Instant, SystemTime},
};

const MAX_THREADS: usize = 512;
const DATA_BYTES: usize = 64 * 1024;
const MAX_MAPPED: usize = 64 * 1024 * 1024;
const CONTEXT_USER: u64 = (-512i64) as u64;
const CONTEXT_MAX: u64 = (-4095i64) as u64;

struct Samples {
    values: Vec<Sample>,
    slots: Vec<usize>,
}
impl Samples {
    fn new() -> Result<Self, Error> {
        let mut values = Vec::new();
        values
            .try_reserve_exact(MAX_SAMPLES)
            .map_err(|_| Error::ResourceLimit)?;
        let mut slots = Vec::new();
        slots
            .try_reserve_exact(MAX_SAMPLES * 2)
            .map_err(|_| Error::ResourceLimit)?;
        slots.resize(MAX_SAMPLES * 2, usize::MAX);
        Ok(Self { values, slots })
    }
    fn add(&mut self, sample: Sample) -> Result<(), Error> {
        use std::hash::{Hash, Hasher};
        let mut hash = std::collections::hash_map::DefaultHasher::new();
        (sample.tid, sample.birth, &sample.frames[..sample.depth]).hash(&mut hash);
        let mut slot = hash.finish() as usize & (self.slots.len() - 1);
        for _ in 0..self.slots.len() {
            let index = self.slots[slot];
            if index == usize::MAX {
                if self.values.len() == MAX_SAMPLES {
                    return Err(Error::ResourceLimit);
                }
                self.slots[slot] = self.values.len();
                self.values.push(sample);
                return Ok(());
            }
            let previous = &mut self.values[index];
            if previous.tid == sample.tid
                && previous.birth == sample.birth
                && previous.depth == sample.depth
                && previous.frames == sample.frames
            {
                previous.count = previous
                    .count
                    .checked_add(sample.count)
                    .ok_or(Error::ResourceLimit)?;
                previous.period = previous
                    .period
                    .checked_add(sample.period)
                    .ok_or(Error::ResourceLimit)?;
                return Ok(());
            }
            slot = (slot + 1) & (self.slots.len() - 1);
        }
        Err(Error::ResourceLimit)
    }
}

// Linux perf_event_attr version 5, through sample_max_stack. Bitfields use the
// little-endian x86-64/ARM64 ABI. Other byte orders are rejected before capture.
#[repr(C)]
#[derive(Default)]
struct Attr {
    kind: u32,
    size: u32,
    config: u64,
    frequency: u64,
    sample_type: u64,
    read_format: u64,
    flags: u64,
    wakeup: u32,
    bp_type: u32,
    config1: u64,
    config2: u64,
    branch: u64,
    regs_user: u64,
    stack_user: u32,
    clock: i32,
    regs_intr: u64,
    aux_watermark: u32,
    max_stack: u16,
    reserved: u16,
}
const _: () = assert!(std::mem::size_of::<Attr>() == 112);
const _: () = assert!(std::mem::offset_of!(Attr, flags) == 40);
const _: () = assert!(std::mem::offset_of!(Attr, max_stack) == 108);

struct Event {
    fd: OwnedFd,
    map: *mut u8,
    length: usize,
    offset: usize,
    size: usize,
    tail: u64,
}
impl Event {
    fn open(tid: u32, page: usize) -> Result<Self, Error> {
        let attr = Attr {
            kind: 1,
            size: 112,
            config: 0,
            frequency: 49,
            sample_type: (1 << 0) | (1 << 1) | (1 << 5) | (1 << 8),
            flags: 1 | (1 << 5) | (1 << 6) | (1 << 10) | (1 << 20) | (1 << 21),
            wakeup: 1,
            max_stack: MAX_DEPTH as u16,
            ..Default::default()
        };
        // SAFETY: attr matches the asserted kernel ABI; pid is a checked task ID.
        let raw = unsafe {
            libc::syscall(
                libc::SYS_perf_event_open,
                &attr,
                tid as libc::pid_t,
                -1i32,
                -1i32,
                8u64,
            )
        };
        if raw < 0 {
            return Err(Error::os(std::io::Error::last_os_error()));
        }
        // SAFETY: syscall returned a new descriptor.
        let fd = unsafe { OwnedFd::from_raw_fd(raw as i32) };
        let size = DATA_BYTES.max(page);
        let length = page.checked_add(size).ok_or(Error::ResourceLimit)?;
        // SAFETY: map an owned perf descriptor, with one metadata page and power-of-two data pages.
        let map = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd.as_raw_fd(),
                0,
            )
        };
        if map == libc::MAP_FAILED {
            return Err(Error::os(std::io::Error::last_os_error()));
        }
        let mut event = Self {
            fd,
            map: map.cast(),
            length,
            offset: page,
            size,
            tail: 0,
        };
        let offset = event.load(1040) as usize;
        let reported = event.load(1048) as usize;
        if offset != page || reported != size {
            return Err(Error::MalformedRecord);
        }
        event.offset = offset;
        // SAFETY: PERF_EVENT_IOC_ENABLE takes no pointer argument.
        if unsafe { libc::ioctl(event.fd.as_raw_fd(), 0x2400) } < 0 {
            return Err(Error::os(std::io::Error::last_os_error()));
        }
        Ok(event)
    }
    fn load(&self, offset: usize) -> u64 {
        // SAFETY: metadata offsets are aligned u64 fields within the first page.
        unsafe { std::ptr::read_volatile(self.map.add(offset).cast::<u64>()) }
    }
    fn stop(&self) -> Result<(), Error> {
        // SAFETY: PERF_EVENT_IOC_DISABLE takes no pointer argument.
        if unsafe { libc::ioctl(self.fd.as_raw_fd(), 0x2401) } < 0 {
            return Err(Error::os(std::io::Error::last_os_error()));
        }
        Ok(())
    }
    fn copy(&self, at: u64, bytes: &mut [u8]) {
        for (index, byte) in bytes.iter_mut().enumerate() {
            let offset = (at.wrapping_add(index as u64) as usize) & (self.size - 1);
            // SAFETY: head/tail ownership prevents the kernel overwriting these published bytes.
            *byte = unsafe { std::ptr::read_volatile(self.map.add(self.offset + offset)) };
        }
    }
    fn drain(
        &mut self,
        samples: &mut Samples,
        tid: u32,
        birth: u64,
        cancel: &AtomicBool,
        deadline: Instant,
    ) -> Result<(), Error> {
        let head = self.load(1024);
        fence(Ordering::Acquire);
        if head.wrapping_sub(self.tail) > self.size as u64 {
            return Err(Error::SampleLoss);
        }
        let mut record = [0u8; 1024];
        while self.tail != head {
            super::check(cancel, deadline)?;
            if head.wrapping_sub(self.tail) < 8 {
                return Err(Error::MalformedRecord);
            }
            self.copy(self.tail, &mut record[..8]);
            let size = u16::from_ne_bytes(record[6..8].try_into().unwrap()) as usize;
            if !(8..=record.len()).contains(&size) || size as u64 > head.wrapping_sub(self.tail) {
                return Err(Error::MalformedRecord);
            }
            self.copy(self.tail, &mut record[..size]);
            if let Some(mut sample) = parse(&record[..size], tid)? {
                sample.birth = birth;
                samples.add(sample)?;
            }
            self.tail = self.tail.wrapping_add(size as u64);
        }
        fence(Ordering::SeqCst);
        // SAFETY: data_tail is the aligned userspace-owned metadata field.
        unsafe { std::ptr::write_volatile(self.map.add(1032).cast::<u64>(), self.tail) };
        Ok(())
    }
}
impl Drop for Event {
    fn drop(&mut self) {
        let _ = self.stop();
        // SAFETY: this object uniquely owns the successful mmap allocation.
        unsafe { libc::munmap(self.map.cast(), self.length) };
    }
}

fn u64_at(bytes: &[u8], at: usize) -> Result<u64, Error> {
    Ok(u64::from_ne_bytes(
        bytes
            .get(at..at + 8)
            .ok_or(Error::MalformedRecord)?
            .try_into()
            .unwrap(),
    ))
}

fn parse(bytes: &[u8], tid: u32) -> Result<Option<Sample>, Error> {
    if bytes.len() < 8 {
        return Err(Error::MalformedRecord);
    }
    let kind = u32::from_ne_bytes(bytes[..4].try_into().unwrap());
    if kind == 2 || kind == 13 || kind == 5 {
        return Err(Error::SampleLoss);
    }
    if kind != 9 {
        return Err(Error::MalformedRecord);
    }
    let mode = u16::from_ne_bytes(bytes[4..6].try_into().unwrap()) & 7;
    if mode != 2 {
        return Err(Error::MalformedRecord);
    }
    let ip = u64_at(bytes, 8)?;
    let identity = u64_at(bytes, 16)?;
    if identity as u32 != std::process::id() || (identity >> 32) as u32 != tid {
        return Err(Error::MalformedRecord);
    }
    let period = u64_at(bytes, 24)?;
    let count = usize::try_from(u64_at(bytes, 32)?).map_err(|_| Error::MalformedRecord)?;
    if count > MAX_DEPTH + 2
        || bytes.len() != 40 + count * 8
        || period == 0
        || ip == 0
        || ip >= CONTEXT_MAX
    {
        return Err(Error::MalformedRecord);
    }
    let mut sample = Sample {
        tid,
        birth: 0,
        count: 1,
        period,
        depth: 1,
        frames: [0; MAX_DEPTH],
    };
    sample.frames[0] = ip;
    let mut first = true;
    for index in 0..count {
        let address = u64_at(bytes, 40 + index * 8)?;
        if address == CONTEXT_USER {
            continue;
        }
        if address >= CONTEXT_MAX {
            return Err(Error::MalformedRecord);
        }
        if address == 0 {
            continue;
        }
        let duplicate_leaf = first && address == ip;
        first = false;
        if duplicate_leaf {
            continue;
        }
        if sample.depth == MAX_DEPTH {
            break;
        }
        sample.frames[sample.depth] = address;
        sample.depth += 1;
    }
    Ok(Some(sample))
}

fn identity(tid: u32, cancel: &AtomicBool, deadline: Instant) -> Result<u64, Error> {
    super::check(cancel, deadline)?;
    let path = format!("/proc/self/task/{tid}/stat");
    let stat = profile::read_bounded(std::path::Path::new(&path), 4096)?;
    super::check(cancel, deadline)?;
    let text = std::str::from_utf8(&stat).map_err(|_| Error::MalformedRecord)?;
    text.rsplit_once(')')
        .and_then(|(_, rest)| rest.split_whitespace().nth(19))
        .and_then(|s| s.parse().ok())
        .ok_or(Error::MalformedRecord)
}

fn tasks(cancel: &AtomicBool, deadline: Instant) -> Result<Vec<(u32, u64)>, Error> {
    super::check(cancel, deadline)?;
    let mut tasks = Vec::new();
    tasks
        .try_reserve_exact(MAX_THREADS)
        .map_err(|_| Error::ResourceLimit)?;
    for entry in std::fs::read_dir("/proc/self/task").map_err(Error::os)? {
        super::check(cancel, deadline)?;
        let entry = entry.map_err(Error::os)?;
        let Some(tid) = entry
            .file_name()
            .to_str()
            .and_then(|s| s.parse::<u32>().ok())
        else {
            continue;
        };
        let start = match identity(tid, cancel, deadline) {
            Ok(start) => start,
            Err(_) if !entry.path().exists() => continue,
            Err(error) => return Err(error),
        };
        if tasks.len() == MAX_THREADS {
            return Err(Error::ResourceLimit);
        }
        tasks.push((tid, start));
    }
    Ok(tasks)
}

pub(super) fn capture(duration: Duration, cancel: &AtomicBool) -> Result<Vec<u8>, Error> {
    let start = Instant::now();
    let wall = SystemTime::now();
    let deadline = start + duration;
    let final_deadline = deadline + Duration::from_secs(5);
    super::check(cancel, deadline)?;
    if !cfg!(target_endian = "little") {
        return Err(Error::Unsupported);
    }
    // SAFETY: sysconf/gettid have no pointer arguments.
    let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    let own_tid = unsafe { libc::syscall(libc::SYS_gettid) } as u32;
    if page < 4096 || !(page as usize).is_power_of_two() {
        return Err(Error::Unsupported);
    }
    let page = page as usize;
    let maps = profile::Mappings::read(cancel, deadline)?;
    super::check(cancel, deadline)?;
    let mut samples = Samples::new()?;
    let mut events: Vec<((u32, u64), Event)> = Vec::new();
    events
        .try_reserve_exact(MAX_THREADS)
        .map_err(|_| Error::ResourceLimit)?;
    let mut seen = Vec::new();
    seen.try_reserve_exact(MAX_THREADS)
        .map_err(|_| Error::ResourceLimit)?;
    let mut scan = start;
    let collected = (|| {
        loop {
            if cancel.load(Ordering::Acquire) {
                return Err(Error::Cancelled);
            }
            let now = Instant::now();
            if now >= deadline {
                break;
            }
            if now >= scan {
                let current = tasks(cancel, deadline)?;
                let mut index = 0;
                while index < events.len() {
                    let ((tid, birth), event) = &mut events[index];
                    if !current.contains(&(*tid, *birth)) {
                        event.stop()?;
                        event.drain(&mut samples, *tid, *birth, cancel, deadline)?;
                        events.swap_remove(index);
                    } else {
                        index += 1;
                    }
                }
                for (tid, birth) in current {
                    super::check(cancel, deadline)?;
                    if tid == own_tid || events.iter().any(|(key, _)| *key == (tid, birth)) {
                        continue;
                    }
                    if seen.len() == MAX_THREADS
                        || (events.len() + 1) * (page + DATA_BYTES.max(page)) > MAX_MAPPED
                    {
                        return Err(Error::ResourceLimit);
                    }
                    match Event::open(tid, page) {
                        Ok(event) => {
                            // An ID can exit and be reused between enumeration and open.
                            // Do not retain an event unless it still names our same task.
                            match identity(tid, cancel, deadline) {
                                Ok(value) if value == birth => (),
                                Ok(_) => continue,
                                Err(Error::Cancelled) => return Err(Error::Cancelled),
                                Err(_)
                                    if !std::path::Path::new(&format!("/proc/self/task/{tid}"))
                                        .exists() =>
                                {
                                    continue;
                                }
                                Err(error) => return Err(error),
                            }
                            events.push(((tid, birth), event));
                            seen.push((tid, birth));
                        }
                        Err(_)
                            if !std::path::Path::new(&format!("/proc/self/task/{tid}"))
                                .exists() =>
                        {
                            continue;
                        }
                        Err(error) => return Err(error),
                    }
                }
                scan = now + Duration::from_millis(100);
            }
            for ((tid, birth), event) in &mut events {
                event.drain(&mut samples, *tid, *birth, cancel, deadline)?;
            }
            std::thread::sleep(
                Duration::from_millis(10).min(deadline.saturating_duration_since(Instant::now())),
            );
        }
        Ok(())
    })();
    let mut stop_error = None;
    for (_, event) in &events {
        if let Err(error) = event.stop() {
            stop_error = Some(error);
        }
    }
    let sampled_duration = start.elapsed();
    if let Some(error) = stop_error {
        return Err(error);
    }
    match collected {
        Err(Error::Cancelled) if !cancel.load(Ordering::Acquire) && Instant::now() >= deadline => {}
        other => other?,
    }
    for ((tid, birth), event) in &mut events {
        event.drain(&mut samples, *tid, *birth, cancel, final_deadline)?;
    }
    drop(events);
    if samples.values.is_empty() {
        return Err(Error::Empty);
    }
    maps.ensure_unchanged(&profile::Mappings::read(cancel, final_deadline)?)?;
    profile::encode(&samples.values, &maps, wall, sampled_duration, cancel)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn aggregation_keeps_counts_periods_and_task_lineage() {
        let mut samples = Samples::new().unwrap();
        let mut sample = parse(&record(), 7).unwrap().unwrap();
        sample.birth = 10;
        samples.add(sample.clone()).unwrap();
        samples.add(sample.clone()).unwrap();
        assert_eq!(samples.values.len(), 1);
        assert_eq!(
            (samples.values[0].count, samples.values[0].period),
            (2, 40_000_000)
        );
        sample.birth = 11;
        samples.add(sample).unwrap();
        assert_eq!(samples.values.len(), 2);
        let mut overflow = samples.values[0].clone();
        overflow.count = u64::MAX;
        assert!(matches!(samples.add(overflow), Err(Error::ResourceLimit)));
        assert!(matches!(
            capture(Duration::from_secs(1), &AtomicBool::new(true)),
            Err(Error::Cancelled)
        ));
    }
    #[test]
    fn ring_wrap_publishes_tail_and_rejects_overrun() {
        let length = 8192;
        // SAFETY: private anonymous test ring; ownership moves to Event.
        let map = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        assert_ne!(map, libc::MAP_FAILED);
        let raw = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC) };
        assert!(raw >= 0);
        let mut event = Event {
            fd: unsafe { OwnedFd::from_raw_fd(raw) },
            map: map.cast(),
            length,
            offset: 4096,
            size: 4096,
            tail: 4090,
        };
        let bytes = record();
        for (index, byte) in bytes.iter().enumerate() {
            unsafe {
                *event.map.add(4096 + ((4090 + index) & 4095)) = *byte;
            }
        }
        unsafe {
            *event.map.add(1024).cast::<u64>() = 4090 + bytes.len() as u64;
        }
        let mut samples = Samples::new().unwrap();
        event
            .drain(
                &mut samples,
                7,
                100,
                &AtomicBool::new(false),
                Instant::now() + Duration::from_secs(1),
            )
            .unwrap();
        assert_eq!(samples.values[0].birth, 100);
        assert_eq!(event.load(1032), 4090 + bytes.len() as u64);
        unsafe {
            *event.map.add(1024).cast::<u64>() = event.tail + 4097;
        }
        assert!(matches!(
            event.drain(
                &mut samples,
                7,
                100,
                &AtomicBool::new(false),
                Instant::now() + Duration::from_secs(1)
            ),
            Err(Error::SampleLoss)
        ));
    }
    fn record() -> Vec<u8> {
        let mut bytes = Vec::new();
        bytes.extend(9u32.to_ne_bytes());
        bytes.extend(2u16.to_ne_bytes());
        bytes.extend(56u16.to_ne_bytes());
        for value in [
            0x1234,
            u64::from(std::process::id()) | (7u64 << 32),
            20_000_000,
            2,
            CONTEXT_USER,
            0x1234,
        ] {
            bytes.extend(value.to_ne_bytes());
        }
        bytes
    }
    #[test]
    fn record_order_identity_context_and_loss() {
        let bytes = record();
        let sample = parse(&bytes, 7).unwrap().unwrap();
        assert_eq!(
            (sample.depth, sample.period, sample.frames[0]),
            (1, 20_000_000, 0x1234)
        );
        assert!(parse(&bytes, 8).is_err());
        assert!(parse(&bytes[..48], 7).is_err());
        let mut kernel = bytes.clone();
        kernel[40..48].copy_from_slice(&((-128i64) as u64).to_ne_bytes());
        assert!(parse(&kernel, 7).is_err());
        let mut recursive = bytes.clone();
        recursive[6..8].copy_from_slice(&64u16.to_ne_bytes());
        recursive[32..40].copy_from_slice(&3u64.to_ne_bytes());
        recursive.extend(0x1234u64.to_ne_bytes());
        assert_eq!(parse(&recursive, 7).unwrap().unwrap().depth, 2);
        for kind in [2u32, 5, 13] {
            let mut lost = bytes.clone();
            lost[..4].copy_from_slice(&kind.to_ne_bytes());
            assert!(matches!(parse(&lost, 7), Err(Error::SampleLoss)));
        }
    }
    #[test]
    fn unprivileged_event_has_explicit_result() {
        let tid = unsafe { libc::syscall(libc::SYS_gettid) } as u32;
        let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
        match Event::open(tid, page) {
            Ok(event) => {
                event.stop().unwrap();
            }
            Err(error) => assert!(
                matches!(
                    error,
                    Error::PermissionDenied | Error::Unsupported | Error::ResourceLimit
                ),
                "{error:?}"
            ),
        }
    }
}
