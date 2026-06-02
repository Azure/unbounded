// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP byte ranges and stripe-set computation.
//!
//! Two concerns live here, both pure and cross-platform:
//!
//! - [`ByteRange`] models the `Range: bytes=...` forms an S3 client may
//!   send (closed, prefix `N-`, suffix `-N`) and resolves them against
//!   a known object length into a concrete `[start, end)` half-open
//!   byte span.
//! - [`stripe_set`] turns a resolved byte span plus the backend's
//!   `stripe_size_bytes` into the ordered list of stripes that must be
//!   fetched, each with the sub-range *within that stripe* the response
//!   actually needs. The GET path feeds this into `stripe_key` +
//!   `pool.read`.
//!
//! Both are deliberately free of any I/O so they can be unit-tested
//! exhaustively without a socket ring or a pool.

/// A parsed but unresolved HTTP `Range` header value. S3 only uses the
/// single-range `bytes=` forms; multi-range is intentionally not
/// modeled (S3 does not require it and it is not on the zero-copy hot
/// path).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ByteRange {
    /// `bytes=START-END`, both inclusive as the HTTP spec defines.
    Closed { start: u64, end: u64 },
    /// `bytes=START-` - from `start` to the end of the object.
    Prefix { start: u64 },
    /// `bytes=-N` - the final `N` bytes of the object.
    Suffix { last: u64 },
}

/// A concrete, resolved half-open byte span `[start, end)` within an
/// object of known length. Produced by [`ByteRange::resolve`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ResolvedRange {
    pub start: u64,
    /// Exclusive end. `end - start` is the byte count; an empty span
    /// (`start == end`) is only produced for a zero-length object.
    pub end: u64,
}

impl ResolvedRange {
    pub fn len(&self) -> u64 {
        self.end - self.start
    }

    pub fn is_empty(&self) -> bool {
        self.start == self.end
    }
}

/// One stripe that a read must touch, with the byte sub-range *relative
/// to the start of the stripe* that the response needs from it.
///
/// `intra_offset` and `intra_len` are exactly the
/// `(intra_stripe_offset, intra_stripe_len)` arguments the GET path
/// passes to `pool.read` for this stripe.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct StripeSlice {
    pub stripe_idx: u64,
    pub intra_offset: u64,
    pub intra_len: u64,
}

/// Error parsing a `Range` header value or resolving it against an
/// object length.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RangeError {
    /// The header did not start with the required `bytes=` unit, or was
    /// otherwise syntactically malformed.
    Malformed,
    /// The numeric components did not parse as `u64`.
    BadNumber,
    /// A closed range whose `start > end`.
    Inverted,
    /// The range is wholly outside the object (HTTP 416). Carries the
    /// object length so the caller can build a `Content-Range: bytes
    /// */LEN` response.
    Unsatisfiable { object_len: u64 },
}

impl ByteRange {
    /// Parse a single `Range` header *value* (the part after the
    /// `Range:` field name), e.g. `"bytes=0-1023"`, `"bytes=1024-"`,
    /// `"bytes=-512"`.
    ///
    /// Only the single-range `bytes=` unit is accepted. Whitespace
    /// around the value is tolerated; an embedded comma (multi-range)
    /// or any other unit is [`RangeError::Malformed`].
    pub fn parse(value: &str) -> Result<Self, RangeError> {
        let value = value.trim();
        let spec = value.strip_prefix("bytes=").ok_or(RangeError::Malformed)?;
        // Multi-range is not supported; reject rather than silently
        // serving only the first range.
        if spec.contains(',') {
            return Err(RangeError::Malformed);
        }
        let (lhs, rhs) = spec.split_once('-').ok_or(RangeError::Malformed)?;
        let lhs = lhs.trim();
        let rhs = rhs.trim();
        match (lhs.is_empty(), rhs.is_empty()) {
            // "-N": suffix.
            (true, false) => {
                let last = parse_u64(rhs)?;
                Ok(ByteRange::Suffix { last })
            }
            // "N-": prefix.
            (false, true) => {
                let start = parse_u64(lhs)?;
                Ok(ByteRange::Prefix { start })
            }
            // "N-M": closed.
            (false, false) => {
                let start = parse_u64(lhs)?;
                let end = parse_u64(rhs)?;
                if start > end {
                    return Err(RangeError::Inverted);
                }
                Ok(ByteRange::Closed { start, end })
            }
            // "-" with nothing on either side is malformed.
            (true, true) => Err(RangeError::Malformed),
        }
    }

