use super::*;
use crate::read::{PAGE_ATTEMPTS, PAGE_LINKS, SELECTIONS};

struct Harness {
    window: ReadWindow,
    ids: EffectIds,
    now: Instant,
    facts: Facts,
    credits: usize,
}

fn config(end: u64, window_pages: usize) -> WindowConfig {
    WindowConfig {
        start: 0,
        end,
        window_pages,
        sealed: false,
        passthrough: false,
        spill: 0,
        budget: PageBudget::Client {
            deadline: Instant::now() + Duration::from_secs(60),
        },
    }
}

impl Harness {
    fn new(config: WindowConfig) -> Self {
        Self {
            window: ReadWindow::new(config),
            ids: EffectIds::new(),
            now: Instant::now(),
            facts: Facts {
                leased: true,
                spare: 0,
                ..Facts::default()
            },
            credits: usize::MAX,
        }
    }

    fn pump(&mut self) -> Vec<Effect> {
        let mut out = Vec::new();
        let credits = &mut self.credits;
        self.window.pump(
            self.now,
            &self.facts,
            |_| {
                if *credits == 0 {
                    return false;
                }
                *credits -= 1;
                true
            },
            &mut self.ids,
            &mut out,
        );
        out
    }

    fn on(&mut self, input: Input) -> Vec<Effect> {
        self.on_scope(input, true)
    }

    fn on_scope(&mut self, input: Input, live: bool) -> Vec<Effect> {
        let mut out = Vec::new();
        self.window
            .on(input, live, &mut self.ids, &mut out)
            .expect("accepted");
        out
    }

    fn next(&mut self) -> (Next, Vec<Effect>) {
        let mut out = Vec::new();
        (self.window.next(&mut out), out)
    }

    /// Answer every probe in `effects` with a miss.
    fn miss_all(&mut self, effects: &[Effect]) {
        for effect in effects {
            if let Effect::Probe { id, .. } = effect {
                self.on(Input::Probed {
                    id: *id,
                    found: false,
                });
            }
        }
    }

    fn kinds(&self) -> Vec<SlotKind> {
        self.window.slots().map(Slot::kind).collect()
    }
}

fn probes(effects: &[Effect]) -> Vec<u64> {
    effects
        .iter()
        .filter_map(|e| match e {
            Effect::Probe { page, .. } => Some(*page),
            _ => None,
        })
        .collect()
}

fn fetches(effects: &[Effect]) -> Vec<(u64, EffectId)> {
    effects
        .iter()
        .filter_map(|e| match e {
            Effect::Fetch { page, id, .. } => Some((*page, *id)),
            _ => None,
        })
        .collect()
}

fn selects(effects: &[Effect]) -> Vec<(EffectId, BTreeSet<u64>)> {
    effects
        .iter()
        .filter_map(|e| match e {
            Effect::Select { id, pages, .. } => Some((*id, pages.clone())),
            _ => None,
        })
        .collect()
}

#[test]
fn refill_takes_one_credit_per_page_and_stops_at_the_window() {
    let mut h = Harness::new(config(10, 4));
    h.credits = 3;
    assert_eq!(probes(&h.pump()), [0, 1, 2]);
    h.credits = 5;
    assert_eq!(probes(&h.pump()), [3]);
    // The range end bounds the window too.
    let mut h = Harness::new(config(2, 4));
    assert_eq!(probes(&h.pump()), [0, 1]);
}

#[test]
fn found_pages_deliver_in_order() {
    let mut h = Harness::new(config(3, 3));
    let effects = h.pump();
    let ids: Vec<_> = effects
        .iter()
        .map(|e| match e {
            Effect::Probe { id, .. } => *id,
            _ => unreachable!(),
        })
        .collect();
    // Page 1 is found first; nothing is delivered until page 0.
    h.on(Input::Probed {
        id: ids[1],
        found: true,
    });
    assert_eq!(h.next().0, Next::Wait);
    h.on(Input::Probed {
        id: ids[0],
        found: true,
    });
    assert_eq!(
        h.next().0,
        Next::Page {
            page: 0,
            handle: ids[0],
            sealed: false
        }
    );
    assert_eq!(
        h.next().0,
        Next::Page {
            page: 1,
            handle: ids[1],
            sealed: false
        }
    );
    assert_eq!(h.next().0, Next::Wait);
    h.on(Input::Probed {
        id: ids[2],
        found: true,
    });
    assert!(matches!(h.next().0, Next::Page { page: 2, .. }));
    assert_eq!(h.next().0, Next::End);
}

