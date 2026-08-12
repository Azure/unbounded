//! What a page is allowed to hold, and whether the cluster's answers could have
//! come from a real register.
//!
//! Every mutation the campaign issues carries a fresh token, and the token is
//! stamped through the whole payload. A read therefore identifies exactly which
//! write it saw, and a page assembled out of two different writes, or out of a
//! write and a hole, fails to decode at all. That is the difference between
//! "the first byte looked plausible" and "this is byte for byte the page some
//! client actually wrote".
//!
//! The history checker is Wing and Gong's search, run over one segment of the
//! history at a time. A segment boundary is any moment when the page has no
//! operation in flight: real time then orders everything before the boundary
//! ahead of everything after it, so the segments compose by carrying forward
//! the set of values the page could be holding. That keeps the search small no
//! matter how long the campaign runs.

use std::collections::{BTreeSet, HashSet};
use std::fmt;

/// A small page, and the unit every register operation works in.
pub const BLOCK: usize = 4096;

/// An immutable page. Written once, whole.
pub const HUGE: usize = 4 << 20;

/// The largest segment the exact checker will take on. Segments are bounded by
/// how many operations the generator lets pile up on one page, so crossing this
/// is a bug in the generator rather than something to paper over.
const SEGMENT: usize = 48;

/// How many search steps one segment may take. Generous next to a segment that
/// holds a couple of dozen operations, and small enough that a checker gone
/// quadratic is reported rather than waited on.
const BUDGET: u64 = 2_000_000;

/// What a page holds.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub enum Value {
    /// Never written, or trimmed since.
    Hole,
    /// Holding the payload one particular mutation carried.
    Token(u32),
}

impl fmt::Display for Value {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Value::Hole => write!(f, "a hole"),
            Value::Token(t) => write!(f, "token {t}"),
        }
    }
}

/// How a page answers a mutation.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Class {
    /// Last writer wins, and a trim puts the hole back.
    Lww,
    /// Filled once. A second fill is refused rather than applied.
    Immutable,
}

/// What a client asked for.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Kind {
    /// Put this value in the page. A trim is a write of a hole.
    Write(Value),
    /// Tell me what the page holds.
    Read,
}

/// One client operation, from the moment it was submitted to the moment its
/// result came back.
#[derive(Copy, Clone, Debug)]
pub struct Call {
    /// What was asked.
    pub kind: Kind,
    /// When it was submitted, on a counter that only ever moves forward.
    pub start: u64,
    /// When its result was collected, on the same counter.
    pub end: u64,
    /// True when the client was told it succeeded.
    pub ok: bool,
    /// What a successful read returned.
    pub saw: Option<Value>,
    /// Which node it was submitted to, so a failure names a culprit.
    pub who: usize,
}

impl fmt::Display for Call {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let what = match self.kind {
            Kind::Write(Value::Hole) => "trim".to_string(),
            Kind::Write(v) => format!("write {v}"),
            Kind::Read => "read".to_string(),
        };
        let got = match (self.ok, self.saw) {
            (false, _) => "failed".to_string(),
            (true, Some(v)) => format!("returned {v}"),
            (true, None) => "succeeded".to_string(),
        };

        write!(
            f,
            "{what} on node {} over [{}, {}] {got}",
            self.who, self.start, self.end
        )
    }
}

/// Stamps `v` through `buf`, which must be a whole number of words long.
pub fn stamp(v: Value, buf: &mut [u8]) {
    assert!(
        buf.len() >= 8 && buf.len() % 8 == 0,
        "a page is whole words"
    );

    let t = match v {
        Value::Hole => {
            buf.fill(0);

            return;
        }
        Value::Token(t) => t,
    };

    assert!(t != 0, "token zero would read back as a hole");

    for (w, out) in buf.chunks_exact_mut(8).enumerate() {
        out.copy_from_slice(&word(t, w).to_le_bytes());
    }
}

/// Reads `buf` back. `None` means the bytes are not any value a client ever
/// wrote: torn between two writes, corrupt, or half a hole.
pub fn parse(buf: &[u8]) -> Option<Value> {
    assert!(
        buf.len() >= 8 && buf.len() % 8 == 0,
        "a page is whole words"
    );

    let head = u64::from_le_bytes(buf[..8].try_into().unwrap());
    let t = head as u32;

    if t == 0 {
        return buf.iter().all(|&b| b == 0).then_some(Value::Hole);
    }

    for (w, got) in buf.chunks_exact(8).enumerate() {
        if u64::from_le_bytes(got.try_into().unwrap()) != word(t, w) {
            return None;
        }
    }

    Some(Value::Token(t))
}