    /// Resolve this range against an object of `object_len` bytes into
    /// a concrete half-open `[start, end)` span, clamping the end to
    /// the object length (HTTP allows an end past EOF).
    ///
    /// Returns [`RangeError::Unsatisfiable`] when the requested span
    /// lies entirely past the end of the object (HTTP 416), matching
    /// S3's behavior.
    pub fn resolve(&self, object_len: u64) -> Result<ResolvedRange, RangeError> {
        match *self {
            ByteRange::Closed { start, end } => {
                if start >= object_len {
                    return Err(RangeError::Unsatisfiable { object_len });
                }
                // `end` is inclusive; clamp to the last byte.
                let end_excl = end.saturating_add(1).min(object_len);
                Ok(ResolvedRange {
                    start,
                    end: end_excl,
                })
            }
            ByteRange::Prefix { start } => {
                if start >= object_len {
                    return Err(RangeError::Unsatisfiable { object_len });
                }
                Ok(ResolvedRange {
                    start,
                    end: object_len,
                })
            }
            ByteRange::Suffix { last } => {
                if last == 0 || object_len == 0 {
                    // "bytes=-0" requests zero bytes from the end, and a
                    // suffix of any size against a zero-length object can
                    // only yield an empty span; HTTP treats both as
                    // unsatisfiable. Guarding `object_len == 0` here keeps
                    // the resolved-empty invariant (Closed/Prefix already
                    // reject empty objects via `start >= object_len`) so
                    // callers never see a zero-length `[0, 0)` span and
                    // underflow when computing an inclusive end.
                    return Err(RangeError::Unsatisfiable { object_len });
                }
                let start = object_len.saturating_sub(last);
                Ok(ResolvedRange {
                    start,
                    end: object_len,
                })
            }
        }
    }
}

/// The full object as a resolved span. Convenience for the no-`Range`
/// GET/HEAD path, where the whole object is served.
pub fn full_object(object_len: u64) -> ResolvedRange {
    ResolvedRange {
        start: 0,
        end: object_len,
    }
}

/// Compute the ordered set of stripes (and their intra-stripe
/// sub-ranges) that cover the resolved byte span `range`, given the
/// backend's `stripe_size_bytes`.
///
/// The GET path iterates the returned slices in order, deriving each
/// stripe's `StripeKey` from `stripe_idx` and calling `pool.read(&req,
/// intra_offset, intra_len)`.
///
/// Invariants (all asserted by the unit tests):
/// - Stripe indices are contiguous and ascending.
/// - Each `intra_offset + intra_len <= stripe_size`.
/// - The concatenation of all slices reproduces exactly `range`.
/// - An empty `range` (zero-length object or empty span) yields no
///   stripes.
///
/// `stripe_size` must be non-zero; a zero size is a configuration bug
/// and panics in debug builds (returns an empty set in release to avoid
/// a divide-by-zero, since this is pure logic with no fallible return).
pub fn stripe_set(range: ResolvedRange, stripe_size: u64) -> Vec<StripeSlice> {
    debug_assert!(stripe_size > 0, "stripe_size must be non-zero");
    if stripe_size == 0 || range.is_empty() {
        return Vec::new();
    }

    let first = range.start / stripe_size;
    // `end` is exclusive; the last touched stripe is the one containing
    // the last byte at index `end - 1`.
    let last = (range.end - 1) / stripe_size;

    let mut out = Vec::with_capacity((last - first + 1) as usize);
    for stripe_idx in first..=last {
        let stripe_start = stripe_idx * stripe_size;
        let stripe_end = stripe_start + stripe_size;
        // Intersect [range.start, range.end) with this stripe's span.
        let lo = range.start.max(stripe_start);
        let hi = range.end.min(stripe_end);
        out.push(StripeSlice {
            stripe_idx,
            intra_offset: lo - stripe_start,
            intra_len: hi - lo,
        });
    }
    out
}