#[test]
fn misses_fill_the_head_directly_and_select_the_rest() {
    let mut h = Harness::new(config(8, 6));
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    assert_eq!(
        fetches(&effects).iter().map(|f| f.0).collect::<Vec<_>>(),
        [0, 1],
        "head plus DIRECT_AHEAD"
    );
    let selections = selects(&effects);
    assert_eq!(
        selections.len(),
        1,
        "only the first selection without spare flights"
    );
    assert_eq!(selections[0].1, (2..6).collect());
    let budget = effects.iter().find_map(|e| match e {
        Effect::Fetch { budget, .. } => Some(*budget),
        _ => None,
    });
    assert_eq!(
        budget.map(|b| (b.attempts, b.links)),
        Some((PAGE_ATTEMPTS, PAGE_LINKS))
    );
    assert!(h.pump().is_empty(), "nothing more to plan");
}

#[test]
fn spare_flights_split_the_selection_demand() {
    let mut h = Harness::new(config(10, 10));
    h.facts.spare = 3;
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    let selections = selects(&effects);
    assert_eq!(selections.len(), SELECTIONS);
    let all: BTreeSet<u64> = selections.iter().flat_map(|s| s.1.clone()).collect();
    assert_eq!(all, (2..10).collect(), "disjoint shares cover every hole");
}

#[test]
fn pages_busy_elsewhere_are_left_to_the_other_window_except_the_head() {
    let mut h = Harness::new(config(4, 4));
    h.facts.elsewhere = [0, 1, 2].into();
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    assert_eq!(
        fetches(&effects).iter().map(|f| f.0).collect::<Vec<_>>(),
        [0, 3]
    );
    assert!(selects(&effects).is_empty());
}

#[test]
fn without_a_lease_or_in_passthrough_and_sealed_modes_nothing_is_selected() {
    let mut h = Harness::new(config(4, 4));
    h.facts.leased = false;
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    assert_eq!(fetches(&effects).len(), 2);
    assert!(selects(&effects).is_empty());

    for (sealed, passthrough) in [(true, false), (false, true)] {
        let mut h = Harness::new(WindowConfig {
            sealed,
            passthrough,
            ..config(4, 4)
        });
        let effects = h.pump();
        h.miss_all(&effects);
        let effects = h.pump();
        assert_eq!(
            fetches(&effects).len(),
            4,
            "the whole window is filled directly"
        );
        assert!(selects(&effects).is_empty());
        assert!(
            effects
                .iter()
                .all(|e| !matches!(e, Effect::Fetch { sealed: s, .. } if *s != sealed))
        );
    }
}

/// The fill restart number and its backoff, from the fetch for page 0.
fn retry_of(effects: &[Effect]) -> Option<(EffectId, u8)> {
    effects.iter().find_map(|e| match e {
        Effect::Fetch {
            page: 0, id, retry, ..
        } => Some((*id, *retry)),
        _ => None,
    })
}

#[test]
fn unavailable_retries_up_to_the_page_limit_with_backoff() {
    let mut h = Harness::new(config(1, 1));
    let effects = h.pump();
    h.miss_all(&effects);
    for round in 0..=PAGE_RETRIES {
        let effects = h.pump();
        let Some((id, retry)) = retry_of(&effects) else {
            panic!("round {round}: {effects:?}");
        };
        // Every restart reports how many came before it, for the backoff.
        assert_eq!(retry, round, "round {round}");
        // Only the last attempt's `Unavailable` is not restarted.
        assert_eq!(
            h.window.restarts_unavailable(0),
            round < PAGE_RETRIES,
            "round {round}"
        );
        h.on(Input::Fetched {
            id,
            result: Err(Error::Unavailable),
        });
    }
    assert_eq!(h.kinds(), [SlotKind::Failed]);
    assert!(h.pump().is_empty());
    assert!(matches!(h.next().0, Next::Fail(Error::Unavailable)));
}

