//! Cost models for control thread hot spots, for the simulator.

/// Work done by one `exec::Signal::notify` with `waiters` tasks waiting.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Notify {
    /// Tasks woken and polled.
    pub wakes: u64,
    /// `Waker::will_wake` comparisons when every woken task waits again.
    /// `Signal::changed` scans the waker list before pushing, so the k-th
    /// re-registration compares against k-1 entries: W(W-1)/2 in total.
    pub comparisons: u64,
}

/// Cost of one notify when all `waiters` re-register (no new data for them).
pub fn notify(waiters: u64) -> Notify {
    Notify {
        wakes: waiters,
        comparisons: waiters.saturating_mul(waiters.saturating_sub(1)) / 2,
    }
}

/// Write rounds a seat needs to answer `polls` waiting long polls with
/// `write_slots` concurrent body writes, when every poll wants the new body.
/// Polls beyond the free slots answer 429 and retry.
pub fn write_rounds(polls: u64, write_slots: u64) -> u64 {
    polls.div_ceil(write_slots.max(1))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        exec::{Executor, Signal},
        limits,
    };
    use std::{cell::Cell, rc::Rc};

    #[test]
    fn quadratic_model() {
        assert_eq!(
            notify(0),
            Notify {
                wakes: 0,
                comparisons: 0
            }
        );
        assert_eq!(notify(1).comparisons, 0);
        assert_eq!(notify(4).comparisons, 6);
        assert_eq!(notify(12_500).comparisons, 78_118_750);
        assert_eq!(write_rounds(12_500, limits::MAX_WRITES as u64), 196);
        assert_eq!(write_rounds(0, 64), 0);
        assert_eq!(write_rounds(5, 0), 5);
    }

    /// The model's wake count matches the real Signal: every waiter is
    /// polled once per notify.
    #[test]
    fn signal_wakes_every_waiter() {
        let mut ex = Executor::new(64).unwrap();
        let signal = Signal::default();
        let woken = Rc::new(Cell::new(0u64));
        ex.block_on(async {
            for _ in 0..8 {
                let s = signal.clone();
                let w = woken.clone();
                crate::exec::spawn(async move {
                    let g = s.generation();
                    s.changed(g).await;
                    w.set(w.get() + 1);
                })
                .unwrap();
            }
            crate::exec::sleep(std::time::Duration::from_millis(5)).await;
            signal.notify();
            crate::exec::sleep(std::time::Duration::from_millis(5)).await;
        });
        assert_eq!(woken.get(), notify(8).wakes);
    }
}