fn parse_u64(s: &str) -> Result<u64, RangeError> {
    s.parse::<u64>().map_err(|_| RangeError::BadNumber)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_closed_prefix_suffix() {
        assert_eq!(
            ByteRange::parse("bytes=0-1023"),
            Ok(ByteRange::Closed {
                start: 0,
                end: 1023
            })
        );
        assert_eq!(
            ByteRange::parse("bytes=1024-"),
            Ok(ByteRange::Prefix { start: 1024 })
        );
        assert_eq!(
            ByteRange::parse("bytes=-512"),
            Ok(ByteRange::Suffix { last: 512 })
        );
        // Whitespace around the value and components is tolerated.
        assert_eq!(
            ByteRange::parse("  bytes=10 - 20 "),
            Ok(ByteRange::Closed { start: 10, end: 20 })
        );
    }

    #[test]
    fn parse_rejects_malformed() {
        assert_eq!(ByteRange::parse("0-1023"), Err(RangeError::Malformed));
        assert_eq!(ByteRange::parse("items=0-1"), Err(RangeError::Malformed));
        assert_eq!(ByteRange::parse("bytes="), Err(RangeError::Malformed));
        assert_eq!(ByteRange::parse("bytes=-"), Err(RangeError::Malformed));
        // Multi-range is explicitly unsupported.
        assert_eq!(
            ByteRange::parse("bytes=0-1,2-3"),
            Err(RangeError::Malformed)
        );
        assert_eq!(
            ByteRange::parse("bytes=abc-def"),
            Err(RangeError::BadNumber)
        );
        assert_eq!(ByteRange::parse("bytes=5-2"), Err(RangeError::Inverted));
    }

    #[test]
    fn resolve_closed_clamps_end() {
        let r = ByteRange::Closed { start: 0, end: 99 };
        assert_eq!(r.resolve(50), Ok(ResolvedRange { start: 0, end: 50 }));
        assert_eq!(r.resolve(1000), Ok(ResolvedRange { start: 0, end: 100 }));
    }

    #[test]
    fn resolve_prefix_and_suffix() {
        assert_eq!(
            ByteRange::Prefix { start: 10 }.resolve(100),
            Ok(ResolvedRange {
                start: 10,
                end: 100
            })
        );
        assert_eq!(
            ByteRange::Suffix { last: 30 }.resolve(100),
            Ok(ResolvedRange {
                start: 70,
                end: 100
            })
        );
        // Suffix larger than the object clamps the start to 0.
        assert_eq!(
            ByteRange::Suffix { last: 500 }.resolve(100),
            Ok(ResolvedRange { start: 0, end: 100 })
        );
    }

    #[test]
    fn resolve_unsatisfiable() {
        assert_eq!(
            ByteRange::Closed {
                start: 100,
                end: 200
            }
            .resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        );
        assert_eq!(
            ByteRange::Prefix { start: 100 }.resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        );
        assert_eq!(
            ByteRange::Suffix { last: 0 }.resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        );
    }

    #[test]
    fn resolve_suffix_zero_length_object_is_unsatisfiable() {
        // `bytes=-5` against a 0-byte object must not resolve to an empty
        // `[0, 0)` span (which would underflow `end - 1` in the partial
        // head); it is unsatisfiable, like Closed/Prefix on an empty
        // object.
        assert_eq!(
            ByteRange::Suffix { last: 5 }.resolve(0),
            Err(RangeError::Unsatisfiable { object_len: 0 })
        );
        // Regression guard: a normal suffix on a non-zero object still
        // resolves to a valid non-empty span.
        assert_eq!(
            ByteRange::Suffix { last: 5 }.resolve(20),
            Ok(ResolvedRange { start: 15, end: 20 })
        );
    }

    /// Helper: assert that a stripe set reproduces exactly the input
    /// span and respects the per-stripe invariants.
    fn assert_covers(range: ResolvedRange, stripe_size: u64, expected: &[StripeSlice]) {
        let got = stripe_set(range, stripe_size);
        assert_eq!(got, expected, "slice list mismatch");

        // Reconstruct the absolute byte span from the slices and check
        // it equals the input.
        let mut covered = 0u64;
        let mut prev_idx: Option<u64> = None;
        let mut abs_start: Option<u64> = None;
        let mut abs_end = 0u64;
        for s in &got {
            assert!(s.intra_offset + s.intra_len <= stripe_size);
            assert!(s.intra_len > 0, "no empty slices");
            if let Some(p) = prev_idx {
                assert_eq!(s.stripe_idx, p + 1, "stripes contiguous and ascending");
            }
            prev_idx = Some(s.stripe_idx);
            let start = s.stripe_idx * stripe_size + s.intra_offset;
            if abs_start.is_none() {
                abs_start = Some(start);
            }
            assert_eq!(start, abs_end.max(start));
            abs_end = start + s.intra_len;
            covered += s.intra_len;
        }
        if range.is_empty() {
            assert!(got.is_empty());
        } else {
            assert_eq!(abs_start, Some(range.start));
            assert_eq!(abs_end, range.end);
            assert_eq!(covered, range.len());
        }
    }

    #[test]
    fn stripe_set_full_object() {
        // 10 bytes, stripe 4 -> stripes 0 [0,4), 1 [0,4), 2 [0,2).
        assert_covers(
            full_object(10),
            4,
            &[
                StripeSlice {
                    stripe_idx: 0,
                    intra_offset: 0,
                    intra_len: 4,
                },
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4,
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2,
                },
            ],
        );
    }

    #[test]
    fn stripe_set_mid_object_within_one_stripe() {
        // [5,7) with stripe 4 -> stripe 1, intra [1,3).
        assert_covers(
            ResolvedRange { start: 5, end: 7 },
            4,
            &[StripeSlice {
                stripe_idx: 1,
                intra_offset: 1,
                intra_len: 2,
            }],
        );
    }

    #[test]
    fn stripe_set_spans_boundary() {
        // [3,9) with stripe 4 -> stripe 0 [3,4), stripe 1 [0,4),
        // stripe 2 [0,1).
        assert_covers(
            ResolvedRange { start: 3, end: 9 },
            4,
            &[
                StripeSlice {
                    stripe_idx: 0,
                    intra_offset: 3,
                    intra_len: 1,
                },
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4,
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 1,
                },
            ],
        );
    }

    #[test]
    fn stripe_set_single_byte() {
        // [6,7) with stripe 4 -> stripe 1, intra [2,3).
        assert_covers(
            ResolvedRange { start: 6, end: 7 },
            4,
            &[StripeSlice {
                stripe_idx: 1,
                intra_offset: 2,
                intra_len: 1,
            }],
        );
    }

    #[test]
    fn stripe_set_suffix_range() {
        // Object 10, suffix 3 -> [7,10), stripe 4 -> stripe 1 [3,4),
        // stripe 2 [0,2).
        let r = ByteRange::Suffix { last: 3 }.resolve(10).unwrap();
        assert_covers(
            r,
            4,
            &[
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 3,
                    intra_len: 1,
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2,
                },
            ],
        );
    }

    #[test]
    fn stripe_set_prefix_range() {
        // Object 10, prefix from 4 -> [4,10), stripe 4 -> stripe 1
        // [0,4), stripe 2 [0,2).
        let r = ByteRange::Prefix { start: 4 }.resolve(10).unwrap();
        assert_covers(
            r,
            4,
            &[
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4,
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2,
                },
            ],
        );
    }

    #[test]
    fn stripe_set_empty_object() {
        assert_covers(full_object(0), 4, &[]);
        assert_covers(ResolvedRange { start: 5, end: 5 }, 4, &[]);
    }

    #[test]
    fn stripe_set_exact_stripe_alignment() {
        // [4,8) with stripe 4 -> exactly stripe 1 [0,4).
        assert_covers(
            ResolvedRange { start: 4, end: 8 },
            4,
            &[StripeSlice {
                stripe_idx: 1,
                intra_offset: 0,
                intra_len: 4,
            }],
        );
    }

    #[test]
    fn stripe_set_large_realistic() {
        // 4 MiB stripes, a 9 MiB object, range covering [5MiB, 9MiB).
        let mib = 1024 * 1024;
        let stripe = 4 * mib;
        let r = ResolvedRange {
            start: 5 * mib,
            end: 9 * mib,
        };
        let got = stripe_set(r, stripe);
        assert_eq!(got.len(), 2);
        assert_eq!(got[0].stripe_idx, 1);
        assert_eq!(got[0].intra_offset, mib);
        assert_eq!(got[0].intra_len, 3 * mib);
        assert_eq!(got[1].stripe_idx, 2);
        assert_eq!(got[1].intra_offset, 0);
        assert_eq!(got[1].intra_len, mib);
    }
}
