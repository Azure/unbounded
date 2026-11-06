//! The single choice stream every nondeterministic decision is drawn from.
//!
//! A run of the DST is a pure function of its scenario and its [`Choices`].
//! Which role steps next, operation latency, fault timing, workload
//! generation and the seeds of every simulated worker's entropy all come from
//! here, so recording the stream is enough to replay a run exactly.
//!
//! # Backends
//!
//! - [`Choices::seeded`]: a xoshiro256** generator. CI runs a fixed seed set.
//! - [`Choices::from_bytes`]: a byte slice, eight little-endian bytes per
//!   draw. Any byte source, such as a fuzzer, can drive a run this way.
//! - [`Choices::replay`]: a recorded list of raw draws (see
//!   [`Choices::recording`]); this is what the shrinker edits and what
//!   regression seeds check in.
//!
//! Finite backends return zero once exhausted, so a truncated stream is still
//! a valid run.
//!
//! # Shrinking contract
//!
//! Every derived choice maps the raw draw `0` to its "simplest" value: the
//! lower bound of a range, the first weighted option, `false` for a
//! probability. So zeroing or deleting raw draws (what [`shrink`] does) moves
//! a failing run toward fewer faults, smaller values and default orderings.
//! New choice helpers must keep this property.
//!
//! # Sharing
//!
//! `Choices` is a cheap handle: clones share one cursor and one recording.
//! It implements [`racer_io::ctx::ChoiceSource`], so simulated OS scheduling
//! and worker entropy draw from the same stream.
//! Simulated components (origins, nodes) hold a clone, and the world stays
//! single-threaded, so the interleaving of their draws is itself determined by
//! the schedule.
use std::{
    fmt,
    sync::{Arc, Mutex, MutexGuard},
};

/// Handle to the shared choice stream.
#[derive(Clone)]
pub struct Choices(Arc<Mutex<Inner>>);

struct Inner {
    backend: Backend,
    /// Raw draws so far, when recording.
    record: Option<Vec<u64>>,
    /// Number of raw draws so far.
    drawn: u64,
}

enum Backend {
    Seeded([u64; 4]),
    Bytes { data: Vec<u8>, pos: usize },
    Words { data: Vec<u64>, pos: usize },
}

impl fmt::Debug for Choices {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let inner = self.lock();
        let kind = match &inner.backend {
            Backend::Seeded(_) => "seeded",
            Backend::Bytes { .. } => "bytes",
            Backend::Words { .. } => "replay",
        };
        f.debug_struct("Choices")
            .field("backend", &kind)
            .field("drawn", &inner.drawn)
            .finish()
    }
}

/// SplitMix64 finalizer.
pub const fn mix(mut z: u64) -> u64 {
    z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    z ^ (z >> 31)
}

impl racer_io::ctx::ChoiceSource for Choices {
    /// One raw draw ([`Choices::raw`]), so racer-io's simulated OS and
    /// entropy share this stream.
    fn next_u64(&mut self) -> u64 {
        self.raw()
    }
}

