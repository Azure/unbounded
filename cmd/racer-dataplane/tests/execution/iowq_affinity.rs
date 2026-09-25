// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::{collections::BTreeSet, fs, num::NonZeroUsize};

#[test]
fn iowq_real_helpers_leave_all_reactor_cores_and_preserve_file_io_cancellation() {
    // Probe existing kernel prerequisites before starting pinned workers. Forced
    // runs must fail rather than count an unavailable kernel as passing coverage.
    let Some(probe) = crate::conformance::kernel_ring(1, Config::default()) else {
        return;
    };
    drop(probe);
    let cores = workers::physical_core_count().unwrap();
    if cores < 3 {
        assert!(
            std::env::var_os("RACER_REQUIRE_URING").is_none(),
            "need three allowed physical cores"
        );
        eprintln!("SKIP io_wq affinity: need three allowed physical cores");
        return;
    }
    let plan = workers::CpuPlan::discover_bounded(
        workers::Config {
            shard_count: NonZeroUsize::new(2).unwrap(),
        },
        workers::WorkerCounts::default(),
        crate::tuning::Limits {
            cpu_quota: Some(4),
            available_memory: 8 << 30,
            memlock: 8 << 30,
        },
        2,
    )
    .unwrap();
    // The eight-logical-CPU profile exercises two reactors. Smaller allowed
    // topologies can retain one under the existing automatic CPU budget.
    assert!(!plan.io().is_empty());
    let excluded: BTreeSet<_> = plan
        .io()
        .iter()
        .flat_map(|p| {
            let text = fs::read_to_string(format!(
                "/sys/devices/system/cpu/cpu{}/topology/thread_siblings_list",
                p.cpu_id().0
            ))
            .unwrap();
            parse_list(&text)
        })
        .collect();
    let pools = buffers::Pools::new(buffers::Config::new(NonZeroUsize::new(1).unwrap()));
    let workers = workers::Workers::start_planned(plan, move |placement| {
        let pool = pools.test_for_worker(placement)?;
        let mut ring = Ring::new(placement, pool, Config::default())?;
        assert!(!placement.iowq_cpus.is_empty());
        let expected: BTreeSet<_> = placement.iowq_cpus.iter().map(|c| c.0).collect();
        assert!(expected.is_disjoint(&excluded));
        let path = std::env::temp_dir().join(format!(
            "racer-helper-affinity-{}-{}",
            std::process::id(),
            placement.worker_id().0
        ));
        let file = fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(&path)?;
        fs::remove_file(&path)?;
        file.set_len(4096)?;
        let file = File::new(file.into());
        let mut write = ring
            .write_page(
                file.clone().into(),
                Box::new(Page([0x5a; 4096])),
                FileOffset::new(0)?,
            )
            .unwrap();
        assert_ne!(
            ring.core
                .as_ref()
                .unwrap()
                .raw
                .unpublished(write.id)
                .unwrap()
                .flags
                & abi::ASYNC,
            0
        );
        drive(&mut ring, |r| {
            r.take_page(&mut write).unwrap().is_some_and(|c| {
                assert_eq!(c.result.unwrap(), 4096);
                true
            })
        });
        let mut read = ring
            .read_bytes(file.clone().into(), vec![0; 4096].into_boxed_slice(), 0)
            .unwrap();
        assert_ne!(
            ring.core
                .as_ref()
                .unwrap()
                .raw
                .unpublished(read.id)
                .unwrap()
                .flags
                & abi::ASYNC,
            0
        );
        drive(&mut ring, |r| {
            r.take_bytes(&mut read).unwrap().is_some_and(|c| {
                assert_eq!(c.result.unwrap(), 4096);
                assert!(c.resource.iter().all(|&b| b == 0x5a));
                true
            })
        });

        // Observe actual kernel-created helper tasks, not just our requested mask.
        // Each worker has a distinct issuing TID and therefore a distinct io_wq.
        // SAFETY: gettid has no pointer arguments.
        let tid = unsafe { libc::syscall(libc::SYS_gettid) };
        let name = format!("iou-wrk-{tid}");
        let mut helpers = 0;
        for entry in fs::read_dir("/proc/self/task")? {
            let path = entry?.path();
            let Ok(comm) = fs::read_to_string(path.join("comm")) else {
                continue;
            };
            if comm.trim() != name {
                continue;
            }
            let status = fs::read_to_string(path.join("status"))?;
            let cpus = status
                .lines()
                .find_map(|line| line.strip_prefix("Cpus_allowed_list:"))
                .unwrap();
            let actual = parse_list(cpus);
            assert_eq!(actual, expected, "helper {}", path.display());
            assert!(actual.is_disjoint(&excluded));
            helpers += 1;
        }
        assert!(helpers > 0, "no actual {name} helpers found");
        let status = fs::read_to_string(format!("/proc/self/task/{tid}/status"))?;
        let cpus = status
            .lines()
            .find_map(|line| line.strip_prefix("Cpus_allowed_list:"))
            .unwrap();
        assert_eq!(parse_list(cpus), BTreeSet::from([placement.cpu_id().0]));
        eprintln!(
            "observed {helpers} {name} helpers on {expected:?}, reactor {} unchanged",
            placement.cpu_id().0
        );

        let mut read = ring
            .read_bytes(file.clone().into(), vec![0; 4096].into_boxed_slice(), 0)
            .unwrap();
        ring.progress()?;
        let mut cancel = ring.cancel(&read)?;
        drive(&mut ring, |r| r.take_cancel(&mut cancel).unwrap().is_some());
        // Regular-file IO may win the cancellation race. Both terminal outcomes
        // return the owned allocation; cancellation acknowledgment alone cannot.
        drive(&mut ring, |r| {
            r.take_bytes(&mut read).unwrap().is_some_and(|c| {
                match c.result {
                    Ok(n) => {
                        assert_eq!(n, 4096);
                        assert!(c.resource.iter().all(|&b| b == 0x5a));
                    }
                    Err(e) => assert_eq!(e.raw_os_error(), Some(libc::ECANCELED)),
                }
                assert_eq!(c.resource.len(), 4096);
                true
            })
        });
        let mut bad = ring
            .read_bytes(
                File::new(fs::File::open("/")?.into()).into(),
                vec![0; 4096].into_boxed_slice(),
                0,
            )
            .unwrap();
        drive(&mut ring, |r| {
            r.take_bytes(&mut bad).unwrap().is_some_and(|c| {
                assert_eq!(c.result.unwrap_err().raw_os_error(), Some(libc::EISDIR));
                true
            })
        });
        drop(
            ring.read_bytes(file.into(), vec![0; 4096].into_boxed_slice(), 0)
                .unwrap()
                .cancel_on_drop(),
        );
        ring.shutdown()?;
        ring.pool().assert_recovered();
        Ok(workers::pool_tests::ParkDriver::current())
    })
    .unwrap();
    workers.stop_handle().request_stop();
    workers.join().unwrap();
}

fn parse_list(text: &str) -> BTreeSet<usize> {
    text.trim()
        .split(',')
        .flat_map(|part| {
            let (start, end) = part.split_once('-').unwrap_or((part, part));
            start.parse::<usize>().unwrap()..=end.parse::<usize>().unwrap()
        })
        .collect()
}