/// The word a token puts at word `w`. Word zero carries the token in the clear
/// so a read names its writer in constant time; every word depends on both the
/// token and its offset, so a page built from the right bytes at the wrong
/// offset is caught too.
fn word(t: u32, w: usize) -> u64 {
    if w == 0 {
        return t as u64 | (mix(t, 0) & 0xffff_ffff) << 32;
    }

    mix(t, w)
}

/// Splitmix64 over the token and the word offset.
fn mix(t: u32, w: usize) -> u64 {
    let mut x = (t as u64)
        .wrapping_mul(0x9e37_79b9_7f4a_7c15)
        .wrapping_add(w as u64);

    x = (x ^ x >> 30).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    x = (x ^ x >> 27).wrapping_mul(0x94d0_49bb_1331_11eb);

    x ^ x >> 31
}

/// One page's history, checked as it goes.
pub struct Page {
    /// How this page answers a mutation.
    class: Class,
    /// Every value the page could be holding, given everything checked so far.
    states: BTreeSet<Value>,
    /// Calls since the last moment the page was idle.
    seg: Vec<Call>,
    /// How many calls in `seg` have not come back yet.
    live: usize,
}

impl Page {
    /// A page nobody has touched.
    pub fn new(class: Class) -> Self {
        Self {
            class,
            states: BTreeSet::from([Value::Hole]),
            seg: Vec::new(),
            live: 0,
        }
    }

    /// Records a submission. The returned index names the call to `finish`.
    pub fn begin(&mut self, kind: Kind, start: u64, who: usize) -> usize {
        self.seg.push(Call {
            kind,
            start,
            end: start,
            ok: false,
            saw: None,
            who,
        });
        self.live += 1;

        self.seg.len() - 1
    }

    /// Records a result.
    pub fn finish(&mut self, at: usize, end: u64, ok: bool, saw: Option<Value>) {
        let c = &mut self.seg[at];

        c.end = end;
        c.ok = ok;
        c.saw = saw;
        self.live -= 1;
    }

    /// How many calls are outstanding, which is what the generator throttles.
    pub fn live(&self) -> usize {
        self.live
    }

    /// How much history is waiting for the page to go idle.
    pub fn pending(&self) -> usize {
        self.seg.len()
    }

    /// Every value the page could be holding. Only meaningful once the page has
    /// settled; before that it describes the last settled point.
    pub fn possible(&self) -> &BTreeSet<Value> {
        &self.states
    }

    /// Checks the history accumulated since the page last went idle, and folds
    /// it into the set of values the page could now hold. Does nothing while a
    /// call is still in flight, because real time cannot yet separate what came
    /// before from what comes after.
    pub fn settle(&mut self) -> Result<(), String> {
        if self.live > 0 || self.seg.is_empty() {
            return Ok(());
        }

        if self.seg.len() > SEGMENT {
            return Err(format!(
                "{} operations piled up on one page without it ever going idle, \
                 which the generator is supposed to prevent",
                self.seg.len()
            ));
        }

        let mut next = BTreeSet::new();
        let mut steps = 0u64;

        for &from in &self.states {
            explore(self.class, &self.seg, from, &mut next, &mut steps)?;
        }

        if next.is_empty() {
            return Err(format!(
                "no ordering of these operations explains what the cluster \
                 returned, starting from {}:\n{}",
                describe(&self.states),
                self.seg
                    .iter()
                    .map(|c| format!("  {c}"))
                    .collect::<Vec<_>>()
                    .join("\n")
            ));
        }

        self.states = next;
        self.seg.clear();

        Ok(())
    }
}

/// Every state the page can be left in by some legal ordering of `seg`.
fn explore(
    class: Class,
    seg: &[Call],
    from: Value,
    out: &mut BTreeSet<Value>,
    steps: &mut u64,
) -> Result<(), String> {
    let full = if seg.len() == 64 {
        u64::MAX
    } else {
        (1u64 << seg.len()) - 1
    };
    let mut seen = HashSet::new();
    let mut stack = vec![(0u64, from)];

    while let Some((done, at)) = stack.pop() {
        *steps += 1;

        if *steps > BUDGET {
            return Err(
                "the linearizability search ran out of budget, so the history \
                 is too tangled to decide"
                    .to_string(),
            );
        }

        if done == full {
            out.insert(at);

            continue;
        }

        if !seen.insert((done, at)) {
            continue;
        }

        // A call may go next only when no call still outstanding has already
        // finished before it started.
        let bar = seg
            .iter()
            .enumerate()
            .filter(|(i, _)| done & 1 << i == 0)
            .map(|(_, c)| c.end)
            .min()
            .unwrap();

        for (i, c) in seg.iter().enumerate() {
            if done & 1 << i != 0 || c.start > bar {
                continue;
            }

            for to in step(class, c, at) {
                stack.push((done | 1 << i, to));
            }
        }
    }

    Ok(())
}