impl Choices {
    /// The stream state. The world is single-threaded; the mutex only makes
    /// the handle `Send` so racer-io's `Rng` can hold it.
    fn lock(&self) -> MutexGuard<'_, Inner> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn with(backend: Backend) -> Self {
        Self(Arc::new(Mutex::new(Inner {
            backend,
            record: None,
            drawn: 0,
        })))
    }

    /// A pseudo-random stream from `seed` (CI).
    pub fn seeded(seed: u64) -> Self {
        let mut state = seed;
        let mut next = || {
            state = state.wrapping_add(0x9E37_79B9_7F4A_7C15);
            mix(state)
        };
        Self::with(Backend::Seeded([next(), next(), next(), next()]))
    }

    /// A stream read from fuzzer bytes, eight little-endian bytes per draw.
    pub fn from_bytes(bytes: &[u8]) -> Self {
        Self::with(Backend::Bytes {
            data: bytes.to_vec(),
            pos: 0,
        })
    }

    /// Replay previously recorded raw draws.
    pub fn replay(words: &[u64]) -> Self {
        Self::with(Backend::Words {
            data: words.to_vec(),
            pos: 0,
        })
    }

    /// Start recording raw draws from now on (see [`Self::recorded`]).
    pub fn recording(self) -> Self {
        self.lock().record = Some(Vec::new());
        self
    }

    /// Raw draws recorded so far; replaying them reproduces the run.
    pub fn recorded(&self) -> Option<Vec<u64>> {
        self.lock().record.clone()
    }

    /// Number of raw draws taken so far.
    pub fn drawn(&self) -> u64 {
        self.lock().drawn
    }

    /// One raw 64-bit draw. Every other helper is built on this.
    pub fn raw(&self) -> u64 {
        let mut inner = self.lock();
        let value = match &mut inner.backend {
            Backend::Seeded(s) => {
                let result = s[1].wrapping_mul(5).rotate_left(7).wrapping_mul(9);
                let t = s[1] << 17;
                s[2] ^= s[0];
                s[3] ^= s[1];
                s[1] ^= s[2];
                s[0] ^= s[3];
                s[2] ^= t;
                s[3] = s[3].rotate_left(45);
                result
            }
            Backend::Bytes { data, pos } => {
                let mut word = [0u8; 8];
                let end = (*pos + 8).min(data.len());
                if *pos < end {
                    word[..end - *pos].copy_from_slice(&data[*pos..end]);
                }
                *pos = end;
                u64::from_le_bytes(word)
            }
            Backend::Words { data, pos } => {
                let value = data.get(*pos).copied().unwrap_or(0);
                *pos += 1;
                value
            }
        };
        inner.drawn += 1;
        if let Some(record) = &mut inner.record {
            record.push(value);
        }
        value
    }

    /// A value in `0..bound`; `0` when `bound` is zero.
    pub fn below(&self, bound: u64) -> u64 {
        if bound <= 1 {
            // Still consume a draw so the stream layout does not depend on bounds.
            self.raw();
            return 0;
        }
        self.raw() % bound
    }

    /// A value in `lo..=hi` (`lo` when `hi < lo`).
    pub fn range(&self, lo: u64, hi: u64) -> u64 {
        if hi <= lo {
            self.raw();
            return lo;
        }
        lo + self.below((hi - lo).saturating_add(1))
    }

    /// An index into a slice of `len` items.
    pub fn index(&self, len: usize) -> usize {
        self.below(len as u64) as usize
    }

    /// `true` with probability `num / den`. Raw `0` yields `false`.
    pub fn chance(&self, num: u64, den: u64) -> bool {
        if den == 0 {
            self.raw();
            return false;
        }
        self.below(den) >= den - num.min(den)
    }

    /// `true` with probability `p` (clamped to `[0, 1]`). Raw `0` yields `false`.
    pub fn bool(&self, p: f64) -> bool {
        const SCALE: u64 = 1 << 32;
        let p = if p.is_nan() { 0.0 } else { p.clamp(0.0, 1.0) };
        self.chance((p * SCALE as f64) as u64, SCALE)
    }

    /// Index of an option chosen with the given weights; raw `0` picks the
    /// first option with nonzero weight. All-zero weights pick index 0.
    pub fn weighted(&self, weights: &[u64]) -> usize {
        let total: u64 = weights.iter().fold(0u64, |a, w| a.saturating_add(*w));
        let mut target = self.below(total);
        for (i, w) in weights.iter().enumerate() {
            if target < *w {
                return i;
            }
            target -= w;
        }
        0
    }

    /// A uniform float in `[0, 1)` (53 bits).
    pub fn unit(&self) -> f64 {
        (self.raw() >> 11) as f64 * (1.0 / (1u64 << 53) as f64)
    }

    /// A seed for a derived deterministic generator (for example a
    /// simulated worker's [`racer_io::Rng`]).
    pub fn seed(&self) -> u64 {
        self.raw()
    }
}

