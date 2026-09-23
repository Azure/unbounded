// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Victim selection and durable-root retirement targets, not physical reuse.
use super::*;
impl Allocator {
    pub(super) fn retire(&mut self, class: Class) {
        self.live[class.index()] -= 1;
        // An in-flight snapshot may still contain this version. Two subsequent
        // publications exclude it from BOTH roots; leases can hold it longer.
        self.retired_until[class.index()] = self
            .generation()
            .saturating_add(2 + u64::from(self.pipeline.is_some()));
    }
    pub(super) fn reclaim_target(&self, class: Class) -> usize {
        (self.space.geometry.range(class).1 / 4)
            .max(1)
            .min(RECLAIM_BATCH)
            .min(self.config.max_pending_values)
    }
    pub(super) fn replenish_payload_reserve(&mut self) {
        let class = Class::Payload;
        let target = self.reclaim_target(class);
        let low = target / 2;
        // Hysteresis bounds lost residency and avoids a checkpoint per fill.
        // One-extent batches cannot provide a useful low/high interval.
        // Retired extents already count toward the target in reclaim(); never
        // evict another batch just because their roots or readers still pin them.
        if low != 0
            && self.space.maps[class.index()].borrow().free <= low
            && self.generation() >= self.reclaim_until
        {
            self.reclaim(class, Kind::Payload);
        }
    }
    pub(super) fn reclaim(&mut self, class: Class, kind: Kind) {
        let index = class.index();
        let capacity = self.space.geometry.range(class).1;
        // Non-live extents include replacements, explicit removals, snapshots,
        // outstanding kernel requests and leases. Count them toward the target
        // even when none are physically free yet: retries must not evict another
        // batch while that space is pinned.
        let missing = self
            .reclaim_target(class)
            .saturating_sub(capacity - self.live[index]);
        for _ in 0..missing {
            if self.evict_sample(kind, self.now, true).is_none() {
                break;
            }
            self.disk_cache_evictions = self.disk_cache_evictions.wrapping_add(1);
        }
        if capacity - self.live[index] > self.space.maps[index].borrow().free {
            self.reclaim_until = self.reclaim_until.max(self.retired_until[index]);
            // Count the already scheduled publication too. A retry during its
            // final sync must not schedule an unnecessary third rotation.
            let scheduled = self
                .generation()
                .saturating_add(u64::from(self.pipeline.is_some()));
            self.rotate |= scheduled < self.reclaim_until;
        }
    }
    /// Bounded approximate LFU. A removed victim can remain physically pinned
    /// until the older checkpoint rotates out; poll then retry admission.
    pub fn evict(&mut self, kind: Kind, now: u64) -> Option<Key> {
        self.evict_sample(kind, now, false)
    }
    pub(super) fn evict_sample(&mut self, kind: Kind, now: u64, durable_only: bool) -> Option<Key> {
        if self.failed {
            return None;
        }
        self.now = self.now.max(now);
        if self.heat.is_empty() {
            return None;
        }
        let epoch = self.hits / self.config.aging_interval;
        let mut best = None;
        for _ in 0..self.config.eviction_samples {
            self.random ^= self.random << 13;
            self.random ^= self.random >> 7;
            self.random ^= self.random << 17;
            let i = self.random as usize % self.heat.len();
            let heat = &self.heat[i];
            let value = self.root.get(&heat.key).unwrap();
            if value.kind() != kind {
                continue;
            }
            // A response can retain this file while awaiting another page's
            // admission. Evicting it cannot release its extent and would fill
            // the reclaim quota with a pin whose release depends on admission.
            // Explicit eviction still permits retiring a pinned generation.
            if durable_only
                && value
                    .payload()
                    .is_some_and(|v| Arc::strong_count(&v.allocation.pin) > 1)
            {
                continue;
            }
            if durable_only
                && !self.checkpoints.iter().flatten().any(|c| {
                    c.root
                        .get(&heat.key)
                        .and_then(Entry::payload)
                        .zip(value.payload())
                        .is_some_and(|(old, value)| Rc::ptr_eq(&old.allocation, &value.allocation))
                })
            {
                // Written is insufficient: the snapshot's final sync may still
                // be pending. Never churn uncheckpointed admissions for space.
                continue;
            }
            let count = if matches!(value, Entry::Metadata(m) if m.expires <= now) {
                0
            } else {
                1 + (heat.count as u32 >> (epoch - heat.epoch).min(16))
            };
            if best.is_none_or(|(_, score)| count < score) {
                best = Some((heat.key, count));
            }
        }
        let key = best?.0;
        self.remove(&key);
        Some(key)
    }
}