/// Local pressure waits for other reads to release memory: it backs off and
/// retries past the `Unavailable` cap, and still counts toward the backoff.
#[test]
fn local_pressure_retries_without_the_page_limit() {
    let mut h = Harness::new(config(1, 1));
    let effects = h.pump();
    h.miss_all(&effects);
    let rounds = 4 * u16::from(PAGE_RETRIES);
    for round in 0..rounds {
        let (id, retry) = retry_of(&h.pump()).expect("restarted");
        assert_eq!(u16::from(retry), round.min(255));
        let error = if round % 2 == 0 {
            Error::Overloaded
        } else {
            Error::FlightFull
        };
        h.on(Input::Fetched {
            id,
            result: Err(error),
        });
        assert_eq!(h.kinds(), [SlotKind::Missing { select: false }]);
    }
    // `Unavailable` keeps its own full allowance after local pressure.
    for _ in 0..PAGE_RETRIES {
        let (id, _) = retry_of(&h.pump()).expect("restarted");
        h.on(Input::Fetched {
            id,
            result: Err(Error::Unavailable),
        });
        assert_eq!(h.kinds(), [SlotKind::Missing { select: false }]);
    }
    let (id, _) = retry_of(&h.pump()).expect("restarted");
    h.on(Input::Fetched {
        id,
        result: Err(Error::Unavailable),
    });
    assert_eq!(h.kinds(), [SlotKind::Failed]);
}

/// A dead scope ends local-pressure retries at once.
#[test]
fn local_pressure_does_not_retry_after_the_scope_ends() {
    let mut h = Harness::new(config(1, 1));
    let effects = h.pump();
    h.miss_all(&effects);
    let (id, retry) = retry_of(&h.pump()).expect("fetched");
    assert_eq!(retry, 0);
    h.on_scope(
        Input::Fetched {
            id,
            result: Err(Error::Overloaded),
        },
        false,
    );
    assert_eq!(h.kinds(), [SlotKind::Failed]);
    assert!(matches!(h.next().0, Next::Fail(Error::Overloaded)));
}

#[test]
fn retry_delay_doubles_from_the_minimum_to_the_cap() {
    assert_eq!(retry_delay(0), Duration::ZERO);
    assert_eq!(retry_delay(1), RETRY_DELAY_MIN);
    assert_eq!(retry_delay(2), 2 * RETRY_DELAY_MIN);
    assert_eq!(retry_delay(3), 4 * RETRY_DELAY_MIN);
    assert_eq!(retry_delay(8), RETRY_DELAY_MAX.min(128 * RETRY_DELAY_MIN));
    assert_eq!(retry_delay(9), RETRY_DELAY_MAX);
    assert_eq!(retry_delay(u8::MAX), RETRY_DELAY_MAX);
    for retry in 1..u8::MAX {
        assert!(retry_delay(retry) <= retry_delay(retry + 1));
    }
}

#[test]
fn sealed_fills_share_the_retry_policy_and_other_errors_are_final() {
    let mut h = Harness::new(WindowConfig {
        sealed: true,
        ..config(2, 2)
    });
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    let f = fetches(&effects);
    h.on(Input::Fetched {
        id: f[0].1,
        result: Err(Error::Overloaded),
    });
    h.on(Input::Fetched {
        id: f[1].1,
        result: Err(Error::CorruptRecord),
    });
    assert_eq!(
        h.kinds(),
        [SlotKind::Missing { select: false }, SlotKind::Failed]
    );
    // An ended scope does not retry.
    let effects = h.pump();
    let f = fetches(&effects);
    assert_eq!(f.len(), 1);
    h.on_scope(
        Input::Fetched {
            id: f[0].1,
            result: Err(Error::Overloaded),
        },
        false,
    );
    assert_eq!(h.kinds(), [SlotKind::Failed, SlotKind::Failed]);
}