/// Shrink a failing recorded stream.
///
/// `fails` replays a candidate stream and reports whether it still fails.
/// Passes delete chunks, then zero words, then halve words, until a full pass
/// makes no progress or `budget` replays are spent. The result still fails
/// (or is the input if it never did).
pub fn shrink(words: &[u64], budget: usize, mut fails: impl FnMut(&[u64]) -> bool) -> Vec<u64> {
    let mut best = words.to_vec();
    let mut spent = 0;
    let mut attempt = |candidate: &[u64], spent: &mut usize| -> bool {
        if *spent >= budget {
            return false;
        }
        *spent += 1;
        fails(candidate)
    };
    loop {
        let mut progress = false;
        for size in [16usize, 8, 4, 2, 1] {
            let mut start = 0;
            while start + size <= best.len() {
                let mut candidate = best.clone();
                candidate.drain(start..start + size);
                if attempt(&candidate, &mut spent) {
                    best = candidate;
                    progress = true;
                } else {
                    start += 1;
                }
            }
        }
        for i in 0..best.len() {
            for value in [0, best[i] / 2] {
                if value == best[i] {
                    continue;
                }
                let mut candidate = best.clone();
                candidate[i] = value;
                if attempt(&candidate, &mut spent) {
                    best = candidate;
                    progress = true;
                    break;
                }
            }
        }
        // Trailing zeros are implied by exhaustion.
        while best.last() == Some(&0) {
            best.pop();
        }
        if !progress || spent >= budget {
            return best;
        }
    }
}

/// How one checked-in run reproduces its choice stream.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Source {
    /// [`Choices::seeded`].
    Seed(u64),
    /// [`Choices::replay`] of a (usually shrunk) recording.
    Words(Vec<u64>),
}

impl Source {
    /// The stream this source describes.
    pub fn choices(&self) -> Choices {
        match self {
            Self::Seed(seed) => Choices::seeded(*seed),
            Self::Words(words) => Choices::replay(words),
        }
    }
}

impl fmt::Display for Source {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Seed(seed) => write!(f, "seed={seed}"),
            Self::Words(words) => {
                f.write_str("words=")?;
                for (i, w) in words.iter().enumerate() {
                    if i > 0 {
                        f.write_str(",")?;
                    }
                    write!(f, "{w:x}")?;
                }
                Ok(())
            }
        }
    }
}

/// One line of a seed file: `<scenario> seed=<u64>` or
/// `<scenario> words=<hex>,<hex>,...`.
///
/// Seed files (`dst/seeds/*.txt`) hold one entry per line; `#` starts a
/// comment. CI seeds use `seed=`; shrunk regressions found by CI or the
/// fuzzer are checked in with `words=` so they keep reproducing even if the
/// seeded generator changes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SeedEntry {
    /// Scenario name.
    pub scenario: String,
    /// Choice stream.
    pub source: Source,
}

impl fmt::Display for SeedEntry {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} {}", self.scenario, self.source)
    }
}

