//! Affinity and topology checked against what Linux reports through procfs and sysfs.

use std::collections::BTreeSet;
use std::fs;
use std::path::Path;
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use uring_runtime::ErrorKind;
use uring_runtime::affinity::{CpuSet, Topology, current_cpus, pin_cpu, set_cpus};

/// Decode `Cpus_allowed` from procfs independently of the library's syscall path.
fn proc_cpus() -> CpuSet {
    let status = fs::read_to_string("/proc/thread-self/status").unwrap();
    let mask = status
        .lines()
        .find_map(|line| line.strip_prefix("Cpus_allowed:"))
        .unwrap();
    mask.trim()
        .split(',')
        .rev()
        .enumerate()
        .flat_map(|(word, hex)| {
            let bits = u32::from_str_radix(hex, 16).unwrap();
            (0..32)
                .filter(move |bit| bits & (1 << bit) != 0)
                .map(move |bit| word * 32 + bit)
        })
        .collect()
}

#[test]
fn affinity_round_trips_without_affecting_another_thread() {
    let original = proc_cpus();
    assert!(!original.is_empty());
    assert_eq!(current_cpus().unwrap(), original);
    let (pinned_tx, pinned_rx) = mpsc::channel();
    let (resume_tx, resume_rx) = mpsc::channel();
    let worker = thread::spawn(move || {
        let allowed = proc_cpus();
        let cpu = allowed.first().unwrap();
        pin_cpu(cpu).unwrap();
        assert_eq!(proc_cpus(), CpuSet::from_iter([cpu]));
        assert_eq!(current_cpus().unwrap(), proc_cpus());
        // SAFETY: sched_getcpu has no arguments or preconditions.
        assert_eq!(unsafe { libc::sched_getcpu() }, cpu as i32);
        pinned_tx.send(()).unwrap();
        resume_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        if allowed.len() > 1 {
            let pair = CpuSet::from_iter([cpu, allowed.last().unwrap()]);
            set_cpus(&pair).unwrap();
            assert_eq!(proc_cpus(), pair);
            assert_eq!(current_cpus().unwrap(), pair);
            assert_eq!(Topology::discover().unwrap().cpu_set(), pair);
        }
        set_cpus(&allowed).unwrap();
        assert_eq!(proc_cpus(), allowed);
    });
    pinned_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert_eq!(proc_cpus(), original);
    resume_tx.send(()).unwrap();
    worker.join().unwrap();
}

#[test]
fn kernel_rejected_cpu_preserves_the_mask() {
    thread::spawn(|| {
        let allowed = proc_cpus();
        let possible: CpuSet = fs::read_to_string("/sys/devices/system/cpu/possible")
            .unwrap()
            .parse()
            .unwrap();
        let absent = possible.last().unwrap() + 1;
        assert!(absent <= CpuSet::MAX_CPU);
        let error = pin_cpu(absent).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
        assert_eq!(error.kind(), ErrorKind::InvalidInput);
        assert_eq!(proc_cpus(), allowed);
    })
    .join()
    .unwrap();
}

#[test]
fn discovered_topology_matches_sysfs() {
    let topology = Topology::discover().unwrap();
    assert!(topology.cpu_set().is_subset(&proc_cpus()));
    for cpu in &topology.cpus {
        let path = Path::new("/sys/devices/system/cpu").join(format!("cpu{}", cpu.cpu));
        for (file, actual) in [("physical_package_id", cpu.package), ("core_id", cpu.core)] {
            let expected = fs::read_to_string(path.join("topology").join(file))
                .ok()
                .and_then(|value| usize::try_from(value.trim().parse::<i64>().unwrap()).ok());
            assert_eq!(actual, expected, "cpu{} {file}", cpu.cpu);
        }
        let nodes: BTreeSet<usize> = fs::read_dir(&path)
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .filter_map(|name| name.to_str()?.strip_prefix("node")?.parse().ok())
            .collect();
        assert_eq!(cpu.numa_node, nodes.first().copied());
    }
    for device in &topology.devices {
        let path = Path::new("/sys/class")
            .join(&device.class)
            .join(&device.name);
        let expected = fs::read_to_string(path.join("device/numa_node"))
            .ok()
            .and_then(|value| value.trim().parse::<usize>().ok());
        assert_eq!(device.numa_node, expected, "{}", device.name);
    }
}
