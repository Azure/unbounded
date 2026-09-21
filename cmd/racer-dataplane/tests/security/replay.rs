// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;

    fn nonce(n: u64) -> [u8; 32] {
        *blake3::hash(&n.to_le_bytes()).as_bytes()
    }

    #[test]
    fn capacity_expiry_121_seconds_no_live_eviction_and_metrics() {
        let ledger = ReplayLedger::new(Config {
            capacity: 2,
            shards: 1,
        })
        .unwrap();
        let now = Instant::now();
        ledger.accept(nonce(0), now).unwrap();
        ledger
            .accept(nonce(1), now + Duration::from_secs(1))
            .unwrap();
        for seconds in [1, 60, 120] {
            let t = now + Duration::from_secs(seconds);
            assert_eq!(
                ledger.accept(nonce(2), t).unwrap_err().kind(),
                io::ErrorKind::WouldBlock
            );
            assert_eq!(
                ledger.accept(nonce(0), t).unwrap_err().kind(),
                io::ErrorKind::InvalidData
            );
        }
        let mut metrics = String::new();
        ledger.render(&mut metrics);
        for expected in [
            "replay_capacity 2",
            "replay_occupancy 2",
            "replay_accepted_total 2",
            "reason=\"capacity\"} 3",
            "reason=\"replay\"} 3",
        ] {
            assert!(metrics.contains(expected), "{metrics}");
        }
        ledger.accept(nonce(2), now + LIFETIME).unwrap();
        assert_eq!(
            ledger.accept(nonce(1), now + LIFETIME).unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
        ledger
            .accept(nonce(0), now + Duration::from_secs(122))
            .unwrap();
    }

    #[test]
    fn bounded_cleanup_and_shard_capacity_without_spill_or_live_eviction() {
        let ledger = ReplayLedger::new(Config {
            capacity: 256,
            shards: 1,
        })
        .unwrap();
        let now = Instant::now();
        for n in 0..256 {
            ledger.accept(nonce(n), now).unwrap();
        }
        ledger.accept(nonce(256), now + LIFETIME).unwrap();
        assert_eq!(
            ledger.shards[0].occupancy.load(Ordering::Relaxed),
            256 - CLEANUP_BUDGET + 1
        );
        for n in 257..260 {
            ledger.accept(nonce(n), now + LIFETIME).unwrap();
        }
        assert_eq!(ledger.shards[0].occupancy.load(Ordering::Relaxed), 4);
        let ledger = ReplayLedger::new(Config {
            capacity: 129,
            shards: 4,
        })
        .unwrap();
        assert_eq!(ledger.shards.iter().map(|s| s.capacity).sum::<usize>(), 129);
        let mut admitted = Vec::new();
        for n in 0..10_000 {
            if ledger.accept(nonce(n), now).is_ok() {
                admitted.push(n);
            }
        }
        assert_eq!(admitted.len(), 129);
        for n in admitted {
            assert_eq!(
                ledger.accept(nonce(n), now).unwrap_err().kind(),
                io::ErrorKind::InvalidData
            );
        }
    }

    #[test]
    fn concurrent_workers_and_volumes_share_exactly_once_admission() {
        let ledger = ReplayLedger::new(Config {
            capacity: 8192,
            shards: 16,
        })
        .unwrap();
        let barrier = std::sync::Barrier::new(8);
        let accepted = AtomicUsize::new(0);
        let now = Instant::now();
        std::thread::scope(|scope| {
            for _worker in 0..8 {
                scope.spawn(|| {
                    barrier.wait();
                    for n in 0..512 {
                        // The API deliberately has no worker/volume namespace.
                        match ledger.accept(nonce(n), now) {
                            Ok(()) => {
                                accepted.fetch_add(1, Ordering::Relaxed);
                            }
                            Err(e) => assert_eq!(e.kind(), io::ErrorKind::InvalidData),
                        }
                    }
                });
            }
        });
        assert_eq!(accepted.load(Ordering::Relaxed), 512);
        assert_eq!(
            ledger
                .shards
                .iter()
                .map(|s| s.replayed.load(Ordering::Relaxed))
                .sum::<u64>(),
            7 * 512
        );
    }

    #[test]
    fn configuration_is_validated_before_allocation() {
        for config in [
            Config {
                capacity: 0,
                shards: 1,
            },
            Config {
                capacity: usize::MAX,
                shards: 1,
            },
            Config {
                capacity: 1,
                shards: 0,
            },
            Config {
                capacity: 1,
                shards: 2,
            },
            Config {
                capacity: 4096,
                shards: 1025,
            },
        ] {
            assert_eq!(
                ReplayLedger::new(config).err().unwrap().kind(),
                io::ErrorKind::InvalidInput
            );
        }
        Config::default().validate().unwrap();
    }

    #[test]
    fn fixed_table_churn_matches_reference_and_never_reallocates() {
        let ledger = ReplayLedger::new(Config {
            capacity: 37,
            shards: 1,
        })
        .unwrap();
        let pointers = {
            let entries = ledger.shards[0].entries.lock().unwrap();
            (entries.slots.as_ptr(), entries.buckets.as_ptr())
        };
        let mut reference = std::collections::HashMap::new();
        let start = Instant::now();
        for n in 0..20_000u64 {
            let now = start + Duration::from_secs(n);
            reference.retain(|_, expires| *expires > now);
            // Mix repeated nonces, fresh nonces, full-table refusals, expiry and wrap.
            let value = nonce((n * 17) % 193);
            let expected = if reference.contains_key(&value) {
                Err(io::ErrorKind::InvalidData)
            } else if reference.len() == 37 {
                Err(io::ErrorKind::WouldBlock)
            } else {
                reference.insert(value, now + LIFETIME);
                Ok(())
            };
            assert_eq!(ledger.accept(value, now).map_err(|e| e.kind()), expected);
            let entries = ledger.shards[0].entries.lock().unwrap();
            assert_eq!(entries.len, reference.len());
            assert_eq!((entries.slots.as_ptr(), entries.buckets.as_ptr()), pointers);
        }
    }

    #[test]
    fn default_capacity_admits_beyond_old_ceiling_and_has_documented_storage() {
        let ledger = ReplayLedger::new(Config::default()).unwrap();
        let now = Instant::now();
        for n in 0..100_000 {
            ledger.accept(nonce(n), now).unwrap();
        }
        let bytes: usize = ledger
            .shards
            .iter()
            .map(|s| {
                let entries = s.entries.lock().unwrap();
                entries.slots.capacity() * std::mem::size_of::<Entry>()
                    + entries.buckets.capacity() * std::mem::size_of::<usize>()
            })
            .sum();
        if cfg!(target_pointer_width = "64") {
            assert_eq!(bytes, 72 * 1024 * 1024);
        }
        assert_eq!(
            ledger
                .shards
                .iter()
                .map(|s| s.occupancy.load(Ordering::Relaxed))
                .sum::<usize>(),
            100_000
        );
    }
}