/// Parse a seed file. Errors name the 1-based line.
pub fn parse_seeds(text: &str) -> Result<Vec<SeedEntry>, String> {
    let mut out = Vec::new();
    for (n, line) in text.lines().enumerate() {
        let line = line.split('#').next().unwrap_or("").trim();
        if line.is_empty() {
            continue;
        }
        let err = |m: &str| format!("line {}: {m}: {line:?}", n + 1);
        let mut parts = line.split_whitespace();
        let scenario = parts.next().ok_or_else(|| err("missing scenario"))?;
        let spec = parts.next().ok_or_else(|| err("missing seed= or words="))?;
        if parts.next().is_some() {
            return Err(err("trailing fields"));
        }
        let source = if let Some(seed) = spec.strip_prefix("seed=") {
            Source::Seed(seed.parse().map_err(|_| err("bad seed"))?)
        } else if let Some(words) = spec.strip_prefix("words=") {
            let words = if words.is_empty() {
                Vec::new()
            } else {
                words
                    .split(',')
                    .map(|w| u64::from_str_radix(w, 16))
                    .collect::<Result<_, _>>()
                    .map_err(|_| err("bad hex word"))?
            };
            Source::Words(words)
        } else {
            return Err(err("expected seed= or words="));
        };
        out.push(SeedEntry {
            scenario: scenario.to_owned(),
            source,
        });
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn seeded_is_reproducible_and_seed_sensitive() {
        let a: Vec<u64> = (0..8).map(|_| Choices::seeded(5).raw()).collect();
        let c = Choices::seeded(5);
        let b: Vec<u64> = (0..8).map(|_| c.raw()).collect();
        assert!(a.iter().all(|v| *v == a[0]), "fresh handles restart");
        assert_eq!(a[0], b[0]);
        assert_ne!(b[0], b[1]);
        assert_ne!(Choices::seeded(6).raw(), b[0]);
    }

    #[test]
    fn clones_share_one_cursor() {
        let a = Choices::seeded(1);
        let b = a.clone();
        let x = a.raw();
        let y = b.raw();
        assert_ne!(x, y);
        assert_eq!(a.drawn(), 2);
    }

    #[test]
    fn bytes_backend_reads_le_words_then_zeros() {
        let c = Choices::from_bytes(&[1, 0, 0, 0, 0, 0, 0, 0, 2, 3]);
        assert_eq!(c.raw(), 1);
        assert_eq!(c.raw(), 0x0302);
        assert_eq!(c.raw(), 0);
        assert_eq!(c.drawn(), 3);
    }

    #[test]
    fn record_and_replay_reproduce_derived_choices() {
        let c = Choices::seeded(42).recording();
        let first: Vec<u64> = (0..50).map(|i| c.range(3, 3 + i)).collect();
        let words = c.recorded().unwrap();
        assert_eq!(words.len(), 50);
        let r = Choices::replay(&words);
        let again: Vec<u64> = (0..50).map(|i| r.range(3, 3 + i)).collect();
        assert_eq!(first, again);
    }

    #[test]
    fn zero_draws_map_to_simplest_values() {
        let z = Choices::replay(&[]);
        assert_eq!(z.below(10), 0);
        assert_eq!(z.range(7, 9), 7);
        assert!(!z.chance(99, 100));
        assert!(!z.bool(0.999));
        assert_eq!(z.weighted(&[0, 5, 1]), 1);
        assert_eq!(z.unit(), 0.0);
    }

    #[test]
    fn helpers_respect_bounds_and_edges() {
        let c = Choices::seeded(9);
        for _ in 0..1000 {
            assert!(c.below(7) < 7);
            let v = c.range(10, 12);
            assert!((10..=12).contains(&v));
            assert!(c.unit() < 1.0);
            assert_ne!(c.weighted(&[1, 0, 3]), 1);
        }
        assert_eq!(c.below(0), 0);
        assert_eq!(c.range(5, 1), 5);
        assert!(c.chance(1, 1));
        assert!(!c.chance(0, 1));
        assert!(!c.chance(1, 0));
        assert!(c.bool(1.0));
        assert!(!c.bool(0.0));
        assert!(!c.bool(f64::NAN));
        assert_eq!(c.weighted(&[]), 0);
        assert_eq!(c.weighted(&[0, 0]), 0);
        c.range(0, u64::MAX);
        // Rough frequency check of chance().
        let hits = (0..10_000).filter(|_| c.chance(1, 4)).count();
        assert!((2_000..3_000).contains(&hits), "{hits}");
    }

    #[test]
    fn shrink_minimizes_a_failing_stream() {
        // Fails when any word is >= 100.
        let fails = |w: &[u64]| w.iter().any(|v| *v >= 100);
        let input = vec![5, 7, 1_000_000, 9, 3, 12345, 0, 0];
        let out = shrink(&input, 10_000, fails);
        assert_eq!(out.len(), 1, "{out:?}");
        assert!((100..200).contains(&out[0]), "{out:?}");
        assert!(fails(&out));
        // Never-failing input is returned unchanged (minus nothing).
        assert_eq!(shrink(&[1, 2], 100, |_| false), vec![1, 2]);
        // Budget is respected.
        let mut calls = 0;
        shrink(&input, 3, |w| {
            calls += 1;
            fails(w)
        });
        assert_eq!(calls, 3);
    }

    #[test]
    fn seed_files_round_trip_and_reject_garbage() {
        let text = "# ci\nlane-pair seed=7\n\nlane-pair words=0,ff,10 # shrunk\nx words=\n";
        let seeds = parse_seeds(text).unwrap();
        assert_eq!(seeds.len(), 3);
        assert_eq!(seeds[0].source, Source::Seed(7));
        assert_eq!(seeds[1].source, Source::Words(vec![0, 255, 16]));
        assert_eq!(seeds[2].source, Source::Words(vec![]));
        let printed: Vec<String> = seeds.iter().map(ToString::to_string).collect();
        assert_eq!(
            parse_seeds(&printed.join("\n")).unwrap(),
            seeds,
            "display round-trips"
        );
        for bad in ["x", "x seed=a", "x words=zz", "x foo=1", "x seed=1 y"] {
            assert!(parse_seeds(bad).is_err(), "{bad}");
        }
        assert_eq!(Source::Words(vec![1, 2]).choices().raw(), 1);
    }
}
