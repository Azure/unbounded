//! A separate seeded pin model keeps the original I/O lifecycle model intact.
//! Pin ownership is metadata-only; lease and freeze interleavings need no I/O.
use super::{Context_, Rng, context, env, seed_range};
use page_alloc::{
    Alignment, Error, Generation, SegmentId, SegmentSnapshot, SegmentState, Segments,
};

#[derive(Clone)]
struct Slot {
    image: SegmentSnapshot,
    pending: usize,
    published: bool,
    charged: bool,
}

impl Slot {
    fn pinned(&self) -> bool {
        self.image.pinned || self.charged
    }
}

/// Cancellation may release process-local pin ownership during a freeze, but
/// cannot authorize reuse. Published pin release remains retryable after thaw.
#[test]
fn frozen_pin_release_and_restore_never_allow_early_reuse() {
    for published in [false, true] {
        let s = Segments::new(1024);
        s.configure(2048, 2, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let (shared, shared_extent) = s.append(512).unwrap();
        let (pinned, pinned_extent) = s.append_in(512, true).unwrap();
        let id = pinned.id();
        let generation = pinned.generation();
        if published {
            s.pin_segment(id).unwrap();
        }
        let frozen = s.freeze().unwrap();
        let cut = s.snapshot();
        assert_eq!(s.clear_pinned(id), Err(Error::Busy));
        assert_eq!(s.snapshot(), cut);
        drop(pinned);
        assert_eq!(s.pinned(id), Ok(published));
        assert_eq!(s.validate(id, generation, &pinned_extent), Ok(()));
        assert_eq!(s.validate_lease(&shared, &shared_extent), Ok(()));
        assert_eq!(s.begin_evict(id), Err(Error::Busy));
        assert_eq!(s.recycle(id), Err(Error::Busy));
        assert_eq!(s.restore(cut.clone()), Err(Error::Busy));
        drop(frozen);
        assert_eq!(s.restore(cut.clone()), Err(Error::Busy));
        drop(shared);
        // The live cut retains both open classes and its pin flag. Restore seals
        // both tails and pins remain protected until their owner clears them.
        s.restore(cut).unwrap();
        assert_eq!(s.state(id), Ok(SegmentState::Sealed));
        assert_eq!(s.pinned(id), Ok(true));
        assert_eq!(s.begin_evict(id), Err(Error::Busy));
        s.clear_pinned(id).unwrap();
        let read = s.lease(id, generation).unwrap();
        s.begin_evict(id).unwrap();
        assert_eq!(s.validate_lease(&read, &pinned_extent), Ok(()));
        assert_eq!(s.recycle(id), Err(Error::Busy));
        drop(read);
        s.recycle(id).unwrap();
        assert_eq!(s.lease(id, generation).err(), Some(Error::Stale));
        assert_eq!(
            s.snapshot()[id.0 as usize].generation,
            Generation(generation.0 + 1)
        );
    }
}

/// Shared and pinned tails, cancellation, publication, release, freeze, reclaim,
/// and restore are composed rather than tested only as isolated happy paths.
#[test]
fn pinned_lifecycles_match_seeded_model() {
    let _context = Context_;
    let mut coverage = [0usize; 12];
    let (start, count) = seed_range(env);
    let steps = env("DST_STEPS").unwrap_or(400);
    for seed in (0..count).map(|offset| start.checked_add(offset).unwrap()) {
        let mut rng = Rng(seed);
        let s = Segments::new(1024);
        s.configure(3072, 3, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let mut slots: Vec<_> = s
            .snapshot()
            .into_iter()
            .map(|image| Slot {
                image,
                pending: 0,
                published: false,
                charged: false,
            })
            .collect();
        let mut leases = Vec::new();
        let mut frozen = None;
        let mut appends_frozen = false;
        for step in 0..steps {
            let action = rng.below(12) as usize;
            let position = rng.below(3) as usize;
            let id = SegmentId(position as u64);
            context(format!(
                "pinned seed {seed} step {step}: action {action} slot {position}"
            ));
            coverage[action] += 1;
            let before = s.snapshot();
            let generation = s.generation();
            match action {
                0 | 1 => {
                    let pinned = action == 1;
                    let length = if rng.chance(50) { 512 } else { 1024 };
                    let open = slots.iter().position(|slot| {
                        slot.image.state == SegmentState::Open && slot.image.pinned == pinned
                    });
                    let target = open
                        .filter(|&i| slots[i].image.used_bytes + length <= 1024)
                        .or_else(|| {
                            slots
                                .iter()
                                .position(|slot| slot.image.state == SegmentState::Free)
                        });
                    let result = s.append_in(length as usize, pinned);
                    if appends_frozen {
                        assert_eq!(result.err(), Some(Error::Busy));
                    } else {
                        if let Some(i) = open.filter(|i| Some(*i) != target) {
                            slots[i].image.state = SegmentState::Sealed;
                        }
                        match target {
                            None => assert_eq!(result.err(), Some(Error::Busy)),
                            Some(i) => {
                                let (lease, extent) = result.unwrap();
                                assert_eq!(lease.id(), slots[i].image.id);
                                assert_eq!(lease.generation(), slots[i].image.generation);
                                assert_eq!(
                                    extent.offset(),
                                    i as u64 * 1024 + slots[i].image.used_bytes
                                );
                                let slot = &mut slots[i];
                                slot.image.used_bytes += length;
                                slot.image.pinned = pinned;
                                slot.image.state = if slot.image.used_bytes == 1024 {
                                    SegmentState::Sealed
                                } else {
                                    SegmentState::Open
                                };
                                slot.pending += usize::from(pinned);
                                leases.push((lease, pinned));
                            }
                        }
                    }
                }
                2 => {
                    if !leases.is_empty() {
                        let pick = rng.below(leases.len() as u64) as usize;
                        let (lease, pinned) = leases.swap_remove(pick);
                        let slot = &mut slots[lease.id().0 as usize];
                        if pinned {
                            slot.pending -= 1;
                            if slot.pending == 0 && !slot.published && !slot.charged {
                                slot.image.pinned = false;
                                if slot.image.state == SegmentState::Open {
                                    slot.image.state = SegmentState::Sealed;
                                }
                            }
                        }
                        drop(lease);
                    }
                }
                3 => {
                    let slot = &mut slots[position];
                    let result = s.pin_segment(id);
                    if matches!(
                        slot.image.state,
                        SegmentState::Free | SegmentState::Evicting
                    ) {
                        assert_eq!(result, Err(Error::Corrupt));
                    } else {
                        result.unwrap();
                        if slot.image.state == SegmentState::Open && !slot.image.pinned {
                            slot.charged = true;
                        } else {
                            slot.image.pinned = true;
                            slot.published = true;
                        }
                    }
                }
                4 => {
                    let result = s.clear_pinned(id);
                    if frozen.is_some() {
                        assert_eq!(result, Err(Error::Busy));
                    } else {
                        result.unwrap();
                        let slot = &mut slots[position];
                        slot.published = false;
                        slot.charged = false;
                        if slot.pending == 0 {
                            if slot.image.pinned && slot.image.state == SegmentState::Open {
                                slot.image.state = SegmentState::Sealed;
                            }
                            slot.image.pinned = false;
                        }
                    }
                }
                5 => {
                    let result = s.freeze();
                    if frozen.is_some() {
                        assert_eq!(result.err(), Some(Error::Busy));
                    } else {
                        frozen = Some(result.unwrap());
                        appends_frozen = true;
                    }
                }
                6 => {
                    if let Some(guard) = &frozen {
                        guard.thaw_appends();
                        appends_frozen = false;
                    }
                }
                7 => {
                    frozen = None;
                    appends_frozen = false;
                }
                8 => {
                    let slot = &mut slots[position];
                    let expected = if frozen.is_some()
                        || slot.pinned()
                        || !matches!(
                            slot.image.state,
                            SegmentState::Sealed | SegmentState::Evicting
                        ) {
                        Err(Error::Busy)
                    } else {
                        slot.image.state = SegmentState::Evicting;
                        Ok(())
                    };
                    assert_eq!(s.begin_evict(id), expected);
                }
                9 => {
                    let slot = &mut slots[position];
                    let expected = if frozen.is_some()
                        || slot.image.state != SegmentState::Evicting
                        || leases.iter().any(|(lease, _)| lease.id() == id)
                    {
                        Err(Error::Busy)
                    } else {
                        slot.image.state = SegmentState::Free;
                        slot.image.generation.0 += 1;
                        slot.image.used_bytes = 0;
                        slot.image.pinned = false;
                        Ok(())
                    };
                    assert_eq!(s.recycle(id), expected);
                }
                10 => {
                    let expected = if frozen.is_some() || !leases.is_empty() {
                        Err(Error::Busy)
                    } else {
                        for slot in &mut slots {
                            if slot.image.state == SegmentState::Open {
                                slot.image.state = SegmentState::Sealed;
                            }
                            slot.pending = 0;
                            slot.published = slot.image.pinned;
                            slot.charged = false;
                        }
                        Ok(())
                    };
                    assert_eq!(s.restore(before.clone()), expected);
                }
                _ => {
                    let slot = &slots[position];
                    let result = s.lease(id, slot.image.generation);
                    if matches!(slot.image.state, SegmentState::Open | SegmentState::Sealed) {
                        leases.push((result.unwrap(), false));
                    } else {
                        assert_eq!(result.err(), Some(Error::Stale));
                    }
                }
            }
            let expected: Vec<_> = slots.iter().map(|slot| slot.image.clone()).collect();
            assert_eq!(s.snapshot(), expected);
            if expected != before {
                assert_ne!(
                    s.generation(),
                    generation,
                    "snapshot changed without a generation bump"
                );
            }
            assert_eq!(
                s.pinned_count(),
                slots.iter().filter(|slot| slot.pinned()).count()
            );
            assert_eq!(
                s.free_count(),
                slots
                    .iter()
                    .filter(|slot| slot.image.state == SegmentState::Free)
                    .count()
            );
            for slot in &slots {
                assert_eq!(s.pinned(slot.image.id), Ok(slot.pinned()));
                assert_ne!(slot.image.generation, Generation(0));
            }
        }
    }
    if count > 1 {
        assert!(coverage.iter().all(|count| *count > 0));
    }
}
