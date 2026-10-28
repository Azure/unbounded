//! Cost models for control thread hot spots, for the simulator.

/// Work done by one `exec::Signal::notify` with `waiters` tasks waiting.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Notify {
    /// Tasks woken and polled.
    pub wakes: u64,
    /// Waker slab operations when every woken task waits again: one O(1)
    /// insert each (`exec::Changed` owns its slab entry), so O(W) in total.
    /// Before round 3 each registration scanned the list with `will_wake`,
    /// W(W-1)/2 comparisons per notify.
    pub registrations: u64,
}

/// Cost of one notify when all `waiters` re-register (no new data for them).
pub fn notify(waiters: u64) -> Notify {
    Notify {
        wakes: waiters,
        registrations: waiters,
    }
}

/// Write rounds a seat needs to answer `polls` waiting long polls with
/// `write_slots` concurrent body writes, when every poll wants the new body.
pub fn write_rounds(polls: u64, write_slots: u64) -> u64 {
    polls.div_ceil(write_slots.max(1))
}

/// Outcome of a mass wake drained through the write queue.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Drain {
    /// Polls answered with the new body.
    pub replies: u64,
    /// Polls whose queue wait ran out first (429).
    pub rejected: u64,
    /// When the last reply finished, from the wake.
    pub last_ns: u64,
}

/// `polls` woken together, `write_slots` concurrent writes of `write_ns`
/// each, FIFO queue (`step::admission::WriteQueue`), each poll willing to
/// wait `wait_ns` for a slot and at most `max_waiting` queued. Round k
/// starts at `k * write_ns`.
pub fn drain(polls: u64, write_slots: u64, write_ns: u64, wait_ns: u64, max_waiting: u64) -> Drain {
    let slots = write_slots.max(1);
    let queued = polls.saturating_sub(slots);
    let overflow = queued.saturating_sub(max_waiting);
    let admitted = polls - overflow;
    // Rounds whose start is within the wait: 0..=wait_ns / write_ns.
    let rounds_in_time = wait_ns
        .checked_div(write_ns)
        .map_or(u64::MAX, |r| r.saturating_add(1));
    let replies = admitted.min(rounds_in_time.saturating_mul(slots));
    Drain {
        replies,
        rejected: polls - replies,
        last_ns: write_rounds(replies, slots).saturating_mul(write_ns),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_cluster_control::limits;

    #[test]
    fn linear_model() {
        assert_eq!(
            notify(0),
            Notify {
                wakes: 0,
                registrations: 0
            }
        );
        assert_eq!(notify(1).registrations, 1);
        assert_eq!(notify(12_500).registrations, 12_500);
        assert_eq!(write_rounds(12_500, limits::MAX_WRITES as u64), 196);
        assert_eq!(write_rounds(0, 64), 0);
        assert_eq!(write_rounds(5, 0), 5);
    }

    #[test]
    fn drain_queues_within_the_wait() {
        // 3125 polls, 64 slots, 2 ms writes, 5 s wait: 49 rounds, no 429.
        let d = drain(3125, 64, 2_000_000, 5_000_000_000, 16_384);
        assert_eq!((d.replies, d.rejected, d.last_ns), (3125, 0, 98_000_000));
        // Wait shorter than the drain: rounds starting after it are 429.
        let d = drain(1000, 10, 1_000, 9_999, 16_384);
        assert_eq!((d.replies, d.rejected), (100, 900));
        // Queue bound: beyond slots + max_waiting is 429 at once.
        let d = drain(100, 10, 1, u64::MAX, 20);
        assert_eq!((d.replies, d.rejected), (30, 70));
        // Zero write time: everything admitted is answered.
        assert_eq!(drain(100, 10, 0, 0, 1000).replies, 100);
        assert_eq!(
            drain(0, 64, 1, 1, 1),
            Drain {
                replies: 0,
                rejected: 0,
                last_ns: 0
            }
        );
    }
}
