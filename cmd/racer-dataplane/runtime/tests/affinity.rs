#![cfg(target_os = "linux")]

use std::{collections::BTreeSet, fs, path::Path, sync::mpsc, thread, time::Duration};
use uring_runtime::{
    Error,
    affinity::{EffectiveTopology, current_cpus, pin_cpu, set_cpus},
};

// Decode procfs's hexadecimal mask independently of the runtime's syscall reader.
fn proc_cpus() -> BTreeSet<usize> {
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
fn affinity_round_trips_through_linux_without_affecting_another_thread() {
    let original = proc_cpus();
    assert!(!original.is_empty());
    assert_eq!(current_cpus().unwrap(), original);
    let (pinned_tx, pinned_rx) = mpsc::channel();
    let (resume_tx, resume_rx) = mpsc::channel();
    let worker = thread::spawn(move || {
        let allowed = proc_cpus();
        let cpu = *allowed.first().unwrap();
        pin_cpu(cpu).unwrap();
        assert_eq!(proc_cpus(), BTreeSet::from([cpu]));
        assert_eq!(current_cpus().unwrap(), proc_cpus());
        // SAFETY: sched_getcpu has no pointer arguments or preconditions.
        assert_eq!(unsafe { libc::sched_getcpu() }, cpu as i32);
        pinned_tx.send(()).unwrap();
        resume_rx.recv_timeout(Duration::from_secs(5)).unwrap();

        if allowed.len() > 1 {
            let pair = BTreeSet::from([cpu, *allowed.last().unwrap()]);
            set_cpus(&pair).unwrap();
            assert_eq!(proc_cpus(), pair);
            assert_eq!(current_cpus().unwrap(), pair);
            assert_eq!(
                EffectiveTopology::discover()
                    .unwrap()
                    .cpus
                    .iter()
                    .map(|c| c.cpu)
                    .collect::<BTreeSet<_>>(),
                pair
            );
        } else {
            eprintln!("single allowed CPU: multi-CPU affinity check not applicable");
        }
        set_cpus(&allowed).unwrap();
        assert_eq!(proc_cpus(), allowed);
        assert_eq!(current_cpus().unwrap(), allowed);
    });
    pinned_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    assert_eq!(proc_cpus(), original);
    assert_eq!(current_cpus().unwrap(), original);
    resume_tx.send(()).unwrap();
    worker.join().unwrap();
}

#[test]
fn kernel_rejected_affinity_preserves_the_mask() {
    thread::spawn(|| {
        let allowed = proc_cpus();
        let possible = fs::read_to_string("/sys/devices/system/cpu/possible").unwrap();
        let absent = possible
            .trim()
            .split([',', '-'])
            .map(|n| n.parse::<usize>().unwrap())
            .max()
            .unwrap()
            + 1;
        assert!(
            absent <= 1_048_575,
            "host leaves no representable absent CPU"
        );
        assert_eq!(pin_cpu(absent), Err(Error::Io));
        assert_eq!(proc_cpus(), allowed);
        assert_eq!(current_cpus().unwrap(), allowed);
    })
    .join()
    .unwrap();
}

#[test]
fn discovered_topology_matches_host_sysfs() {
    let allowed = proc_cpus();
    let topology = EffectiveTopology::discover().unwrap();
    assert_eq!(
        topology.cpus.iter().map(|c| c.cpu).collect::<BTreeSet<_>>(),
        allowed
    );
    for cpu in &topology.cpus {
        let path = Path::new("/sys/devices/system/cpu").join(format!("cpu{}", cpu.cpu));
        for (file, actual) in [("physical_package_id", cpu.package), ("core_id", cpu.core)] {
            assert_eq!(
                actual,
                fs::read_to_string(path.join("topology").join(file))
                    .unwrap()
                    .trim()
                    .parse::<usize>()
                    .unwrap()
            );
        }
        let nodes: BTreeSet<usize> = fs::read_dir(&path)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .filter_map(|name| name.to_str()?.strip_prefix("node")?.parse().ok())
            .collect();
        assert_eq!(cpu.numa_node, nodes.first().copied());
    }
    let mut expected = Vec::new();
    for directory in ["/sys/class/net", "/sys/class/infiniband"] {
        if !Path::new(directory).exists() {
            continue;
        }
        for entry in fs::read_dir(directory).unwrap() {
            let entry = entry.unwrap();
            let node = match fs::read_to_string(entry.path().join("device/numa_node")) {
                Ok(value) => usize::try_from(value.trim().parse::<i64>().unwrap()).ok(),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
                Err(e) => panic!("{}: {e}", entry.path().display()),
            };
            expected.push((entry.file_name().to_string_lossy().into_owned(), node));
        }
    }
    let mut actual: Vec<_> = topology
        .nics
        .into_iter()
        .map(|nic| (nic.device, nic.numa_node))
        .collect();
    expected.sort();
    actual.sort();
    assert_eq!(actual, expected);
    assert_eq!(proc_cpus(), allowed);
    assert_eq!(current_cpus().unwrap(), allowed);
}