/// Where a page can be left after one call, or nowhere if the call could not
/// have returned what it did.
fn step(class: Class, c: &Call, at: Value) -> Vec<Value> {
    match (c.kind, c.ok) {
        // A read has to have seen the page as it stood.
        (Kind::Read, true) => (c.saw == Some(at)).then_some(at).into_iter().collect(),
        // A read that failed saw nothing and says nothing.
        (Kind::Read, false) => vec![at],
        // A write that was acknowledged took effect, and an immutable page can
        // only acknowledge a fill it had room for.
        (Kind::Write(v), true) => match class {
            Class::Immutable if at != Value::Hole => Vec::new(),
            _ => vec![v],
        },
        // A write that failed may still have landed, so both futures are open.
        (Kind::Write(v), false) => match class {
            Class::Immutable if at != Value::Hole => vec![at],
            _ if v == at => vec![at],
            _ => vec![at, v],
        },
    }
}

/// Names a set of values for a failure message.
pub fn describe(vs: &BTreeSet<Value>) -> String {
    let all: Vec<String> = vs.iter().map(|v| v.to_string()).collect();

    match all.len() {
        0 => "nothing".to_string(),
        1 => all[0].clone(),
        _ => format!("any of {}", all.join(", ")),
    }
}

// --- Tests, of the checker itself ---

#[cfg(test)]
mod tests {
    use super::*;

    fn call(kind: Kind, start: u64, end: u64, ok: bool, saw: Option<Value>) -> Call {
        Call {
            kind,
            start,
            end,
            ok,
            saw,
            who: 0,
        }
    }

    fn run(class: Class, calls: &[Call]) -> Result<BTreeSet<Value>, String> {
        let mut p = Page::new(class);

        for c in calls {
            let at = p.begin(c.kind, c.start, c.who);

            p.finish(at, c.end, c.ok, c.saw);
        }

        p.settle()?;

        Ok(p.possible().clone())
    }

    #[test]
    fn a_token_survives_a_round_trip() {
        for len in [BLOCK, HUGE] {
            let mut buf = vec![0u8; len];

            stamp(Value::Token(7), &mut buf);
            assert_eq!(parse(&buf), Some(Value::Token(7)));

            stamp(Value::Hole, &mut buf);
            assert_eq!(parse(&buf), Some(Value::Hole));
        }
    }

    #[test]
    fn a_page_torn_between_two_writes_does_not_parse() {
        let mut a = vec![0u8; BLOCK];
        let mut b = vec![0u8; BLOCK];

        stamp(Value::Token(1), &mut a);
        stamp(Value::Token(2), &mut b);
        a[BLOCK / 2..].copy_from_slice(&b[BLOCK / 2..]);

        assert_eq!(parse(&a), None);
    }

    #[test]
    fn a_page_torn_against_a_hole_does_not_parse() {
        let mut a = vec![0u8; BLOCK];

        stamp(Value::Token(3), &mut a);
        a[..BLOCK / 2].fill(0);
        assert_eq!(parse(&a), None);

        stamp(Value::Token(3), &mut a);
        a[BLOCK / 2..].fill(0);
        assert_eq!(parse(&a), None);
    }

    #[test]
    fn a_single_flipped_byte_does_not_parse() {
        let mut a = vec![0u8; BLOCK];

        stamp(Value::Token(9), &mut a);
        a[17] ^= 0x40;
        assert_eq!(parse(&a), None);
    }

    #[test]
    fn the_right_bytes_at_the_wrong_offset_do_not_parse() {
        let mut a = vec![0u8; HUGE];

        stamp(Value::Token(11), &mut a);
        a.copy_within(3 * BLOCK..4 * BLOCK, BLOCK);
        assert_eq!(parse(&a), None);
    }