#[test]
fn an_error_reaches_the_client_only_at_the_head() {
    let mut h = Harness::new(config(4, 4));
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    let f = fetches(&effects);
    let (_, selection) = selects(&effects)[0].clone();
    // Page 1 fails while page 0 is still being fetched.
    h.on(Input::Fetched {
        id: f[1].1,
        result: Err(Error::CorruptRecord),
    });
    assert_eq!(h.next().0, Next::Wait);
    // Admission stops at the failure: the selection's pages do not come back as new work.
    h.on(Input::Selected {
        id: selects(&effects)[0].0,
        ok: false,
        started: true,
    });
    assert!(selection.iter().all(|p| *p > 1));
    assert!(h.pump().is_empty());
    h.on(Input::Fetched {
        id: f[0].1,
        result: Ok(Fetched::Page),
    });
    assert!(matches!(h.next().0, Next::Page { page: 0, .. }));
    let (next, _) = h.next();
    assert_eq!(next, Next::Fail(Error::CorruptRecord));
    // Terminal: it stays failed.
    assert_eq!(h.next().0, Next::Fail(Error::CorruptRecord));
}

#[test]
fn selection_outcomes_decide_how_pages_are_filled_again() {
    for (ok, started, select) in [
        (true, true, true),
        (false, true, false),
        (true, false, false),
    ] {
        let mut h = Harness::new(config(4, 4));
        let effects = h.pump();
        h.miss_all(&effects);
        let effects = h.pump();
        let (id, _) = selects(&effects)[0].clone();
        h.on(Input::Selected { id, ok, started });
        assert_eq!(
            h.kinds()[2..],
            [SlotKind::Missing { select }, SlotKind::Missing { select }]
        );
    }
}

#[test]
fn offered_pages_fill_wanted_slots_and_extras_are_discarded() {
    let mut h = Harness::new(config(4, 4));
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    let f = fetches(&effects);
    let offer = EffectId(1000);
    assert!(
        h.on(Input::Offered {
            page: 3,
            handle: offer
        })
        .is_empty()
    );
    assert_eq!(h.kinds()[3], SlotKind::Ready);
    // Page 0 is being fetched directly: the offer is not taken.
    let extra = EffectId(1001);
    assert_eq!(
        h.on(Input::Offered {
            page: 0,
            handle: extra
        }),
        [Effect::Discard(extra)]
    );
    // Out of the window.
    let far = EffectId(1002);
    assert_eq!(
        h.on(Input::Offered {
            page: 9,
            handle: far
        }),
        [Effect::Discard(far)]
    );
    let _ = f;
}

#[test]
fn stale_completions_are_refused() {
    let mut h = Harness::new(config(2, 2));
    h.pump();
    let mut out = Vec::new();
    let bogus = EffectId(999);
    for input in [
        Input::Probed {
            id: bogus,
            found: true,
        },
        Input::Fetched {
            id: bogus,
            result: Ok(Fetched::Page),
        },
        Input::Selected {
            id: bogus,
            ok: true,
            started: true,
        },
        Input::SpillFilled {
            id: bogus,
            ok: true,
        },
    ] {
        assert_eq!(
            h.window.on(input, true, &mut h.ids, &mut out),
            Err(Stale(bogus))
        );
    }
    assert!(out.is_empty());
}

#[test]
fn a_passed_client_deadline_fails_the_read() {
    let mut h = Harness::new(WindowConfig {
        budget: PageBudget::Client {
            deadline: Instant::now(),
        },
        ..config(2, 2)
    });
    h.now += Duration::from_secs(1);
    let effects = h.pump();
    h.miss_all(&effects);
    assert!(fetches(&h.pump()).is_empty());
    assert_eq!(h.next().0, Next::Fail(Error::DeadlineExceeded));
}