    #[test]
    fn a_write_read_back_is_legal() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Read, 2, 3, true, Some(Value::Token(1))),
        ];

        assert_eq!(
            run(Class::Lww, &h).unwrap(),
            BTreeSet::from([Value::Token(1)])
        );
    }

    #[test]
    fn a_read_that_goes_backwards_is_caught() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Write(Value::Token(2)), 2, 3, true, None),
            call(Kind::Read, 4, 5, true, Some(Value::Token(1))),
        ];

        assert!(run(Class::Lww, &h).is_err());
    }

    #[test]
    fn a_read_of_a_value_nobody_wrote_is_caught() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Read, 2, 3, true, Some(Value::Token(8))),
        ];

        assert!(run(Class::Lww, &h).is_err());
    }

    #[test]
    fn a_trim_leaves_a_hole() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Write(Value::Hole), 2, 3, true, None),
            call(Kind::Read, 4, 5, true, Some(Value::Hole)),
        ];

        assert_eq!(run(Class::Lww, &h).unwrap(), BTreeSet::from([Value::Hole]));
    }

    #[test]
    fn a_read_may_see_either_of_two_concurrent_writes() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 9, true, None),
            call(Kind::Write(Value::Token(2)), 1, 9, true, None),
            call(Kind::Read, 2, 8, true, Some(Value::Token(1))),
        ];

        assert!(run(Class::Lww, &h).is_ok());
    }

    #[test]
    fn a_read_may_not_undo_a_write_that_already_finished() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Write(Value::Token(2)), 2, 3, true, None),
            call(Kind::Read, 4, 5, true, Some(Value::Token(1))),
            call(Kind::Read, 6, 7, true, Some(Value::Token(2))),
        ];

        assert!(run(Class::Lww, &h).is_err());
    }

    #[test]
    fn a_failed_write_may_or_may_not_have_landed() {
        let h = [call(Kind::Write(Value::Token(4)), 0, 1, false, None)];

        assert_eq!(
            run(Class::Lww, &h).unwrap(),
            BTreeSet::from([Value::Hole, Value::Token(4)])
        );
    }

    #[test]
    fn a_failed_write_read_back_is_legal() {
        let h = [
            call(Kind::Write(Value::Token(4)), 0, 1, false, None),
            call(Kind::Read, 2, 3, true, Some(Value::Token(4))),
        ];

        assert_eq!(
            run(Class::Lww, &h).unwrap(),
            BTreeSet::from([Value::Token(4)])
        );
    }

    #[test]
    fn a_failed_write_cannot_appear_after_it_was_read_as_absent() {
        let h = [
            call(Kind::Write(Value::Token(4)), 0, 1, false, None),
            call(Kind::Read, 2, 3, true, Some(Value::Hole)),
            call(Kind::Read, 4, 5, true, Some(Value::Token(4))),
        ];

        assert!(run(Class::Lww, &h).is_err());
    }

    #[test]
    fn an_immutable_page_is_filled_once() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Write(Value::Token(2)), 2, 3, false, None),
            call(Kind::Read, 4, 5, true, Some(Value::Token(1))),
        ];

        assert_eq!(
            run(Class::Immutable, &h).unwrap(),
            BTreeSet::from([Value::Token(1)])
        );
    }

    #[test]
    fn an_immutable_page_that_took_a_second_fill_is_caught() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Write(Value::Token(2)), 2, 3, true, None),
        ];

        assert!(run(Class::Immutable, &h).is_err());
    }

    #[test]
    fn an_immutable_page_never_goes_back_to_a_hole() {
        let h = [
            call(Kind::Write(Value::Token(1)), 0, 1, true, None),
            call(Kind::Read, 2, 3, true, Some(Value::Hole)),
        ];

        assert!(run(Class::Immutable, &h).is_err());
    }

    #[test]
    fn nodes_that_disagree_after_everything_settles_are_caught() {
        let mut p = Page::new(Class::Lww);
        let at = p.begin(Kind::Write(Value::Token(1)), 0, 0);

        p.finish(at, 1, true, None);
        p.settle().unwrap();

        // Two reads of a quiet page, from two nodes, that do not match.
        for (who, saw) in [(0, Value::Token(1)), (1, Value::Hole)] {
            let at = p.begin(Kind::Read, 2, who);

            p.finish(at, 3, true, Some(saw));
        }

        assert!(p.settle().is_err());
    }

    #[test]
    fn history_composes_across_settle_points() {
        let mut p = Page::new(Class::Lww);

        for t in 1..40u32 {
            let at = p.begin(Kind::Write(Value::Token(t)), t as u64 * 2, 0);

            p.finish(at, t as u64 * 2 + 1, true, None);
            p.settle().unwrap();
        }

        let at = p.begin(Kind::Read, 100, 0);

        p.finish(at, 101, true, Some(Value::Token(39)));
        p.settle().unwrap();
        assert_eq!(p.possible(), &BTreeSet::from([Value::Token(39)]));
    }

    #[test]
    fn a_page_only_settles_once_it_is_idle() {
        let mut p = Page::new(Class::Lww);
        let a = p.begin(Kind::Write(Value::Token(1)), 0, 0);
        let b = p.begin(Kind::Read, 1, 0);

        p.finish(b, 2, true, Some(Value::Hole));
        p.settle().unwrap();
        assert_eq!(p.pending(), 2, "an in flight call holds the segment open");

        p.finish(a, 3, true, None);
        p.settle().unwrap();
        assert_eq!(p.possible(), &BTreeSet::from([Value::Token(1)]));
    }
}