#[test]
fn progressing_pages_get_their_own_deadline() {
    let mut h = Harness::new(WindowConfig {
        budget: PageBudget::Progressing {
            timeout: Duration::from_secs(5),
        },
        ..config(1, 1)
    });
    let effects = h.pump();
    h.miss_all(&effects);
    let effects = h.pump();
    let deadline = effects.iter().find_map(|e| match e {
        Effect::Fetch { budget, .. } => Some(budget.deadline),
        _ => None,
    });
    assert_eq!(deadline, Some(h.now + Duration::from_secs(5)));
}

fn spill_config() -> WindowConfig {
    WindowConfig {
        spill: 2,
        ..config(6, 2)
    }
}

fn holds(effects: &[Effect]) -> Vec<(u64, EffectId)> {
    effects
        .iter()
        .filter_map(|e| match e {
            Effect::Hold { page, id, .. } => Some((*page, *id)),
            _ => None,
        })
        .collect()
}

fn releases(effects: &[Effect]) -> Vec<(u64, Epoch)> {
    effects
        .iter()
        .flat_map(|e| match e {
            Effect::Release(pages) => pages.clone(),
            _ => Vec::new(),
        })
        .collect()
}

#[test]
fn spilled_pages_are_read_back_and_released_once_on_delivery() {
    let mut h = Harness::new(spill_config());
    let effects = h.pump();
    assert_eq!(probes(&effects), [0, 1]);
    let held = holds(&effects);
    assert_eq!(held.iter().map(|h| h.0).collect::<Vec<_>>(), [2, 3]);
    let fill = h.on(Input::Held {
        id: held[0].1,
        result: Ok(7),
    });
    let [Effect::SpillFill { id: fill, page: 2 }] = fill[..] else {
        panic!("{fill:?}");
    };
    h.on(Input::SpillFilled { id: fill, ok: true });
    // Deliver pages 0 and 1 so page 2 enters the window.
    for effect in &effects {
        if let Effect::Probe { id, .. } = effect {
            h.on(Input::Probed {
                id: *id,
                found: true,
            });
        }
    }
    assert!(releases(&h.next().1).is_empty());
    assert!(releases(&h.next().1).is_empty());
    let effects = h.pump();
    assert!(effects.iter().any(|e| matches!(
        e,
        Effect::Probe {
            page: 2,
            from: ProbeFrom::Held { after: None },
            ..
        }
    )));
    let probe = effects
        .iter()
        .find_map(|e| match e {
            Effect::Probe { page: 2, id, .. } => Some(*id),
            _ => None,
        })
        .unwrap();
    h.on(Input::Probed {
        id: probe,
        found: true,
    });
    let (next, out) = h.next();
    assert!(matches!(next, Next::Page { page: 2, .. }));
    assert_eq!(releases(&out), [(2, 7)]);
    // Ending the read releases nothing twice.
    let mut out = Vec::new();
    h.window.terminate(&mut out);
    assert!(!releases(&out).contains(&(2, 7)));
}

#[test]
fn a_hold_the_window_overtook_is_released_at_once() {
    let mut h = Harness::new(spill_config());
    let effects = h.pump();
    let held = holds(&effects);
    // Pages 0 and 1 deliver and pages 2, 3 enter the window before the hold completes.
    for effect in &effects {
        if let Effect::Probe { id, .. } = effect {
            h.on(Input::Probed {
                id: *id,
                found: true,
            });
        }
    }
    h.next();
    h.next();
    let effects = h.pump();
    assert!(probes(&effects).contains(&2));
    let out = h.on(Input::Held {
        id: held[0].1,
        result: Ok(3),
    });
    assert_eq!(releases(&out), [(2, 3)]);
}

#[test]
fn a_failed_spill_fill_releases_its_hold_and_stops_spilling() {
    let mut h = Harness::new(spill_config());
    let effects = h.pump();
    let held = holds(&effects);
    let out = h.on(Input::Held {
        id: held[0].1,
        result: Ok(1),
    });
    let [Effect::SpillFill { id: fill, .. }] = out[..] else {
        panic!()
    };
    let out = h.on(Input::SpillFilled {
        id: fill,
        ok: false,
    });
    assert_eq!(releases(&out), [(2, 1)]);
    assert!(holds(&h.pump()).is_empty());
    // A refused hold stops spilling too.
    let mut h = Harness::new(spill_config());
    let held = holds(&h.pump());
    h.on(Input::Held {
        id: held[0].1,
        result: Err(Error::Overloaded),
    });
    assert!(holds(&h.pump()).is_empty());
}

#[test]
fn terminate_releases_held_pages_and_pending_holds_on_completion() {
    let mut h = Harness::new(spill_config());
    let held = holds(&h.pump());
    h.on(Input::Held {
        id: held[0].1,
        result: Ok(5),
    });
    let mut out = Vec::new();
    h.window.terminate(&mut out);
    assert_eq!(releases(&out), [(2, 5)]);
    // The pending hold completes after the read ended: released then, exactly once.
    let out = h.on(Input::Held {
        id: held[1].1,
        result: Ok(6),
    });
    assert_eq!(releases(&out), [(3, 6)]);
    let mut out = Vec::new();
    assert!(
        h.window
            .on(
                Input::Held {
                    id: held[1].1,
                    result: Ok(6)
                },
                true,
                &mut h.ids,
                &mut out
            )
            .is_err()
    );
}

#[test]
fn cancel_discards_ready_pages_and_late_fills() {
    let mut h = Harness::new(config(2, 2));
    let effects = h.pump();
    let ids: Vec<_> = effects
        .iter()
        .filter_map(|e| match e {
            Effect::Probe { id, .. } => Some(*id),
            _ => None,
        })
        .collect();
    h.on(Input::Probed {
        id: ids[0],
        found: true,
    });
    let out = h.on(Input::Cancel(Error::Canceled));
    assert_eq!(out, [Effect::Discard(ids[0])]);
    assert_eq!(
        h.on(Input::Probed {
            id: ids[1],
            found: true
        }),
        [Effect::Discard(ids[1])]
    );
    assert_eq!(h.next().0, Next::Fail(Error::Canceled));
    assert!(h.pump().is_empty());
}

/// Seeded random completions: pages deliver in order, each hold epoch is released exactly once
/// over the read, and the read always ends.
#[test]
fn seeded_reads_deliver_in_order_and_release_each_hold_once() {
    let mut seed = 0x2545_f491_4f6c_dd1du64;
    let mut rand = move || {
        seed ^= seed << 13;
        seed ^= seed >> 7;
        seed ^= seed << 17;
        seed
    };
    for _ in 0..300 {
        let end = 1 + rand() % 12;
        let mut h = Harness::new(WindowConfig {
            spill: (rand() % 3) as usize,
            sealed: rand() % 5 == 0,
            ..config(end, 1 + (rand() % 4) as usize)
        });
        h.facts.spare = (rand() % 3) as usize;
        let mut outstanding: Vec<Effect> = Vec::new();
        let mut epochs = 0u64;
        let mut granted: BTreeMap<u64, u64> = BTreeMap::new();
        let mut released: Vec<(u64, Epoch)> = Vec::new();
        let mut delivered = Vec::new();
        let mut ended = false;
        for _ in 0..2000 {
            let effects = h.pump();
            released.extend(releases(&effects));
            outstanding.extend(
                effects
                    .into_iter()
                    .filter(|e| !matches!(e, Effect::Release(_) | Effect::Discard(_))),
            );
            let (next, out) = h.next();
            released.extend(releases(&out));
            match next {
                Next::Page { page, .. } => delivered.push(page),
                Next::End | Next::Fail(_) => {
                    ended = true;
                    break;
                }
                Next::Wait => {}
            }
            if outstanding.is_empty() {
                continue;
            }
            let effect = outstanding.swap_remove((rand() as usize) % outstanding.len());
            let pick = rand();
            let input = match effect {
                Effect::Probe { id, .. } => Input::Probed {
                    id,
                    found: pick % 3 == 0,
                },
                Effect::Fetch { id, .. } => Input::Fetched {
                    id,
                    result: match pick % 5 {
                        0 => Err(Error::Overloaded),
                        1 if pick % 7 == 0 => Err(Error::Io),
                        _ => Ok(Fetched::Page),
                    },
                },
                Effect::Select { id, .. } => Input::Selected {
                    id,
                    ok: pick % 2 == 0,
                    started: pick % 3 != 0,
                },
                Effect::Hold { id, page, .. } => {
                    if pick % 4 == 0 {
                        Input::Held {
                            id,
                            result: Err(Error::Overloaded),
                        }
                    } else {
                        epochs += 1;
                        granted.insert(page, epochs);
                        Input::Held {
                            id,
                            result: Ok(epochs),
                        }
                    }
                }
                Effect::SpillFill { id, .. } => Input::SpillFilled {
                    id,
                    ok: pick % 4 != 0,
                },
                Effect::Release(_) | Effect::Discard(_) => unreachable!(),
            };
            let mut out = Vec::new();
            let _ = h.window.on(input, true, &mut h.ids, &mut out);
            released.extend(releases(&out));
            outstanding.extend(
                out.into_iter()
                    .filter(|e| matches!(e, Effect::SpillFill { .. })),
            );
        }
        assert!(ended, "the read ends");
        // Finish: terminate, then complete everything still outstanding.
        let mut out = Vec::new();
        h.window.terminate(&mut out);
        released.extend(releases(&out));
        for effect in outstanding {
            let mut out = Vec::new();
            let input = match effect {
                Effect::Hold { id, page, .. } => {
                    epochs += 1;
                    granted.insert(page, epochs);
                    Input::Held {
                        id,
                        result: Ok(epochs),
                    }
                }
                Effect::SpillFill { id, .. } => Input::SpillFilled { id, ok: true },
                _ => continue,
            };
            let _ = h.window.on(input, true, &mut h.ids, &mut out);
            released.extend(releases(&out));
        }
        let expected: Vec<_> = (0..delivered.len() as u64).collect();
        assert_eq!(delivered, expected, "in order, no gaps");
        let mut seen = BTreeSet::new();
        for release in &released {
            assert!(seen.insert(*release), "released twice: {release:?}");
            assert_eq!(granted.get(&release.0), Some(&release.1));
        }
        for (page, epoch) in granted {
            assert!(
                seen.contains(&(page, epoch)),
                "never released: {page} {epoch}"
            );
        }
    }
}

#[test]
fn index_of_tracks_probes_and_fetches_and_forgets_completions() {
    let mut h = Harness::new(config(3, 3));
    let effects = h.pump();
    let ids: Vec<_> = effects
        .iter()
        .filter_map(|e| match e {
            Effect::Probe { id, .. } => Some(*id),
            _ => None,
        })
        .collect();
    for (index, id) in ids.iter().enumerate() {
        assert_eq!(h.window.index_of(*id), Some(index));
    }
    assert_eq!(h.window.index_of(EffectId(999)), None);
    // A miss turns into a direct fill at the same index.
    let effects = h.on(Input::Probed {
        id: ids[0],
        found: false,
    });
    assert_eq!(h.window.index_of(ids[0]), None);
    let effects = [effects, h.pump()].concat();
    let fill = fetches(&effects);
    assert_eq!(fill[0].0, 0);
    assert_eq!(h.window.index_of(fill[0].1), Some(0));
    // A repeated completion is stale.
    h.on(Input::Probed {
        id: ids[1],
        found: true,
    });
    let mut out = Vec::new();
    assert_eq!(
        h.window.on(
            Input::Probed {
                id: ids[1],
                found: true
            },
            true,
            &mut h.ids,
            &mut out
        ),
        Err(Stale(ids[1]))
    );
}
